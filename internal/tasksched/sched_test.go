package tasksched

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTask 是最小实现：调度器只认 TaskID。
type fakeTask struct{ id string }

func (f fakeTask) TaskID() string { return f.id }

// sourceFunc 让测试用一行闭包提供到期任务，不必为每种场景写一个结构体。
type sourceFunc[T Task] func(ctx context.Context, now time.Time, limit int) ([]T, error)

func (f sourceFunc[T]) Due(ctx context.Context, now time.Time, limit int) ([]T, error) {
	return f(ctx, now, limit)
}

func fixedSource(tasks ...fakeTask) sourceFunc[fakeTask] {
	return func(context.Context, time.Time, int) ([]fakeTask, error) { return tasks, nil }
}

func noopHandler(context.Context, fakeTask) {}

// drainSoon 给在跑的任务一个有限的收尾窗口，避免测试残留 goroutine。
func drainSoon(t *testing.T, s *Scheduler[fakeTask]) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Drain(ctx); err != nil {
		t.Fatalf("等待任务结束超时：%v", err)
	}
}

// TestTickDoesNotRunSameTaskTwice 锁住"同一个任务不被并发执行"。
//
// 这是抽取调度器时最容易丢的一处：tick 是十秒级，而一次尝试可能跑数十秒
// （自动建网、容量预检、创建实例）。丢了闸门不会有任何报错，现象是同一个
// 任务自己和自己抢同一份配额，尝试次数记成实际的两倍。
func TestTickDoesNotRunSameTaskTwice(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var runs int32

	s := New(Options[fakeTask]{
		Source: fixedSource(fakeTask{id: "a"}),
		Handle: func(context.Context, fakeTask) {
			atomic.AddInt32(&runs, 1)
			started <- struct{}{}
			<-release
		},
	})

	s.Tick(context.Background())
	<-started // 第一轮已经进入 handler 并卡住

	s.Tick(context.Background()) // 第二轮：同一个任务仍在执行中

	select {
	case <-started:
		t.Fatal("同一个任务被执行了两次——防并发闸门失效")
	case <-time.After(80 * time.Millisecond):
	}

	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Fatalf("handler 执行了 %d 次，期望 1 次", got)
	}
	if got := s.Inflight(); got != 1 {
		t.Fatalf("Inflight = %d，期望 1", got)
	}

	close(release)
	drainSoon(t, s)

	if got := s.Inflight(); got != 0 {
		t.Fatalf("任务结束后 Inflight = %d，期望 0——占用没释放，这个任务会永远不再执行", got)
	}
}

// TestTickReleasesClaimAfterHandlerReturns 确认跑完之后能再次被捞到。
//
// 与上一条互补：占用不释放的话，任务只会执行一次然后永久静默，
// 现象是"任务显示运行中但再也没动过"。
func TestTickReleasesClaimAfterHandlerReturns(t *testing.T) {
	var runs int32
	s := New(Options[fakeTask]{
		Source: fixedSource(fakeTask{id: "a"}),
		Handle: func(context.Context, fakeTask) { atomic.AddInt32(&runs, 1) },
	})

	for i := 0; i < 3; i++ {
		s.Tick(context.Background())
		drainSoon(t, s)
	}
	if got := atomic.LoadInt32(&runs); got != 3 {
		t.Fatalf("handler 执行了 %d 次，期望 3 次——占用没有释放", got)
	}
}

// TestTickStaggersBatch 确认同一批任务被错开。
//
// 进程重启时所有任务的 next_at 都是过去时刻，一次 tick 会把它们全部捞出来。
// 不错峰就是同一秒内 N 个请求同时发出——一个凭空造出来的尖峰，
// 而重启恰恰是最容易被风控盯上的时刻（新 IP、新会话、突发流量）。
func TestTickStaggersBatch(t *testing.T) {
	const stagger = 40 * time.Millisecond
	starts := make(chan time.Time, 3)

	s := New(Options[fakeTask]{
		Stagger: stagger,
		Source:  fixedSource(fakeTask{id: "a"}, fakeTask{id: "b"}, fakeTask{id: "c"}),
		Handle:  func(context.Context, fakeTask) { starts <- time.Now() },
	})

	s.Tick(context.Background())

	first := <-starts
	<-starts
	last := <-starts
	drainSoon(t, s)

	// 名义上是 2×40ms，留足调度抖动的余量后取 1×40ms 作为下限。
	if d := last.Sub(first); d < stagger {
		t.Fatalf("三个任务在 %v 内全部启动，错峰没有生效（期望至少 %v）", d, 2*stagger)
	}
}

// TestStaggerZeroRunsImmediately 确认不配错峰时立即执行。
//
// 抢机之外的任务类型（一次性的运维操作）没有并发尖峰问题，不该被强制
// 拖慢 i×5 秒。
func TestStaggerZeroRunsImmediately(t *testing.T) {
	done := make(chan struct{}, 3)
	s := New(Options[fakeTask]{
		Stagger: 0,
		Source:  fixedSource(fakeTask{id: "a"}, fakeTask{id: "b"}, fakeTask{id: "c"}),
		Handle:  func(context.Context, fakeTask) { done <- struct{}{} },
	})

	s.Tick(context.Background())
	for i := 0; i < 3; i++ {
		select {
		case <-done:
		case <-time.After(50 * time.Millisecond):
			t.Fatalf("第 %d 个任务没有立即执行", i+1)
		}
	}
	drainSoon(t, s)
}

// TestTickReportsSourceError 确认读取失败会走 OnError 而不是静默。
//
// 静默的后果是任务全部不跑，而界面上一片"运行中"，没有任何线索指向
// 存储层。
func TestTickReportsSourceError(t *testing.T) {
	want := errors.New("数据库锁住了")
	var got error
	s := New(Options[fakeTask]{
		Source: sourceFunc[fakeTask](func(context.Context, time.Time, int) ([]fakeTask, error) {
			return nil, want
		}),
		Handle:  func(context.Context, fakeTask) { t.Error("读取失败时不该执行任何任务") },
		OnError: func(err error) { got = err },
	})

	s.Tick(context.Background())
	if !errors.Is(got, want) {
		t.Fatalf("OnError 收到 %v，期望 %v", got, want)
	}
}

// TestTickWithoutOnErrorDoesNotPanic 确认没配 OnError 时只是丢弃。
func TestTickWithoutOnErrorDoesNotPanic(t *testing.T) {
	s := New(Options[fakeTask]{
		Source: sourceFunc[fakeTask](func(context.Context, time.Time, int) ([]fakeTask, error) {
			return nil, errors.New("boom")
		}),
		Handle: noopHandler,
	})
	s.Tick(context.Background()) // 不 panic 即通过
}

// TestTickPassesLimitToSource 确认并发上限传到了查询层。
//
// 上限只在查询层生效：捞回来再丢弃等于先把任务读进内存、白占一次连接，
// 而且返回的条数会超过调用方的预期。
func TestTickPassesLimitToSource(t *testing.T) {
	var got int
	s := New(Options[fakeTask]{
		Limit: 3,
		Source: sourceFunc[fakeTask](func(_ context.Context, _ time.Time, limit int) ([]fakeTask, error) {
			got = limit
			return nil, nil
		}),
		Handle: noopHandler,
	})
	s.Tick(context.Background())
	if got != 3 {
		t.Fatalf("Source 收到的 limit = %d，期望 3", got)
	}
}

// TestOptionsDefaults 确认零值参数被补成默认值。
func TestOptionsDefaults(t *testing.T) {
	s := New(Options[fakeTask]{Source: fixedSource(), Handle: noopHandler})
	if s.tick != DefaultTick {
		t.Errorf("Tick = %v，期望 %v", s.tick, DefaultTick)
	}
	if s.limit != DefaultLimit {
		t.Errorf("Limit = %d，期望 %d", s.limit, DefaultLimit)
	}
	if s.stagger != 0 {
		t.Errorf("未指定 Stagger 时应当为 0，实际 %v", s.stagger)
	}
}

// TestNewPanicsOnMissingWiring 确认接线错误在启动时就炸。
//
// 这两种情况都是程序员错误，静默降级的结果是"服务起来了但任务永远不跑"，
// 排查成本远高于启动失败。
func TestNewPanicsOnMissingWiring(t *testing.T) {
	t.Run("缺少 Source", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("缺少 Source 时应当 panic")
			}
		}()
		New(Options[fakeTask]{Handle: noopHandler})
	})
	t.Run("缺少 Handle", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("缺少 Handle 时应当 panic")
			}
		}()
		New(Options[fakeTask]{Source: fixedSource()})
	})
}

// TestRunReturnsOnContextCancel 确认 Run 会随 ctx 退出。
func TestRunReturnsOnContextCancel(t *testing.T) {
	s := New(Options[fakeTask]{
		Tick:   5 * time.Millisecond,
		Source: fixedSource(),
		Handle: noopHandler,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 没有在 ctx 取消后返回")
	}
}

// TestDrainGivesUpAtDeadline 确认 Drain 不会无限等。
//
// 停机时陪着一次四分钟的任务耗到底，用户看到的是"服务关不掉"。
func TestDrainGivesUpAtDeadline(t *testing.T) {
	block := make(chan struct{})
	defer close(block)

	s := New(Options[fakeTask]{
		Source: fixedSource(fakeTask{id: "a"}),
		Handle: func(context.Context, fakeTask) { <-block },
	})
	s.Tick(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.Drain(ctx); err == nil {
		t.Fatal("任务仍在执行时 Drain 应当超时返回")
	}
}

// TestDrainReturnsImmediatelyWhenIdle 确认没有任务时 Drain 不等待。
func TestDrainReturnsImmediatelyWhenIdle(t *testing.T) {
	s := New(Options[fakeTask]{Source: fixedSource(), Handle: noopHandler})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if err := s.Drain(ctx); err != nil {
		t.Fatalf("空闲时 Drain 返回错误：%v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("空闲时 Drain 花了 %v", d)
	}
}
