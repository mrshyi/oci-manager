package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AccountHealth 是一个账号最近一次校验的结论。
//
// 与 Account.Status 的区别：Status 只有 ok/error 两态，是"最后一次点校验按钮
// 的结果"；这里记录的是"为什么错、错了多久、是不是已经确认坏了"。健康度
// 需要区分"被限流了"和"密钥失效了"——前者等一会儿就好，后者必须人去控制台。
type AccountHealth struct {
	AccountID string `json:"accountId"`
	// LastOKAt 是最近一次校验成功的时刻。nil 表示从来没成功过。
	LastOKAt *time.Time `json:"lastOkAt"`
	// LastErrorClass 是最近一次失败的分类名，取 ociclient.Class.String()。
	// 成功时清空——留着它会让"刚恢复"和"正在坏"看起来一样。
	LastErrorClass string `json:"lastErrorClass"`
	// LastErrorAt 保留成功之前的最后一次失败时刻，是历史事实，成功不清空。
	LastErrorAt      *time.Time `json:"lastErrorAt"`
	LastErrorMessage string     `json:"lastErrorMessage"`
	// ConsecutiveFailures 是连续凭据类失败次数。
	ConsecutiveFailures int `json:"consecutiveFailures"`
	// Fatal 表示已确认凭据不可用（连续失败达到阈值）。
	Fatal     bool      `json:"fatal"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// GetAccountHealth 取回账号健康度。没有记录时返回零值而不是错误：
// "还没校验过"是正常状态，不是异常。
func (s *Store) GetAccountHealth(ctx context.Context, accountID string) (AccountHealth, error) {
	h, err := scanHealth(s.db.QueryRowContext(ctx, `
		SELECT account_id, last_ok_at, last_error_class, last_error_at,
		       last_error_message, consecutive_failures, fatal, updated_at
		  FROM account_health WHERE account_id = ?`, accountID))
	if errors.Is(err, sql.ErrNoRows) {
		return AccountHealth{AccountID: accountID}, nil
	}
	if err != nil {
		return AccountHealth{}, fmt.Errorf("store: 查询账号健康度失败: %w", err)
	}
	return h, nil
}

// AllAccountHealth 一次取回所有账号的健康度。
//
// 列表页每行都要画色块，逐账号查就是 N+1 次往返。这张表的行数与账号数相同，
// 全量读一次比按需查便宜得多。
func (s *Store) AllAccountHealth(ctx context.Context) (map[string]AccountHealth, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT account_id, last_ok_at, last_error_class, last_error_at,
		       last_error_message, consecutive_failures, fatal, updated_at
		  FROM account_health`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询账号健康度失败: %w", err)
	}
	defer rows.Close()

	out := make(map[string]AccountHealth)
	for rows.Next() {
		h, err := scanHealth(rows)
		if err != nil {
			return nil, err
		}
		out[h.AccountID] = h
	}
	return out, rows.Err()
}

// SaveAccountHealth 整体写入一个账号的健康度（upsert）。
//
// 整体写而不是增量写：健康度是"当前结论"，不是事件流。调用方算好最终状态
// 一次写进去，避免"读-改-写"在两次校验并发时互相覆盖。
func (s *Store) SaveAccountHealth(ctx context.Context, h AccountHealth) error {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM accounts WHERE id = ?`, h.AccountID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: 确认账号存在失败: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO account_health
		    (account_id, last_ok_at, last_error_class, last_error_at,
		     last_error_message, consecutive_failures, fatal, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET
		    last_ok_at           = excluded.last_ok_at,
		    last_error_class     = excluded.last_error_class,
		    last_error_at        = excluded.last_error_at,
		    last_error_message   = excluded.last_error_message,
		    consecutive_failures = excluded.consecutive_failures,
		    fatal                = excluded.fatal,
		    updated_at           = excluded.updated_at`,
		h.AccountID, nullableUnix(h.LastOKAt), h.LastErrorClass,
		nullableUnix(h.LastErrorAt), h.LastErrorMessage,
		h.ConsecutiveFailures, boolToInt(h.Fatal), nowUnix())
	if err != nil {
		return fmt.Errorf("store: 写入账号健康度失败: %w", err)
	}
	return nil
}

// ClearAccountHealth 删除一个账号的健康度记录。
//
// 用户手工"清除告警"时用：他可能已经在控制台确认过账号确实被停用了，
// 不想再看到那个红块。下一次校验会重新写一条记录。
func (s *Store) ClearAccountHealth(ctx context.Context, accountID string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM account_health WHERE account_id = ?`, accountID); err != nil {
		return fmt.Errorf("store: 清除账号健康度失败: %w", err)
	}
	return nil
}

func scanHealth(row rowScanner) (AccountHealth, error) {
	var (
		h               AccountHealth
		lastOK, lastErr sql.NullInt64
		fatal           int
		updated         int64
	)
	if err := row.Scan(&h.AccountID, &lastOK, &h.LastErrorClass, &lastErr,
		&h.LastErrorMessage, &h.ConsecutiveFailures, &fatal, &updated); err != nil {
		return AccountHealth{}, err
	}
	h.LastOKAt = nullUnixToTime(lastOK)
	h.LastErrorAt = nullUnixToTime(lastErr)
	h.Fatal = fatal != 0
	h.UpdatedAt = unixToTime(updated)
	return h, nil
}
