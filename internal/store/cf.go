package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CFZone 是一个 Cloudflare 域名区。
//
// 与 Proxy 同一条规矩：这个结构体会被直接序列化成 API 响应，所以
// **不含 token**。token 只在 CFZoneToken() 里解密一次，用完即弃。
type CFZone struct {
	ID string `json:"id"`
	// AccountID 是可选的归属账号，只用于列表筛选。
	AccountID string `json:"accountId"`
	ZoneID    string `json:"zoneId"`
	ZoneName  string `json:"zoneName"`
	// HasToken 只说明有没有配 token，不透露内容。
	HasToken bool `json:"hasToken"`
	Enabled  bool `json:"enabled"`

	LastSyncAt time.Time `json:"lastSyncAt"`
	LastError  string    `json:"lastError"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Display 是给人看的标识。域名取不到时退回 zone id。
func (z CFZone) Display() string {
	if strings.TrimSpace(z.ZoneName) != "" {
		return z.ZoneName
	}
	return z.ZoneID
}

// CFRecord 是一条「跟随某台实例」的 DNS 记录绑定。
type CFRecord struct {
	ID       string `json:"id"`
	ZonePK   string `json:"zonePk"`
	RecordID string `json:"recordId"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Proxied  bool   `json:"proxied"`
	TTL      int    `json:"ttl"`

	AccountID  string `json:"accountId"`
	InstanceID string `json:"instanceId"`

	LastIP     string    `json:"lastIp"`
	LastSyncAt time.Time `json:"lastSyncAt"`
	LastError  string    `json:"lastError"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// ZoneName 是列表查询顺带 join 出来的，方便界面直接显示。
	ZoneName string `json:"zoneName"`
}

const cfZoneColumns = `id, account_id, zone_id, zone_name,
	token_ciphertext, token_nonce, enabled,
	last_sync_at, last_error, created_at, updated_at`

// ErrCFZoneExists 表示同一个 zone 已经录过了。
var ErrCFZoneExists = errors.New("这个域名已经添加过了")

// CreateCFZone 落库一个 zone，token 加密存储（AAD 绑定本行 id）。
func (s *Store) CreateCFZone(ctx context.Context, z CFZone, token string) (*CFZone, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}

	var cipher, nonce []byte
	if token = strings.TrimSpace(token); token != "" {
		cipher, nonce, err = s.box.Seal([]byte(token), id)
		if err != nil {
			return nil, fmt.Errorf("store: 加密 Cloudflare token 失败: %w", err)
		}
	}

	now := nowUnix()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO cf_zones
			(id, account_id, zone_id, zone_name, token_ciphertext, token_nonce,
			 enabled, last_sync_at, last_error, created_at, updated_at)
		VALUES (?,?,?,?,?,?,1,0,'',?,?)`,
		id, strings.TrimSpace(z.AccountID), strings.TrimSpace(z.ZoneID),
		strings.TrimSpace(z.ZoneName), cipher, nonce, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, ErrCFZoneExists
		}
		return nil, fmt.Errorf("store: 写入 Cloudflare 域名失败: %w", err)
	}
	return s.GetCFZone(ctx, id)
}

// GetCFZone 按 id 取一条。
func (s *Store) GetCFZone(ctx context.Context, id string) (*CFZone, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+cfZoneColumns+` FROM cf_zones WHERE id = ?`, id)
	z, _, _, err := scanCFZone(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return z, nil
}

// ListCFZones 返回全部 zone，按域名排序，顺序稳定。
func (s *Store) ListCFZones(ctx context.Context) ([]CFZone, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+cfZoneColumns+` FROM cf_zones ORDER BY zone_name, zone_id`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询 Cloudflare 域名失败: %w", err)
	}
	defer rows.Close()

	out := make([]CFZone, 0)
	for rows.Next() {
		z, _, _, err := scanCFZone(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *z)
	}
	return out, rows.Err()
}

// CFZoneToken 解密并返回 API token。
//
// 这是 token 明文唯一出现的地方，返回值不该被记日志、不该进 API 响应。
func (s *Store) CFZoneToken(ctx context.Context, id string) (string, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+cfZoneColumns+` FROM cf_zones WHERE id = ?`, id)
	z, cipher, nonce, err := scanCFZone(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if len(cipher) == 0 {
		return "", nil
	}
	plain, err := s.box.Open(cipher, nonce, z.ID)
	if err != nil {
		return "", fmt.Errorf("store: 解密 Cloudflare token 失败（主密钥是否换过？）: %w", err)
	}
	return string(plain), nil
}

// CFZoneUpdate 描述可修改的字段，nil 表示不动。
type CFZoneUpdate struct {
	AccountID *string
	ZoneName  *string
	Enabled   *bool
	// Token 非 nil 时重设 token；空字符串表示清除。
	Token *string
}

// UpdateCFZone 修改一个 zone。
func (s *Store) UpdateCFZone(ctx context.Context, id string, up CFZoneUpdate) (*CFZone, error) {
	sets := []string{"updated_at = ?"}
	args := []any{nowUnix()}
	add := func(clause string, v any) {
		sets = append(sets, clause)
		args = append(args, v)
	}

	if up.AccountID != nil {
		add("account_id = ?", strings.TrimSpace(*up.AccountID))
	}
	if up.ZoneName != nil {
		add("zone_name = ?", strings.TrimSpace(*up.ZoneName))
	}
	if up.Enabled != nil {
		add("enabled = ?", boolToInt(*up.Enabled))
	}
	if up.Token != nil {
		if strings.TrimSpace(*up.Token) == "" {
			add("token_ciphertext = ?", nil)
			add("token_nonce = ?", nil)
		} else {
			cipher, nonce, err := s.box.Seal([]byte(strings.TrimSpace(*up.Token)), id)
			if err != nil {
				return nil, fmt.Errorf("store: 加密 Cloudflare token 失败: %w", err)
			}
			add("token_ciphertext = ?", cipher)
			add("token_nonce = ?", nonce)
		}
	}

	args = append(args, id)
	res, err := s.db.ExecContext(ctx,
		`UPDATE cf_zones SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 更新 Cloudflare 域名失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.GetCFZone(ctx, id)
}

// RecordCFZoneResult 写回一次与 Cloudflare 交互的结果。
//
// last_sync_at 只在成功时推进：那一列的含义是「最后一次成功的时刻」，
// 失败也推进的话，界面就没法区分「三天没同步」和「刚试过但失败了」。
func (s *Store) RecordCFZoneResult(ctx context.Context, id, lastErr string) error {
	now := nowUnix()
	sets := []string{"last_error = ?", "updated_at = ?"}
	args := []any{lastErr, now}
	if lastErr == "" {
		sets = append(sets, "last_sync_at = ?")
		args = append(args, now)
	}
	args = append(args, id)

	if _, err := s.db.ExecContext(ctx,
		`UPDATE cf_zones SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return fmt.Errorf("store: 写入 Cloudflare 同步结果失败: %w", err)
	}
	return nil
}

// DeleteCFZone 删除一个 zone，连同它下面的绑定。
//
// 与 DeleteProxy 不同，这里不拦「还有绑定」：绑定只是一行配置，删掉之后
// 重新添加一次即可；而留着一条没有 zone 的绑定，换 IP 时会每次都去查一个
// 不存在的 token，报一堆莫名其妙的错。
func (s *Store) DeleteCFZone(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM cf_records WHERE zone_pk = ?`, id); err != nil {
		return fmt.Errorf("store: 删除 Cloudflare 记录绑定失败: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM cf_zones WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: 删除 Cloudflare 域名失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交事务失败: %w", err)
	}
	return nil
}

/* ---------- 记录绑定 ---------- */

const cfRecordColumns = `r.id, r.zone_pk, r.record_id, r.name, r.type, r.proxied, r.ttl,
	r.account_id, r.instance_id, r.last_ip, r.last_sync_at, r.last_error,
	r.created_at, r.updated_at, COALESCE(z.zone_name, '')`

// ErrCFRecordExists 表示这条记录已经绑过了。
var ErrCFRecordExists = errors.New("这条记录已经绑定了")

// CreateCFRecord 落库一条绑定。
func (s *Store) CreateCFRecord(ctx context.Context, r CFRecord) (*CFRecord, error) {
	if strings.TrimSpace(r.ZonePK) == "" {
		return nil, errors.New("store: 绑定必须指定域名")
	}
	if strings.TrimSpace(r.RecordID) == "" {
		return nil, errors.New("store: 绑定必须指定记录")
	}
	// zone 必须存在，否则会留下一行永远查不到 token 的孤儿绑定。
	if _, err := s.GetCFZone(ctx, r.ZonePK); err != nil {
		return nil, err
	}

	id, err := newID()
	if err != nil {
		return nil, err
	}
	typ := strings.TrimSpace(r.Type)
	if typ == "" {
		typ = "A"
	}
	ttl := r.TTL
	if ttl <= 0 {
		ttl = 1
	}
	now := nowUnix()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO cf_records
			(id, zone_pk, record_id, name, type, proxied, ttl,
			 account_id, instance_id, last_ip, last_sync_at, last_error,
			 created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,'',0,'',?,?)`,
		id, r.ZonePK, strings.TrimSpace(r.RecordID), strings.TrimSpace(r.Name), typ,
		boolToInt(r.Proxied), ttl,
		strings.TrimSpace(r.AccountID), strings.TrimSpace(r.InstanceID), now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, ErrCFRecordExists
		}
		return nil, fmt.Errorf("store: 写入记录绑定失败: %w", err)
	}
	return s.GetCFRecord(ctx, id)
}

// GetCFRecord 按 id 取一条绑定。
func (s *Store) GetCFRecord(ctx context.Context, id string) (*CFRecord, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+cfRecordColumns+`
		FROM cf_records r LEFT JOIN cf_zones z ON z.id = r.zone_pk WHERE r.id = ?`, id)
	r, err := scanCFRecord(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return r, nil
}

// CFRecordFilter 是列表查询的过滤条件。零值表示不过滤。
type CFRecordFilter struct {
	ZonePK     string
	AccountID  string
	InstanceID string
}

// ListCFRecords 返回绑定列表，按域名与记录名排序。
func (s *Store) ListCFRecords(ctx context.Context, f CFRecordFilter) ([]CFRecord, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.ZonePK != "" {
		where = append(where, "r.zone_pk = ?")
		args = append(args, f.ZonePK)
	}
	if f.AccountID != "" {
		where = append(where, "r.account_id = ?")
		args = append(args, f.AccountID)
	}
	if f.InstanceID != "" {
		where = append(where, "r.instance_id = ?")
		args = append(args, f.InstanceID)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT `+cfRecordColumns+`
		FROM cf_records r LEFT JOIN cf_zones z ON z.id = r.zone_pk
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY COALESCE(z.zone_name,''), r.name`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询记录绑定失败: %w", err)
	}
	defer rows.Close()

	out := make([]CFRecord, 0)
	for rows.Next() {
		r, err := scanCFRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// CFRecordsForInstance 返回跟随某台实例的全部绑定。
//
// 换 IP 之后要同步的就是这一批。accountID 为空时按实例 id 匹配即可——
// 实例 id 本身已经全局唯一。
func (s *Store) CFRecordsForInstance(ctx context.Context, accountID, instanceID string) ([]CFRecord, error) {
	if strings.TrimSpace(instanceID) == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+cfRecordColumns+`
		FROM cf_records r
		JOIN cf_zones z ON z.id = r.zone_pk AND z.enabled = 1
		WHERE r.instance_id = ? AND (? = '' OR r.account_id = ? OR r.account_id = '')
		ORDER BY r.name`, instanceID, accountID, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询实例的记录绑定失败: %w", err)
	}
	defer rows.Close()

	out := make([]CFRecord, 0)
	for rows.Next() {
		r, err := scanCFRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// RecordCFRecordResult 写回一次同步的结果。
//
// ip 为空表示这次没拿到新地址（失败或未执行），此时保留上次成功写入的
// last_ip——把失败时的空值写进去，界面就会显示「当前指向空」，比不显示更糟。
func (s *Store) RecordCFRecordResult(ctx context.Context, id, ip, lastErr string) error {
	now := nowUnix()
	sets := []string{"last_error = ?", "updated_at = ?"}
	args := []any{lastErr, now}
	if ip != "" {
		sets = append(sets, "last_ip = ?")
		args = append(args, ip)
	}
	if lastErr == "" {
		sets = append(sets, "last_sync_at = ?")
		args = append(args, now)
	}
	args = append(args, id)

	if _, err := s.db.ExecContext(ctx,
		`UPDATE cf_records SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return fmt.Errorf("store: 写入记录同步结果失败: %w", err)
	}
	return nil
}

// DeleteCFRecord 删除一条绑定。只删本地配置，不动 Cloudflare 上的记录。
func (s *Store) DeleteCFRecord(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM cf_records WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: 删除记录绑定失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

/* ---------- 内部 ---------- */

func scanCFZone(row rowScanner) (*CFZone, []byte, []byte, error) {
	var (
		z                  CFZone
		accountID, zoneName sql.NullString
		cipher, nonce      []byte
		enabled            int
		lastSync, created  int64
		updated            int64
	)
	err := row.Scan(&z.ID, &accountID, &z.ZoneID, &zoneName,
		&cipher, &nonce, &enabled,
		&lastSync, &z.LastError, &created, &updated)
	if err != nil {
		return nil, nil, nil, err
	}
	z.AccountID = accountID.String
	z.ZoneName = zoneName.String
	z.HasToken = len(cipher) > 0
	z.Enabled = enabled == 1
	z.LastSyncAt = unixToTime(lastSync)
	z.CreatedAt = unixToTime(created)
	z.UpdatedAt = unixToTime(updated)
	return &z, cipher, nonce, nil
}

func scanCFRecord(row rowScanner) (*CFRecord, error) {
	var (
		r                          CFRecord
		name, accountID, instance  sql.NullString
		lastIP, zoneName           sql.NullString
		proxied                    int
		lastSync, created, updated int64
	)
	err := row.Scan(&r.ID, &r.ZonePK, &r.RecordID, &name, &r.Type, &proxied, &r.TTL,
		&accountID, &instance, &lastIP, &lastSync, &r.LastError,
		&created, &updated, &zoneName)
	if err != nil {
		return nil, err
	}
	r.Name = name.String
	r.AccountID = accountID.String
	r.InstanceID = instance.String
	r.LastIP = lastIP.String
	r.ZoneName = zoneName.String
	r.Proxied = proxied == 1
	r.LastSyncAt = unixToTime(lastSync)
	r.CreatedAt = unixToTime(created)
	r.UpdatedAt = unixToTime(updated)
	return &r, nil
}
