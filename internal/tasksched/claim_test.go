package tasksched

import (
	"sync"
	"testing"
)

// TestClaimSetTryAcquireIsExclusive 锁住最基本的互斥语义。
func TestClaimSetTryAcquireIsExclusive(t *testing.T) {
	c := NewClaimSet()
	if !c.TryAcquire("a") {
		t.Fatal("首次占用应当成功")
	}
	if c.TryAcquire("a") {
		t.Fatal("重复占用应当被拒——放行就等于没有闸门")
	}
	if !c.TryAcquire("b") {
		t.Fatal("不同 ID 互不影响")
	}
	if got := c.Len(); got != 2 {
		t.Fatalf("Len = %d，期望 2", got)
	}
}

// TestClaimSetReleaseAllowsReacquire 确认释放后能再次占用。
func TestClaimSetReleaseAllowsReacquire(t *testing.T) {
	c := NewClaimSet()
	c.TryAcquire("a")
	c.Release("a")
	if c.Has("a") {
		t.Fatal("释放之后 Has 仍为 true")
	}
	if !c.TryAcquire("a") {
		t.Fatal("释放之后应当能再次占用")
	}
}

// TestClaimSetReleaseUnknownIsNoop 确认重复释放不会 panic。
//
// 执行路径上有多层 defer，任何一条提前返回都可能触发重复释放。
func TestClaimSetReleaseUnknownIsNoop(t *testing.T) {
	c := NewClaimSet()
	c.Release("never-acquired") // 不 panic 即通过
	if got := c.Len(); got != 0 {
		t.Fatalf("Len = %d，期望 0", got)
	}
}

// TestClaimSetRejectsEmptyID 确认空 ID 一律拒绝。
//
// 空 ID 无法去重：放行等于对所有"没有 ID"的任务放弃保护，而且它们会互相
// 挡住。宁可挡住不跑——那会立刻暴露 TaskID 实现有问题。
func TestClaimSetRejectsEmptyID(t *testing.T) {
	c := NewClaimSet()
	if c.TryAcquire("") {
		t.Fatal("空 ID 不该被占用")
	}
	if got := c.Len(); got != 0 {
		t.Fatalf("Len = %d，期望 0", got)
	}
}

// TestClaimSetConcurrentAcquireOnlyOneWins 确认并发下只有一个能拿到。
//
// 两轮 tick 在极端时序下可能同时执行（Run 的 ticker 与一次手动 Tick 重叠），
// 那时互斥完全依赖这把锁。
func TestClaimSetConcurrentAcquireOnlyOneWins(t *testing.T) {
	c := NewClaimSet()

	const n = 64
	var wg sync.WaitGroup
	var wins int32
	var mu sync.Mutex

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if c.TryAcquire("same") {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("%d 个 goroutine 中有 %d 个拿到占用，期望恰好 1 个", n, wins)
	}
}
