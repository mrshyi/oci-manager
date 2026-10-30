// Package tasksched 提供通用的后台任务调度循环。
//
// 抢机（huntsvc）与运维任务（opssvc）的差别只在"一轮具体做什么"，而"什么
// 时候该做、同时做几个、同一个任务会不会被做两遍"是同一套问题。把它抽出来
// 是为了让第二个任务类型不再复制一遍 tick 循环——复制一旦分叉，两边的并发
// 上限与重启行为就会不一致，而这种不一致只有线上才看得出来。
//
// 调度器只管三件事：
//   - 定时问 Source 要到期任务（到期判定落在持久化的 next_at 上）；
//   - 用 ClaimSet 保证同一个任务不会被两轮 tick 同时执行；
//   - 按 Stagger 把同一批任务错开，避免重启瞬间一起开火。
//
// 它不碰业务：一轮要做什么、做完把 next_at 推到什么时候，全部由 Handler
// 决定。这个边界是刻意的——抢机的业务逻辑一行都不该因为抽调度器而改变。
package tasksched

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// defaultRand 用 math/rand/v2：全局实例自带随机种子，不需要调用方先播种。
// 用 math/rand 的全局 Rand 而不播种的话每次启动都是同一串数，抖动等于不存在。
var defaultRand = rand.Float64

const (
	// DefaultTick 是默认轮询间隔。
	DefaultTick = 10 * time.Second

	// DefaultLimit 是默认单轮最多捞取的任务数，同时也是并发上限。
	//
	// 这个数字在抢机场景里不是性能参数而是风控参数：并发越高，Oracle 看到的
	// 瞬时请求量越大。别的场景可以按需调大，但默认值保持保守。
	DefaultLimit = 4

	// DefaultStagger 是同一批任务之间的默认错峰间隔。
	//
	// 进程重启时所有任务的 next_at 都已经是过去时刻，一次 tick 会把它们全部
	// 捞出来。没有错峰就是同一秒内 N 个请求同时发出，形成一个尖峰——这正是
	// 风控最容易注意到的形状。
	DefaultStagger = 5 * time.Second
)

// Task 是调度器眼里的任务：只需要一个能用于去重的标识。
//
// 刻意不定义 NextAt / State 之类的方法：那些是存储层的事，调度器不读它们，
// 也就没有理由要求实现方暴露。
type Task interface {
	TaskID() string
}

// Source 是任务来源，由实现方决定"到期"的判定口径。
type Source[T Task] interface {
	// Due 返回到期该执行的任务，按紧急程度升序，最多 limit 条。
	Due(ctx context.Context, now time.Time, limit int) ([]T, error)
}

// Handler 执行一次任务。
//
// 传进来的 ctx 可能在调度器停止后仍然有效（实现方通常用 WithoutCancel
// 派生），因为一次执行可能已经开始创建资源，中途掐断会留下半成品。
type Handler[T Task] func(ctx context.Context, t T)

// Options 是构造参数。
type Options[T Task] struct {
	Tick    time.Duration
	Limit   int
	Stagger time.Duration

	Source Source[T]
	Handle Handler[T]

	// OnError 接收调度器自身的错误（读取到期任务失败等）。
	// 为 nil 时静默丢弃——调用方不关心就不该被迫写一个空函数。
	OnError func(err error)

	// Now 是测试缝，nil 用 time.Now。
	Now func() time.Time
}

// Scheduler 是调度循环本体。
//
// 零值不可用，必须经 New 构造。
type Scheduler[T Task] struct {
	tick    time.Duration
	limit   int
	stagger time.Duration

	src    Source[T]
	handle Handler[T]
	onErr  func(error)
	now    func() time.Time

	claims *ClaimSet

	wg sync.WaitGroup
}

// New 构造调度器。
//
// Source 或 Handle 为空属于接线错误，直接 panic：这类错误在启动瞬间暴露，
// 比"服务起来了但任务永远不跑"要便宜得多。
func New[T Task](o Options[T]) *Scheduler[T] {
	if o.Source == nil {
		panic("tasksched: Source 不能为空")
	}
	if o.Handle == nil {
		panic("tasksched: Handle 不能为空")
	}
	if o.Tick <= 0 {
		o.Tick = DefaultTick
	}
	if o.Limit <= 0 {
		o.Limit = DefaultLimit
	}
	if o.Stagger < 0 {
		o.Stagger = 0
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Scheduler[T]{
		tick:    o.Tick,
		limit:   o.Limit,
		stagger: o.Stagger,
		src:     o.Source,
		handle:  o.Handle,
		onErr:   o.OnError,
		now:     o.Now,
		claims:  NewClaimSet(),
	}
}

// Run 启动调度循环，直到 ctx 结束。
//
// 返回时不等待在跑的任务——需要等就再调 Drain。分成两步是因为等待必须有时限，
// 而时限是调用方的决定，不是调度器能替他定的。
func (s *Scheduler[T]) Run(ctx context.Context) {
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick 执行一轮：捞取到期任务并并发执行。
//
// 导出是为了让测试能确定性地推进一轮，而不必等一个真实的 tick 周期。
func (s *Scheduler[T]) Tick(ctx context.Context) {
	tasks, err := s.src.Due(ctx, s.now(), s.limit)
	if err != nil {
		if s.onErr != nil {
			s.onErr(err)
		}
		return
	}

	for i := range tasks {
		// 取一份拷贝：任务在 goroutine 里被读写，直接引用切片元素会与
		// 下一轮的查询结果共享底层数组。
		t := tasks[i]
		id := t.TaskID()
		if !s.claims.TryAcquire(id) {
			// 上一轮还在跑。一次尝试可能耗时数十秒（自动建网、容量预检），
			// 而 tick 是十秒级——不挡住就会并发执行同一个任务，
			// 变成自己和自己抢同一份配额。
			continue
		}

		// 错峰：第 i 个任务延后 i×Stagger 再动手。
		wait := time.Duration(i) * s.stagger

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.claims.Release(id)

			if wait > 0 {
				timer := time.NewTimer(wait)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
				}
			}
			s.handle(ctx, t)
		}()
	}
}

// Drain 等待正在执行的任务结束，最多等到 ctx 结束。
//
// 停机时用它给在跑的任务一个收尾窗口：一次 LaunchInstance 请求已经发出去了，
// 进程直接退出的话响应丢掉、结果没落库，用户看到的是"任务卡在 running"
// 而云端多了一台没人认领的实例。
func (s *Scheduler[T]) Drain(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Inflight 返回正在执行的任务数，用于观测。
func (s *Scheduler[T]) Inflight() int {
	return s.claims.Len()
}

// Step 是一级退避阶梯。
//
// After 是触发阈值（累计尝试次数），Factor 是触发后对基准间隔的倍数。
// 只取满足条件的**最高**一级，不做连乘：连乘会让一个跑了很久的任务退到
// 几小时一次，用户看着"运行中"却半天不动，和卡死没有区别。
type Step struct {
	After  int
	Factor float64
}

// Backoff 把"撞墙次数"映射成下一次的等待时长。
//
// 三层结构：基准间隔（业务方给）→ 阶梯倍数（撞墙越多越慢）→ 抖动。
// 抖动的作用不是美观：多个任务的间隔如果都是整分钟，它们会永远在同一秒
// 一起开火，形成周期性尖峰。
type Backoff struct {
	// Max 是阶梯放大后的上界。抖动在它之后叠加，所以实际值可能略超。
	Max time.Duration
	// Jitter 是抖动幅度，0.2 表示 ±20%。0 表示不抖动。
	Jitter float64
	// Steps 按 After 升序排列。
	Steps []Step
	// Rand 是测试缝，nil 用 math/rand。
	Rand func() float64
}

// Delay 计算下一次的等待时长。
//
// base 由调用方按自己的口径算好（例如"用户填的间隔夹到允许范围内"），
// Backoff 只负责阶梯与抖动，不猜 base 怎么来。
func (b Backoff) Delay(base time.Duration, attempts int) time.Duration {
	if base <= 0 {
		return 0
	}

	factor := 1.0
	for _, st := range b.Steps {
		if attempts >= st.After {
			factor = st.Factor
		}
	}

	d := time.Duration(float64(base) * factor)
	if b.Max > 0 && d > b.Max {
		d = b.Max
	}
	return b.jitter(d)
}

// jitter 在 d 上叠加 ±Jitter 的偏移。
//
// 上界判定放在抖动之前是刻意的：Max 约束的是"退避不会无限增长"这件事，
// 抖动只是在它附近浮动。抖动之后不再夹一次，否则贴着上界的那批任务
// 会因为永远取不到上方的值而全部落到同一个点上——抖动反而失效了。
func (b Backoff) jitter(d time.Duration) time.Duration {
	if b.Jitter <= 0 || d <= 0 {
		return d
	}
	r := b.Rand
	if r == nil {
		r = defaultRand
	}
	offset := (r()*2 - 1) * b.Jitter
	return time.Duration(float64(d) * (1 + offset))
}
