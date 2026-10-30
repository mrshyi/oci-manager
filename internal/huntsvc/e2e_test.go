package huntsvc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"ocicore/internal/cryptobox"
	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

// 这一组测试补的是 P0 未满足的验收项：端到端确认「一次守候只产生一台实例」。
//
// 机制层已经有单测（三次尝试携带同一个 opc-retry-token），但机制正确不等于
// 结果正确——令牌生成位置、调用点标记、查重标签三处任意一处写错，现象都是
// 「一切正常，只是多了一台机器」，而多出来的那台会静默吃掉 ARM 免费额度。
// 只有把整条链路跑一遍才能发现。

// fakeOCI 是一个最小的 OCI 计算接口替身。
//
// 只实现守候真正会打的两个接口：创建实例与列出实例。其余一律 404，
// 这样一旦 attempt 的调用形状变了（多打了别的接口），测试会立刻失败。
type fakeOCI struct {
	mu sync.Mutex
	// instances 是"云端"真实存在的实例。
	instances []ociclient.Instance
	// posts 统计收到的创建请求数，含重试。
	posts int
	// tokens 记录令牌 → 首次创建的实例 ID，用来实现服务端幂等。
	tokens map[string]string

	// mode 决定创建请求怎么失败。
	mode failMode
}

type failMode int

const (
	// failNone 正常返回。
	failNone failMode = iota
	// failAfterCreate 建成之后才返回 500（响应在网络上丢了）。
	// 客户端会用同一个令牌重试，服务端应当返回第一次的结果而不是再建一台。
	failAfterCreate
	// failAlways 建成之后每次都返回 500。
	// 模拟"请求实际成功、但客户端始终拿不到响应"，逼下一轮走标签查重。
	failAlways
)

func newFakeOCI(mode failMode) *fakeOCI {
	return &fakeOCI{tokens: make(map[string]string), mode: mode}
}

func (f *fakeOCI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/20160918/instances", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			f.create(w, r)
		case http.MethodGet:
			f.list(w)
		default:
			http.Error(w, "不支持的方法", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func (f *fakeOCI) create(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.posts++
	token := r.Header.Get("opc-retry-token")

	// 服务端幂等：同一个令牌重复 POST 只返回第一次的结果。
	// 这正是 opc-retry-token 的语义，也是"超时重试不会多建一台"的根据。
	if id, ok := f.tokens[token]; ok && token != "" {
		if f.mode == failAlways {
			f.writeLost(w)
			return
		}
		f.writeInstance(w, http.StatusOK, f.byID(id))
		return
	}

	var body ociclient.LaunchInstanceRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	inst := ociclient.Instance{
		ID:                 "ocid1.instance.oc1..i" + strconv.Itoa(len(f.instances)+1),
		DisplayName:        body.DisplayName,
		AvailabilityDomain: body.AvailabilityDomain,
		Shape:              body.Shape,
		LifecycleState:     "RUNNING",
		FreeformTags:       body.FreeformTags,
	}
	f.instances = append(f.instances, inst)
	if token != "" {
		f.tokens[token] = inst.ID
	}

	if f.mode != failNone {
		f.writeLost(w)
		return
	}
	f.writeInstance(w, http.StatusOK, inst)
}

// writeLost 模拟「服务端已经建成，但响应没能回到客户端」。
//
// 带 Retry-After 只是为了把客户端的重试等待从默认的秒级压到 1 秒——
// 这个头不影响被测逻辑，只影响测试跑多久。
func (f *fakeOCI) writeLost(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(`{"code":"InternalError","message":"响应在网络上丢了"}`))
}

func (f *fakeOCI) list(w http.ResponseWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// OCI 的列表接口返回裸数组，不是 {"data": [...]}。
	if err := json.NewEncoder(w).Encode(f.instances); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (f *fakeOCI) writeInstance(w http.ResponseWriter, status int, inst ociclient.Instance) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(inst)
}

func (f *fakeOCI) byID(id string) ociclient.Instance {
	for _, inst := range f.instances {
		if inst.ID == id {
			return inst
		}
	}
	return ociclient.Instance{}
}

func (f *fakeOCI) snapshot() (posts, instances int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts, len(f.instances)
}

// rewriteTransport 把发往真实 OCI 域名的请求改投到本地 httptest。
//
// 不能用 srv.Client() 直接当传输层：它只换了 TLS 配置，URL 里的 host 还是
// oci.oraclecloud.com，请求会真的发出去。
type rewriteTransport struct {
	target *url.URL
	next   http.RoundTripper
}

func (t *rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	clone.Host = t.target.Host
	return t.next.RoundTrip(clone)
}

// fakeConn 是 Connector 的测试实现：不查库、不解密，直接给一份客户端。
type fakeConn struct {
	client *ociclient.Client
	acc    *store.Account
}

func (f fakeConn) ForID(context.Context, string) (*ociclient.Client, *store.Account, error) {
	return f.client, f.acc, nil
}

func newHuntStore(t *testing.T) *store.Store {
	t.Helper()
	key := make([]byte, cryptobox.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	box, err := cryptobox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "hunt.db"), box)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newTestClient(t *testing.T, srv *httptest.Server, key *rsa.PrivateKey) *ociclient.Client {
	t.Helper()
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ociclient.New(&ociclient.Credentials{
		TenancyOCID: "ocid1.tenancy.oc1..aaaa",
		UserOCID:    "ocid1.user.oc1..aaaa",
		Fingerprint: ociclient.FingerprintOf(&key.PublicKey),
		Region:      "ap-tokyo-1",
		PrivateKey:  key,
	}, ociclient.WithHTTPClient(&http.Client{
		Timeout:   15 * time.Second,
		Transport: &rewriteTransport{target: target, next: srv.Client().Transport},
	}))
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}
	return c
}

// newHuntFixture 组装一套"一个账号 + 一条任务 + 一个假 OCI"的现场。
//
// 账号必须真落库：hunt_tasks.account_id 上有外键。这同时也让任务与账号的
// 关联走一遍真实路径，而不是靠测试自己拼一个 ID。
func newHuntFixture(t *testing.T, mode failMode) (*Service, *store.Store, *store.HuntTask, *fakeOCI) {
	t.Helper()

	ctx := context.Background()
	st := newHuntStore(t)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	acc, err := st.CreateAccount(ctx, store.NewAccount{
		Alias:           "测试账号",
		Code:            "test",
		TenancyOCID:     "ocid1.tenancy.oc1..aaaa",
		UserOCID:        "ocid1.user.oc1..aaaa",
		Fingerprint:     ociclient.FingerprintOf(&key.PublicKey),
		PrivateKeyPEM:   string(keyPEM),
		DefaultRegion:   "ap-tokyo-1",
		CompartmentOCID: "ocid1.compartment.oc1..aaaa",
	})
	if err != nil {
		t.Fatalf("创建测试账号失败: %v", err)
	}

	fake := newFakeOCI(mode)
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	svc := New(Deps{
		Store: st,
		Conns: fakeConn{
			client: newTestClient(t, srv, key),
			acc:    acc,
		},
	})

	spec, err := EncodeSpec(Spec{
		DisplayName:    "arm-1",
		Shape:          "VM.Standard.A1.Flex",
		Ocpus:          2,
		MemoryInGBs:    12,
		ImageID:        "ocid1.image.oc1..aaaa",
		SubnetID:       "ocid1.subnet.oc1..aaaa", // 给定子网，跳过自动建网
		AssignPublicIP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.CreateHuntTask(ctx, store.HuntTask{
		AccountID: acc.ID,
		Region:    "ap-tokyo-1",
		Name:      "arm-1",
		Spec:      spec,
		ADs:       "AD-1", // 给定可用域，跳过 ListAvailabilityDomains
		// 容量预检默认开，这里关掉：假端点没有 capacityReport，
		// 打开只会多一次 404（按设计会放行，但会污染 posts 计数）。
		PrecheckCapacity: false,
		IntervalSeconds:  60,
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, st, task, fake
}

// TestHuntCreatesExactlyOneInstanceDespiteRetries 覆盖"响应丢失 + 重试"。
//
// 场景：服务端第一次 POST 已经建成实例，但响应丢了（500）。客户端用同一个
// 令牌重试，服务端必须返回第一次的结果，而不是再建一台。
//
// 三处防线里，这里验证的是前两处：令牌在重试循环外生成（否则重试会换新
// 令牌、服务端当成新请求）、创建调用点标了 Idempotent（否则压根不带令牌）。
func TestHuntCreatesExactlyOneInstanceDespiteRetries(t *testing.T) {
	svc, st, task, fake := newHuntFixture(t, failAfterCreate)

	svc.attempt(context.Background(), task)

	got, err := st.GetHuntTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.HuntSucceeded {
		t.Fatalf("任务状态 %q，期望 %q（最后错误：%s）", got.State, store.HuntSucceeded, got.LastError)
	}

	posts, instances := fake.snapshot()
	if instances != 1 {
		t.Fatalf("云端有 %d 台实例，期望恰好 1 台——重复创建会静默吃掉免费额度", instances)
	}
	if posts < 2 {
		t.Fatalf("只收到 %d 次创建请求，重试路径没有被覆盖到", posts)
	}
	if got.InstanceID == "" {
		t.Fatal("任务没有记下实例 ID")
	}
}

// TestHuntFindsInstanceFromPreviousRound 覆盖"上一轮其实已经建成"。
//
// 场景：服务端每轮都返回 500，客户端始终拿不到响应，第一轮只能按瞬时错误
// 重新排期。第二轮开始前先按标签查一遍——查到就直接宣布成功，不再发创建
// 请求。没有这道查重的话，每一轮都会多建一台。
func TestHuntFindsInstanceFromPreviousRound(t *testing.T) {
	svc, st, task, fake := newHuntFixture(t, failAlways)

	// 第一轮：全部失败，任务回到运行中并排了下一轮。
	svc.attempt(context.Background(), task)
	first, err := st.GetHuntTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != store.HuntRunning {
		t.Fatalf("第一轮之后状态 %q，期望 %q", first.State, store.HuntRunning)
	}
	if first.Attempts != 1 {
		t.Fatalf("第一轮之后 attempts = %d，期望 1", first.Attempts)
	}

	postsAfterFirst, instancesAfterFirst := fake.snapshot()
	if instancesAfterFirst != 1 {
		t.Fatalf("第一轮之后云端有 %d 台实例，期望 1 台", instancesAfterFirst)
	}

	// 第二轮：带着 Attempts=1 重新进 attempt，应当走查重分支。
	svc.attempt(context.Background(), first)

	second, err := st.GetHuntTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.State != store.HuntSucceeded {
		t.Fatalf("第二轮之后状态 %q，期望 %q（最后错误：%s）", second.State, store.HuntSucceeded, second.LastError)
	}

	postsAfterSecond, instancesAfterSecond := fake.snapshot()
	if postsAfterSecond != postsAfterFirst {
		t.Fatalf("第二轮又发了 %d 次创建请求——查重没有生效，云端会多出一台",
			postsAfterSecond-postsAfterFirst)
	}
	if instancesAfterSecond != 1 {
		t.Fatalf("云端有 %d 台实例，期望恰好 1 台", instancesAfterSecond)
	}
	if second.InstanceID == "" {
		t.Fatal("任务没有记下查到的实例 ID")
	}
}
