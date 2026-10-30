package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ocicore/internal/cf"
	"ocicore/internal/store"
)

const testCFZoneID = "a0000000000000000000000000000001"

// withFakeCF 把服务里的 Cloudflare 客户端指向假服务端。
//
// 直接替换 s.cf 而不是改构造参数：联动是可选能力，生产构造用的是默认
// 客户端，测试只需要在需要出网的地方换成假的。
func withFakeCF(t *testing.T, h *harness, token string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`))
			return
		}
		switch {
		case r.URL.Path == "/zones" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"success":true,"result":[
				{"id":"` + testCFZoneID + `","name":"example.com","status":"active"}
			]}`))
		case strings.HasSuffix(r.URL.Path, "/dns_records") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"success":true,"result":[
				{"id":"rec1","type":"A","name":"a.example.com","content":"198.51.100.1","ttl":1}
			]}`))
		case strings.HasSuffix(r.URL.Path, "/dns_records/rec1") && r.Method == http.MethodPut:
			_, _ = w.Write([]byte(`{"success":true,"result":{"id":"rec1","type":"A","name":"a.example.com","content":"203.0.113.9"}}`))
		default:
			_, _ = w.Write([]byte(`{"success":true,"result":{"id":"` + testCFZoneID + `","name":"example.com","status":"active"}}`))
		}
	}))
	t.Cleanup(srv.Close)

	h.api.cf = cf.New(cf.Deps{
		Store: h.store,
		NewClient: func(tok string) *cf.Client {
			return cf.NewClient(tok, cf.WithBaseURL(srv.URL))
		},
	})
}

// readAll 读完整响应体。断言里要看原文的测试用它。
func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf)
}

// seedCFInstance 建一个账号 + 一台带公网 IP 的实例。
func seedCFInstance(t *testing.T, h *harness, publicIP string) *store.Account {
	t.Helper()
	acc, err := h.store.CreateAccount(t.Context(), storeTestAccount(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertInstance(t.Context(), store.Instance{
		ID: "i-1", AccountID: acc.ID, Region: "ap-tokyo-1",
		DisplayName: "web-01", LifecycleState: "RUNNING", PublicIP: publicIP,
	}); err != nil {
		t.Fatal(err)
	}
	return acc
}

// 添加域名时立刻验证 token，成功后列表里能看到它，且响应里没有 token 明文。
func TestCFZoneCreateAndList(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	withFakeCF(t, h, "tok-good")

	resp := h.post("/api/cf/zones", map[string]string{
		"zone": "example.com", "token": "tok-good",
	})
	expectStatus(t, resp, http.StatusOK)
	created := decode[struct {
		Zone store.CFZone `json:"zone"`
	}](t, resp)

	if created.Zone.ZoneName != "example.com" {
		t.Fatalf("域名没回填: %+v", created.Zone)
	}
	if !created.Zone.HasToken {
		t.Error("HasToken 应为 true")
	}

	resp = h.get("/api/cf/zones")
	expectStatus(t, resp, http.StatusOK)
	body := readAll(t, resp)
	if strings.Contains(body, "tok-good") {
		t.Fatal("响应里出现了 token 明文")
	}
	if !strings.Contains(body, "example.com") {
		t.Fatalf("列表里没有刚加的域名: %s", body)
	}
	if !strings.Contains(body, "Zone.DNS") {
		t.Error("响应里应带上「需要什么权限」的说明")
	}
}

// token 不对时要在添加这一步就拦住，而不是等到第一次换 IP 之后。
func TestCFZoneCreateWithBadToken(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	withFakeCF(t, h, "tok-good")

	resp := h.post("/api/cf/zones", map[string]string{
		"zone": "example.com", "token": "tok-bad",
	})
	expectStatus(t, resp, http.StatusBadRequest)
	e := decode[struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}](t, resp)

	if e.Code != "cf_zone_unresolved" {
		t.Errorf("错误码 = %q", e.Code)
	}
	if strings.Contains(e.Message, "tok-bad") {
		t.Error("错误信息里出现了 token")
	}
	if !strings.Contains(e.Message, "Zone") {
		t.Errorf("文案应说明需要什么权限: %s", e.Message)
	}
}

// 未登录不能读写任何 Cloudflare 配置。
func TestCFRequiresAuth(t *testing.T) {
	h := newHarness(t) // 刻意不 setupAndLogin

	resp := h.get("/api/cf/zones")
	expectStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()

	resp = h.post("/api/cf/zones", map[string]string{"zone": "example.com", "token": "x"})
	expectStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}

// 绑定一条 A 记录，然后立刻同步到实例当前公网 IP。
func TestCFRecordSync(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	withFakeCF(t, h, "tok-good")
	acc := seedCFInstance(t, h, "203.0.113.9")

	zone, err := h.store.CreateCFZone(t.Context(), store.CFZone{
		AccountID: acc.ID, ZoneID: testCFZoneID, ZoneName: "example.com",
	}, "tok-good")
	if err != nil {
		t.Fatal(err)
	}

	resp := h.post("/api/cf/records", map[string]any{
		"zonePk": zone.ID, "recordId": "rec1", "name": "a.example.com",
		"ttl": 1, "accountId": acc.ID, "instanceId": "i-1",
	})
	expectStatus(t, resp, http.StatusOK)
	rec := decode[struct {
		Record store.CFRecord `json:"record"`
	}](t, resp)

	resp = h.post("/api/cf/records/"+rec.Record.ID+"/sync", nil)
	expectStatus(t, resp, http.StatusOK)
	res := decode[struct {
		Status string `json:"status"`
		Detail string `json:"detail"`
		IP     string `json:"ip"`
	}](t, resp)

	if res.Status != cf.StatusUpdated {
		t.Fatalf("同步状态 = %q（detail: %s）", res.Status, res.Detail)
	}
	if res.IP != "203.0.113.9" {
		t.Errorf("IP = %q", res.IP)
	}

	// 拉一次 Cloudflare 侧的记录列表，验证「已绑定」标记。
	resp = h.get("/api/cf/zones/" + zone.ID + "/records")
	expectStatus(t, resp, http.StatusOK)
	body := readAll(t, resp)
	if !strings.Contains(body, `"bound":true`) {
		t.Errorf("已绑定的记录应被标记: %s", body)
	}
}

// 实例没有公网 IP 时要明确拒绝，而不是把空字符串写进 A 记录。
func TestCFRecordSyncWithoutPublicIP(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	withFakeCF(t, h, "tok-good")
	seedCFInstance(t, h, "")

	zone, err := h.store.CreateCFZone(t.Context(), store.CFZone{
		ZoneID: testCFZoneID, ZoneName: "example.com",
	}, "tok-good")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := h.store.CreateCFRecord(t.Context(), store.CFRecord{
		ZonePK: zone.ID, RecordID: "rec1", Name: "a.example.com",
		Type: "A", TTL: 1, InstanceID: "i-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := h.post("/api/cf/records/"+rec.ID+"/sync", nil)
	expectStatus(t, resp, http.StatusBadRequest)
	e := decode[struct {
		Code string `json:"code"`
	}](t, resp)
	if e.Code != "no_public_ip" {
		t.Errorf("错误码 = %q，期望 no_public_ip", e.Code)
	}
}

// 绑定必须指向存在的实例。
func TestCFRecordRequiresExistingInstance(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	withFakeCF(t, h, "tok-good")

	zone, err := h.store.CreateCFZone(t.Context(), store.CFZone{
		ZoneID: testCFZoneID, ZoneName: "example.com",
	}, "tok-good")
	if err != nil {
		t.Fatal(err)
	}
	resp := h.post("/api/cf/records", map[string]any{
		"zonePk": zone.ID, "recordId": "rec1", "name": "a.example.com",
		"instanceId": "not-exist",
	})
	expectStatus(t, resp, http.StatusBadRequest)
}

// 删域名会连带删掉它的绑定，并把这次删除写进审计。
func TestCFZoneDeleteCascadesAndAudits(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	withFakeCF(t, h, "tok-good")

	zone, err := h.store.CreateCFZone(t.Context(), store.CFZone{
		ZoneID: testCFZoneID, ZoneName: "example.com",
	}, "tok-good")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := h.store.CreateCFRecord(t.Context(), store.CFRecord{
		ZonePK: zone.ID, RecordID: "rec1", Name: "a.example.com",
		Type: "A", TTL: 1, InstanceID: "i-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := h.do("DELETE", "/api/cf/zones/"+zone.ID, nil, true)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	if _, err := h.store.GetCFRecord(t.Context(), rec.ID); err == nil {
		t.Fatal("绑定应随域名一起删除")
	}

	entries, _, err := h.store.ListAudit(t.Context(), store.AuditFilter{Action: "cf_zone_delete"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("删除域名应写审计")
	}
}
