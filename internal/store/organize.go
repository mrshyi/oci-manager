package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AccountMeta 是账号的附加信息：分组、备注、到期时间、排序权重。
//
// 与 Account 分开：这些字段都可选，且大多数账号不会填。塞进主表会让
// accounts 的列继续膨胀，而每次列表查询都要为几个通常为空的列付代价。
type AccountMeta struct {
	AccountID string `json:"accountId"`
	// GroupName 为空表示未分组，前端把它归到最后一组。
	GroupName string `json:"groupName"`
	Note      string `json:"note"`
	// ExpiresAt 是账号到期时间，nil 表示不设提醒。
	ExpiresAt *time.Time `json:"expiresAt"`
	// SortWeight 大的排在前面。全为 0 时退回创建时间顺序。
	SortWeight int `json:"sortWeight"`
	// HuntAckAt 预留给抢机页的「已知悉」标记，当前只做存取。
	HuntAckAt *time.Time `json:"huntAckAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

// TagUsage 是一个标签及其被多少个账号使用。
type TagUsage struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// AccountTags 返回某个账号的标签，按字母序。
func (s *Store) AccountTags(ctx context.Context, accountID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tag FROM account_tags WHERE account_id = ? ORDER BY tag ASC`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询账号标签失败: %w", err)
	}
	defer rows.Close()

	tags := make([]string, 0, 4)
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		tags = append(tags, t)
	}
	return tags, rows.Err()
}

// SetAccountTags 整体替换某个账号的标签。
//
// 语义是 PUT 而不是 PATCH：调用方给出完整的目标集合，服务端不猜「没提到的
// 是要删掉还是保持不动」。这样前端取消勾选一个标签不需要额外发一个 DELETE。
func (s *Store) SetAccountTags(ctx context.Context, accountID string, tags []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开始事务失败: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // 提交成功后回滚是空操作

	// 先确认账号存在。不查的话，外键失败会报成一句 "FOREIGN KEY constraint
	// failed"，接口层只能翻译成 500，而实际上这是 404。
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM accounts WHERE id = ?`, accountID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: 确认账号存在失败: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM account_tags WHERE account_id = ?`, accountID); err != nil {
		return fmt.Errorf("store: 清除旧标签失败: %w", err)
	}
	now := nowUnix()
	for _, tag := range tags {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO account_tags (account_id, tag, created_at) VALUES (?, ?, ?)`,
			accountID, tag, now); err != nil {
			return fmt.Errorf("store: 写入标签失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交标签失败: %w", err)
	}
	return nil
}

// AllAccountTags 一次取回所有账号的标签。
//
// 列表页要给每一行画标签，按账号逐个查会变成 N+1 次查询；账号一多，
// 打开页面就是几十次往返。
func (s *Store) AllAccountTags(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT account_id, tag FROM account_tags ORDER BY account_id, tag ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询标签失败: %w", err)
	}
	defer rows.Close()

	out := make(map[string][]string)
	for rows.Next() {
		var id, tag string
		if err := rows.Scan(&id, &tag); err != nil {
			return nil, err
		}
		out[id] = append(out[id], tag)
	}
	return out, rows.Err()
}

// TagUsage 统计每个标签被多少个账号使用，用于筛选器与输入联想。
//
// 按使用次数降序：用户最可能点的标签排在最前面。
func (s *Store) TagUsage(ctx context.Context) ([]TagUsage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tag, COUNT(*) AS n FROM account_tags GROUP BY tag ORDER BY n DESC, tag ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: 统计标签失败: %w", err)
	}
	defer rows.Close()

	out := make([]TagUsage, 0, 8)
	for rows.Next() {
		var u TagUsage
		if err := rows.Scan(&u.Tag, &u.Count); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetAccountMeta 取回账号附加信息。没有记录时返回零值而不是错误：
// 「没填过」是正常状态，不是异常。
func (s *Store) GetAccountMeta(ctx context.Context, accountID string) (AccountMeta, error) {
	var (
		m       AccountMeta
		expires sql.NullInt64
		ack     sql.NullInt64
		updated int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT account_id, group_name, note, expires_at, sort_weight, hunt_ack_at, updated_at
		   FROM account_meta WHERE account_id = ?`, accountID).
		Scan(&m.AccountID, &m.GroupName, &m.Note, &expires, &m.SortWeight, &ack, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountMeta{AccountID: accountID}, nil
	}
	if err != nil {
		return m, fmt.Errorf("store: 查询账号信息失败: %w", err)
	}
	m.ExpiresAt = nullUnixToTime(expires)
	m.HuntAckAt = nullUnixToTime(ack)
	m.UpdatedAt = unixToTime(updated)
	return m, nil
}

// SetAccountMeta 写入账号附加信息（upsert）。
func (s *Store) SetAccountMeta(ctx context.Context, m AccountMeta) error {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM accounts WHERE id = ?`, m.AccountID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: 确认账号存在失败: %w", err)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO account_meta
		     (account_id, group_name, note, expires_at, sort_weight, hunt_ack_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(account_id) DO UPDATE SET
		     group_name  = excluded.group_name,
		     note        = excluded.note,
		     expires_at  = excluded.expires_at,
		     sort_weight = excluded.sort_weight,
		     hunt_ack_at = excluded.hunt_ack_at,
		     updated_at  = excluded.updated_at`,
		m.AccountID, m.GroupName, m.Note,
		nullableUnix(m.ExpiresAt), m.SortWeight, nullableUnix(m.HuntAckAt), nowUnix())
	if err != nil {
		return fmt.Errorf("store: 写入账号信息失败: %w", err)
	}
	return nil
}

// AllAccountMeta 一次取回所有账号的附加信息。
func (s *Store) AllAccountMeta(ctx context.Context) (map[string]AccountMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT account_id, group_name, note, expires_at, sort_weight, hunt_ack_at, updated_at
		   FROM account_meta`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询账号信息失败: %w", err)
	}
	defer rows.Close()

	out := make(map[string]AccountMeta)
	for rows.Next() {
		var (
			m       AccountMeta
			expires sql.NullInt64
			ack     sql.NullInt64
			updated int64
		)
		if err := rows.Scan(&m.AccountID, &m.GroupName, &m.Note,
			&expires, &m.SortWeight, &ack, &updated); err != nil {
			return nil, err
		}
		m.ExpiresAt = nullUnixToTime(expires)
		m.HuntAckAt = nullUnixToTime(ack)
		m.UpdatedAt = unixToTime(updated)
		out[m.AccountID] = m
	}
	return out, rows.Err()
}

// nullableUnix 把可空时间转成可写进 SQL 的值。nil 存 NULL，而不是 0——
// 0 会被 unixToTime 当成"零值时间"，与"没设置"混在一起就分不开了。
func nullableUnix(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.Unix()
}

// NormalizeTag 是标签的唯一规范形式：去掉首尾空白。
//
// 只做这一件事，不做大小写折叠：「东京」「JP」「日本」都可能是用户想要的
// 标签，强行统一大小写反而会让 "JP" 和 "jp" 变成同一个，而用户可能就是
// 想区分它们。去空白是为了让 "东京 " 和 "东京" 不要变成两个标签。
func NormalizeTag(raw string) string {
	return strings.TrimSpace(raw)
}
