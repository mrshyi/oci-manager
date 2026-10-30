package httpapi

import (
	"context"
	"net/http"
	"testing"

	"ocicore/internal/accountsvc"
	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

// healthEnvelope 是列表接口里账号健康度的形状。
type healthEnvelope struct {
	Level  string `json:"level"`
	Label  string `json:"label"`
	Advice string `json:"advice"`
	store.AccountHealth
}

type healthAccountsEnvelope struct {
	Accounts []struct {
		ID     string         `json:"id"`
		Health healthEnvelope `json:"health"`
	} `json:"accounts"`
}

// createAccountForHealth 建一个账号并返回 ID（跳过联网校验）。
func createAccountForHealth(t *testing.T, h *harness, alias, code, suffix string) string {
	t.Helper()
	resp := h.post("/api/accounts", testAccountBody(t, alias, code, suffix))
	expectStatus(t, resp, http.StatusCreated)
	created := decode[struct {
		Account store.Account `json:"account"`
	}](t, resp)
	return created.Account.ID
}

// authFailResult 构造一次"OCI 返回 401"的校验结果。
//
// 这里模拟的是 accountsvc 归一化之后的产物，而不是 OCI 的原始响应——
// 健康度只关心"哪一类错、是不是凭据问题"，从 Result 往下才是本卡的职责。
func authFailResult() accountsvc.Result {
	return accountsvc.Result{
		OK:           false,
		ErrorCode:    "NotAuthenticated",
		ErrorText:    "The required information to complete authentication was not provided",
		ErrorClass:   ociclient.ClassAuthFailed.String(),
		AccountFatal: true,
		Steps: []accountsvc.Step{
			{Key: "identity", Label: "调用 GetUser", OK: false},
		},
	}
}

func okResult() accountsvc.Result {
	return accountsvc.Result{OK: true, UserName: "tester"}
}

// 这是 P2-2 的验收条件：连续 2 次 AuthFailed 才翻红，恢复后归零。
//
// 走的是完整链路——applyAccountHealth 落库，再通过 GET /api/accounts
// 读回来，断言的是前端真正会看到的那个 level。
func TestHealthNeedsTwoAuthFailuresToTurnRed(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	id := createAccountForHealth(t, h, "东京主号", "TYO", "health1")

	ctx := context.Background()
	// 先成功一次，建立"本来是好的"这个前提。
	h.api.applyAccountHealth(ctx, id, okResult())
	if got := readLevel(t, h, id); got != "green" {
		t.Fatalf("首次成功校验后 level = %q，期望 green", got)
	}

	// 第一次 401：黄。
	h.api.applyAccountHealth(ctx, id, authFailResult())
	if got := readLevel(t, h, id); got != "yellow" {
		t.Errorf("单次 401 后 level = %q，期望 yellow", got)
	}

	// 第二次 401：红。
	h.api.applyAccountHealth(ctx, id, authFailResult())
	if got := readLevel(t, h, id); got != "red" {
		t.Errorf("连续两次 401 后 level = %q，期望 red", got)
	}

	// 库里要能查到 fatal，前端顶部红区块靠它筛。
	rec, err := h.store.GetAccountHealth(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Fatal || rec.ConsecutiveFailures != 2 {
		t.Errorf("fatal/连续失败次数 = %v/%d，期望 true/2", rec.Fatal, rec.ConsecutiveFailures)
	}

	// 恢复：必须立刻变绿并归零，否则下次再失败一次就又红了，
	// 等于阈值退化成 1。
	h.api.applyAccountHealth(ctx, id, okResult())
	if got := readLevel(t, h, id); got != "green" {
		t.Errorf("恢复后 level = %q，期望 green", got)
	}
	rec, err = h.store.GetAccountHealth(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Fatal || rec.ConsecutiveFailures != 0 {
		t.Errorf("恢复后 fatal/连续失败次数 = %v/%d，期望 false/0",
			rec.Fatal, rec.ConsecutiveFailures)
	}
}

// readLevel 从列表接口里取出某个账号的 level。
//
// 特意走 HTTP 而不是直接读库：这一层要验证的是"level 被算出来并塞进了
// 响应"，直接读库会漏掉序列化与字段名写错这类问题。
func readLevel(t *testing.T, h *harness, accountID string) string {
	t.Helper()
	resp := h.get("/api/accounts")
	expectStatus(t, resp, http.StatusOK)
	listed := decode[healthAccountsEnvelope](t, resp)
	for _, a := range listed.Accounts {
		if a.ID == accountID {
			return a.Health.Level
		}
	}
	t.Fatalf("列表里找不到账号 %s", accountID)
	return ""
}

// 限流与配额各有自己的颜色：前者等一会儿会好，后者必须人去调限额。
// 都画成红色会让用户对红块麻木。
func TestHealthColorByErrorClass(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	ctx := context.Background()

	cases := []struct {
		class string
		want  string
		fatal bool
	}{
		{ociclient.ClassThrottled.String(), "yellow", false},
		{ociclient.ClassQuotaExceeded.String(), "orange", false},
		// 网络抖动说明不了账号有问题。
		{ociclient.ClassTransient.String(), "green", false},
		// 单次凭据失败：黄。
		{ociclient.ClassAuthFailed.String(), "yellow", true},
		{ociclient.ClassNotAuthorized.String(), "yellow", true},
	}

	for i, c := range cases {
		suffix := "cls" + string(rune('a'+i))
		id := createAccountForHealth(t, h, "账号"+suffix, "C"+string(rune('A'+i)), suffix)
		// 先成功一次，让 LastOKAt 早于 LastErrorAt。
		h.api.applyAccountHealth(ctx, id, okResult())
		h.api.applyAccountHealth(ctx, id, accountsvc.Result{
			OK: false, ErrorCode: "X", ErrorText: "出错了",
			ErrorClass: c.class, AccountFatal: c.fatal,
		})
		if got := readLevel(t, h, id); got != c.want {
			t.Errorf("%s 的 level = %q，期望 %q", c.class, got, c.want)
		}
	}
}

// 红块文案必须引导用户去控制台确认，不能断言"已被封号"。
//
// OCI 对「无权限」和「资源不存在」返回同一个错误码，从 API 侧区分不出来。
func TestHealthRedAdviceWording(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	id := createAccountForHealth(t, h, "东京主号", "TYO", "wording")

	ctx := context.Background()
	h.api.applyAccountHealth(ctx, id, authFailResult())
	h.api.applyAccountHealth(ctx, id, authFailResult())

	resp := h.get("/api/accounts/" + id + "/health")
	expectStatus(t, resp, http.StatusOK)
	got := decode[healthEnvelope](t, resp)

	if got.Level != "red" {
		t.Fatalf("level = %q，期望 red", got.Level)
	}
	if got.Label != accountsvc.HealthLabelRed {
		t.Errorf("label = %q，期望 %q", got.Label, accountsvc.HealthLabelRed)
	}
	if got.Advice != accountsvc.HealthAdviceRed {
		t.Errorf("advice = %q，期望 %q", got.Advice, accountsvc.HealthAdviceRed)
	}
}

// 从未校验过的账号应该是 unknown，而不是被当成绿。
//
// 把"没查过"画成绿，等于向用户保证了一件没人验证过的事。
func TestHealthUnknownForFreshAccount(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	id := createAccountForHealth(t, h, "东京主号", "TYO", "fresh")

	if got := readLevel(t, h, id); got != "unknown" {
		t.Errorf("新建账号 level = %q，期望 unknown", got)
	}

	resp := h.get("/api/accounts/" + id + "/health")
	expectStatus(t, resp, http.StatusOK)
	if got := decode[healthEnvelope](t, resp); got.Level != "unknown" {
		t.Errorf("单账号接口 level = %q，期望 unknown", got.Level)
	}
}

func TestClearHealthRemovesRedBlock(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	id := createAccountForHealth(t, h, "东京主号", "TYO", "clear")

	ctx := context.Background()
	h.api.applyAccountHealth(ctx, id, authFailResult())
	h.api.applyAccountHealth(ctx, id, authFailResult())
	if got := readLevel(t, h, id); got != "red" {
		t.Fatalf("准备阶段 level = %q，期望 red", got)
	}

	resp := h.do("DELETE", "/api/accounts/"+id+"/health", nil, true)
	expectStatus(t, resp, http.StatusOK)
	if got := decode[healthEnvelope](t, resp); got.Level != "unknown" {
		t.Errorf("清除后 level = %q，期望 unknown", got.Level)
	}

	if got := readLevel(t, h, id); got != "unknown" {
		t.Errorf("清除后列表 level = %q，期望 unknown", got)
	}

	// 清除必须留痕：它让一个告警消失了，事后要能查到是谁点的。
	logs, _, err := h.store.ListAudit(ctx, store.AuditFilter{AccountID: id, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range logs {
		if e.Action == "account_health_clear" {
			found = true
		}
	}
	if !found {
		t.Error("清除健康度应记审计")
	}
}

// 清除之后再失败一次，红块必须重新出现。
//
// 如果清除被实现成"永久静音"，一个真的坏掉的账号会一直安静地躺在列表里。
func TestClearHealthDoesNotMuteFutureFailures(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	id := createAccountForHealth(t, h, "东京主号", "TYO", "mute")

	ctx := context.Background()
	h.api.applyAccountHealth(ctx, id, authFailResult())
	h.api.applyAccountHealth(ctx, id, authFailResult())

	resp := h.do("DELETE", "/api/accounts/"+id+"/health", nil, true)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// 清除后计数从零开始，所以单次失败还是黄。
	h.api.applyAccountHealth(ctx, id, authFailResult())
	if got := readLevel(t, h, id); got != "yellow" {
		t.Errorf("清除后单次失败 level = %q，期望 yellow", got)
	}
	// 第二次才红。
	h.api.applyAccountHealth(ctx, id, authFailResult())
	if got := readLevel(t, h, id); got != "red" {
		t.Errorf("清除后两次失败 level = %q，期望 red", got)
	}
}

func TestHealthEndpointsRequireAuth(t *testing.T) {
	h := newHarness(t)

	resp := h.get("/api/accounts/some-id/health")
	expectStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()

	// 清除是写操作，缺 CSRF 头必须被拒。
	resp = h.do("DELETE", "/api/accounts/some-id/health", nil, false)
	expectStatus(t, resp, http.StatusForbidden)
	resp.Body.Close()
}

func TestGetHealthUnknownAccount(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	// 不存在的账号必须 404，不能返回一个"尚未校验"的块——
	// 那会让前端以为账号是好的。
	resp := h.get("/api/accounts/does-not-exist/health")
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}
