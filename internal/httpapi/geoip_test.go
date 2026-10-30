package httpapi

import (
	"net/http"
	"testing"

	"ocicore/internal/store"
)

// seedGeoInstance 建一台带公网 IP 的实例。
func seedGeoInstance(t *testing.T, h *harness, id, accountID, region, publicIP string) {
	t.Helper()
	if err := h.store.UpsertInstance(t.Context(), store.Instance{
		ID: id, AccountID: accountID, Region: region,
		DisplayName: "web-" + id, LifecycleState: "RUNNING", PublicIP: publicIP,
	}); err != nil {
		t.Fatal(err)
	}
}

// 没配离线库时，分布图退回区域坐标，且 source 明确标成 region。
//
// 这是最关键的一条：GeoLite2 要用户自己去 MaxMind 下载，绝大多数部署
// 都没有。图上一片空白等于这个功能没做。
func TestGeoDistributionFallsBackToRegion(t *testing.T) {
	h := newHarness(t) // 不传 GeoIP，模拟「没装库」
	h.setupAndLogin()

	acc, err := h.store.CreateAccount(t.Context(), storeTestAccount(t))
	if err != nil {
		t.Fatal(err)
	}
	seedGeoInstance(t, h, "i-1", acc.ID, "ap-tokyo-1", "203.0.113.9")
	seedGeoInstance(t, h, "i-2", acc.ID, "us-ashburn-1", "198.51.100.7")

	resp := h.get("/api/geo/distribution")
	expectStatus(t, resp, http.StatusOK)
	d := decode[struct {
		Status struct {
			Enabled bool   `json:"enabled"`
			Notice  string `json:"notice"`
		} `json:"status"`
		Points []GeoPoint `json:"points"`
		Notice string     `json:"notice"`
	}](t, resp)

	if d.Status.Enabled {
		t.Error("没传 GeoIP 时不该报告已启用")
	}
	if len(d.Points) != 2 {
		t.Fatalf("点数 = %d，期望 2", len(d.Points))
	}
	for _, p := range d.Points {
		if !p.Found {
			t.Errorf("%s 应能按区域定位", p.InstanceID)
		}
		if p.Source != "region" {
			t.Errorf("%s 的 source = %q，期望 region（不能冒充精确结果）", p.InstanceID, p.Source)
		}
		if p.Latitude == 0 && p.Longitude == 0 {
			t.Errorf("%s 没有坐标", p.InstanceID)
		}
		if p.AccountCode == "" {
			t.Errorf("%s 缺账号代号", p.InstanceID)
		}
	}
	if d.Notice == "" {
		t.Error("应带上「按区域估算」的说明")
	}
}

// 区域不在表里、又没库时，点仍然返回，但 found=false。
//
// 让它消失比让它「查不到」更糟：用户会以为面板漏了这台机器。
func TestGeoDistributionUnknownRegionStaysVisible(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	acc, err := h.store.CreateAccount(t.Context(), storeTestAccount(t))
	if err != nil {
		t.Fatal(err)
	}
	seedGeoInstance(t, h, "i-1", acc.ID, "xx-nowhere-1", "203.0.113.9")

	resp := h.get("/api/geo/distribution")
	expectStatus(t, resp, http.StatusOK)
	d := decode[struct {
		Points []GeoPoint `json:"points"`
	}](t, resp)

	if len(d.Points) != 1 {
		t.Fatalf("点数 = %d，期望 1（查不到也要返回）", len(d.Points))
	}
	if d.Points[0].Found {
		t.Error("未知区域不该报告已定位")
	}
}

// 没有公网 IP 的实例不进图：图上画不出它，硬塞一个点等于说谎。
func TestGeoDistributionSkipsInstancesWithoutIP(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	acc, err := h.store.CreateAccount(t.Context(), storeTestAccount(t))
	if err != nil {
		t.Fatal(err)
	}
	seedGeoInstance(t, h, "i-1", acc.ID, "ap-tokyo-1", "")
	seedGeoInstance(t, h, "i-2", acc.ID, "ap-tokyo-1", "203.0.113.9")

	resp := h.get("/api/geo/distribution")
	expectStatus(t, resp, http.StatusOK)
	d := decode[struct {
		Points []GeoPoint `json:"points"`
	}](t, resp)

	if len(d.Points) != 1 || d.Points[0].InstanceID != "i-2" {
		t.Fatalf("应只返回有公网 IP 的那台: %+v", d.Points)
	}
}

// 手填的归属地要能盖掉区域估算，并且写进审计。
func TestGeoManualOverridesRegion(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	acc, err := h.store.CreateAccount(t.Context(), storeTestAccount(t))
	if err != nil {
		t.Fatal(err)
	}
	seedGeoInstance(t, h, "i-1", acc.ID, "ap-tokyo-1", "203.0.113.9")

	resp := h.do("PUT", "/api/geo/manual", map[string]any{
		"ip": "203.0.113.9", "country": "日本", "city": "大阪",
		"latitude": 34.69, "longitude": 135.50,
	}, true)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = h.get("/api/geo/distribution")
	expectStatus(t, resp, http.StatusOK)
	d := decode[struct {
		Points []GeoPoint `json:"points"`
	}](t, resp)

	if len(d.Points) != 1 {
		t.Fatalf("点数 = %d", len(d.Points))
	}
	p := d.Points[0]
	if p.Source != "manual" {
		t.Errorf("source = %q，期望 manual", p.Source)
	}
	if p.City != "大阪" {
		t.Errorf("city = %q，期望 大阪", p.City)
	}
	if p.Latitude != 34.69 || p.Longitude != 135.50 {
		t.Errorf("坐标没用手填的值: %v,%v", p.Latitude, p.Longitude)
	}

	entries, _, err := h.store.ListAudit(t.Context(), store.AuditFilter{Action: "geo_manual_set"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("手填归属地应写审计")
	}
}

// 经纬度越界直接拒绝：0,0 之外的越界值会让点画到画布外。
func TestGeoManualRejectsOutOfRange(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	cases := []map[string]any{
		{"ip": "203.0.113.9", "latitude": 91, "longitude": 0},
		{"ip": "203.0.113.9", "latitude": 0, "longitude": 181},
		{"ip": "", "latitude": 10, "longitude": 10},
	}
	for i, body := range cases {
		resp := h.do("PUT", "/api/geo/manual", body, true)
		if resp.StatusCode != http.StatusBadRequest {
			resp.Body.Close()
			t.Errorf("case %d 状态码 = %d，期望 400", i, resp.StatusCode)
			continue
		}
		resp.Body.Close()
	}
}

// 手填的记录可以被撤销，撤销后回到区域估算。
func TestGeoManualCanBeCleared(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	acc, err := h.store.CreateAccount(t.Context(), storeTestAccount(t))
	if err != nil {
		t.Fatal(err)
	}
	seedGeoInstance(t, h, "i-1", acc.ID, "ap-tokyo-1", "203.0.113.9")

	resp := h.do("PUT", "/api/geo/manual", map[string]any{
		"ip": "203.0.113.9", "city": "大阪", "latitude": 34.69, "longitude": 135.50,
	}, true)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = h.do("DELETE", "/api/geo/manual/203.0.113.9", nil, true)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = h.get("/api/geo/distribution")
	expectStatus(t, resp, http.StatusOK)
	d := decode[struct {
		Points []GeoPoint `json:"points"`
	}](t, resp)
	if d.Points[0].Source != "region" {
		t.Errorf("撤销手填后应退回 region，实际 %q", d.Points[0].Source)
	}
}

// 状态接口要说明「没启用」以及该做什么。
func TestGeoStatusWithoutDB(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	resp := h.get("/api/geo/status")
	expectStatus(t, resp, http.StatusOK)
	d := decode[struct {
		Status struct {
			Enabled bool   `json:"enabled"`
			Notice  string `json:"notice"`
		} `json:"status"`
		CachedIps    int64 `json:"cachedIps"`
		KnownRegions int   `json:"knownRegions"`
	}](t, resp)

	if d.Status.Enabled {
		t.Error("没装库时不该报告已启用")
	}
	if d.Status.Notice == "" {
		t.Error("应说明为什么不可用、该做什么")
	}
	if d.KnownRegions < 30 {
		t.Errorf("区域表只有 %d 条", d.KnownRegions)
	}
}

// IP 情报属于账号资产，未登录不能读。
func TestGeoRequiresAuth(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{"/api/geo/status", "/api/geo/distribution"} {
		resp := h.get(path)
		if resp.StatusCode != http.StatusUnauthorized {
			resp.Body.Close()
			t.Errorf("%s 状态码 = %d，期望 401", path, resp.StatusCode)
			continue
		}
		resp.Body.Close()
	}
}
