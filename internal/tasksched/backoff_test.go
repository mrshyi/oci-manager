package tasksched

import (
	"testing"
	"time"
)

// huntLikeBackoff 复刻抢机任务的阶梯参数，用来对照抽取前的行为。
var huntLikeBackoff = Backoff{
	Max:    30 * time.Minute,
	Jitter: 0.2,
	Steps: []Step{
		{After: 10, Factor: 2},
		{After: 30, Factor: 5},
	},
}

func noJitter(b Backoff) Backoff {
	b.Jitter = 0
	return b
}

// TestBackoffStaircase 确认阶梯的档位与倍数。
//
// 数值直接对着抽取前 huntsvc.nextDelay 的行为写：base 60s，撞 10 次变 2 倍，
// 撞 30 次变 5 倍。抽取是纯重构，这里的数字变了就说明行为漂了。
func TestBackoffStaircase(t *testing.T) {
	b := noJitter(huntLikeBackoff)
	cases := []struct {
		attempts int
		want     time.Duration
		why      string
	}{
		{0, 60 * time.Second, "刚开始，用基准间隔"},
		{9, 60 * time.Second, "还没到第一档"},
		{10, 120 * time.Second, "到第一档，2 倍"},
		{29, 120 * time.Second, "还没到第二档"},
		{30, 300 * time.Second, "到第二档，5 倍"},
		{1000, 300 * time.Second, "再往上没有档位了，保持 5 倍"},
	}
	for _, c := range cases {
		if got := b.Delay(60*time.Second, c.attempts); got != c.want {
			t.Errorf("attempts=%d 时 %v，期望 %v（%s）", c.attempts, got, c.want, c.why)
		}
	}
}

// TestBackoffTakesHighestStepOnly 确认档位不连乘。
//
// 连乘的话撞 30 次就是 2×5=10 倍，60s 变 10 分钟；再叠上限与抖动，
// 用户会看到"运行中"但半小时不动一次，与卡死无法区分。
func TestBackoffTakesHighestStepOnly(t *testing.T) {
	b := noJitter(huntLikeBackoff)
	got := b.Delay(60*time.Second, 30)
	if got != 300*time.Second {
		t.Fatalf("撞 30 次得到 %v，期望 300s（5 倍，不是 2×5=10 倍）", got)
	}
}

// TestBackoffCeiling 确认退避有上界。
//
// 没有上界时，一个跑了很久的任务会退到几小时一次。
func TestBackoffCeiling(t *testing.T) {
	b := noJitter(huntLikeBackoff)
	got := b.Delay(3600*time.Second, 10_000)
	if got != 30*time.Minute {
		t.Fatalf("得到 %v，期望被夹到上限 %v", got, 30*time.Minute)
	}
}

// TestBackoffJitterRange 确认抖动幅度就是声明的比例。
//
// 用注入的 Rand 取值边界，避免随机采样导致的偶发失败：
// Rand=0 → 1-Jitter，Rand=1 → 1+Jitter，Rand=0.5 → 原值。
func TestBackoffJitterRange(t *testing.T) {
	cases := []struct {
		r    float64
		want time.Duration
	}{
		{0, 48 * time.Second},    // 60 × 0.8
		{0.5, 60 * time.Second},  // 60 × 1.0
		{1, 72 * time.Second},    // 60 × 1.2
		{0.25, 54 * time.Second}, // 60 × 0.9
	}
	for _, c := range cases {
		b := huntLikeBackoff
		r := c.r
		b.Rand = func() float64 { return r }
		if got := b.Delay(60*time.Second, 0); got != c.want {
			t.Errorf("Rand=%.2f 时得到 %v，期望 %v", c.r, got, c.want)
		}
	}
}

// TestBackoffDefaultRandVaries 确认不注入 Rand 时确实有抖动。
//
// 用 math/rand 而不播种的话每次启动都是同一串数，抖动等于不存在——
// 而这件事在单次运行里完全看不出来。
func TestBackoffDefaultRandVaries(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[huntLikeBackoff.Delay(60*time.Second, 0)] = true
	}
	if len(seen) < 5 {
		t.Fatalf("50 次取样只得到 %d 个不同值，抖动可能失效", len(seen))
	}
}

// TestBackoffJitterKeepsCeilingNearby 确认抖动不会把上限抬得太远。
//
// 上界判定在抖动之前，所以实际值最多超出 Max×Jitter。超出太多的话
// "最长 30 分钟"这个承诺就不成立了。
func TestBackoffJitterKeepsCeilingNearby(t *testing.T) {
	limit := time.Duration(float64(30*time.Minute) * 1.25)
	for i := 0; i < 200; i++ {
		if d := huntLikeBackoff.Delay(3600*time.Second, 10_000); d > limit {
			t.Fatalf("退避 %v 超过了上限的 1.25 倍（%v）", d, limit)
		}
	}
}

// TestBackoffZeroBase 确认零基准直接返回零。
//
// base 来自调用方的"用户填的间隔"，可能被规范化成 0。返回非零会让
// 一个本该停下的任务继续排期。
func TestBackoffZeroBase(t *testing.T) {
	if got := huntLikeBackoff.Delay(0, 100); got != 0 {
		t.Fatalf("base=0 时得到 %v，期望 0", got)
	}
	if got := huntLikeBackoff.Delay(-time.Second, 100); got != 0 {
		t.Fatalf("base 为负时得到 %v，期望 0", got)
	}
}

// TestBackoffNoSteps 确认没有配阶梯时只做抖动。
//
// 一次性的运维任务没有"撞墙越久越慢"的概念，阶梯为空是正常用法。
func TestBackoffNoSteps(t *testing.T) {
	b := Backoff{Max: time.Minute, Jitter: 0}
	if got := b.Delay(45*time.Second, 999); got != 45*time.Second {
		t.Fatalf("得到 %v，期望 45s", got)
	}
}
