// Package riskguard 按账号统计对 OCI 的调用量，为任务层提供降频依据。
//
// 这个包的存在理由不是限流。限流（429）是可逆的：被限了等一会儿就好，
// 技术手段完全可控。这里防的是另一件事——单账号的调用量级本身就是风控
// 特征。oci-helper 抢机每轮发 25–34 次调用，单账号一天约 8.6 万次；而
// 一个正常的脚本化操作一天只有几千次。同样的行为、同样的结果，量级差
// 三十倍，这三十倍才是被判定的原因。
//
// 所以这里的策略是「统计 + 让任务层自己降频」，而不是「超限就报错」：
//
//   - 不在传输层阻塞。传输层阻塞会把一次用户点击变成一个转圈十几秒的请求，
//     用户会重试，重试又增加调用量，反而更糟。
//   - 由任务层拉长间隔。抢机这类循环任务本来就有 interval，翻倍是最自然的
//     降频方式，对用户的表现只是「慢了一点」，而不是「坏了」。
//
// 预算是**每账号每小时**的软上限，超过之后不禁止调用，只是不再算「便宜」。
package riskguard

import (
	"context"
	"sync"
	"time"
)

// DefaultHourlyLimit 是单账号每小时的调用预算。
//
// 取 120 的依据：一个需要人工盯着看的循环任务，间隔 30 秒刚好是 120 次/小时。
// 再密就不像是人在操作了，而且对抢机这类场景也没有额外收益——OCI 的容量
// 释放不是以秒为粒度的。
const DefaultHourlyLimit = 120

// Window 是预算的统计窗口。
const Window = time.Hour

// Counter 是预算计数的存储。抽成接口是为了让 riskguard 不依赖 store：
// 生产环境用 SQLite（重启不清零），测试用内存实现。
type Counter interface {
	// Add 给某账号在指定窗口累加一次调用，返回累加后的次数。
	Add(ctx context.Context, accountID string, windowStart int64) (int, error)
	// Used 返回某账号在指定窗口已用的次数。
	Used(ctx context.Context, accountID string, windowStart int64) (int, error)
}

// bucket 是计数的键：账号 + 小时窗口起点。
type bucket struct {
	accountID string
	window    int64
}

// MemoryCounter 是进程内计数。重启清零，仅用于测试与无持久化时的兜底。
type MemoryCounter struct {
	mu   sync.Mutex
	used map[bucket]int
}

// NewMemoryCounter 创建一个内存计数器。
func NewMemoryCounter() *MemoryCounter {
	return &MemoryCounter{used: make(map[bucket]int)}
}

// Add 累加并返回新值。
func (m *MemoryCounter) Add(_ context.Context, accountID string, windowStart int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := bucket{accountID: accountID, window: windowStart}
	m.used[key]++
	return m.used[key], nil
}

// Used 读取当前值。
func (m *MemoryCounter) Used(_ context.Context, accountID string, windowStart int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.used[bucket{accountID: accountID, window: windowStart}], nil
}

// Guard 是预算的对外入口。可安全地被多 goroutine 共用。
type Guard struct {
	counter Counter
	limit   int

	// now 可注入，测试用来跨小时窗口。
	now func() time.Time
}

// Option 定制 Guard。
type Option func(*Guard)

// WithLimit 覆盖每小时的调用预算。
func WithLimit(n int) Option {
	return func(g *Guard) {
		if n > 0 {
			g.limit = n
		}
	}
}

// WithClock 注入时钟，仅测试使用。
func WithClock(now func() time.Time) Option {
	return func(g *Guard) { g.now = now }
}

// New 创建预算守卫。counter 为 nil 时退回进程内计数。
func New(counter Counter, opts ...Option) *Guard {
	if counter == nil {
		counter = NewMemoryCounter()
	}
	g := &Guard{counter: counter, limit: DefaultHourlyLimit, now: time.Now}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// WindowStart 返回 t 所在小时窗口的起点（unix 秒）。
//
// 用 Unix 时间整除而不是「进程启动后的小时数」：前者跨重启连续，后者
// 每次重启都会把窗口清零，于是重启就成了绕过预算的手段。
func WindowStart(t time.Time) int64 {
	return t.Unix() - t.Unix()%int64(Window/time.Second)
}

// Record 记一次调用。observer 出错时静默吞掉——预算统计失败不该让
// 用户的正常操作失败，代价只是这一小时少算了一次。
func (g *Guard) Record(ctx context.Context, accountID string) {
	if accountID == "" {
		return
	}
	_, _ = g.counter.Add(ctx, accountID, WindowStart(g.now()))
}

// Usage 返回当前窗口已用次数与预算上限。
func (g *Guard) Usage(ctx context.Context, accountID string) (used, limit int) {
	used, _ = g.counter.Used(ctx, accountID, WindowStart(g.now()))
	return used, g.limit
}

// Multiplier 返回任务层应当把间隔放大的倍数。
//
//	未超预算       → 1
//	1–2 倍预算内   → 2
//	2 倍预算以上   → 4
//
// 用倍数而不是「拒绝执行」：任务被拒绝后用户会再点一次，反而更多调用。
// 拉长间隔的观感是「慢」，而慢不会被重试。
func (g *Guard) Multiplier(ctx context.Context, accountID string) int {
	used, limit := g.Usage(ctx, accountID)
	switch {
	case limit <= 0:
		return 1
	case used < limit:
		return 1
	case used < limit*2:
		return 2
	default:
		return 4
	}
}

// Limit 返回每小时预算上限。
func (g *Guard) Limit() int { return g.limit }
