package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"ocicore/internal/accountsvc"
	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

// fakeOCIProbe 冒充 OCI，按路径决定返回 200 还是 403。
//
// 用来复现 P2-3 的验收场景：账号缺 ListVolumes 权限。
type fakeOCIProbe struct {
	srv *httptest.Server
	mu  sync.Mutex
	// deniedPath 含此子串的请求返回 403。
	deniedPath string
}

func newFakeOCIProbe(t *testing.T, deniedPath string) *fakeOCIProbe {
	t.Helper()
	f := &fakeOCIProbe{deniedPath: deniedPath}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		denied := f.deniedPath
		f.mu.Unlock()

		path := r.URL.Path
		if denied != "" && strings.Contains(path, denied) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"NotAuthorizedOrNotFound","message":"not authorized"}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(path, "/instances"):
			_, _ = w.Write([]byte(`[{"id":"ocid1.instance.oc1..a"}]`))
		case strings.HasSuffix(path, "/vcns"):
			_, _ = w.Write([]byte(`[{"id":"ocid1.vcn.oc1..a"}]`))
		case strings.HasSuffix(path, "/volumes"):
			_, _ = w.Write([]byte(`[]`))
		case strings.Contains(path, "usage"):
			_, _ = w.Write([]byte(`{"items":[]}`))
		case strings.Contains(path, "metrics"):
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// capabilityEnvelope 是 GET/POST capability-probe 的响应形状。
type capabilityEnvelope struct {
	Probe  *accountsvc.CapabilityProbe `json:"probe"`
	Notice string                      `json:"notice"`
	Specs  []accountsvc.CapabilitySpec `json:"specs"`
}

// accountsCapabilityEnvelope 是列表接口里的能力字段。
type accountsCapabilityEnvelope struct {
	Accounts []struct {
		ID         string                    `json:"id"`
		Capability *accountsvc.CapabilityProbe `json:"capability"`
	} `json:"accounts"`
}

// 这是 P2-3 的验收条件：缺 ListVolumes 权限时 storage 为 false、其余为 true，
// 并且结果要真的落到 accounts.capability_probe 里。
func TestCapabilityProbeMissingStorage(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	id := createAccountForHealth(t, h, "东京主号", "TYO", "capprobe")

	fake := newFakeOCIProbe(t, "/volumes")
	h.api.conns.SetEndpointResolver(func(ociclient.Service, string) string { return fake.srv.URL })

	resp := h.post("/api/accounts/"+id+"/capability-probe", nil)
	expectStatus(t, resp, http.StatusOK)
	got := decode[capabilityEnvelope](t, resp)
	if got.Probe == nil {
		t.Fatal("应返回探测结果")
	}

	byKey := map[string]accountsvc.Capability{}
	for _, c := range got.Probe.Items {
		byKey[c.Key] = c
	}
	if len(byKey) != 5 {
		t.Fatalf("应有 5 族结论，实际 %d", len(byKey))
	}
	if byKey[accountsvc.CapStorage].OK {
		t.Error("缺 ListVolumes 权限时 storage 不应为 ok")
	}
	if !byKey[accountsvc.CapStorage].Denied {
		t.Error("storage 应标记为缺权限")
	}
	for _, key := range []string{
		accountsvc.CapCompute, accountsvc.CapNetwork,
		accountsvc.CapUsage, accountsvc.CapMonitoring,
	} {
		if !byKey[key].OK {
			t.Errorf("%s 应为 ok，实际 %+v", key, byKey[key])
		}
	}
	if got.Probe.OKCount() != 4 {
		t.Errorf("可用族数 = %d，期望 4", got.Probe.OKCount())
	}

	// 结果必须落库：下次打开页面不该再跑一遍。
	acc, err := h.store.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if acc.CapabilityProbe == "" {
		t.Fatal("探测结果应写进 accounts.capability_probe")
	}
	if back := accountsvc.DecodeCapabilityProbe(acc.CapabilityProbe); back == nil || back.OKCount() != 4 {
		t.Errorf("落库的结果读回来不对: %+v", back)
	}

	// 列表接口要带上它，否则账号页还得逐个请求。
	resp = h.get("/api/accounts")
	expectStatus(t, resp, http.StatusOK)
	listed := decode[accountsCapabilityEnvelope](t, resp)
	var found bool
	for _, a := range listed.Accounts {
		if a.ID == id {
			found = true
			if a.Capability == nil {
				t.Error("列表接口应带出 capability")
			} else if a.Capability.OKCount() != 4 {
				t.Errorf("列表里的可用族数 = %d", a.Capability.OKCount())
			}
		}
	}
	if !found {
		t.Fatal("列表里找不到账号")
	}

	// 自检要留痕：它打了五次 OCI，事后要能查到是谁点的。
	logs, _, err := h.store.ListAudit(context.Background(),
		store.AuditFilter{AccountID: id, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var audited bool
	for _, e := range logs {
		if e.Action == "account_capability_probe" {
			audited = true
		}
	}
	if !audited {
		t.Error("权限自检应记审计")
	}
}

// 没自检过时返回 probe=null 与五族清单，而不是 404。
//
// 这不是"资源不存在"，而是"还没查过"——前端要据此显示"尚未自检"和按钮，
// 五族清单则让前端不必自己维护一份（后端加了第六族前端会静默少显示一项）。
func TestGetCapabilityProbeBeforeAnyRun(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	id := createAccountForHealth(t, h, "东京主号", "TYO", "capnone")

	resp := h.get("/api/accounts/" + id + "/capability-probe")
	expectStatus(t, resp, http.StatusOK)
	got := decode[capabilityEnvelope](t, resp)

	if got.Probe != nil {
		t.Error("没自检过时 probe 应为 null")
	}
	if got.Notice == "" {
		t.Error("应给出说明文案")
	}
	if len(got.Specs) != 5 {
		t.Fatalf("应下发 5 族描述，实际 %d", len(got.Specs))
	}
	for _, s := range got.Specs {
		if s.Key == "" || s.Label == "" || s.Policy == "" {
			t.Errorf("族描述缺字段: %+v", s)
		}
	}
}

// 停用的账号不该被拿去调 API：用户停用它可能正是不想再产生流量。
func TestCapabilityProbeRejectsDisabledAccount(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	id := createAccountForHealth(t, h, "东京主号", "TYO", "capoff")

	resp := h.do("PATCH", "/api/accounts/"+id, map[string]any{"enabled": false}, true)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = h.post("/api/accounts/"+id+"/capability-probe", nil)
	expectStatus(t, resp, http.StatusBadRequest)
	if e := decode[errorBody](t, resp); e.Code != "account_disabled" {
		t.Errorf("错误码 = %q，期望 account_disabled", e.Code)
	}
}

func TestCapabilityProbeEndpointsRequireAuth(t *testing.T) {
	h := newHarness(t)

	resp := h.get("/api/accounts/some-id/capability-probe")
	expectStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()

	// 触发探测是写操作（会记审计），缺 CSRF 头必须被拒。
	resp = h.do("POST", "/api/accounts/some-id/capability-probe", nil, false)
	expectStatus(t, resp, http.StatusForbidden)
	resp.Body.Close()
}

func TestCapabilityProbeUnknownAccount(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	resp := h.get("/api/accounts/does-not-exist/capability-probe")
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()

	resp = h.post("/api/accounts/does-not-exist/capability-probe", nil)
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

// 库里的坏数据不能让列表接口 500。
//
// 旧版本写下的 JSON 格式变了、或者被人手工改坏了，都只该表现为"没探测过"。
func TestListAccountsToleratesCorruptProbeJSON(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	id := createAccountForHealth(t, h, "东京主号", "TYO", "capbad")

	if err := h.store.SetAccountCapabilityProbe(context.Background(), id, "{not json"); err != nil {
		t.Fatal(err)
	}

	resp := h.get("/api/accounts")
	expectStatus(t, resp, http.StatusOK)
	listed := decode[accountsCapabilityEnvelope](t, resp)
	for _, a := range listed.Accounts {
		if a.ID == id && a.Capability != nil {
			t.Errorf("坏数据应解析成 nil，实际 %+v", a.Capability)
		}
	}
}
