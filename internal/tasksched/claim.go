package tasksched

import "sync"

// ClaimSet 记录"哪些任务正在执行"，用来防止同一个任务被并发执行。
//
// 为什么必须挡：tick 是十秒级，而一次尝试可能耗时数十秒（自动建网、容量
// 预检、创建实例）。不挡的话第二轮 tick 会把同一个任务再捞出来并发跑一遍，
// 结果是同一个任务自己和自己抢同一份配额——既浪费调用量，又会让"尝试次数"
// 记成实际的两倍。
//
// 只存在内存里，不落库。理由是它描述的是"本进程此刻在跑什么"，进程退出后
// 这个信息就没有意义了；重启后重新捞取任务本身就是正确的行为。
type ClaimSet struct {
	mu   sync.Mutex
	live map[string]struct{}
}

// NewClaimSet 构造一个空的集合。
func NewClaimSet() *ClaimSet {
	return &ClaimSet{live: make(map[string]struct{})}
}

// TryAcquire 尝试占用 id，已在占用中返回 false。
//
// 语义是"尝试"而不是"等待"：调用方（调度器）拿到 false 时应当直接跳过这个
// 任务，而不是阻塞等它跑完——等待会把 tick 卡住，进而拖住同一批里的其他任务。
func (c *ClaimSet) TryAcquire(id string) bool {
	if id == "" {
		// 空 ID 无法去重，放行等于放弃保护。调度器的 TaskID 实现有问题时，
		// 这里挡住比默默并发执行要好。
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.live[id]; ok {
		return false
	}
	c.live[id] = struct{}{}
	return true
}

// Release 释放占用。对未占用的 id 调用是安全的空操作——
// 执行路径上会有多个 defer，重复释放不该 panic。
func (c *ClaimSet) Release(id string) {
	c.mu.Lock()
	delete(c.live, id)
	c.mu.Unlock()
}

// Has 报告 id 是否正在执行。
func (c *ClaimSet) Has(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.live[id]
	return ok
}

// Len 返回正在执行的任务数。
func (c *ClaimSet) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.live)
}
