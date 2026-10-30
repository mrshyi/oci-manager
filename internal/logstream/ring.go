package logstream

import (
	"sync"
	"time"
)

// DefaultCapacity 是环形缓冲的默认容量。
//
// 5000 条：按本工具正常运行时的日志量（几分钟一条），够回看一两天；
// 按出故障时（每秒几条），够回看最近一两个小时。再多也只是把内存
// 花在一个不负责留存的工具上。
const DefaultCapacity = 5000

// subscriberBuffer 是每个订阅者的 channel 缓冲。
//
// 256 条：一条 SSE 连接正常时是毫秒级消费，256 条足够吸收一次突发。
// 真的满了说明这个客户端已经跟不上，丢掉新条目比堵住日志写入好。
const subscriberBuffer = 256

// Entry 是一条日志。字段名就是 SSE 里 JSON 的字段名，前端直接按它取值。
type Entry struct {
	// Seq 是全局单调递增的序号，用于「先订阅再快照」时去重。
	Seq   uint64            `json:"seq"`
	Time  time.Time         `json:"time"`
	Level string            `json:"level"`
	Msg   string            `json:"msg"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

// Ring 是固定容量的日志环形缓冲。
//
// 写满覆盖最旧的。没有落库、没有持久化——需要留存请用 docker logs。
type Ring struct {
	mu   sync.Mutex
	buf  []Entry
	next int // 下一条要写入的位置
	size int // 当前有效条数
	seq  uint64

	subs    map[int]chan Entry
	nextSub int
	// dropped 统计因订阅者 channel 满而丢弃的条数，供排障时判断
	// 「日志看着不全」是丢的还是本来就没写。
	dropped uint64
}

// NewRing 构造一个环形缓冲。capacity <= 0 时用 DefaultCapacity。
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Ring{
		buf:  make([]Entry, capacity),
		subs: map[int]chan Entry{},
	}
}

// Push 写入一条日志并推给全部订阅者。
//
// 订阅者 channel 满时直接丢弃这条，绝不阻塞——日志写入慢下来会连带
// 拖住调用它的业务代码，那是把可观测性变成故障源。
func (r *Ring) Push(e Entry) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.seq++
	e.Seq = r.seq
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	r.buf[r.next] = e
	r.next = (r.next + 1) % len(r.buf)
	if r.size < len(r.buf) {
		r.size++
	}
	subs := make([]chan Entry, 0, len(r.subs))
	for _, ch := range r.subs {
		subs = append(subs, ch)
	}
	for _, ch := range subs {
		select {
		case ch <- e:
		default:
			r.dropped++
		}
	}
	r.mu.Unlock()
}

// Snapshot 返回最近 limit 条，按时间正序（最旧的在前）。
//
// limit <= 0 或超过当前条数时返回全部。
func (r *Ring) Snapshot(limit int) []Entry {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	n := r.size
	if limit > 0 && limit < n {
		n = limit
	}
	if n == 0 {
		return nil
	}
	out := make([]Entry, 0, n)
	// 最旧的条目下标：写满时是 next（下一个要被覆盖的），没写满时是 0。
	start := 0
	if r.size == len(r.buf) {
		start = r.next
	}
	skip := r.size - n
	for i := 0; i < n; i++ {
		out = append(out, r.buf[(start+skip+i)%len(r.buf)])
	}
	return out
}

// Len 返回当前有效条数。
func (r *Ring) Len() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.size
}

// Dropped 返回因订阅者跟不上而丢弃的条数。
func (r *Ring) Dropped() uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

// Subscribe 订阅后续日志。返回的 cancel 必须调用，否则 channel 会一直留着。
func (r *Ring) Subscribe() (<-chan Entry, func()) {
	if r == nil {
		ch := make(chan Entry)
		close(ch)
		return ch, func() {}
	}
	ch := make(chan Entry, subscriberBuffer)

	r.mu.Lock()
	id := r.nextSub
	r.nextSub++
	r.subs[id] = ch
	r.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.subs, id)
			r.mu.Unlock()
			close(ch)
		})
	}
	return ch, cancel
}
