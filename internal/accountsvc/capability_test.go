package accountsvc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

// testCreds 生成一份通过离线校验的凭据。
//
// 指纹必须由私钥算出来：ociclient.New 会先做一次离线校验，指纹对不上
// 直接报错，请求根本发不出去。
func testCreds(t *testing.T) *ociclient.Credentials {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &ociclient.Credentials{
		TenancyOCID: "ocid1.tenancy.oc1..aaaa",
		UserOCID:    "ocid1.user.oc1..bbbb",
		Fingerprint: ociclient.FingerprintOf(&key.PublicKey),
		Region:      "ap-tokyo-1",
		PrivateKey:  key,
	}
}

// fakeOCI 冒充 OCI 的五个端点，按路径决定返回什么。
//
// 这是 P2-3 验收的关键：真去控制台删一条策略再跑一次的成本太高，
// 而"缺某一族权限时其余四族仍正常"正是这个功能存在的全部意义。
type fakeOCI struct {
	srv *httptest.Server
	// denied 是应当返回 403 的服务版本段，例如 "20160918" 下的 volumes。
	denied map[string]bool
	// status 按完整路径覆盖状态码，优先级高于 denied。
	status map[string]int
	calls  atomic.Int32
}

func newFakeOCI(t *testing.T) *fakeOCI {
	t.Helper()
	f := &fakeOCI{denied: map[string]bool{}, status: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		path := r.URL.Path

		if code, ok := f.status[path]; ok {
			writeFakeError(w, code, "Forced")
			return
		}
		for marker := range f.denied {
			if strings.Contains(path, marker) {
				writeFakeError(w, http.StatusForbidden, "NotAuthorizedOrNotFound")
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(path, "/instances"):
			_, _ = w.Write([]byte(`[{"id":"ocid1.instance.oc1..a","displayName":"a"},
				{"id":"ocid1.instance.oc1..b","displayName":"b"}]`))
		case strings.HasSuffix(path, "/vcns"):
			_, _ = w.Write([]byte(`[{"id":"ocid1.vcn.oc1..a","displayName":"v"}]`))
		case strings.HasSuffix(path, "/volumes"):
			_, _ = w.Write([]byte(`[]`))
		case strings.Contains(path, "usage"):
			_, _ = w.Write([]byte(`{"items":[{"computedAmount":1.5}],"groupBy":[]}`))
		case strings.Contains(path, "metrics"):
			_, _ = w.Write([]byte(`[{"name":"CpuUtilization","aggregatedDatapoints":[]}]`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func writeFakeError(w http.ResponseWriter, code int, ociCode string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`{"code":"` + ociCode + `","message":"fake denial"}`))
}

// client 建一个指向 fake 服务端的客户端。
func (f *fakeOCI) client(t *testing.T) *ociclient.Client {
	t.Helper()
	c, err := ociclient.New(testCreds(t),
		// 所有服务都指向同一个 fake 服务端，靠 path 区分。
		ociclient.WithEndpointResolver(func(ociclient.Service, string) string { return f.srv.URL }),
		ociclient.WithMaxRetries(0),
	)
	if err != nil {
		t.Fatalf("建客户端失败: %v", err)
	}
	return c
}

// 这是 P2-3 的验收条件：缺 ListVolumes 权限时 storage 为 false，其余为 true。
func TestProbeCapabilitiesMissingStorageOnly(t *testing.T) {
	f := newFakeOCI(t)
	f.denied["/volumes"] = true

	probe := ProbeCapabilities(context.Background(), f.client(t), CapabilityOptions{
		Region: "ap-tokyo-1", CompartmentID: "ocid1.compartment.oc1..cccc",
		TenancyID: "ocid1.tenancy.oc1..aaaa",
	})

	if len(probe.Items) != 5 {
		t.Fatalf("应有 5 族结论，实际 %d", len(probe.Items))
	}
	for _, c := range probe.Items {
		switch c.Key {
		case CapStorage:
			if c.OK {
				t.Error("缺 ListVolumes 权限时 storage 不应为 ok")
			}
			if !c.Denied {
				t.Error("storage 应标记为 denied（缺权限），而不是普通错误")
			}
			if c.Policy == "" {
				t.Error("缺权限时必须给出该补的策略，否则用户不知道去加什么")
			}
		default:
			if !c.OK {
				t.Errorf("%s 应为 ok，实际 ok=%v denied=%v detail=%q",
					c.Key, c.OK, c.Denied, c.Detail)
			}
		}
	}
	if probe.OKCount() != 4 {
		t.Errorf("可用族数 = %d，期望 4", probe.OKCount())
	}
	if !strings.Contains(probe.Summary, "存储") {
		t.Errorf("结论里应点名缺哪一族，实际 %q", probe.Summary)
	}
}

func TestProbeCapabilitiesAllOK(t *testing.T) {
	f := newFakeOCI(t)
	probe := ProbeCapabilities(context.Background(), f.client(t), CapabilityOptions{})

	if probe.OKCount() != 5 {
		t.Fatalf("五族应全部可用，实际 %d：%+v", probe.OKCount(), probe.Items)
	}
	if !strings.Contains(probe.Summary, "全部可用") {
		t.Errorf("结论 = %q", probe.Summary)
	}
	// 数量要真的带回来：实例 2 台、VCN 1 个、卷 0 个。
	if c, _ := probe.FindCapability(CapCompute); c.Count != 2 {
		t.Errorf("compute 数量 = %d，期望 2", c.Count)
	}
	if c, _ := probe.FindCapability(CapStorage); c.Count != 0 {
		t.Errorf("storage 数量 = %d，期望 0（空列表也是成功）", c.Count)
	}
}

// 签名类失败（401）时不该再打另外四个接口。
//
// 同一份坏签名连打五次只会让账号更快被限流，而结论完全相同。
// 而且要把"未探测"写清楚，不能让它看起来像是探测过且真的缺权限。
func TestProbeCapabilitiesStopsOnAuthFailure(t *testing.T) {
	f := newFakeOCI(t)
	// compute 返回 401。
	f.status["/20160918/instances"] = http.StatusUnauthorized

	probe := ProbeCapabilities(context.Background(), f.client(t), CapabilityOptions{})

	if got := f.calls.Load(); got != 1 {
		t.Errorf("401 之后不应再发请求，实际发了 %d 次", got)
	}
	// 第一项是真的探测过（就是它返回的 401），Detail 是它自己的错误。
	if first := probe.Items[0]; first.OK {
		t.Error("compute 不应为 ok")
	}
	// 其余四项没发请求，必须标"未探测"——不能让它看起来像是探测过且真的缺权限。
	for _, c := range probe.Items[1:] {
		if c.OK {
			t.Errorf("%s 不应为 ok", c.Key)
		}
		if !strings.Contains(c.Detail, "未探测") {
			t.Errorf("%s 的说明应标注未探测，实际 %q", c.Key, c.Detail)
		}
	}
	if probe.OKCount() != 0 {
		t.Errorf("可用族数 = %d，期望 0", probe.OKCount())
	}
}

// 非权限类错误（500）算"不可用"，但不能标成 denied——
// 那种情况重试可能就好了，加策略是白费功夫。
func TestProbeCapabilitiesNonAuthErrorIsNotDenied(t *testing.T) {
	f := newFakeOCI(t)
	f.status["/20160918/volumes"] = http.StatusInternalServerError

	probe := ProbeCapabilities(context.Background(), f.client(t), CapabilityOptions{})
	c, ok := probe.FindCapability(CapStorage)
	if !ok {
		t.Fatal("找不到 storage 项")
	}
	if c.OK {
		t.Error("500 时不应判为可用")
	}
	if c.Denied {
		t.Error("500 不是权限问题，不该标成 denied")
	}
	if c.Class == "" {
		t.Error("应记录错误分类，便于排障")
	}
}

// 探测请求必须不计入调用预算。
//
// 否则一个刚接入、还没做任何事的账号会先被扣掉五次调用——而预算的用意是
// 约束业务操作量，不是惩罚用户点了一下「自检」。
func TestProbeCapabilitiesSkipsBudget(t *testing.T) {
	f := newFakeOCI(t)

	var counted atomic.Int32
	client, err := ociclient.New(testCreds(t),
		ociclient.WithEndpointResolver(func(ociclient.Service, string) string { return f.srv.URL }),
		ociclient.WithMaxRetries(0),
		ociclient.WithCallObserver(func() { counted.Add(1) }),
	)
	if err != nil {
		t.Fatal(err)
	}

	ProbeCapabilities(context.Background(), client, CapabilityOptions{})
	if n := counted.Load(); n != 0 {
		t.Errorf("探测请求不应计入预算，实际计了 %d 次", n)
	}
	if f.calls.Load() == 0 {
		t.Error("请求应该真的发出去了——计数为 0 必须是「跳过计数」而不是「没发请求」")
	}

	// 反向确认：不带标记的普通调用照常计数，否则上面的断言可能只是
	// 因为 observer 根本没接上。
	if _, err := client.ListVcns(context.Background(), "ap-tokyo-1", ""); err != nil {
		t.Fatal(err)
	}
	if counted.Load() != 1 {
		t.Errorf("普通调用应计 1 次，实际 %d", counted.Load())
	}
}

func TestCapabilityProbeEncodeDecode(t *testing.T) {
	probe := CapabilityProbe{
		AccountID: "acc1", Region: "ap-tokyo-1", ProbedAt: time.Unix(1_800_000_000, 0).UTC(),
		Items: []Capability{
			{Key: CapCompute, Label: "实例", OK: true, Count: 3},
			{Key: CapStorage, Label: "存储", Denied: true, Class: "NotAuthorized"},
		},
		Summary: "1/2 项可用",
	}
	raw := EncodeCapabilityProbe(probe)
	if raw == "" {
		t.Fatal("应能序列化")
	}
	back := DecodeCapabilityProbe(raw)
	if back == nil {
		t.Fatal("应能反序列化")
	}
	if back.AccountID != "acc1" || len(back.Items) != 2 {
		t.Errorf("往返后数据不一致: %+v", back)
	}
	if c, ok := back.FindCapability(CapStorage); !ok || !c.Denied {
		t.Error("denied 标记应保留")
	}
}

// 空串与坏数据都当"没探测过"，不报错。
//
// 库里的旧数据格式变了不该让整个列表接口 500。
func TestDecodeCapabilityProbeToleratesGarbage(t *testing.T) {
	for _, raw := range []string{"", "   ", "{", "null", `{"items":[]}`, "[]"} {
		if got := DecodeCapabilityProbe(raw); got != nil {
			t.Errorf("DecodeCapabilityProbe(%q) = %+v，期望 nil", raw, got)
		}
	}
	if got := EncodeCapabilityProbe(CapabilityProbe{}); got != "" {
		t.Errorf("没有 items 时不应写入半截 JSON，实际 %q", got)
	}
}

// 五族的 key 必须稳定：前端按 key 匹配图标与文案，改了就是静默失配。
func TestCapabilitySpecsStable(t *testing.T) {
	specs := CapabilitySpecs()
	want := []string{CapCompute, CapNetwork, CapStorage, CapUsage, CapMonitoring}
	if len(specs) != len(want) {
		t.Fatalf("族数 = %d，期望 %d", len(specs), len(want))
	}
	for i, s := range specs {
		if s.Key != want[i] {
			t.Errorf("第 %d 族的 key = %q，期望 %q", i, s.Key, want[i])
		}
		if s.Label == "" || s.Policy == "" {
			t.Errorf("%s 缺 label 或 policy", s.Key)
		}
	}
}

// ProbeFromAccount 从账号记录里取出上次结果。
func TestProbeFromAccount(t *testing.T) {
	if got := ProbeFromAccount(store.Account{}); got != nil {
		t.Error("没探测过的账号应返回 nil")
	}

	raw := EncodeCapabilityProbe(CapabilityProbe{
		AccountID: "a", Items: []Capability{{Key: CapCompute, OK: true}},
	})
	acc := store.Account{CapabilityProbe: raw}
	if got := ProbeFromAccount(acc); got == nil || len(got.Items) != 1 {
		t.Errorf("应解析出 1 项，实际 %+v", got)
	}
}

// json 字段名是前端的契约，改名等于破坏接口。
func TestCapabilityProbeJSONShape(t *testing.T) {
	probe := CapabilityProbe{
		AccountID: "a", Region: "r", ProbedAt: time.Unix(1_800_000_000, 0).UTC(),
		Items:   []Capability{{Key: "compute", Label: "实例", OK: true, Count: 1}},
		Summary: "1 项能力全部可用",
	}
	data, err := json.Marshal(probe)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"accountId"`, `"region"`, `"probedAt"`, `"items"`, `"summary"`,
		`"key"`, `"label"`, `"ok"`, `"denied"`, `"count"`,
	} {
		if !strings.Contains(string(data), field) {
			t.Errorf("JSON 里缺少字段 %s: %s", field, data)
		}
	}
}
