package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RiskBudgetCounter 是调用预算的持久化实现，满足 riskguard.Counter。
//
// 抽成独立类型而不是直接给 Store 加 Add/Used 方法：Add 这个名字放在
// Store 上太泛，读代码时完全看不出在加什么。
type RiskBudgetCounter struct {
	st *Store
}

// NewRiskBudgetCounter 创建计数器。
func NewRiskBudgetCounter(st *Store) *RiskBudgetCounter {
	return &RiskBudgetCounter{st: st}
}

// Add 给某账号在指定小时窗口累加一次调用，返回累加后的次数。
//
// 用 UPSERT + RETURNING 一条语句完成「读改写」：本包的连接池被限制为
// 单连接（见 Open 里的 SetMaxOpenConns），先 SELECT 再 UPDATE 在并发下
// 会因为往返交错而丢计数，而丢计数正是这套机制最隐蔽的失效方式——
// 预算看起来合理，却永远用不满，降频永远不触发。
func (c *RiskBudgetCounter) Add(ctx context.Context, accountID string, windowStart int64) (int, error) {
	const q = `
INSERT INTO risk_budget (account_id, window_start, calls)
VALUES (?, ?, 1)
ON CONFLICT(account_id, window_start) DO UPDATE SET calls = calls + 1
RETURNING calls`

	var calls int
	err := c.st.db.QueryRowContext(ctx, q, accountID, windowStart).Scan(&calls)
	if err != nil {
		return 0, fmt.Errorf("store: 累加调用预算失败: %w", err)
	}
	return calls, nil
}

// Used 返回某账号在指定窗口已用的次数。没有记录视为 0。
func (c *RiskBudgetCounter) Used(ctx context.Context, accountID string, windowStart int64) (int, error) {
	const q = `SELECT calls FROM risk_budget WHERE account_id = ? AND window_start = ?`

	var calls int
	err := c.st.db.QueryRowContext(ctx, q, accountID, windowStart).Scan(&calls)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: 读取调用预算失败: %w", err)
	}
	return calls, nil
}

// PurgeRiskBudget 删除 windowStart 早于 before 的计数行。调用方应定期执行。
//
// 不清的话这张表每小时给每个账号长一行，一年下来是 8.7 万行/账号——
// 查询都带窗口条件所以不影响性能，但没有理由留着。
func (s *Store) PurgeRiskBudget(ctx context.Context, before int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM risk_budget WHERE window_start < ?`, before)
	if err != nil {
		return 0, fmt.Errorf("store: 清理调用预算失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
