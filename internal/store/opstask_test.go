package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// newOpsTask 建一个挂着真实账号的任务。外键要求账号必须先存在。
func newOpsTask(t *testing.T, st *Store, accountID string, mutate func(*OpsTask)) *OpsTask {
	t.Helper()
	in := OpsTask{
		Kind:       OpsKindChangeIP,
		AccountID:  accountID,
		InstanceID: "ocid1.instance.oc1..aaaa",
		Name:       "换 IP",
		Spec:       `{"cidr":"203.0.113.0/24"}`,
	}
	if mutate != nil {
		mutate(&in)
	}
	task, err := st.CreateOpsTask(context.Background(), in)
	if err != nil {
		t.Fatalf("创建运维任务失败: %v", err)
	}
	return task
}

// TestOpsStateMachineMatrix 穷举迁移表，确认只有写明的组合被放行。
//
// 状态机写错的典型症状是"多放行一条"：用户从 succeeded 又点了一次恢复，
// 任务重新跑起来，而所有日志看起来都正常。用穷举而不是挑几条测，
// 是因为漏掉的那条恰好就是将来会被人踩到的那条。
func TestOpsStateMachineMatrix(t *testing.T) {
	states := []string{OpsPending, OpsRunning, OpsSucceeded, OpsFailed, OpsPaused, OpsCanceled}
	allowed := map[[2]string]bool{
		{OpsPending, OpsRunning}:  true,
		{OpsPending, OpsCanceled}: true,

		{OpsRunning, OpsSucceeded}: true,
		{OpsRunning, OpsFailed}:    true,
		{OpsRunning, OpsPaused}:    true,
		{OpsRunning, OpsCanceled}:  true,

		{OpsPaused, OpsRunning}:  true,
		{OpsPaused, OpsCanceled}: true,

		{OpsFailed, OpsPending}:  true,
		{OpsFailed, OpsCanceled}: true,

		{OpsSucceeded, OpsSucceeded}: true,
		{OpsCanceled, OpsCanceled}:   true,
		{OpsPending, OpsPending}:     true,
		{OpsRunning, OpsRunning}:     true,
		{OpsFailed, OpsFailed}:       true,
		{OpsPaused, OpsPaused}:       true,
	}

	for _, from := range states {
		for _, to := range states {
			want := allowed[[2]string{from, to}]
			if got := CanTransitionOps(from, to); got != want {
				t.Errorf("CanTransitionOps(%s → %s) = %v，期望 %v", from, to, got, want)
			}
		}
	}
}

// TestOpsStateMachineRejectsUnknownState 确认未知状态一律拒绝。
func TestOpsStateMachineRejectsUnknownState(t *testing.T) {
	if CanTransitionOps(OpsPending, "done") {
		t.Error("未知目标状态不该被放行")
	}
	if CanTransitionOps("weird", OpsRunning) {
		t.Error("未知起始状态不该被放行")
	}
	if CanTransitionOps("", "") {
		t.Error("空状态不该被当成合法的自迁移")
	}
}

// TestTransitionOpsTaskRejectsIllegal 确认非法迁移被数据库层拒绝。
//
// 表里的白名单只是第一道；真正的防线是 TransitionOpsTask 里的事务校验，
// 因为调用方可能拿到一个过期的任务对象直接改。
func TestTransitionOpsTaskRejectsIllegal(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	acc, err := st.CreateAccount(ctx, testAccount(t, "迁移测试", "trn", "aaaa"))
	if err != nil {
		t.Fatal(err)
	}

	task := newOpsTask(t, st, acc.ID, nil)

	// pending → succeeded 不允许：没跑过就宣布成功，等于跳过了执行。
	if _, err := st.TransitionOpsTask(ctx, task.ID, OpsSucceeded); !errors.Is(err, ErrOpsTaskIllegalTransition) {
		t.Fatalf("pending → succeeded 应当被拒，实际 %v", err)
	}
	// 状态没被改坏。
	got, err := st.GetOpsTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != OpsPending {
		t.Fatalf("非法迁移之后状态变成了 %q，期望保持 %q", got.State, OpsPending)
	}

	// 走完合法路径之后，终态不能再出来。
	if _, err := st.TransitionOpsTask(ctx, task.ID, OpsRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionOpsTask(ctx, task.ID, OpsSucceeded); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionOpsTask(ctx, task.ID, OpsRunning); !errors.Is(err, ErrOpsTaskIllegalTransition) {
		t.Fatalf("succeeded → running 应当被拒，实际 %v", err)
	}
}

// TestTransitionOpsTaskIsIdempotent 确认重复表达同一意图不算错误。
//
// 用户连点两次"暂停"、或者前端重试一次请求，都不该看到报错。
func TestTransitionOpsTaskIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	acc, err := st.CreateAccount(ctx, testAccount(t, "幂等测试", "idm", "bbbb"))
	if err != nil {
		t.Fatal(err)
	}
	task := newOpsTask(t, st, acc.ID, nil)

	if _, err := st.TransitionOpsTask(ctx, task.ID, OpsRunning); err != nil {
		t.Fatal(err)
	}
	first, err := st.TransitionOpsTask(ctx, task.ID, OpsPaused)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.TransitionOpsTask(ctx, task.ID, OpsPaused)
	if err != nil {
		t.Fatalf("重复暂停不该报错：%v", err)
	}
	if first.State != second.State {
		t.Fatalf("两次暂停得到不同状态：%q / %q", first.State, second.State)
	}
}

// TestTransitionOpsTaskTimestamps 确认时间戳的语义。
//
// started_at 只在第一次进入 running 时写：暂停再恢复不该把它刷新成"刚启动"，
// 否则"这个任务跑了多久"永远显示几秒钟。
func TestTransitionOpsTaskTimestamps(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	acc, err := st.CreateAccount(ctx, testAccount(t, "时间戳测试", "tsp", "cccc"))
	if err != nil {
		t.Fatal(err)
	}
	task := newOpsTask(t, st, acc.ID, nil)

	if !task.StartedAt.IsZero() {
		t.Fatalf("新建任务的 startedAt 应当是零值，实际 %v", task.StartedAt)
	}
	if task.NextAt.IsZero() {
		t.Fatal("新建任务应当立刻可执行（nextAt 非零）")
	}

	running, err := st.TransitionOpsTask(ctx, task.ID, OpsRunning)
	if err != nil {
		t.Fatal(err)
	}
	if running.StartedAt.IsZero() {
		t.Fatal("进入 running 之后 startedAt 应当有值")
	}
	firstStart := running.StartedAt

	// 暂停 → 恢复，started_at 保持不变。
	if _, err := st.TransitionOpsTask(ctx, task.ID, OpsPaused); err != nil {
		t.Fatal(err)
	}
	// 时间戳精度是秒，中间跨一秒才看得出差别；这里只要求不被刷新成更晚的值。
	resumed, err := st.TransitionOpsTask(ctx, task.ID, OpsRunning)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.StartedAt.Equal(firstStart) {
		t.Fatalf("恢复之后 startedAt 从 %v 变成了 %v——暂停再恢复不该重置启动时间",
			firstStart, resumed.StartedAt)
	}

	done, err := st.TransitionOpsTask(ctx, task.ID, OpsSucceeded)
	if err != nil {
		t.Fatal(err)
	}
	if done.FinishedAt.IsZero() {
		t.Fatal("进入终态之后 finishedAt 应当有值")
	}
	if !done.NextAt.IsZero() {
		t.Fatalf("终态任务的 nextAt 应当清零，实际 %v——否则调度器还会捞它", done.NextAt)
	}
}

// TestDueOpsTasksExcludesPaused 是 P1-1 验收里的一条。
//
// 暂停的任务被捞到的话，"暂停"按钮点了等于没点，而界面上的状态确实是 paused
// ——看起来完全正常。
func TestDueOpsTasksExcludesPaused(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	acc, err := st.CreateAccount(ctx, testAccount(t, "到期测试", "due", "dddd"))
	if err != nil {
		t.Fatal(err)
	}

	running := newOpsTask(t, st, acc.ID, func(t *OpsTask) { t.Name = "在跑的" })
	if _, err := st.TransitionOpsTask(ctx, running.ID, OpsRunning); err != nil {
		t.Fatal(err)
	}
	paused := newOpsTask(t, st, acc.ID, func(t *OpsTask) { t.Name = "暂停的" })
	if _, err := st.TransitionOpsTask(ctx, paused.ID, OpsRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionOpsTask(ctx, paused.ID, OpsPaused); err != nil {
		t.Fatal(err)
	}
	pending := newOpsTask(t, st, acc.ID, func(t *OpsTask) { t.Name = "还没跑的" })

	due, err := st.DueOpsTasks(ctx, time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, task := range due {
		ids[task.ID] = true
	}
	if !ids[running.ID] {
		t.Error("running 的任务应当被捞到")
	}
	if !ids[pending.ID] {
		t.Error("pending 的任务应当被捞到")
	}
	if ids[paused.ID] {
		t.Error("paused 的任务不该被捞到——暂停按钮会变成摆设")
	}
}

// TestDueOpsTasksExcludesTerminal 确认终态任务不再被捞。
func TestDueOpsTasksExcludesTerminal(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	acc, err := st.CreateAccount(ctx, testAccount(t, "终态测试", "trm", "eeee"))
	if err != nil {
		t.Fatal(err)
	}

	ok := newOpsTask(t, st, acc.ID, func(t *OpsTask) { t.Name = "成功的" })
	if _, err := st.TransitionOpsTask(ctx, ok.ID, OpsRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionOpsTask(ctx, ok.ID, OpsSucceeded); err != nil {
		t.Fatal(err)
	}
	bad := newOpsTask(t, st, acc.ID, func(t *OpsTask) { t.Name = "失败的" })
	if _, err := st.TransitionOpsTask(ctx, bad.ID, OpsRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionOpsTask(ctx, bad.ID, OpsFailed); err != nil {
		t.Fatal(err)
	}

	due, err := st.DueOpsTasks(ctx, time.Now().Add(24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("终态与 failed 的任务都不该被捞到，实际捞到 %d 条", len(due))
	}
}

// TestResetRunningOpsTasks 是 P1-1 验收里的另一条。
//
// 进程被杀时正在执行的任务，数据库里停在 running，而那个 goroutine 已经不存在。
// 不重置的话这些任务永远不会再被捞到——界面显示"运行中"，实际一动不动。
func TestResetRunningOpsTasks(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	acc, err := st.CreateAccount(ctx, testAccount(t, "重置测试", "rst", "ffff"))
	if err != nil {
		t.Fatal(err)
	}

	stuck := newOpsTask(t, st, acc.ID, func(t *OpsTask) { t.Name = "被杀的" })
	if _, err := st.TransitionOpsTask(ctx, stuck.ID, OpsRunning); err != nil {
		t.Fatal(err)
	}
	other := newOpsTask(t, st, acc.ID, func(t *OpsTask) { t.Name = "暂停的" })
	if _, err := st.TransitionOpsTask(ctx, other.ID, OpsRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionOpsTask(ctx, other.ID, OpsPaused); err != nil {
		t.Fatal(err)
	}

	n, err := st.ResetRunningOpsTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("重置了 %d 条，期望恰好 1 条（只有 running 的）", n)
	}

	got, err := st.GetOpsTask(ctx, stuck.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != OpsPending {
		t.Fatalf("重置后状态 %q，期望 %q", got.State, OpsPending)
	}
	// 重置后必须立刻可执行，否则重启之后还要等一个间隔才动。
	due, err := st.DueOpsTasks(ctx, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, task := range due {
		if task.ID == stuck.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("重置后的任务应当立刻出现在到期列表里")
	}

	// 暂停的不受影响。
	paused, err := st.GetOpsTask(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != OpsPaused {
		t.Fatalf("暂停的任务被改成了 %q", paused.State)
	}
}

// TestCreateOpsTaskForcesPending 确认新任务不能直接建成 running。
func TestCreateOpsTaskForcesPending(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	acc, err := st.CreateAccount(ctx, testAccount(t, "初始态测试", "ini", "gggg"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.CreateOpsTask(ctx, OpsTask{
		Kind: OpsKindRescue, AccountID: acc.ID, State: OpsRunning,
	}); err == nil {
		t.Fatal("直接建成 running 应当被拒——那会绕过 started_at 的记录")
	}

	if _, err := st.CreateOpsTask(ctx, OpsTask{
		Kind: "", AccountID: acc.ID,
	}); err == nil {
		t.Fatal("缺少 kind 应当被拒")
	}
}

// TestOpsTaskDefaults 确认默认值被补上。
func TestOpsTaskDefaults(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	acc, err := st.CreateAccount(ctx, testAccount(t, "默认值测试", "def", "hhhh"))
	if err != nil {
		t.Fatal(err)
	}
	task := newOpsTask(t, st, acc.ID, nil)

	if task.State != OpsPending {
		t.Errorf("初始状态 %q，期望 %q", task.State, OpsPending)
	}
	if task.MaxAttempts != defaultOpsMaxAttempts {
		t.Errorf("MaxAttempts = %d，期望 %d", task.MaxAttempts, defaultOpsMaxAttempts)
	}
	if task.IntervalSeconds != defaultOpsIntervalSeconds {
		t.Errorf("IntervalSeconds = %d，期望 %d", task.IntervalSeconds, defaultOpsIntervalSeconds)
	}
	if task.Spec == "" || task.Progress == "" {
		t.Errorf("Spec / Progress 应当有默认的 {}，实际 %q / %q", task.Spec, task.Progress)
	}
}

// TestAdvanceOpsTaskCountsAttempts 确认 attempts 是自增而不是覆盖。
//
// 多轮任务（换 IP 循环 30 次）的尝试次数是判断"该停了"的唯一依据，
// 读-改-写丢一次计数就会让它多打几十次 OCI 接口。
func TestAdvanceOpsTaskCountsAttempts(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	acc, err := st.CreateAccount(ctx, testAccount(t, "进展测试", "adv", "iiii"))
	if err != nil {
		t.Fatal(err)
	}
	task := newOpsTask(t, st, acc.ID, nil)

	for i := 1; i <= 3; i++ {
		if err := st.AdvanceOpsTask(ctx, task.ID, OpsTaskProgress{
			Stage:     "ASSIGN",
			LastError: "没命中 CIDR",
			NextAt:    time.Now().Add(20 * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
		got, err := st.GetOpsTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Attempts != i {
			t.Fatalf("第 %d 轮之后 attempts = %d", i, got.Attempts)
		}
		if got.Stage != "ASSIGN" {
			t.Fatalf("stage = %q，期望 ASSIGN", got.Stage)
		}
	}
}

// TestAdvanceOpsTaskNotFound 确认对不存在的任务返回明确错误。
func TestAdvanceOpsTaskNotFound(t *testing.T) {
	st := newTestStore(t)
	err := st.AdvanceOpsTask(context.Background(), "nope", OpsTaskProgress{})
	if !errors.Is(err, ErrOpsTaskNotFound) {
		t.Fatalf("期望 ErrOpsTaskNotFound，实际 %v", err)
	}
}

// TestListOpsTasksFilters 确认过滤条件生效。
func TestListOpsTasksFilters(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	accA, err := st.CreateAccount(ctx, testAccount(t, "账号甲", "aaa", "jjjj"))
	if err != nil {
		t.Fatal(err)
	}
	accB, err := st.CreateAccount(ctx, testAccount(t, "账号乙", "bbb", "kkkk"))
	if err != nil {
		t.Fatal(err)
	}

	newOpsTask(t, st, accA.ID, func(t *OpsTask) { t.Kind = OpsKindChangeIP })
	newOpsTask(t, st, accA.ID, func(t *OpsTask) { t.Kind = OpsKindRescue })
	newOpsTask(t, st, accB.ID, func(t *OpsTask) { t.Kind = OpsKindChangeIP })

	cases := []struct {
		name string
		f    OpsTaskFilter
		want int
	}{
		{"全部", OpsTaskFilter{}, 3},
		{"按账号", OpsTaskFilter{AccountID: accA.ID}, 2},
		{"按类型", OpsTaskFilter{Kind: OpsKindChangeIP}, 2},
		{"账号 + 类型", OpsTaskFilter{AccountID: accA.ID, Kind: OpsKindRescue}, 1},
		{"按状态", OpsTaskFilter{State: OpsRunning}, 0},
	}
	for _, c := range cases {
		got, err := st.ListOpsTasks(ctx, c.f)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got) != c.want {
			t.Errorf("%s: 得到 %d 条，期望 %d 条", c.name, len(got), c.want)
		}
	}
}

// TestDeleteOpsTaskRejectsRunning 确认运行中的任务不能被删。
//
// 删掉行之后 goroutine 还在推进，进展无处可写，用户看到的是"任务消失了
// 但云端资源还在变"。
func TestDeleteOpsTaskRejectsRunning(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	acc, err := st.CreateAccount(ctx, testAccount(t, "删除测试", "del", "llll"))
	if err != nil {
		t.Fatal(err)
	}
	task := newOpsTask(t, st, acc.ID, nil)
	if _, err := st.TransitionOpsTask(ctx, task.ID, OpsRunning); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteOpsTask(ctx, task.ID); err == nil {
		t.Fatal("运行中的任务不该被删除")
	}

	if _, err := st.TransitionOpsTask(ctx, task.ID, OpsCanceled); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteOpsTask(ctx, task.ID); err != nil {
		t.Fatalf("终态任务应当可以删除：%v", err)
	}
	if _, err := st.GetOpsTask(ctx, task.ID); !errors.Is(err, ErrOpsTaskNotFound) {
		t.Fatalf("删除之后仍能读到：%v", err)
	}
}

// TestOpsTaskCascadeOnAccountDelete 确认账号删除时任务跟着走。
//
// 外键写了 ON DELETE CASCADE，但没有测试的话，将来有人改表结构时删掉
// 这一条也没人发现——直到某次删账号留下了一堆孤儿任务。
func TestOpsTaskCascadeOnAccountDelete(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	acc, err := st.CreateAccount(ctx, testAccount(t, "级联测试", "csc", "mmmm"))
	if err != nil {
		t.Fatal(err)
	}
	task := newOpsTask(t, st, acc.ID, nil)

	if err := st.DeleteAccount(ctx, acc.ID); err != nil {
		t.Fatalf("删除账号失败: %v", err)
	}
	if _, err := st.GetOpsTask(ctx, task.ID); !errors.Is(err, ErrOpsTaskNotFound) {
		t.Fatalf("账号删除后任务应当级联消失，实际 %v", err)
	}
}
