package huntsvc

import (
	"context"
	"testing"
	"time"

	"ocicore/internal/riskguard"
	"ocicore/internal/store"
)

// TestNormalizeIntervalEnforcesFloor 锁住间隔下限。
//
// 这是整个功能里唯一一个"用户填得越激进、风险越大"的参数。下限必须是硬的：
// 一旦能通过接口传 1 秒进来，UI 上的警告就成了摆设。
func TestNormalizeIntervalEnforcesFloor(t *testing.T) {
	cases := []struct {
		in, want int
		why      string
	}{
		{0, DefaultIntervalSeconds, "未指定时用默认值"},
		{-5, DefaultIntervalSeconds, "负数当未指定"},
		{1, MinIntervalSeconds, "1 秒必须被抬到下限"},
		{29, MinIntervalSeconds, "刚好低于下限也要抬"},
		{30, 30, "等于下限原样放行"},
		{120, 120, "正常值不动"},
		{99999, 3600, "上限封顶一小时"},
	}
	for _, c := range cases {
		if got := NormalizeInterval(c.in); got != c.want {
			t.Errorf("NormalizeInterval(%d) = %d，期望 %d（%s）", c.in, got, c.want, c.why)
		}
	}
}

// TestNextDelayGrowsWithAttempts 确认撞得越久退避越长。
//
// 一个已经试了几百次的任务说明这个组合近期确实没容量，继续保持初始频率
// 只是徒增被限流的概率。这里只校验量级关系，不校验具体数值——中间有 ±20%
// 抖动，写死数值的测试会随机失败。
func TestNextDelayGrowsWithAttempts(t *testing.T) {
	base := &store.HuntTask{IntervalSeconds: 60, Attempts: 0}
	mid := &store.HuntTask{IntervalSeconds: 60, Attempts: slowdownAfter}
	slow := &store.HuntTask{IntervalSeconds: 60, Attempts: crawlAfter}

	// 抖动是 ±20%，取多次的最小/最大值来比较量级，避免单次采样撞上边界。
	minOf := func(t *store.HuntTask) time.Duration {
		d := time.Hour
		for i := 0; i < 50; i++ {
			if v := nextDelay(t); v < d {
				d = v
			}
		}
		return d
	}
	maxOf := func(t *store.HuntTask) time.Duration {
		var d time.Duration
		for i := 0; i < 50; i++ {
			if v := nextDelay(t); v > d {
				d = v
			}
		}
		return d
	}

	if maxOf(base) >= minOf(mid) {
		t.Errorf("撞了 %d 次之后间隔应当明显变长：base 最大 %v，mid 最小 %v",
			slowdownAfter, maxOf(base), minOf(mid))
	}
	if maxOf(mid) >= minOf(slow) {
		t.Errorf("撞了 %d 次之后间隔应当再次变长：mid 最大 %v，slow 最小 %v",
			crawlAfter, maxOf(mid), minOf(slow))
	}
}

// TestNextDelayRespectsCeiling 确认退避不会无限增长。
//
// 没有上限的话，一个跑了很久的任务会退到几小时，用户看着"运行中"却
// 半天不动一次，和卡死没有区别。
func TestNextDelayRespectsCeiling(t *testing.T) {
	t2 := &store.HuntTask{IntervalSeconds: 3600, Attempts: 10_000}
	for i := 0; i < 50; i++ {
		// 上限之上还叠了 ±20% 抖动，所以按 1.2 倍留余量。
		if d := nextDelay(t2); d > time.Duration(float64(maxBackoff)*1.25) {
			t.Fatalf("退避 %v 超过了上限 %v", d, maxBackoff)
		}
	}
}

// TestNextDelayHasJitter 确认抖动确实存在。
//
// 多个任务如果都在整分钟同时开火，会形成一个尖峰——正是风控最容易注意到
// 的形状。抖动被误删掉时这个测试会失败。
func TestNextDelayHasJitter(t *testing.T) {
	task := &store.HuntTask{IntervalSeconds: 60}
	seen := map[time.Duration]bool{}
	for i := 0; i < 30; i++ {
		seen[nextDelay(task)] = true
	}
	if len(seen) < 5 {
		t.Errorf("30 次取样只得到 %d 个不同的间隔，抖动可能失效了", len(seen))
	}
}

// TestSpecRoundTrip 确认参数快照能原样取回。
//
// 任务在创建时快照一次、之后每轮重放。字段丢失的话表现是"抢到的机器
// 配置不对"，而且要等到抢到那一刻才暴露。
func TestSpecRoundTrip(t *testing.T) {
	in := Spec{
		DisplayName: "arm-1", Shape: "VM.Standard.A1.Flex",
		Ocpus: 2, MemoryInGBs: 12, ImageID: "ocid1.image.x",
		BootVolumeGB: 50, SubnetID: "ocid1.subnet.x",
		AutoCreateNetwork: true, AssignPublicIP: true, EnableIPv6: true,
		SSHPublicKey: "ssh-ed25519 AAAA", CloudInit: "#!/bin/sh\necho hi",
	}
	raw, err := EncodeSpec(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeSpec(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("往返之后不一致：\n得到 %+v\n期望 %+v", out, in)
	}
}

// TestBuildLaunchRequestAlwaysTagsTask 锁住防重复创建的依据。
//
// LaunchInstance 非幂等：请求实际成功而响应在网络上丢了的话，重试就会创建
// 出第二台，直接吃掉 ARM 免费额度而用户毫不知情。查重完全依赖这个标签——
// 标签没打上，防线就是不存在的，而且不会有任何报错。
func TestBuildLaunchRequestAlwaysTagsTask(t *testing.T) {
	req := buildLaunchRequest("ocid1.compartment.x", "AD-1", "ocid1.subnet.x", "task-abc",
		Spec{Shape: "VM.Standard.A1.Flex", Ocpus: 2, MemoryInGBs: 12})

	if got := req.FreeformTags[tagKey]; got != "task-abc" {
		t.Fatalf("任务标签是 %q，期望 %q——查重靠它，丢了会静默多创建实例", got, "task-abc")
	}
	if req.ShapeConfig == nil || req.ShapeConfig.Ocpus != 2 {
		t.Error("弹性规格的 ShapeConfig 没有带上")
	}
	if req.FaultDomain != "" {
		t.Error("故障域应当留空——指定它只会缩小可调度范围")
	}
}

// TestBuildLaunchRequestOmitsShapeConfigForFixedShapes 确认固定规格不带 ShapeConfig。
//
// E2.1.Micro 这类固定规格带上 ShapeConfig 会被 OCI 拒绝，而错误信息
// 指向的是参数格式，很难联想到是这里多塞了一个字段。
func TestBuildLaunchRequestOmitsShapeConfigForFixedShapes(t *testing.T) {
	req := buildLaunchRequest("c", "AD-1", "s", "t", Spec{Shape: "VM.Standard.E2.1.Micro"})
	if req.ShapeConfig != nil {
		t.Errorf("固定规格不该带 ShapeConfig，实际 %+v", req.ShapeConfig)
	}
}

// TestDelayForScalesWithBudget 确认超预算后间隔按倍数拉长。
//
// 预算的降频必须落在任务层而不是传输层：传输层阻塞会把一次用户点击变成
// 转圈十几秒的请求，用户会重试，重试又增加调用量——想压低的那个数字反而
// 被自己推高了。拉长间隔的观感只是「慢」，慢不会被重试。
func TestDelayForScalesWithBudget(t *testing.T) {
	const interval = 60
	task := &store.HuntTask{IntervalSeconds: interval, AccountID: "acc-1"}

	// 未超预算：与不接预算时完全一致。
	free := New(Deps{Budget: riskguard.New(nil, riskguard.WithLimit(10))})
	for i := 0; i < 30; i++ {
		d := free.delayFor(context.Background(), task)
		// 60s ±20%
		if d < 48*time.Second || d > 72*time.Second {
			t.Fatalf("未超预算时间隔 %v 超出 [48s, 72s]", d)
		}
	}

	// 用满预算 → 倍数 2。
	used := riskguard.New(nil, riskguard.WithLimit(2))
	used.Record(context.Background(), "acc-1")
	used.Record(context.Background(), "acc-1")
	twice := New(Deps{Budget: used})
	for i := 0; i < 30; i++ {
		d := twice.delayFor(context.Background(), task)
		// 120s ±20%
		if d < 96*time.Second || d > 144*time.Second {
			t.Fatalf("超预算后间隔 %v 超出 [96s, 144s]——降频没生效", d)
		}
	}

	// 两倍预算以上 → 倍数 4。
	for i := 0; i < 4; i++ {
		used.Record(context.Background(), "acc-1")
	}
	for i := 0; i < 30; i++ {
		d := twice.delayFor(context.Background(), task)
		// 240s ±20%
		if d < 192*time.Second || d > 288*time.Second {
			t.Fatalf("超两倍预算后间隔 %v 超出 [192s, 288s]", d)
		}
	}
}

// TestDelayForWithoutBudgetMatchesNextDelay 确认不接预算时行为不变。
//
// 这是 P0-3 对抢机路径的唯一影响面：一旦降频算错，只要不注入 Budget
// 就应该回到改动前的样子，给线上留一条只摘依赖的回退路径。
func TestDelayForWithoutBudgetMatchesNextDelay(t *testing.T) {
	s := New(Deps{})
	task := &store.HuntTask{IntervalSeconds: 60, Attempts: 0}
	for i := 0; i < 30; i++ {
		got := s.delayFor(context.Background(), task)
		if got < 48*time.Second || got > 72*time.Second {
			t.Fatalf("无预算时间隔 %v 超出 [48s, 72s]", got)
		}
	}
}
