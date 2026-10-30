package opssvc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

// fakeRescue 是一台"假的 OCI"。
//
// 救援流程最需要被验证的是"在某一步、某个状态下该不该动手"，而这些状态用
// 真账号根本复现不了——总不能为了跑测试去拔一台生产机器的引导卷。
type fakeRescue struct {
	insts map[string]*ociclient.Instance
	atts  []ociclient.BootVolumeAttachment
	// calls 按顺序记录发生过的动作，用来断言"没有多动手"。
	calls []string

	attachErr error
	detachErr error
	seq       int
}

func newFakeRescue() *fakeRescue {
	return &fakeRescue{insts: map[string]*ociclient.Instance{}}
}

func (f *fakeRescue) addInstance(id, name, ad, state string) *ociclient.Instance {
	inst := &ociclient.Instance{
		ID:                 id,
		DisplayName:        name,
		AvailabilityDomain: ad,
		LifecycleState:     state,
		CompartmentID:      "ocid1.compartment.oc1..test",
	}
	f.insts[id] = inst
	return inst
}

func (f *fakeRescue) addAttachment(id, instanceID, bootVolumeID, ad, state string) {
	f.atts = append(f.atts, ociclient.BootVolumeAttachment{
		ID:                 id,
		InstanceID:         instanceID,
		BootVolumeID:       bootVolumeID,
		AvailabilityDomain: ad,
		LifecycleState:     state,
	})
}

func (f *fakeRescue) GetInstance(_ context.Context, _ string, instanceID string) (*ociclient.Instance, error) {
	f.calls = append(f.calls, "get:"+instanceID)
	inst, ok := f.insts[instanceID]
	if !ok {
		return nil, fmt.Errorf("实例 %s 不存在", instanceID)
	}
	cp := *inst
	return &cp, nil
}

func (f *fakeRescue) InstanceAction(_ context.Context, _ string, instanceID, action string) (*ociclient.Instance, error) {
	f.calls = append(f.calls, action+":"+instanceID)
	inst, ok := f.insts[instanceID]
	if !ok {
		return nil, fmt.Errorf("实例 %s 不存在", instanceID)
	}
	if action == ociclient.ActionStart {
		inst.LifecycleState = ociclient.LifecycleStarting
	} else {
		inst.LifecycleState = ociclient.LifecycleStopping
	}
	cp := *inst
	return &cp, nil
}

func (f *fakeRescue) ListBootVolumeAttachments(_ context.Context, _, _, _, instanceID string) ([]ociclient.BootVolumeAttachment, error) {
	out := make([]ociclient.BootVolumeAttachment, 0, len(f.atts))
	for _, a := range f.atts {
		if instanceID != "" && a.InstanceID != instanceID {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func (f *fakeRescue) DetachBootVolume(_ context.Context, _ string, attachmentID string) error {
	f.calls = append(f.calls, "detach:"+attachmentID)
	if f.detachErr != nil {
		return f.detachErr
	}
	// 真的分离是异步的，但测试里让它立刻生效：这样"下一轮看不到挂载关系"
	// 就能被验证到，而不是把测试拖成靠 sleep 等状态。
	kept := make([]ociclient.BootVolumeAttachment, 0, len(f.atts))
	for _, a := range f.atts {
		if a.ID == attachmentID {
			continue
		}
		kept = append(kept, a)
	}
	f.atts = kept
	return nil
}

func (f *fakeRescue) AttachBootVolume(_ context.Context, _, instanceID, bootVolumeID, _ string) (*ociclient.BootVolumeAttachment, error) {
	f.calls = append(f.calls, "attach:"+instanceID+":"+bootVolumeID)
	if f.attachErr != nil {
		return nil, f.attachErr
	}
	f.seq++
	att := ociclient.BootVolumeAttachment{
		ID:             fmt.Sprintf("att-%d", f.seq),
		InstanceID:     instanceID,
		BootVolumeID:   bootVolumeID,
		LifecycleState: bootAttachAttached,
	}
	f.atts = append(f.atts, att)
	return &att, nil
}

// converge 模拟云平台在两轮之间收敛：过渡态进入稳定态。
func (f *fakeRescue) converge() {
	for _, inst := range f.insts {
		switch inst.LifecycleState {
		case ociclient.LifecycleStopping:
			inst.LifecycleState = ociclient.LifecycleStopped
		case ociclient.LifecycleStarting:
			inst.LifecycleState = ociclient.LifecycleRunning
		}
	}
}

func (f *fakeRescue) setState(instanceID, state string) { f.insts[instanceID].LifecycleState = state }

// countCalls 统计以 prefix 开头的动作次数。
func (f *fakeRescue) countCalls(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

const (
	testTarget = "ocid1.instance.oc1..target"
	testRescue = "ocid1.instance.oc1..rescue"
	testBoot   = "ocid1.bootvolume.oc1..boot"
	testAD     = "Uocm:AP-TOKYO-1-AD-1"
)

// newRescueFixture 搭一个"目标实例已关机、引导卷挂在它名下"的起点。
func newRescueFixture() (*fakeRescue, RescueSpec, *store.OpsTask) {
	f := newFakeRescue()
	f.addInstance(testTarget, "web-1", testAD, ociclient.LifecycleStopped)
	f.addInstance(testRescue, "repair-1", testAD, ociclient.LifecycleStopped)
	f.addAttachment("att-target", testTarget, testBoot, testAD, bootAttachAttached)

	spec := RescueSpec{
		Region:           "ap-tokyo-1",
		TargetInstanceID: testTarget,
		RescueInstanceID: testRescue,
		CompartmentID:    "ocid1.compartment.oc1..test",
	}
	raw, err := EncodeRescueSpec(spec)
	if err != nil {
		panic(err)
	}
	task := &store.OpsTask{
		ID:    "task-1",
		Kind:  store.OpsKindRescue,
		Spec:  raw,
		State: store.OpsPending,
	}
	return f, spec, task
}

// applyOutcome 模拟 run() 把结果写回任务，让下一轮看到推进后的状态。
func applyOutcome(task *store.OpsTask, out outcome) {
	if out.Stage != "" {
		task.Stage = out.Stage
	}
	if out.Progress != "" {
		task.Progress = out.Progress
	}
	if out.Result != "" {
		task.Result = out.Result
	}
	if out.Err != nil {
		task.LastError = out.Err.Error()
	}
	// 第一轮之后任务已经不在 pending 了，与 run() 里的状态迁移对应。
	task.State = store.OpsRunning
}

// drive 反复执行直到任务终结，返回走过的阶段序列。
//
// 遇到暂停点会当作"用户点了继续"自动往下走——暂停本身由
// TestRescuePausesForRepairThenResumes 单独验证。
func drive(t *testing.T, task *store.OpsTask, spec RescueSpec, f *fakeRescue, maxRounds int) []string {
	t.Helper()
	var stages []string
	for i := 0; i < maxRounds; i++ {
		out := rescueStep(context.Background(), task, spec, f)
		if out.Stage != "" {
			stages = append(stages, out.Stage)
		}
		applyOutcome(task, out)
		if out.Done {
			if out.Err != nil {
				t.Fatalf("第 %d 轮意外失败（阶段 %s）：%v", i+1, out.Stage, out.Err)
			}
			return stages
		}
		// 暂停 → 模拟用户点「继续」：状态回到 running，阶段不变。
		if out.Pause {
			task.State = store.OpsRunning
		}
		f.converge()
	}
	t.Fatalf("流程在 %d 轮内没有结束，走过的阶段：%v", maxRounds, stages)
	return stages
}

// TestRescueHappyPath 走完整条链路，并断言每一步只动一次手。
func TestRescueHappyPath(t *testing.T) {
	f, spec, task := newRescueFixture()

	stages := drive(t, task, spec, f, 40)

	want := []string{
		RescueStopping, RescueDetaching, RescueAttaching,
		RescueReadyForRepair, RescueDetachingRescue, RescueReattaching,
		RescueStarting, RescueDone,
	}
	// 只看"阶段首次出现"的顺序，跳过同一阶段的等待轮次。
	got := uniqueStages(stages)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("阶段序列不对\n得到：%v\n期望：%v", got, want)
	}

	// 卷 ID 必须来自挂载关系，而不是 spec 里任何字段。
	if f.countCalls("attach:"+testRescue+":"+testBoot) != 1 {
		t.Fatalf("挂到救援机应恰好一次，实际 %d 次：%v", f.countCalls("attach:"), f.calls)
	}
	if f.countCalls("attach:"+testTarget+":"+testBoot) != 1 {
		t.Fatalf("挂回原实例应恰好一次，实际 %d 次：%v", f.countCalls("attach:"), f.calls)
	}
	if f.countCalls("detach:") != 2 {
		t.Fatalf("分离应恰好两次（原实例 + 救援机），实际 %d 次：%v", f.countCalls("detach:"), f.calls)
	}
	if task.Stage != RescueDone {
		t.Fatalf("收尾阶段应为 %s，实际 %s", RescueDone, task.Stage)
	}
}

// TestRescuePausesForRepairThenResumes 验证人工介入点不会自己往下走。
func TestRescuePausesForRepairThenResumes(t *testing.T) {
	f, spec, task := newRescueFixture()

	// 直接跳到"卷已挂到救援机"的状态。真实流程走到这一步时，progress 里
	// 一定已经存了推导出的卷 ID 与救援机名字（PRECHECK/ATTACHING 写的）。
	task.Stage = RescueReadyForRepair
	task.State = store.OpsRunning
	task.Progress = encodeRescueProgress(rescueProgress{
		Stage:            RescueReadyForRepair,
		StageSince:       time.Now(),
		BootVolumeID:     testBoot,
		TargetInstanceID: testTarget,
		TargetName:       "web-1",
		RescueInstanceID: testRescue,
		RescueName:       "repair-1",
	})

	out := rescueStep(context.Background(), task, spec, f)
	if !out.Pause || out.Done {
		t.Fatalf("第一次到达 READY_FOR_REPAIR 应当暂停，得到 %+v", out)
	}
	if !strings.Contains(out.Result, testBoot) || !strings.Contains(out.Result, "repair-1") {
		t.Fatalf("暂停提示里应当说明是哪块卷、哪台救援机，得到 %q", out.Result)
	}
	applyOutcome(task, out)

	// 用户点了「继续」：状态回到 running，阶段还是 READY_FOR_REPAIR。
	task.State = store.OpsRunning
	out = rescueStep(context.Background(), task, spec, f)
	if out.Pause {
		t.Fatal("恢复后又被暂停回去了：RepairResumed 标记没起作用")
	}
	if out.Stage != RescueDetachingRescue {
		t.Fatalf("恢复后应进入 %s，实际 %s", RescueDetachingRescue, out.Stage)
	}
}

// TestRescueResumesFromStageAfterRestart 验证进程在 DETACHING 被杀、重启后
// 从该阶段继续，而不是回到 PRECHECK 重新推导。
func TestRescueResumesFromStageAfterRestart(t *testing.T) {
	f, spec, task := newRescueFixture()

	// 模拟：上一轮已经在 DETACHING，库里存着推导出的卷 ID，进程随后被杀。
	task.Stage = RescueDetaching
	task.State = store.OpsPending // ResetRunningOpsTasks 的结果
	task.Progress = encodeRescueProgress(rescueProgress{
		Stage:        RescueDetaching,
		StageSince:   time.Now(),
		BootVolumeID: testBoot,
		AttachmentID: "att-target",
	})

	out := rescueStep(context.Background(), task, spec, f)
	if out.Done {
		t.Fatalf("DETACHING 阶段应继续等待而不是结束：%+v", out)
	}
	if out.Stage != RescueDetaching {
		t.Fatalf("应留在 %s，实际 %s", RescueDetaching, out.Stage)
	}
	if f.countCalls("detach:att-target") != 1 {
		t.Fatalf("应按库里的挂载关系发起分离，实际调用：%v", f.calls)
	}
	// 关键：没有重新跑 PRECHECK（那会多查一次救援机）。
	if f.countCalls("get:"+testRescue) != 0 {
		t.Fatalf("重启后不该回到 PRECHECK，实际调用：%v", f.calls)
	}
}

// TestRescuePrecheckRefusesWithoutAttachment 没有挂载关系时必须停手。
func TestRescuePrecheckRefusesWithoutAttachment(t *testing.T) {
	f, spec, task := newRescueFixture()
	f.atts = nil

	out := rescueStep(context.Background(), task, spec, f)
	if !out.Done || out.Err == nil {
		t.Fatalf("没有引导卷挂载关系时应失败，得到 %+v", out)
	}
	if out.Stage != RescuePrecheck {
		t.Fatalf("失败应停在 %s，实际 %s", RescuePrecheck, out.Stage)
	}
	if f.countCalls("detach:") != 0 || f.countCalls("attach:") != 0 {
		t.Fatalf("预检失败后不该有任何挂载/分离动作：%v", f.calls)
	}
}

// TestRescuePrecheckRefusesAmbiguousAttachments 多条挂载关系时不猜。
func TestRescuePrecheckRefusesAmbiguousAttachments(t *testing.T) {
	f, spec, task := newRescueFixture()
	f.addAttachment("att-2", testTarget, "ocid1.bootvolume.oc1..other", testAD, bootAttachAttached)

	out := rescueStep(context.Background(), task, spec, f)
	if !out.Done || out.Err == nil {
		t.Fatalf("存在多条挂载关系时应失败，得到 %+v", out)
	}
	if !strings.Contains(out.Err.Error(), "无法确定") {
		t.Fatalf("错误信息应说明无法确定该操作哪一块，得到 %q", out.Err.Error())
	}
}

// TestRescuePrecheckRejectsCrossAD 引导卷不能跨可用域挂载。
func TestRescuePrecheckRejectsCrossAD(t *testing.T) {
	f, spec, task := newRescueFixture()
	f.insts[testRescue].AvailabilityDomain = "Uocm:AP-TOKYO-1-AD-2"

	out := rescueStep(context.Background(), task, spec, f)
	if !out.Done || out.Err == nil {
		t.Fatalf("跨可用域应失败，得到 %+v", out)
	}
	if !strings.Contains(out.Err.Error(), "可用域") {
		t.Fatalf("错误信息应说明可用域问题，得到 %q", out.Err.Error())
	}
}

// TestRescuePrecheckRejectsCompartmentMismatch 归属校验。
func TestRescuePrecheckRejectsCompartmentMismatch(t *testing.T) {
	f, spec, task := newRescueFixture()
	f.insts[testTarget].CompartmentID = "ocid1.compartment.oc1..another"

	out := rescueStep(context.Background(), task, spec, f)
	if !out.Done || out.Err == nil {
		t.Fatalf("目标实例不在指定 compartment 时应失败，得到 %+v", out)
	}
	if !strings.Contains(out.Err.Error(), "compartment") {
		t.Fatalf("错误信息应说明归属问题，得到 %q", out.Err.Error())
	}
}

// TestRescuePrecheckCrossCheckMismatch 界面上显示的卷与实际不一致时停手，
// 而不是按界面上的值去操作。
func TestRescuePrecheckCrossCheckMismatch(t *testing.T) {
	f, spec, task := newRescueFixture()
	spec.ExpectBootVolumeID = "ocid1.bootvolume.oc1..stale"
	raw, _ := EncodeRescueSpec(spec)
	task.Spec = raw

	out := rescueStep(context.Background(), task, spec, f)
	if !out.Done || out.Err == nil {
		t.Fatalf("交叉校验不一致时应失败，得到 %+v", out)
	}
	if f.countCalls("detach:") != 0 {
		t.Fatalf("不该对任何卷动手：%v", f.calls)
	}
}

// TestRescueDetachingWaitsForInstanceStopped 实例没停就不分离。
func TestRescueDetachingWaitsForInstanceStopped(t *testing.T) {
	f, spec, task := newRescueFixture()
	f.setState(testTarget, ociclient.LifecycleRunning)
	task.Stage = RescueDetaching
	task.State = store.OpsRunning
	task.Progress = encodeRescueProgress(rescueProgress{
		Stage: RescueDetaching, StageSince: time.Now(), BootVolumeID: testBoot,
	})

	out := rescueStep(context.Background(), task, spec, f)
	if out.Done {
		t.Fatalf("实例还在运行时不该继续，得到 %+v", out)
	}
	if f.countCalls("detach:") != 0 {
		t.Fatalf("实例未停止就发起了分离：%v", f.calls)
	}
}

// TestRescueAttachingWaitsWhileVolumeStillHeld 分离尚未生效时不挂载。
func TestRescueAttachingWaitsWhileVolumeStillHeld(t *testing.T) {
	f, spec, task := newRescueFixture()
	// 卷还挂在原实例名下（分离还没真正生效）。
	task.Stage = RescueAttaching
	task.State = store.OpsRunning
	task.Progress = encodeRescueProgress(rescueProgress{
		Stage: RescueAttaching, StageSince: time.Now(), BootVolumeID: testBoot,
	})

	out := rescueStep(context.Background(), task, spec, f)
	if out.Done {
		t.Fatalf("卷仍被原实例占用时应等待，得到 %+v", out)
	}
	if f.countCalls("attach:") != 0 {
		t.Fatalf("卷仍被占用却发起了挂载：%v", f.calls)
	}
}

// TestRescueAttachingStopsRunningRescueInstance 救援机在跑就先停。
func TestRescueAttachingStopsRunningRescueInstance(t *testing.T) {
	f, spec, task := newRescueFixture()
	f.atts = nil // 卷已从原实例分离
	f.setState(testRescue, ociclient.LifecycleRunning)
	task.Stage = RescueAttaching
	task.State = store.OpsRunning
	task.Progress = encodeRescueProgress(rescueProgress{
		Stage: RescueAttaching, StageSince: time.Now(), BootVolumeID: testBoot,
	})

	out := rescueStep(context.Background(), task, spec, f)
	if out.Done {
		t.Fatalf("救援机未停止时应等待，得到 %+v", out)
	}
	if f.countCalls(ociclient.ActionSoftStop+":"+testRescue) != 1 {
		t.Fatalf("应先正常关闭救援机，实际调用：%v", f.calls)
	}
	if f.countCalls("attach:") != 0 {
		t.Fatalf("救援机还在运行时不该挂载：%v", f.calls)
	}
}

// TestRescueStoppingEscalatesToHardStop 正常关机超时就升级为强制关机，
// 且只升级一次。
func TestRescueStoppingEscalatesToHardStop(t *testing.T) {
	f, spec, task := newRescueFixture()
	f.setState(testTarget, ociclient.LifecycleStopping)
	task.Stage = RescueStopping
	task.State = store.OpsRunning
	task.Progress = encodeRescueProgress(rescueProgress{
		Stage: RescueStopping, StageSince: time.Now().Add(-softStopGrace - time.Minute),
	})

	out := rescueStep(context.Background(), task, spec, f)
	if out.Done {
		t.Fatalf("升级强制关机后应继续等待，得到 %+v", out)
	}
	if f.countCalls(ociclient.ActionStop+":"+testTarget) != 1 {
		t.Fatalf("应发一次强制关机，实际调用：%v", f.calls)
	}
	applyOutcome(task, out)

	// 下一轮：已经升级过，不该再发一次。
	out = rescueStep(context.Background(), task, spec, f)
	if out.Done {
		t.Fatalf("仍在关机中应继续等待，得到 %+v", out)
	}
	if f.countCalls(ociclient.ActionStop+":"+testTarget) != 1 {
		t.Fatalf("强制关机被重复发送：%v", f.calls)
	}
}

// TestRescueStageTimeout 阶段卡太久要判失败并说清停在哪一步。
func TestRescueStageTimeout(t *testing.T) {
	f, spec, task := newRescueFixture()
	task.Stage = RescueDetaching
	task.State = store.OpsRunning
	task.Progress = encodeRescueProgress(rescueProgress{
		Stage: RescueDetaching, StageSince: time.Now().Add(-rescueStageTimeout - time.Minute),
		BootVolumeID: testBoot,
	})

	out := rescueStep(context.Background(), task, spec, f)
	if !out.Done || out.Err == nil {
		t.Fatalf("阶段超时应失败，得到 %+v", out)
	}
	if !strings.Contains(out.Err.Error(), RescueDetaching) {
		t.Fatalf("错误信息应说明卡在哪个阶段，得到 %q", out.Err.Error())
	}
	if out.Stage != RescueDetaching {
		t.Fatalf("失败时保留阶段 %s 供重试，实际 %s", RescueDetaching, out.Stage)
	}
}

// TestRescueRetryResetsStageTimer 重试时不能沿用上一轮残留的阶段计时，
// 否则用户一点重试就立刻又被判超时。
func TestRescueRetryResetsStageTimer(t *testing.T) {
	f, spec, task := newRescueFixture()
	task.Stage = RescueDetaching
	task.State = store.OpsPending // failed → pending（用户点重试）
	task.Progress = encodeRescueProgress(rescueProgress{
		Stage: RescueDetaching, StageSince: time.Now().Add(-rescueStageTimeout - time.Minute),
		BootVolumeID: testBoot,
	})

	out := rescueStep(context.Background(), task, spec, f)
	if out.Done {
		t.Fatalf("重试后不该立刻超时失败，得到 %+v", out)
	}
	if f.countCalls("detach:att-target") != 1 {
		t.Fatalf("重试后应继续分离动作，实际调用：%v", f.calls)
	}
}

// TestRescueDetachErrorFails 云端报错要判失败并停在当前阶段。
func TestRescueDetachErrorFails(t *testing.T) {
	f, spec, task := newRescueFixture()
	f.detachErr = fmt.Errorf("500 InternalError")
	task.Stage = RescueDetaching
	task.State = store.OpsRunning
	task.Progress = encodeRescueProgress(rescueProgress{
		Stage: RescueDetaching, StageSince: time.Now(), BootVolumeID: testBoot,
	})

	out := rescueStep(context.Background(), task, spec, f)
	if !out.Done || out.Err == nil {
		t.Fatalf("分离失败应判失败，得到 %+v", out)
	}
	if out.Stage != RescueDetaching {
		t.Fatalf("应停在 %s，实际 %s", RescueDetaching, out.Stage)
	}
}

// TestRescueUnknownStageFails 库里的阶段名坏掉时不要瞎猜。
func TestRescueUnknownStageFails(t *testing.T) {
	f, spec, task := newRescueFixture()
	task.Stage = "REBOOTING"

	out := rescueStep(context.Background(), task, spec, f)
	if !out.Done || out.Err == nil {
		t.Fatalf("未知阶段应失败，得到 %+v", out)
	}
	if f.countCalls("detach:") != 0 || f.countCalls("attach:") != 0 {
		t.Fatalf("未知阶段不该动手：%v", f.calls)
	}
}

// TestRescueExpiredFails 总时限到了要停手。
func TestRescueExpiredFails(t *testing.T) {
	f, spec, task := newRescueFixture()
	task.ExpiresAt = time.Now().Add(-time.Minute)

	out := rescueStep(context.Background(), task, spec, f)
	if !out.Done || out.Err == nil {
		t.Fatalf("超过总时限应失败，得到 %+v", out)
	}
}

func TestValidateRescueSpec(t *testing.T) {
	base := RescueSpec{
		Region:           "ap-tokyo-1",
		TargetInstanceID: testTarget,
		RescueInstanceID: testRescue,
		CompartmentID:    "ocid1.compartment.oc1..test",
	}
	if err := ValidateRescueSpec(base); err != nil {
		t.Fatalf("合法参数不该报错：%v", err)
	}

	cases := map[string]func(*RescueSpec){
		"缺区域":           func(s *RescueSpec) { s.Region = " " },
		"缺目标实例":         func(s *RescueSpec) { s.TargetInstanceID = "" },
		"缺救援机":          func(s *RescueSpec) { s.RescueInstanceID = "" },
		"缺 compartment": func(s *RescueSpec) { s.CompartmentID = "" },
		"目标与救援机相同":      func(s *RescueSpec) { s.RescueInstanceID = s.TargetInstanceID },
	}
	for name, mutate := range cases {
		s := base
		mutate(&s)
		if err := ValidateRescueSpec(s); err == nil {
			t.Errorf("%s：应当报错", name)
		}
	}
}

// uniqueStages 去掉连续重复的阶段名。
func uniqueStages(stages []string) []string {
	out := make([]string, 0, len(stages))
	for _, s := range stages {
		if len(out) > 0 && out[len(out)-1] == s {
			continue
		}
		out = append(out, s)
	}
	return out
}
