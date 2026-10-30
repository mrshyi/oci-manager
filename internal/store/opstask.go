package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 运维任务状态。
//
// 与抢机任务（hunt_tasks）是两套状态机，刻意不合并：
//
//	pending ──▶ running ──┬──▶ succeeded
//	   │           │       ├──▶ failed ──▶ pending（重试）
//	   │           │       ├──▶ paused ──▶ running（恢复）
//	   ▼           ▼       └──▶ canceled
//	canceled    canceled
//
// 抢机只有"跑 / 停 / 成功 / 失败"四态，因为它是一个循环；运维任务多出
// paused 与"失败后从此步重试"，因为救援编排这类任务需要停在中间某一步。
const (
	OpsPending   = "pending"
	OpsRunning   = "running"
	OpsSucceeded = "succeeded"
	OpsFailed    = "failed"
	OpsPaused    = "paused"
	OpsCanceled  = "canceled"
)

// 运维任务类型。
//
// kind 决定"一轮做什么"，由 opssvc 按 kind 分派。这里只列已规划的种类，
// 存的是字符串，加新种类不需要改表。
const (
	OpsKindChangeIP  = "change_ip_cidr"
	OpsKindOpenPorts = "open_ports"
	OpsKindEnable500 = "enable_500m"
	OpsKindRescue    = "rescue"
	OpsKindBulk      = "bulk"
)

// 默认值。与 hunt_tasks 的默认值口径一致：保守，宁可慢。
const (
	defaultOpsMaxAttempts     = 30
	defaultOpsIntervalSeconds = 20
)

// OpsTask 是一个通用运维作业。
//
// 与 HuntTask 的关系是"并列"不是"继承"：两者的公共部分只有"有状态、有
// next_at、会被调度器捞走"，那部分已经抽到 internal/tasksched 里了。
// 合表会得到两张表的并集加一堆半空列，而查询条件要多出一堆 kind = ? 分支。
type OpsTask struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	AccountID  string `json:"accountId"`
	InstanceID string `json:"instanceId"`
	// Name 是给人看的标签。方案里的建表语句没有这一列，但审计日志与列表
	// 都需要一个可读的标识，靠 kind + id 拼出来的东西没人认得。
	Name string `json:"name"`

	// Spec 是入参快照。任务在创建时快照一次，之后每轮原样重放——
	// 与抢机任务同样的理由：参数在任务生命周期内不变是刻意的。
	Spec string `json:"-"`

	State string `json:"state"`
	// Stage 是分阶段任务（救援）当前停在哪一步。
	Stage string `json:"stage"`
	// Progress 是结构化进展，JSON。给前端画进度条用。
	Progress string `json:"progress"`

	Attempts        int `json:"attempts"`
	MaxAttempts     int `json:"maxAttempts"`
	IntervalSeconds int `json:"intervalSeconds"`

	LastError string `json:"lastError"`
	Result    string `json:"result"`

	// DNSStatus / DNSError 是「换 IP 之后顺带更新域名」这一步的结果。
	//
	// 单独两列，与 Result 严格分开：DNS 更新失败不能让换 IP 判失败。
	// 空字符串表示这次任务不涉及 DNS（没有绑定 A 记录，或不是换 IP 任务）。
	// 取值见 DNSStatus* 常量。
	DNSStatus string `json:"dnsStatus"`
	DNSError  string `json:"dnsError"`

	ExpiresAt  time.Time `json:"expiresAt"`
	NextAt     time.Time `json:"nextAt"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// TaskID 让 OpsTask 满足 tasksched.Task。
func (t OpsTask) TaskID() string { return t.ID }

// 任务级错误。
var (
	ErrOpsTaskNotFound = errors.New("运维任务不存在")
	// ErrOpsTaskIllegalTransition 表示状态迁移不在允许表里。
	ErrOpsTaskIllegalTransition = errors.New("运维任务状态迁移非法")
	// ErrOpsTaskConflict 表示读到状态之后、写入之前状态被别人改掉了。
	ErrOpsTaskConflict = errors.New("运维任务状态已被其他操作改变")
)

// opsTransitions 是允许的状态迁移表。
//
// 用白名单而不是黑名单：非法组合是无穷的（写错一个字符串就是一个新状态），
// 而合法的就那么几条。终止态用空切片显式声明，避免"忘了列"和"故意没有"
// 看起来一样。
var opsTransitions = map[string][]string{
	OpsPending:   {OpsRunning, OpsCanceled},
	OpsRunning:   {OpsSucceeded, OpsFailed, OpsPaused, OpsCanceled},
	OpsPaused:    {OpsRunning, OpsCanceled},
	OpsFailed:    {OpsPending, OpsCanceled},
	OpsSucceeded: {},
	OpsCanceled:  {},
}

// CanTransitionOps 报告 from → to 是否是合法迁移。
//
// 自己到自己算合法：用户连点两次"暂停"不该报错，那只是重复表达同一个意图。
func CanTransitionOps(from, to string) bool {
	if from == to {
		return isOpsState(from)
	}
	for _, next := range opsTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

func isOpsState(s string) bool {
	_, ok := opsTransitions[s]
	return ok
}

// OpsTerminal 报告该状态是否已终结（不会再被调度）。
func OpsTerminal(s string) bool {
	return s == OpsSucceeded || s == OpsCanceled
}

// 换 IP 顺带更新 DNS 的结果。空字符串表示不涉及。
const (
	// DNSStatusUpdated 表示绑定到这台实例的 A 记录已全部指向新 IP。
	DNSStatusUpdated = "updated"
	// DNSStatusFailed 表示换 IP 成功但 DNS 没更新上。这不是任务失败。
	DNSStatusFailed = "failed"
	// DNSStatusPartial 表示一部分记录更新成功、一部分失败。
	DNSStatusPartial = "partial"
)

const opsColumns = `id, kind, account_id, instance_id, name, spec, state, stage,
	progress, attempts, max_attempts, interval_seconds, last_error, result,
	expires_at, next_at, started_at, finished_at, created_at, updated_at,
	dns_status, dns_error`

func scanOps(row interface{ Scan(...any) error }) (*OpsTask, error) {
	var (
		t                                         OpsTask
		expires, next, started, finished, created int64
		updated                                   int64
	)
	err := row.Scan(&t.ID, &t.Kind, &t.AccountID, &t.InstanceID, &t.Name, &t.Spec,
		&t.State, &t.Stage, &t.Progress, &t.Attempts, &t.MaxAttempts,
		&t.IntervalSeconds, &t.LastError, &t.Result,
		&expires, &next, &started, &finished, &created, &updated,
		&t.DNSStatus, &t.DNSError)
	if err != nil {
		return nil, err
	}
	t.ExpiresAt = unixToTime(expires)
	t.NextAt = unixToTime(next)
	t.StartedAt = unixToTime(started)
	t.FinishedAt = unixToTime(finished)
	t.CreatedAt = unixToTime(created)
	t.UpdatedAt = unixToTime(updated)
	return &t, nil
}

// CreateOpsTask 写入一个新任务。
//
// 新任务一律从 pending 开始。不接受调用方直接指定 running：那等于绕过了
// "谁在什么时候把它启动起来"的记录，`started_at` 就失去了意义。
func (s *Store) CreateOpsTask(ctx context.Context, t OpsTask) (*OpsTask, error) {
	if strings.TrimSpace(t.Kind) == "" {
		return nil, errors.New("store: 运维任务必须指定 kind")
	}
	if t.State != "" && t.State != OpsPending {
		return nil, fmt.Errorf("store: 新任务只能从 %s 开始，收到 %q", OpsPending, t.State)
	}
	if t.ID == "" {
		id, err := newID()
		if err != nil {
			return nil, err
		}
		t.ID = id
	}
	if t.MaxAttempts <= 0 {
		t.MaxAttempts = defaultOpsMaxAttempts
	}
	if t.IntervalSeconds <= 0 {
		t.IntervalSeconds = defaultOpsIntervalSeconds
	}
	if t.Spec == "" {
		t.Spec = "{}"
	}
	if t.Progress == "" {
		t.Progress = "{}"
	}

	now := nowUnix()
	// next_at 留空表示"立刻可执行"，让用户建完马上看到动静。
	next := timeToUnix(t.NextAt)
	if next == 0 {
		next = now
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ops_tasks
			(id, kind, account_id, instance_id, name, spec, state, stage, progress,
			 attempts, max_attempts, interval_seconds, last_error, result,
			 expires_at, next_at, started_at, finished_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,0,?,?,'','',?,?,0,0,?,?)`,
		t.ID, t.Kind, t.AccountID, t.InstanceID, t.Name, t.Spec, OpsPending, t.Stage, t.Progress,
		t.MaxAttempts, t.IntervalSeconds, timeToUnix(t.ExpiresAt), next, now, now)
	if err != nil {
		return nil, fmt.Errorf("store: 创建运维任务失败: %w", err)
	}
	return s.GetOpsTask(ctx, t.ID)
}

// GetOpsTask 按 ID 读取。
func (s *Store) GetOpsTask(ctx context.Context, id string) (*OpsTask, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+opsColumns+` FROM ops_tasks WHERE id = ?`, id)
	t, err := scanOps(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOpsTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取运维任务失败: %w", err)
	}
	return t, nil
}

// OpsTaskFilter 是列表查询的过滤条件。零值表示不过滤。
type OpsTaskFilter struct {
	AccountID string
	Kind      string
	State     string
	// Limit 留空用 200。
	Limit int
}

// ListOpsTasks 返回符合过滤条件的任务，最新创建的在前。
func (s *Store) ListOpsTasks(ctx context.Context, f OpsTaskFilter) ([]OpsTask, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.AccountID != "" {
		where = append(where, "account_id = ?")
		args = append(args, f.AccountID)
	}
	if f.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, f.Kind)
	}
	if f.State != "" {
		where = append(where, "state = ?")
		args = append(args, f.State)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+opsColumns+` FROM ops_tasks WHERE `+strings.Join(where, " AND ")+
			` ORDER BY created_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询运维任务失败: %w", err)
	}
	defer rows.Close()

	out := make([]OpsTask, 0)
	for rows.Next() {
		t, err := scanOps(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// DueOpsTasks 返回到点该执行的任务，按 next_at 升序，最多 limit 条。
//
// 条件是 state IN (pending, running)：pending 是刚创建还没起跑的，running 是
// 多轮任务（换 IP、救援）等在下一轮的。paused / failed / 两个终态一律不在内——
// 暂停的任务被捞到的话，"暂停"按钮点了等于没点，而界面上的状态确实是 paused。
func (s *Store) DueOpsTasks(ctx context.Context, now time.Time, limit int) ([]OpsTask, error) {
	if limit <= 0 {
		limit = 4
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+opsColumns+` FROM ops_tasks
		WHERE state IN (?, ?) AND next_at <= ?
		ORDER BY next_at ASC LIMIT ?`,
		OpsPending, OpsRunning, now.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询到期运维任务失败: %w", err)
	}
	defer rows.Close()

	out := make([]OpsTask, 0, limit)
	for rows.Next() {
		t, err := scanOps(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// TransitionOpsTask 把任务迁移到 to 状态，非法迁移直接拒绝。
//
// 读状态与写状态在同一个事务里，且 UPDATE 带上 `AND state = <读到的状态>`：
// 调度器正在跑这一轮的同时用户点了"取消"，没有这个条件的话取消会被这一轮的
// 结束写回覆盖掉——用户以为取消了，任务还在跑。
func (s *Store) TransitionOpsTask(ctx context.Context, id, to string) (*OpsTask, error) {
	if !isOpsState(to) {
		return nil, fmt.Errorf("store: 未知的任务状态 %q", to)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		from    string
		started int64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT state, started_at FROM ops_tasks WHERE id = ?`, id).Scan(&from, &started)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOpsTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取任务状态失败: %w", err)
	}

	// 重复表达同一个意图不算错误（连点两次"暂停"）。
	if from == to {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("store: 提交事务失败: %w", err)
		}
		return s.GetOpsTask(ctx, id)
	}
	if !CanTransitionOps(from, to) {
		return nil, fmt.Errorf("%w: %s → %s", ErrOpsTaskIllegalTransition, from, to)
	}

	now := nowUnix()
	sets := []string{"state = ?", "updated_at = ?"}
	args := []any{to, now}

	switch to {
	case OpsRunning:
		// 恢复 / 重试 / 首次启动都从这里走：把 next_at 拨到当下，让用户
		// 立刻看到动静，而不是对着一个几分钟的退避倒计时干等。
		sets = append(sets, "next_at = ?")
		args = append(args, now)
		if started == 0 {
			sets = append(sets, "started_at = ?")
			args = append(args, now)
		}
	case OpsPending:
		// failed → pending 是"从此步重试"，同样立刻可执行。
		sets = append(sets, "next_at = ?")
		args = append(args, now)
	case OpsSucceeded, OpsCanceled:
		sets = append(sets, "next_at = 0", "finished_at = ?")
		args = append(args, now)
	case OpsPaused, OpsFailed:
		// 两者都可能被恢复，保留 next_at 不动：暂停期间外部条件没变，
		// 恢复时再统一拨到当下。
	}

	args = append(args, id, from)
	res, err := tx.ExecContext(ctx,
		`UPDATE ops_tasks SET `+strings.Join(sets, ", ")+` WHERE id = ? AND state = ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 更新任务状态失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrOpsTaskConflict
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: 提交事务失败: %w", err)
	}
	return s.GetOpsTask(ctx, id)
}

// OpsTaskProgress 是一轮执行之后要写回的进展。
type OpsTaskProgress struct {
	Stage string
	// Progress 与 Result 留空表示不改动。
	Progress  string
	Result    string
	LastError string
	// NextAt 是下一轮的时刻。零值表示"不再排期"——调用方应当同时把状态
	// 迁到终态，这里不替它做这个决定。
	NextAt time.Time
	// NoAttempt 为 true 时不增加 attempts。
	//
	// 用于限流（429）退避：被 Oracle 限流不是任务自身的一次失败尝试，把它
	// 计入次数会让"重试 30 次"在限流时很快耗尽——用户看到的是"尝试次数用完
	// 了"，而实际上一次正经的换 IP 都还没做。次数应当只数真正打过 OCI 的轮次。
	NoAttempt bool
	// DNSStatus / DNSError 留空表示不改动这两列。
	//
	// 中间轮次也会写：域名在每一轮都该指向当前 IP，而当前 IP 每一轮都在变。
	// 写最新一次的结果，用户看到的就永远是"此刻域名指着哪里"。
	DNSStatus string
	DNSError  string
}

// AdvanceOpsTask 记录一轮执行的进展。
//
// attempts 自增而不是由调用方写绝对值：多轮任务并发推进时，读-改-写会丢计数。
func (s *Store) AdvanceOpsTask(ctx context.Context, id string, p OpsTaskProgress) error {
	sets := []string{
		"next_at = ?",
		"last_error = ?",
		"updated_at = ?",
	}
	now := nowUnix()
	args := []any{timeToUnix(p.NextAt), p.LastError, now}
	// 限流退避不计入尝试次数，见 OpsTaskProgress.NoAttempt。
	if !p.NoAttempt {
		sets = append(sets, "attempts = attempts + 1")
	}

	if p.Stage != "" {
		sets = append(sets, "stage = ?")
		args = append(args, p.Stage)
	}
	if p.Progress != "" {
		sets = append(sets, "progress = ?")
		args = append(args, p.Progress)
	}
	if p.Result != "" {
		sets = append(sets, "result = ?")
		args = append(args, p.Result)
	}
	// 状态与原因必须一起写：上一轮 DNS 失败、这一轮成功时，若只写 status
	// 不写 error，界面上会出现「已更新」外加一条陈旧的失败原因。
	if p.DNSStatus != "" {
		sets = append(sets, "dns_status = ?", "dns_error = ?")
		args = append(args, p.DNSStatus, p.DNSError)
	}
	args = append(args, id)

	res, err := s.db.ExecContext(ctx,
		`UPDATE ops_tasks SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return fmt.Errorf("store: 记录任务进展失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrOpsTaskNotFound
	}
	return nil
}

// ResetRunningOpsTasks 把重启前处于 running 的任务重置为 pending。
//
// 进程被杀时正在执行的任务，数据库里停在 running，而那个 goroutine 已经
// 不存在了。不重置的话这些任务永远不会再被捞到——界面显示"运行中"，
// 实际上一动不动，而且没有任何错误可看。
//
// 返回被重置的条数，供启动日志使用。
func (s *Store) ResetRunningOpsTasks(ctx context.Context) (int64, error) {
	now := nowUnix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE ops_tasks
		SET state = ?, next_at = CASE WHEN next_at = 0 THEN ? ELSE next_at END, updated_at = ?
		WHERE state = ?`, OpsPending, now, now, OpsRunning)
	if err != nil {
		return 0, fmt.Errorf("store: 重置运行中任务失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteOpsTask 删除任务。只允许删终态任务——删一个正在跑的任务等于让它
// 变成孤儿：goroutine 还在推，行没了，进展无处可写。
func (s *Store) DeleteOpsTask(ctx context.Context, id string) error {
	task, err := s.GetOpsTask(ctx, id)
	if err != nil {
		return err
	}
	if !OpsTerminal(task.State) && task.State != OpsPaused && task.State != OpsFailed {
		return fmt.Errorf("%w: %s 状态的任务不能删除，请先取消", ErrOpsTaskIllegalTransition, task.State)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM ops_tasks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: 删除运维任务失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrOpsTaskNotFound
	}
	return nil
}
