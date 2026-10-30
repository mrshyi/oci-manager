package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"ocicore/internal/store"
)

// P0-4 危险操作分级扩面。
//
// 这一组测试防的是「门槛写在注释里但没生效」：分级最容易的退化方式是
// 判定顺序写反（先放行再检查）、或把台数取自请求体里前端自报的字段。
// 两者都不会让功能出问题，只会让保护静默消失。

// seedInstance 往本地库塞一台实例，供批量范围判定读取。
func seedInstance(t *testing.T, h *harness, id, accountID string) {
	t.Helper()
	if err := h.store.UpsertInstance(t.Context(), store.Instance{
		ID: id, AccountID: accountID, Region: "ap-tokyo-1",
		DisplayName: id, LifecycleState: "RUNNING",
	}); err != nil {
		t.Fatal(err)
	}
}

// seedAccount 建一个账号，suffix 用来区分租户 OCID。
func seedAccount(t *testing.T, h *harness, alias, code, suffix string) *store.Account {
	t.Helper()
	body := testAccountBody(t, alias, code, suffix)
	acc, err := h.store.CreateAccount(t.Context(), store.NewAccount{
		Alias:         body["alias"].(string),
		Code:          body["code"].(string),
		TenancyOCID:   body["tenancyOcid"].(string),
		UserOCID:      body["userOcid"].(string),
		Fingerprint:   body["fingerprint"].(string),
		PrivateKeyPEM: body["privateKeyPem"].(string),
		DefaultRegion: body["defaultRegion"].(string),
	})
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

// bulkStart 发一次批量开机。用 START 是因为它不要求 TOTP，
// 于是能单独观察「规模/范围」这一道门槛。
func (h *harness) bulkStart(ids []string, confirm string) *http.Response {
	path := "/api/instances/bulk"
	if confirm != "" {
		path += "?confirm=" + confirm
	}
	return h.post(path, map[string]any{"instanceIds": ids, "action": "START"})
}

// 4 台不该要求确认词：这个规模下选中状态都在一屏里，误选肉眼可辨。
func TestBulkUnderThresholdNeedsNoConfirm(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京主号", "TYO", "tokyo")

	ids := make([]string, 0, bulkConfirmThreshold-1)
	for i := 0; i < bulkConfirmThreshold-1; i++ {
		id := "i-" + itoa(i)
		seedInstance(t, h, id, acc.ID)
		ids = append(ids, id)
	}

	resp := h.bulkStart(ids, "")
	if resp.StatusCode == http.StatusBadRequest {
		body := decode[errorBody](t, resp)
		t.Fatalf("4 台不该触发确认门槛，实际被拒: %s", body.Code)
	}
	resp.Body.Close()
}

// 5 台必须回传账号别名。
func TestBulkAtThresholdRequiresAccountAlias(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京主号", "TYO", "tokyo")

	ids := make([]string, 0, bulkConfirmThreshold)
	for i := 0; i < bulkConfirmThreshold; i++ {
		id := "i-" + itoa(i)
		seedInstance(t, h, id, acc.ID)
		ids = append(ids, id)
	}

	resp := h.bulkStart(ids, "")
	expectStatus(t, resp, http.StatusBadRequest)
	if body := decode[errorBody](t, resp); body.Code != "confirm_required" {
		t.Fatalf("错误码 = %q，期望 confirm_required", body.Code)
	}

	// 回传了别名才放行。放行后的执行结果不重要（测试凭据连不上 OCI），
	// 这里断言的是门槛本身不再拦。
	resp = h.bulkStart(ids, "东京主号")
	if resp.StatusCode == http.StatusBadRequest {
		body := decode[errorBody](t, resp)
		t.Fatalf("回传别名后仍被拒: %s", body.Code)
	}
	resp.Body.Close()
}

// 跨账号批量必须回传 ALL——哪怕只有 2 台。
//
// 跨账号是最容易误选的情形：列表默认跨账号聚合，用户以为自己选的是
// 「这个账号的三台」，实际选中的可能分属两个租户。
func TestBulkAcrossAccountsRequiresAll(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	a := seedAccount(t, h, "东京主号", "TYO", "tokyo")
	b := seedAccount(t, h, "大阪备号", "OSA", "osaka")
	seedInstance(t, h, "i-a", a.ID)
	seedInstance(t, h, "i-b", b.ID)

	resp := h.bulkStart([]string{"i-a", "i-b"}, "")
	expectStatus(t, resp, http.StatusBadRequest)
	body := decode[errorBody](t, resp)
	if body.Code != "confirm_required" {
		t.Fatalf("错误码 = %q，期望 confirm_required", body.Code)
	}
	// 提示里要说清是跨账号，否则用户会以为是自己台数没填对。
	if !strings.Contains(body.Message, "2 个账号") {
		t.Errorf("提示未说明跨账号: %q", body.Message)
	}

	// 回传单个账号的别名不算数：跨账号场景下没有任何一个别名能代表全部。
	resp = h.bulkStart([]string{"i-a", "i-b"}, "东京主号")
	expectStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()

	resp = h.bulkStart([]string{"i-a", "i-b"}, bulkCrossAccountWord)
	if resp.StatusCode == http.StatusBadRequest {
		other := decode[errorBody](t, resp)
		t.Fatalf("回传 ALL 后仍被拒: %s", other.Code)
	}
	resp.Body.Close()
}

// L4：目标名为空时必须中止，不能因为「查不到名字」就跳过确认。
func TestNamedConfirmRejectsUnknownTarget(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/x?confirm=", nil)
	if h.api.requireNamedConfirm(rec, req, "") {
		t.Fatal("目标名为空时不应放行——那等于跳过 L4 的唯一实质")
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 %d", rec.Code, http.StatusConflict)
	}
}

// L4：确认词必须与服务端查到的名字完全一致。
func TestNamedConfirmRequiresExactName(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	const target = "web-01"
	cases := []struct {
		query string
		want  bool
	}{
		{"", false},
		{"web-0", false},
		{"WEB-01", false},
		{" web-01 ", true}, // 前后空格容忍：从别处复制粘贴常带空格
		{"web-01", true},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/x?confirm="+url.QueryEscape(tc.query), nil)
		got := h.api.requireNamedConfirm(rec, req, target)
		if got != tc.want {
			t.Errorf("confirm=%q 时放行 = %v，期望 %v", tc.query, got, tc.want)
		}
	}
}
