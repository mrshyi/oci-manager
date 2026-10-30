package httpapi

import (
	"net/http"
	"testing"

	"ocicore/internal/store"
)

// 运维任务的接口层测试。
//
// 这一层的价值不在于"把请求转发给 store"，而在于三处容易写错的翻译：
// 非法迁移要回 409 而不是 400（请求没错，是状态不对）、未知动作要回 400、
// 进度字段要从 JSON 字符串解成对象。三者写错的表现都是"前端行为诡异"
// 而不是报错。

type opsTaskResponse struct {
	Task   opsTaskDTO `json:"task"`
	Notice string     `json:"notice"`
}

type opsTaskListResponse struct {
	Tasks  []opsTaskDTO `json:"tasks"`
	States []string     `json:"states"`
}

// newOpsTaskFor 直接往库里塞一条任务，绕过还没有的创建接口。
func newOpsTaskFor(t *testing.T, h *harness, accountID, kind, name string) *store.OpsTask {
	t.Helper()
	task, err := h.store.CreateOpsTask(t.Context(), store.OpsTask{
		Kind:       kind,
		AccountID:  accountID,
		InstanceID: "ocid1.instance.oc1..aaaa",
		Name:       name,
		Spec:       `{"cidr":"203.0.113.0/24"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// TestOpsTaskListRequiresAuth 确认未登录读不到任务。
func TestOpsTaskListRequiresAuth(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	// 先登出，模拟未认证会话。
	if resp := h.post("/api/auth/logout", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("登出失败: %d", resp.StatusCode)
	}
	resp := h.get("/api/tasks")
	expectStatus(t, resp, http.StatusUnauthorized)
}

// TestOpsTaskListShowsAliasAndProgress 确认列表把辅助信息拼好了。
//
// 别名与进度都是"多一次查找"就能拿到的东西，交给前端拼装只会多一处不同步。
func TestOpsTaskListShowsAliasAndProgress(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京主号", "TYO", "tokyo")

	task := newOpsTaskFor(t, h, acc.ID, store.OpsKindChangeIP, "换个 IP")
	if err := h.store.AdvanceOpsTask(t.Context(), task.ID, store.OpsTaskProgress{
		Stage:    "ASSIGN",
		Progress: `{"round":3,"current":"203.0.113.9"}`,
	}); err != nil {
		t.Fatal(err)
	}

	resp := h.get("/api/tasks")
	expectStatus(t, resp, http.StatusOK)
	body := decode[opsTaskListResponse](t, resp)

	if len(body.Tasks) != 1 {
		t.Fatalf("任务数 = %d，期望 1", len(body.Tasks))
	}
	got := body.Tasks[0]
	if got.AccountAlias != "东京主号" {
		t.Errorf("账号别名 = %q，期望「东京主号」", got.AccountAlias)
	}
	if got.KindLabel != "更换公网 IP" {
		t.Errorf("类型中文名 = %q", got.KindLabel)
	}
	if got.Progress["round"] != float64(3) {
		t.Errorf("进度没有被解成对象：%+v", got.Progress)
	}
	if len(body.States) != 6 {
		t.Errorf("状态清单 = %v，期望 6 个", body.States)
	}
}

// TestOpsTaskActionLifecycle 走一遍暂停 / 恢复 / 取消。
func TestOpsTaskActionLifecycle(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京主号", "TYO", "tokyo")
	task := newOpsTaskFor(t, h, acc.ID, store.OpsKindRescue, "救援")

	// pending → resume（等价于启动）是合法的：状态机里 pending → running。
	resp := h.post("/api/tasks/"+task.ID+"/resume", nil)
	expectStatus(t, resp, http.StatusOK)
	if got := decode[opsTaskResponse](t, resp).Task.State; got != store.OpsRunning {
		t.Fatalf("resume 之后状态 = %q，期望 %q", got, store.OpsRunning)
	}

	resp = h.post("/api/tasks/"+task.ID+"/pause", nil)
	expectStatus(t, resp, http.StatusOK)
	body := decode[opsTaskResponse](t, resp)
	if body.Task.State != store.OpsPaused {
		t.Fatalf("pause 之后状态 = %q，期望 %q", body.Task.State, store.OpsPaused)
	}
	if body.Notice == "" {
		t.Error("成功响应应当带一句说明")
	}

	resp = h.post("/api/tasks/"+task.ID+"/cancel", nil)
	expectStatus(t, resp, http.StatusOK)
	body = decode[opsTaskResponse](t, resp)
	if body.Task.State != store.OpsCanceled {
		t.Fatalf("cancel 之后状态 = %q，期望 %q", body.Task.State, store.OpsCanceled)
	}
	// 取消的文案必须说清楚"云端已发生的变更不会回滚"，
	// 否则用户会以为取消等于撤销。
	if body.Notice == "" {
		t.Error("取消应当给出说明")
	}
}

// TestOpsTaskIllegalTransitionIs409 确认非法迁移回 409 而不是 400。
//
// 400 会让前端以为参数写错了，去改请求体；实际是任务状态不对，
// 该做的是刷新页面看到真实状态。
func TestOpsTaskIllegalTransitionIs409(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京主号", "TYO", "tokyo")
	task := newOpsTaskFor(t, h, acc.ID, store.OpsKindChangeIP, "换个 IP")

	// pending 状态下没有东西可"恢复"——resume 会把它推到 running，
	// 所以这里改用"从 pending 直接暂停"：状态机里 pending → paused 不允许。
	resp := h.post("/api/tasks/"+task.ID+"/pause", nil)
	expectStatus(t, resp, http.StatusConflict)
	body := decode[errorBody](t, resp)
	if body.Code != "illegal_transition" {
		t.Errorf("错误码 = %q，期望 illegal_transition", body.Code)
	}
}

// TestOpsTaskUnknownAction 确认未知动作回 400。
func TestOpsTaskUnknownAction(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京主号", "TYO", "tokyo")
	task := newOpsTaskFor(t, h, acc.ID, store.OpsKindChangeIP, "换个 IP")

	resp := h.post("/api/tasks/"+task.ID+"/restart", nil)
	expectStatus(t, resp, http.StatusBadRequest)
}

// TestOpsTaskNotFound 确认不存在的任务回 404。
func TestOpsTaskNotFound(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	resp := h.get("/api/tasks/does-not-exist")
	expectStatus(t, resp, http.StatusNotFound)

	resp = h.post("/api/tasks/does-not-exist/pause", nil)
	expectStatus(t, resp, http.StatusNotFound)
}

// TestOpsTaskListFilters 确认查询参数生效。
func TestOpsTaskListFilters(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	accA := seedAccount(t, h, "账号甲", "AAA", "aaaa")
	accB := seedAccount(t, h, "账号乙", "BBB", "bbbb")

	newOpsTaskFor(t, h, accA.ID, store.OpsKindChangeIP, "甲换 IP")
	newOpsTaskFor(t, h, accA.ID, store.OpsKindRescue, "甲救援")
	newOpsTaskFor(t, h, accB.ID, store.OpsKindChangeIP, "乙换 IP")

	cases := []struct {
		query string
		want  int
	}{
		{"", 3},
		{"?accountId=" + accA.ID, 2},
		{"?kind=" + store.OpsKindChangeIP, 2},
		{"?accountId=" + accB.ID + "&kind=" + store.OpsKindRescue, 0},
		{"?state=" + store.OpsSucceeded, 0},
	}
	for _, c := range cases {
		resp := h.get("/api/tasks" + c.query)
		expectStatus(t, resp, http.StatusOK)
		body := decode[opsTaskListResponse](t, resp)
		if len(body.Tasks) != c.want {
			t.Errorf("查询 %q 得到 %d 条，期望 %d 条", c.query, len(body.Tasks), c.want)
		}
	}
}

// TestOpsTaskDeleteOnlyTerminal 确认只有终态任务能被删。
func TestOpsTaskDeleteOnlyTerminal(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京主号", "TYO", "tokyo")

	running := newOpsTaskFor(t, h, acc.ID, store.OpsKindChangeIP, "在跑的")
	if _, err := h.store.TransitionOpsTask(t.Context(), running.ID, store.OpsRunning); err != nil {
		t.Fatal(err)
	}
	resp := h.do("DELETE", "/api/tasks/"+running.ID, nil, true)
	expectStatus(t, resp, http.StatusConflict)

	done := newOpsTaskFor(t, h, acc.ID, store.OpsKindChangeIP, "已取消的")
	if _, err := h.store.TransitionOpsTask(t.Context(), done.ID, store.OpsCanceled); err != nil {
		t.Fatal(err)
	}
	resp = h.do("DELETE", "/api/tasks/"+done.ID, nil, true)
	expectStatus(t, resp, http.StatusOK)
}
