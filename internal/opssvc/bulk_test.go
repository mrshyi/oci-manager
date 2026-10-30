package opssvc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"ocicore/internal/store"
)

// fakeInstances 记录每台实例被操作了几次，并允许按 ID 配置失败。
type fakeInstances struct {
	mu    sync.Mutex
	calls map[string]int
	order []string
	fail  map[string]error
}

func newFakeInstances() *fakeInstances {
	return &fakeInstances{calls: map[string]int{}, fail: map[string]error{}}
}

func (f *fakeInstances) Action(_ context.Context, instanceID, _ string) (*store.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[instanceID]++
	f.order = append(f.order, instanceID)
	if err := f.fail[instanceID]; err != nil {
		return nil, err
	}
	return &store.Instance{ID: instanceID}, nil
}

func (f *fakeInstances) count(instanceID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[instanceID]
}

func (f *fakeInstances) setFail(instanceID string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[instanceID] = err
}

// bulkTask 造一个待执行的批量任务。
func bulkTask(t *testing.T, action string, ids []string) *store.OpsTask {
	t.Helper()
	raw, err := EncodeBulkSpec(BulkSpec{Action: action, InstanceIDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	return &store.OpsTask{ID: "task-bulk", Kind: store.OpsKindBulk, Spec: raw, State: store.OpsPending}
}

// runBulk 反复执行直到任务终结，把每轮的进展写回任务（与 run() 的落库等价）。
func runBulk(t *testing.T, svc *Service, task *store.OpsTask, maxRounds int) []outcome {
	t.Helper()
	outs := make([]outcome, 0, maxRounds)
	for i := 0; i < maxRounds; i++ {
		out := svc.handleBulk(context.Background(), task)
		outs = append(outs, out)
		if out.Progress != "" {
			task.Progress = out.Progress
		}
		if out.Stage != "" {
			task.Stage = out.Stage
		}
		if out.Done {
			return outs
		}
		task.State = store.OpsRunning
	}
	t.Fatalf("批量任务在 %d 轮内没有结束", maxRounds)
	return outs
}

func ids(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("i-%02d", i))
	}
	return out
}

// 25 台要分块跑完，每台恰好操作一次。
func TestBulkProcessesAllInChunks(t *testing.T) {
	f := newFakeInstances()
	svc := &Service{instances: f}
	list := ids(12)
	task := bulkTask(t, "STOP", list)

	outs := runBulk(t, svc, task, 10)

	// 12 台按每轮 5 台，需要 3 轮。
	if len(outs) != 3 {
		t.Fatalf("轮数 = %d，期望 3（12 台 / 每轮 5 台）", len(outs))
	}
	last := outs[len(outs)-1]
	if last.Err != nil {
		t.Fatalf("不应失败：%v", last.Err)
	}
	if last.Stage != bulkStageDone {
		t.Errorf("收尾阶段 = %q，期望 %q", last.Stage, bulkStageDone)
	}
	for _, id := range list {
		if n := f.count(id); n != 1 {
			t.Errorf("%s 被操作 %d 次，期望 1 次", id, n)
		}
	}

	prog := decodeBulkProgress(task.Progress)
	if prog.Total != 12 || prog.Succeeded != 12 {
		t.Errorf("progress = %+v，期望 total=12 succeeded=12", prog)
	}
	if len(prog.Pending) != 0 {
		t.Errorf("收尾后 Pending 应为空，实际 %v", prog.Pending)
	}
	if len(prog.Failures) != 0 {
		t.Errorf("收尾后不该有失败记录，实际 %v", prog.Failures)
	}
	if last.Result != "12 台全部执行成功" {
		t.Errorf("结果 = %q", last.Result)
	}
}

// 前面几台永久失败不能把后面的机器堵住。
//
// 这是"失败项排到队尾"的直接验收：如果失败项留在队首，每轮都会重试同样
// 那 5 台，直到 stuckRounds 用完直接判失败，后面 5 台一台都不会被碰。
func TestBulkFailureDoesNotBlockOthers(t *testing.T) {
	f := newFakeInstances()
	svc := &Service{instances: f}
	list := ids(10)
	for _, id := range list[:5] {
		f.setFail(id, errors.New("InstanceBusy"))
	}
	task := bulkTask(t, "STOP", list)

	outs := runBulk(t, svc, task, 20)
	last := outs[len(outs)-1]

	if last.Err == nil {
		t.Fatal("有 5 台永久失败，任务应当判失败")
	}
	// 后 5 台必须都被成功处理过。
	for _, id := range list[5:] {
		if n := f.count(id); n != 1 {
			t.Errorf("%s 被操作 %d 次，期望 1 次（不该被前面的失败堵住）", id, n)
		}
	}
	prog := decodeBulkProgress(task.Progress)
	if prog.Succeeded != 5 {
		t.Errorf("成功台数 = %d，期望 5", prog.Succeeded)
	}
	// 剩余清单必须恰好是失败的那 5 台——重试只会重试它们。
	if len(prog.Pending) != 5 {
		t.Fatalf("剩余 = %v，期望 5 台", prog.Pending)
	}
	for _, id := range prog.Pending {
		if id != list[0] && id != list[1] && id != list[2] && id != list[3] && id != list[4] {
			t.Errorf("剩余清单里出现了不该有的实例 %s", id)
		}
	}
}

// 失败后重试只重试失败的那几台，已经成功的不再操作一次。
func TestBulkRetryOnlyTouchesFailed(t *testing.T) {
	f := newFakeInstances()
	svc := &Service{instances: f}
	list := ids(6)
	f.setFail(list[0], errors.New("500 InternalError"))
	task := bulkTask(t, "START", list)

	outs := runBulk(t, svc, task, 20)
	if outs[len(outs)-1].Err == nil {
		t.Fatal("有一台永久失败，任务应当判失败")
	}

	// 用户点了「重试」：失败原因消失，任务从库里那份 progress 继续。
	f.setFail(list[0], nil)
	task.State = store.OpsPending
	outs = runBulk(t, svc, task, 5)
	last := outs[len(outs)-1]
	if last.Err != nil {
		t.Fatalf("重试后应当成功：%v", last.Err)
	}

	// 除第一台外，其余 5 台全程只该被操作一次。
	for _, id := range list[1:] {
		if n := f.count(id); n != 1 {
			t.Errorf("%s 被操作 %d 次，期望 1 次", id, n)
		}
	}
	prog := decodeBulkProgress(task.Progress)
	if prog.Succeeded != 6 || len(prog.Pending) != 0 {
		t.Errorf("progress = %+v，期望 6 台全成功且无剩余", prog)
	}
}

// 超过总时限要停手，并保留剩余清单。
func TestBulkExpiredFails(t *testing.T) {
	f := newFakeInstances()
	svc := &Service{instances: f}
	task := bulkTask(t, "STOP", ids(6))
	task.ExpiresAt = time.Now().Add(-time.Minute)

	outs := runBulk(t, svc, task, 3)
	last := outs[len(outs)-1]
	if last.Err == nil {
		t.Fatal("超过总时限应当失败")
	}
	if f.count("i-00") != 0 {
		t.Error("超时后不该再操作任何实例")
	}
	prog := decodeBulkProgress(task.Progress)
	if len(prog.Pending) != 6 {
		t.Errorf("超时后应保留全部剩余，实际 %v", prog.Pending)
	}
}

// 没接实例服务要立刻说清楚，而不是每轮 panic。
func TestBulkWithoutInstanceService(t *testing.T) {
	svc := &Service{}
	task := bulkTask(t, "STOP", ids(3))

	out := svc.handleBulk(context.Background(), task)
	if !out.Done || out.Err == nil {
		t.Fatalf("未接线时应判失败，得到 %+v", out)
	}
}

// 坏参数不该进入执行阶段。
func TestBulkRejectsBadSpec(t *testing.T) {
	svc := &Service{instances: newFakeInstances()}

	for name, raw := range map[string]string{
		"坏 JSON": "{不是 JSON",
		"未知动作":   `{"action":"EXPLODE","instanceIds":["i-1"]}`,
		"空实例列表":  `{"action":"STOP","instanceIds":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			task := &store.OpsTask{ID: "x", Kind: store.OpsKindBulk, Spec: raw}
			out := svc.handleBulk(context.Background(), task)
			if !out.Done || out.Err == nil {
				t.Fatalf("应判失败，得到 %+v", out)
			}
		})
	}
}

// progress 显示已全部完成时，不该再操作一遍。
func TestBulkAlreadyFinished(t *testing.T) {
	f := newFakeInstances()
	svc := &Service{instances: f}
	task := bulkTask(t, "STOP", ids(3))
	task.Progress = encodeBulkProgress(bulkProgress{Action: "STOP", Total: 3, Succeeded: 3, Pending: []string{}})

	out := svc.handleBulk(context.Background(), task)
	if !out.Done || out.Err != nil {
		t.Fatalf("应直接收尾成功，得到 %+v", out)
	}
	if len(f.order) != 0 {
		t.Errorf("不该再操作实例，实际调用了 %v", f.order)
	}
}
