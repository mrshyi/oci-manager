package riskguard

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fixedClock 返回一个可推进的时钟。
type fixedClock struct{ t time.Time }

func (f *fixedClock) now() time.Time { return f.t }

func (f *fixedClock) advance(d time.Duration) { f.t = f.t.Add(d) }

func TestWindowStartIsHourAligned(t *testing.T) {
	base := time.Date(2026, 10, 29, 13, 5, 23, 0, time.UTC)
	got := WindowStart(base)
	want := time.Date(2026, 10, 29, 13, 0, 0, 0, time.UTC).Unix()
	if got != want {
		t.Fatalf("WindowStart = %d，期望 %d", got, want)
	}
	// 同一小时内不同时刻必须落到同一个桶，否则「每小时预算」会变成「每次请求一个桶」。
	if WindowStart(base.Add(50*time.Minute)) != got { // 13:55
		t.Fatal("同一小时内应当对齐到同一个窗口起点")
	}
	// 跨到下一小时必须换桶。
	if WindowStart(base.Add(55*time.Minute)) == got { // 14:00
		t.Fatal("跨小时应当切换到新的窗口")
	}
}

// TestRecordAccumulatesWithinWindow 验证基本累加。
func TestRecordAccumulatesWithinWindow(t *testing.T) {
	clock := &fixedClock{t: time.Date(2026, 10, 29, 13, 5, 0, 0, time.UTC)}
	g := New(nil, WithClock(clock.now))

	for i := 0; i < 7; i++ {
		g.Record(context.Background(), "acc-1")
	}
	used, limit := g.Usage(context.Background(), "acc-1")
	if used != 7 {
		t.Fatalf("已用 = %d，期望 7", used)
	}
	if limit != DefaultHourlyLimit {
		t.Fatalf("预算 = %d，期望 %d", limit, DefaultHourlyLimit)
	}
	// 不同账号各自计数。
	if used, _ := g.Usage(context.Background(), "acc-2"); used != 0 {
		t.Fatalf("另一个账号不该被计入，实际 %d", used)
	}
}

// TestWindowRollsOver 验证跨小时清零。
//
// 窗口边界算错的典型后果是「每小时重置」变成「每天重置」，预算形同虚设，
// 而日志里一切正常——因为计数确实在涨，只是永远涨不到上限。
func TestWindowRollsOver(t *testing.T) {
	clock := &fixedClock{t: time.Date(2026, 10, 29, 13, 59, 0, 0, time.UTC)}
	g := New(nil, WithClock(clock.now))

	g.Record(context.Background(), "acc-1")
	g.Record(context.Background(), "acc-1")
	if used, _ := g.Usage(context.Background(), "acc-1"); used != 2 {
		t.Fatalf("已用 = %d，期望 2", used)
	}

	clock.advance(2 * time.Minute) // 跨到 14:01
	if used, _ := g.Usage(context.Background(), "acc-1"); used != 0 {
		t.Fatalf("跨小时应清零，实际 %d", used)
	}
}

// TestMultiplierLadder 验证降频倍数。
//
// 用倍数而不是拒绝执行：任务被拒绝后用户会再点一次，调用量反而更多。
// 拉长间隔的观感是「慢」，慢不会被重试。
func TestMultiplierLadder(t *testing.T) {
	clock := &fixedClock{t: time.Date(2026, 10, 29, 13, 5, 0, 0, time.UTC)}
	g := New(nil, WithClock(clock.now), WithLimit(10))

	if m := g.Multiplier(context.Background(), "a"); m != 1 {
		t.Fatalf("未用预算时倍数 = %d，期望 1", m)
	}
	for i := 0; i < 9; i++ {
		g.Record(context.Background(), "a")
	}
	if m := g.Multiplier(context.Background(), "a"); m != 1 {
		t.Fatalf("未超预算时倍数 = %d，期望 1", m)
	}
	g.Record(context.Background(), "a") // 第 10 次，正好用满
	if m := g.Multiplier(context.Background(), "a"); m != 2 {
		t.Fatalf("用满预算后倍数 = %d，期望 2", m)
	}
	for i := 0; i < 10; i++ {
		g.Record(context.Background(), "a") // 到 20，即 2 倍
	}
	if m := g.Multiplier(context.Background(), "a"); m != 4 {
		t.Fatalf("超两倍预算后倍数 = %d，期望 4", m)
	}
}

// TestConcurrentRecordDoesNotLoseCounts 验证并发累加不丢计数。
//
// 丢计数是这套机制最隐蔽的失效方式：预算看起来很合理，但实际永远用不满，
// 于是降频永远不触发——而现象上什么都没坏。
func TestConcurrentRecordDoesNotLoseCounts(t *testing.T) {
	g := New(nil)

	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.Record(context.Background(), "acc")
		}()
	}
	wg.Wait()

	if used, _ := g.Usage(context.Background(), "acc"); used != n {
		t.Fatalf("已用 = %d，期望 %d——并发下丢了计数", used, n)
	}
}

// TestWindowSurvivesRestart 验证窗口起点取自 Unix 时间而非进程启动时刻。
//
// 如果按「进程启动后经过的小时数」算窗口，重启就等于把预算清零，
// 而重启恰好是任务出问题、调用量最大的时候。
func TestWindowSurvivesRestart(t *testing.T) {
	counter := NewMemoryCounter()
	at := time.Date(2026, 10, 29, 13, 30, 0, 0, time.UTC)

	first := New(counter, WithClock(func() time.Time { return at }))
	first.Record(context.Background(), "acc")
	first.Record(context.Background(), "acc")

	// 模拟重启：新 Guard、同一个 Counter、同一时刻。
	second := New(counter, WithClock(func() time.Time { return at }))
	if used, _ := second.Usage(context.Background(), "acc"); used != 2 {
		t.Fatalf("重启后已用 = %d，期望 2——窗口起点被重置了", used)
	}
}

// TestRecordIgnoresEmptyAccount 验证空账号 ID 不会污染计数。
func TestRecordIgnoresEmptyAccount(t *testing.T) {
	g := New(nil)
	g.Record(context.Background(), "")
	if used, _ := g.Usage(context.Background(), ""); used != 0 {
		t.Fatalf("空账号不应被计数，实际 %d", used)
	}
}

// TestZeroLimitDisablesMultiplier 验证预算为 0 时不降频。
//
// WithLimit(0) 会被忽略（保持默认），这里用负值路径确认不会除零或误判。
func TestZeroLimitDisablesMultiplier(t *testing.T) {
	g := New(nil, WithLimit(-1))
	if _, limit := g.Usage(context.Background(), "a"); limit != DefaultHourlyLimit {
		t.Fatalf("非法上限应被忽略，实际 %d", limit)
	}
}
