package opssvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

// 批量操作的规模参数。
const (
	// BulkChunkSize 是每一轮处理多少台。
	//
	// 不分块（一次跑完全部）进度就查不到——`progress` 只在轮末写一次，
	// 25 台跑 30 秒的期间用户看到的是一个不动的「运行中」。分块让进度每
	// 10 秒左右前进一格，同时把单轮耗时压在 opTimeout 之内。
	BulkChunkSize = 5

	// bulkConcurrency 是块内的并发度。
	//
	// 压到 3 与同步路径一致：批量操作往往集中在同一个租户上，并发太高很容易
	// 把账号打进限流，反而拖慢所有人。
	bulkConcurrency = 3

	// bulkMaxStuckRounds 是"连续多少轮一台都没成功"就判失败。
	//
	// 有它才不会在全部实例都永久失败时无限重试；取 3 是为了让偶发的 429 /
	// 瞬时 500 有机会自愈。
	bulkMaxStuckRounds = 3
)

// BulkSpec 是批量任务的入参快照。
//
// 一个任务只覆盖一个账号：ops_tasks.account_id 是 NOT NULL 且有外键，
// 而每台实例的凭据都挂在它自己的账号上。跨账号的批量由 HTTP 层按账号拆成
// 多个任务，而不是硬塞进一行里。
type BulkSpec struct {
	Action string `json:"action"`
	// InstanceIDs 是这批要操作的实例。顺序即执行顺序。
	InstanceIDs []string `json:"instanceIds"`
}

// EncodeBulkSpec 把入参序列化成任务里存的那一列。
func EncodeBulkSpec(s BulkSpec) (string, error) {
	buf, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("opssvc: 序列化批量参数失败: %w", err)
	}
	return string(buf), nil
}

// DecodeBulkSpec 从任务里取回入参。
func DecodeBulkSpec(raw string) (BulkSpec, error) {
	var s BulkSpec
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return s, fmt.Errorf("opssvc: 解析批量参数失败: %w", err)
	}
	return s, nil
}

// ValidateBulkSpec 校验入参。
func ValidateBulkSpec(s BulkSpec) error {
	if len(s.InstanceIDs) == 0 {
		return errors.New("没有选中任何实例")
	}
	if err := ociclient.ValidateAction(s.Action); err != nil {
		return err
	}
	return nil
}

// bulkFailure 是一台实例的失败记录。
type bulkFailure struct {
	InstanceID string `json:"instanceId"`
	Name       string `json:"name,omitempty"`
	Error      string `json:"error"`
}

// bulkProgress 是落库的结构化进展。
//
// Pending 只放"还没成功的"，成功的立刻移出——这样失败后重试就自动只重试
// 失败的那几台，而不是把 25 台关机再来一遍。
type bulkProgress struct {
	Action string `json:"action"`
	// Total 是这批的总台数。它同时充当"是否已初始化"的标记：为 0 表示
	// 这是第一轮，Pending 要从 spec 里灌进去。
	Total     int `json:"total"`
	Succeeded int `json:"succeeded"`
	// Pending 是还没成功的实例，按执行顺序排列。
	Pending []string `json:"pending"`
	// Failures 是最近一次失败的原因。重试成功后会被删掉。
	Failures []bulkFailure `json:"failures,omitempty"`
	// StuckRounds 是连续"一台都没成功"的轮数，用于止损。
	StuckRounds int `json:"stuckRounds,omitempty"`
}

// InstanceActioner 执行单台实例的开关机。
//
// 生产实现是 instancesvc.Service：它会校验过渡态、更新本地缓存、启动状态
// 轮询并推事件。直接用 ociclient 也能发请求，但那样批量关掉的机器在界面上
// 会停在旧状态，用户以为没生效。
type InstanceActioner interface {
	Action(ctx context.Context, instanceID, action string) (*store.Instance, error)
}

// handleBulk 是 bulk 类型的处理器。
func (s *Service) handleBulk(ctx context.Context, t *store.OpsTask) outcome {
	if s.instances == nil {
		// 接线漏了要立刻说清楚，而不是让任务每轮 panic 一次。
		return failed(errors.New("批量任务需要接入实例服务，当前未接线"))
	}
	spec, err := DecodeBulkSpec(t.Spec)
	if err != nil {
		return failed(err)
	}
	if err := ValidateBulkSpec(spec); err != nil {
		return failed(err)
	}

	prog := decodeBulkProgress(t.Progress)
	if prog.Total == 0 {
		// 第一轮：把待处理清单灌进 progress，之后每轮都从这里续。
		prog = bulkProgress{
			Action:  spec.Action,
			Total:   len(spec.InstanceIDs),
			Pending: append([]string(nil), spec.InstanceIDs...),
		}
	}

	if len(prog.Pending) == 0 {
		return bulkFinish(prog)
	}
	if !t.ExpiresAt.IsZero() && time.Now().After(t.ExpiresAt) {
		return outcome{
			Done: true,
			Err: fmt.Errorf("已超过最长运行时间，还有 %d 台未处理；点「重试」会只处理剩下的这些",
				len(prog.Pending)),
			Stage:    bulkStageDone,
			Progress: encodeBulkProgress(prog),
		}
	}

	// 取一块。
	n := BulkChunkSize
	if n > len(prog.Pending) {
		n = len(prog.Pending)
	}
	chunk := prog.Pending[:n]
	untouched := prog.Pending[n:]

	results := make([]bulkFailure, len(chunk))
	var okCount int
	runBulkChunk(ctx, s.instances, spec.Action, chunk, results, &okCount)

	// 失败项排到队尾，不是排到队首。
	//
	// 排队首的话，前面几台永久失败就会把后面所有机器堵住：每轮都在重试同样
	// 的几台，直到 stuckRounds 用完直接判失败，剩下十几台一台都没动。
	failed := make([]string, 0, len(chunk)-okCount)
	next := make([]string, 0, len(untouched)+len(chunk)-okCount)
	next = append(next, untouched...)
	failures := make([]bulkFailure, 0, len(prog.Failures)+len(chunk)-okCount)
	failures = append(failures, prog.Failures...)
	for i, id := range chunk {
		if results[i].Error == "" {
			// 之前失败过、这次成功：把失败记录删掉，否则收尾时会误判成
			// "还有失败"。
			failures = removeBulkFailure(failures, id)
			continue
		}
		failed = append(failed, id)
		failures = upsertBulkFailure(failures, results[i])
	}
	next = append(next, failed...)
	prog.Pending = next
	prog.Failures = failures
	prog.Succeeded += okCount

	if okCount > 0 {
		prog.StuckRounds = 0
	} else {
		prog.StuckRounds++
	}

	// 一块全失败且已经连续几轮没有进展：停下来让人看，别继续空转。
	if len(prog.Pending) > 0 && prog.StuckRounds >= bulkMaxStuckRounds {
		return outcome{
			Done: true,
			Err: fmt.Errorf("连续 %d 轮没有一台成功，已停止。剩余 %d 台未完成，点「重试」会只重试这些：%s",
				prog.StuckRounds, len(prog.Pending), bulkFailureHint(prog.Failures)),
			Stage:    bulkStageDone,
			Progress: encodeBulkProgress(prog),
		}
	}

	if len(prog.Pending) == 0 {
		return bulkFinish(prog)
	}
	// 还有得做：立刻排下一块。NextAt 用当下而不是间隔，因为这里没有"等
	// 外部条件"的语义，纯粹是还没做完。
	return outcome{
		Done:     false,
		NextAt:   time.Now(),
		Stage:    bulkStageRunning,
		Progress: encodeBulkProgress(prog),
		Result: fmt.Sprintf("已处理 %d/%d 台，成功 %d 台",
			prog.Total-len(prog.Pending), prog.Total, prog.Succeeded),
	}
}

const (
	bulkStageRunning = "BATCH"
	bulkStageDone    = "DONE"
)

// bulkFinish 收尾：走到这里说明 Pending 空了，也就是每一台都成功过一次。
func bulkFinish(prog bulkProgress) outcome {
	return outcome{
		Done:     true,
		Stage:    bulkStageDone,
		Progress: encodeBulkProgress(prog),
		Result:   fmt.Sprintf("%d 台全部执行成功", prog.Succeeded),
	}
}

// runBulkChunk 并发执行一块，结果写回 results（与 chunk 同序）。
func runBulkChunk(ctx context.Context, act InstanceActioner, action string, chunk []string, results []bulkFailure, okCount *int) {
	sem := make(chan struct{}, bulkConcurrency)
	var wg sync.WaitGroup
	for i, id := range chunk {
		wg.Add(1)
		go func(idx int, instanceID string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if _, err := act.Action(ctx, instanceID, action); err != nil {
				results[idx] = bulkFailure{InstanceID: instanceID, Error: shortErr(err)}
				return
			}
			results[idx] = bulkFailure{InstanceID: instanceID}
		}(i, id)
	}
	wg.Wait()
	for _, r := range results {
		if r.Error == "" {
			*okCount++
		}
	}
}

// removeBulkFailure 删掉某台实例的失败记录。
func removeBulkFailure(list []bulkFailure, instanceID string) []bulkFailure {
	for i := range list {
		if list[i].InstanceID == instanceID {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}

// upsertBulkFailure 按实例 ID 覆盖或追加一条失败记录。
func upsertBulkFailure(list []bulkFailure, f bulkFailure) []bulkFailure {
	for i := range list {
		if list[i].InstanceID == f.InstanceID {
			list[i] = f
			return list
		}
	}
	return append(list, f)
}

// bulkFailureHint 把失败原因压成一行，用于 last_error。
func bulkFailureHint(list []bulkFailure) string {
	if len(list) == 0 {
		return "（无详细原因）"
	}
	parts := make([]string, 0, 3)
	for i, f := range list {
		if i >= 3 {
			parts = append(parts, fmt.Sprintf("等 %d 台", len(list)))
			break
		}
		parts = append(parts, f.InstanceID+"："+f.Error)
	}
	return strings.Join(parts, "；")
}

func decodeBulkProgress(raw string) bulkProgress {
	var p bulkProgress
	if strings.TrimSpace(raw) == "" {
		return p
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return bulkProgress{}
	}
	return p
}

func encodeBulkProgress(p bulkProgress) string {
	buf, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(buf)
}
