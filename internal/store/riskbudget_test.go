package store

import (
	"context"
	"sync"
	"testing"
)

// TestRiskBudgetCounterAccumulates 验证累加与读取。
func TestRiskBudgetCounterAccumulates(t *testing.T) {
	st := newTestStore(t)
	c := NewRiskBudgetCounter(st)
	ctx := context.Background()

	const window = int64(1_790_000_000)
	for i := 1; i <= 5; i++ {
		got, err := c.Add(ctx, "acc-1", window)
		if err != nil {
			t.Fatal(err)
		}
		if got != i {
			t.Fatalf("第 %d 次累加后返回 %d，期望 %d", i, got, i)
		}
	}

	used, err := c.Used(ctx, "acc-1", window)
	if err != nil {
		t.Fatal(err)
	}
	if used != 5 {
		t.Fatalf("已用 = %d，期望 5", used)
	}

	// 不同窗口、不同账号各自独立。
	if used, err := c.Used(ctx, "acc-1", window+3600); err != nil || used != 0 {
		t.Fatalf("另一个窗口应为 0，实际 %d (err=%v)", used, err)
	}
	if used, err := c.Used(ctx, "acc-2", window); err != nil || used != 0 {
		t.Fatalf("另一个账号应为 0，实际 %d (err=%v)", used, err)
	}
}

// TestRiskBudgetCounterConcurrentAdds 验证并发累加不丢计数。
//
// 本包的连接池被限制为单连接，所以这条路径必然串行执行——但「串行」和
// 「不丢」是两件事：先 SELECT 再 UPDATE 的写法在并发下仍会丢。丢计数的
// 后果是预算永远用不满、降频永远不触发，而现象上什么都没坏。
func TestRiskBudgetCounterConcurrentAdds(t *testing.T) {
	st := newTestStore(t)
	c := NewRiskBudgetCounter(st)
	ctx := context.Background()

	const (
		window = int64(1_790_000_000)
		n      = 50
	)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Add(ctx, "acc-1", window); err != nil {
				t.Errorf("累加失败: %v", err)
			}
		}()
	}
	wg.Wait()

	used, err := c.Used(ctx, "acc-1", window)
	if err != nil {
		t.Fatal(err)
	}
	if used != n {
		t.Fatalf("已用 = %d，期望 %d——并发下丢了计数", used, n)
	}
}

// TestRiskBudgetUsedReturnsZeroForMissing 验证没有记录时返回 0 而不是报错。
//
// 新账号、新窗口都没有记录，这条路径每个请求都会走。返回 sql.ErrNoRows
// 会让预算统计在启动后的第一个小时里持续报错，把日志刷满。
func TestRiskBudgetUsedReturnsZeroForMissing(t *testing.T) {
	st := newTestStore(t)
	c := NewRiskBudgetCounter(st)

	used, err := c.Used(context.Background(), "never-seen", 1)
	if err != nil {
		t.Fatalf("缺失记录不应报错: %v", err)
	}
	if used != 0 {
		t.Fatalf("已用 = %d，期望 0", used)
	}
}

// TestPurgeRiskBudget 验证只清理过期窗口。
//
// 边界写错会误删当前窗口里刚攒起来的计数——那等于每小时随机清零一次，
// 而降频恰好因此永远不触发。
func TestPurgeRiskBudget(t *testing.T) {
	st := newTestStore(t)
	c := NewRiskBudgetCounter(st)
	ctx := context.Background()

	const (
		old     = int64(1_790_000_000)
		current = int64(1_790_003_600)
	)
	if _, err := c.Add(ctx, "acc-1", old); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Add(ctx, "acc-1", current); err != nil {
		t.Fatal(err)
	}

	// 截止点正好等于 current：只应删掉比它更早的那一行。
	n, err := st.PurgeRiskBudget(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("删除了 %d 行，期望 1 行", n)
	}
	if used, _ := c.Used(ctx, "acc-1", current); used != 1 {
		t.Fatalf("当前窗口的计数被误删，实际 %d", used)
	}
	if used, _ := c.Used(ctx, "acc-1", old); used != 0 {
		t.Fatalf("过期窗口应被清理，实际 %d", used)
	}
}
