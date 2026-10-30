package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"ocicore/internal/opssvc"
	"ocicore/internal/store"
)

// P1-5 批量任务化。
//
// 这一组防的是「阈值两侧行为错位」：超过同步上限却仍走同步（请求挂住），
// 或者没超过却莫名其妙转成任务（用户以为已经执行完了）。两者的表现都不像
// bug，只像"有点慢"或"怎么没生效"。

// seedInstanceIn 往本地库塞一台指定状态的实例。
//
// 状态用 STOPPING 而不是 RUNNING：instancesvc.Action 会在发请求之前就因为
// "过渡态不允许操作"返回，于是同步路径的测试不必真的去连 Oracle。
func seedInstanceIn(t *testing.T, h *harness, id, accountID, state string) {
	t.Helper()
	if err := h.store.UpsertInstance(t.Context(), store.Instance{
		ID: id, AccountID: accountID, Region: "ap-tokyo-1",
		DisplayName: id, LifecycleState: state,
	}); err != nil {
		t.Fatal(err)
	}
}

func manyIDs(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, "i-"+itoa(i))
	}
	return out
}

// 恰好 20 台交给 jobsvc：202 + 一个 job，不产生 ops_tasks。
//
// 合并上游 v0.5.x 之后这条边界变了。原先是「≤20 台同步执行、>20 台转
// ops_tasks」，上游把同步上限内的批量也改成了后台任务，于是现在的分界是：
//
//	≤ maxBulkSize  → jobsvc 任务（响应体是 job）
//	> maxBulkSize  → opssvc 运维任务（响应体是 tasks）
//
// 两者都返回 202，只能靠响应体区分。仍然要防的是「阈值两侧错位」：
// 20 台不该被拆成 ops_tasks，21 台不该只给一个 job。
func TestBulkAtSyncLimitUsesJobsvc(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京主号", "TYO", "sync20")

	list := manyIDs(maxBulkSize)
	for _, id := range list {
		seedInstanceIn(t, h, id, acc.ID, "STOPPING")
	}

	resp := h.bulkStart(list, acc.Alias)
	expectStatus(t, resp, http.StatusAccepted)
	out := decode[struct {
		Job *struct {
			ID    string `json:"id"`
			Kind  string `json:"kind"`
			Title string `json:"title"`
		} `json:"job"`
		Tasks []any `json:"tasks"`
	}](t, resp)

	if out.Job == nil || out.Job.ID == "" {
		t.Fatal("同步上限内应返回一个 jobsvc 任务")
	}
	if out.Job.Kind != "instance.bulk" {
		t.Errorf("job kind = %q，期望 instance.bulk", out.Job.Kind)
	}
	if len(out.Tasks) != 0 {
		t.Errorf("%d 台不该拆成 ops_tasks，实际产生 %d 个", maxBulkSize, len(out.Tasks))
	}
	if !strings.Contains(out.Job.Title, itoa(maxBulkSize)) {
		t.Errorf("任务标题应写明台数，实际 %q", out.Job.Title)
	}
}

// 21 台自动转任务：202 + 一个 kind=bulk 的任务，任务里带着全部实例。
func TestBulkOverSyncLimitCreatesTask(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京主号", "TYO", "async21")

	list := manyIDs(maxBulkSize + 1)
	for _, id := range list {
		seedInstanceIn(t, h, id, acc.ID, "RUNNING")
	}

	resp := h.bulkStart(list, acc.Alias)
	expectStatus(t, resp, http.StatusAccepted)
	out := decode[struct {
		Tasks []struct {
			ID        string `json:"id"`
			Kind      string `json:"kind"`
			State     string `json:"state"`
			Name      string `json:"name"`
			KindLabel string `json:"kindLabel"`
		} `json:"tasks"`
		Notice string `json:"notice"`
	}](t, resp)

	if len(out.Tasks) != 1 {
		t.Fatalf("同账号的批量应只产生 1 个任务，实际 %d 个", len(out.Tasks))
	}
	task := out.Tasks[0]
	if task.Kind != store.OpsKindBulk {
		t.Errorf("kind = %q，期望 %q", task.Kind, store.OpsKindBulk)
	}
	if task.State != store.OpsPending {
		t.Errorf("新任务应为 pending，实际 %q", task.State)
	}
	if !strings.Contains(task.Name, "21 台") {
		t.Errorf("任务名应写明台数，实际 %q", task.Name)
	}
	if !strings.Contains(out.Notice, "任务") {
		t.Errorf("应说明已转为后台任务：%q", out.Notice)
	}

	// 任务里必须带着全部 21 台，且顺序与请求一致。
	stored, err := h.store.GetOpsTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := opssvc.DecodeBulkSpec(stored.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.InstanceIDs) != len(list) {
		t.Fatalf("任务里只有 %d 台，期望 %d 台", len(spec.InstanceIDs), len(list))
	}
	for i, id := range list {
		if spec.InstanceIDs[i] != id {
			t.Fatalf("第 %d 台 = %q，期望 %q（执行顺序应与请求一致）", i, spec.InstanceIDs[i], id)
		}
	}
	if spec.Action != "START" {
		t.Errorf("action = %q，期望 START", spec.Action)
	}

	// 任务页应能按 kind 过滤到它。
	resp = h.get("/api/tasks?kind=bulk")
	expectStatus(t, resp, http.StatusOK)
	listed := decode[struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}](t, resp)
	if len(listed.Tasks) != 1 || listed.Tasks[0].ID != task.ID {
		t.Fatalf("任务列表未包含新建的批量任务: %+v", listed.Tasks)
	}
}

// 跨账号的批量按账号拆成多个任务：ops_tasks.account_id 是单值且有外键。
func TestBulkSplitsTasksPerAccount(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	a := seedAccount(t, h, "东京主号", "TYO", "splitA")
	b := seedAccount(t, h, "大阪备号", "OSA", "splitB")

	list := make([]string, 0, maxBulkSize+1)
	for i := 0; i < maxBulkSize+1; i++ {
		id := "i-" + itoa(i)
		owner := a.ID
		if i%2 == 1 {
			owner = b.ID
		}
		seedInstanceIn(t, h, id, owner, "RUNNING")
		list = append(list, id)
	}

	// 跨账号要回传 ALL，否则连确认门槛都过不去。
	resp := h.bulkStart(list, bulkCrossAccountWord)
	expectStatus(t, resp, http.StatusAccepted)
	out := decode[struct {
		Tasks []struct {
			ID        string `json:"id"`
			AccountID string `json:"accountId"`
		} `json:"tasks"`
	}](t, resp)

	if len(out.Tasks) != 2 {
		t.Fatalf("两个账号应拆成 2 个任务，实际 %d 个", len(out.Tasks))
	}
	seen := map[string]int{}
	for _, task := range out.Tasks {
		stored, err := h.store.GetOpsTask(t.Context(), task.ID)
		if err != nil {
			t.Fatal(err)
		}
		spec, err := opssvc.DecodeBulkSpec(stored.Spec)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range spec.InstanceIDs {
			inst, err := h.store.GetInstance(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if inst.AccountID != stored.AccountID {
				t.Errorf("任务 %s（账号 %s）里混入了账号 %s 的实例 %s",
					task.ID, stored.AccountID, inst.AccountID, id)
			}
			seen[id]++
		}
	}
	// 每台只出现在一个任务里，且一台都不能漏。
	for _, id := range list {
		if seen[id] != 1 {
			t.Errorf("%s 出现在 %d 个任务里，期望 1 个", id, seen[id])
		}
	}
}

// 本地库里没有的实例要整体拒绝，不能静默跳过。
//
// 静默跳过的后果是：用户以为 25 台都关了，实际有几台根本没被处理，而且
// 界面上看不出任何异常。
func TestBulkRejectsUnknownInstance(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京主号", "TYO", "unknown")

	list := manyIDs(maxBulkSize + 1)
	// 少建一台，模拟"本地库还没同步到"。
	for _, id := range list[:len(list)-1] {
		seedInstanceIn(t, h, id, acc.ID, "RUNNING")
	}

	resp := h.bulkStart(list, acc.Alias)
	expectStatus(t, resp, http.StatusBadRequest)
	body := decode[errorBody](t, resp)
	if body.Code != "unknown_instance" {
		t.Fatalf("错误码 = %q，期望 unknown_instance", body.Code)
	}
	if !strings.Contains(body.Message, list[len(list)-1]) {
		t.Errorf("提示里应点名是哪台实例：%q", body.Message)
	}

	// 被拒之后不该留下半个任务。
	tasks, err := h.store.ListOpsTasks(t.Context(), store.OpsTaskFilter{Kind: store.OpsKindBulk})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("被拒的请求不该建任务，实际 %d 个", len(tasks))
	}
}

// 超过单任务上限要拒绝，而不是悄悄截断。
func TestBulkRejectsTooMany(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	seedAccount(t, h, "东京主号", "TYO", "toomany")

	// 这一步在确认门槛之前，所以不需要 confirm 参数。
	resp := h.bulkStart(manyIDs(maxBulkTaskSize+1), "")
	expectStatus(t, resp, http.StatusBadRequest)
	body := decode[errorBody](t, resp)
	if body.Code != "too_many" {
		t.Fatalf("错误码 = %q，期望 too_many", body.Code)
	}
}

// 重复的实例 ID 要去重：重复提交会让第二遍撞上「实例正在转换中」，
// 用户看到一条莫名其妙的失败。
func TestBulkDeduplicatesIDs(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	acc := seedAccount(t, h, "东京主号", "TYO", "dedupe")

	list := manyIDs(maxBulkSize + 1)
	for _, id := range list {
		seedInstanceIn(t, h, id, acc.ID, "RUNNING")
	}
	// 把第一台重复 5 次。
	withDupes := append(append([]string{}, list...), list[0], list[0], list[0], list[0], list[0])

	resp := h.bulkStart(withDupes, acc.Alias)
	expectStatus(t, resp, http.StatusAccepted)
	out := decode[struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}](t, resp)

	stored, err := h.store.GetOpsTask(t.Context(), out.Tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := opssvc.DecodeBulkSpec(stored.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.InstanceIDs) != len(list) {
		t.Fatalf("去重后应剩 %d 台，实际 %d 台", len(list), len(spec.InstanceIDs))
	}
}
