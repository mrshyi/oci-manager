package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// IPInfo 是一个 IP 的归属地缓存。
//
// Found 单独一列而不是靠「字段全空」推断：查不到和查到了但库里没有城市级
// 信息，在界面上该说不同的话。source 为空 + found=false 表示「查过，没有」，
// 这与「从来没查过」也是两件事——后者靠 updated_at 为零区分。
type IPInfo struct {
	IP          string  `json:"ip"`
	Found       bool    `json:"found"`
	Country     string  `json:"country"`
	CountryCode string  `json:"countryCode"`
	Region      string  `json:"region"`
	City        string  `json:"city"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
	ASN         string  `json:"asn"`
	Org         string  `json:"org"`
	// Source 记录这条是谁填的：mmdb（离线库）或 manual（用户手填）。
	Source    string    `json:"source"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// HasCoords 报告这条记录能不能画到图上。0,0 不算——那是「只知道国家」的占位。
func (i IPInfo) HasCoords() bool {
	return i.Found && !(i.Latitude == 0 && i.Longitude == 0)
}

// UpsertIPInfo 写入或更新一条归属地。
//
// 覆盖写而不是「有就跳过」：离线库每周更新，用户换库之后应当能看到新的结果。
// 唯一的例外是 manual 来源的记录——用户手填的地址不该被自动查询覆盖掉。
func (s *Store) UpsertIPInfo(ctx context.Context, info IPInfo) error {
	ip := strings.TrimSpace(info.IP)
	if ip == "" {
		return errors.New("store: IP 不能为空")
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ip_cache
			(ip, country, country_code, region, city, latitude, longitude,
			 asn, org, source, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(ip) DO UPDATE SET
			country      = excluded.country,
			country_code = excluded.country_code,
			region       = excluded.region,
			city         = excluded.city,
			latitude     = excluded.latitude,
			longitude    = excluded.longitude,
			asn          = excluded.asn,
			org          = excluded.org,
			source       = excluded.source,
			updated_at   = excluded.updated_at
		WHERE ip_cache.source != 'manual'`,
		ip, info.Country, info.CountryCode, info.Region, info.City,
		info.Latitude, info.Longitude, info.ASN, info.Org, info.Source, nowUnix())
	if err != nil {
		return fmt.Errorf("store: 写入 IP 归属地失败: %w", err)
	}
	return nil
}

// GetIPInfo 读一条。没有记录时返回 ErrNotFound。
func (s *Store) GetIPInfo(ctx context.Context, ip string) (*IPInfo, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT ip, country, country_code, region, city, latitude, longitude,
		       asn, org, source, updated_at
		FROM ip_cache WHERE ip = ?`, strings.TrimSpace(ip))
	i, err := scanIPInfo(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取 IP 归属地失败: %w", err)
	}
	return i, nil
}

// ListIPInfo 批量读，返回 ip -> 记录。
//
// 分批查而不是一次拼 500 个占位符：SQLite 的变量上限是可配的（默认 999），
// 一个装了几百台机器的面板很容易撞上，而撞上时的报错完全看不出是这个原因。
func (s *Store) ListIPInfo(ctx context.Context, ips []string) (map[string]IPInfo, error) {
	out := make(map[string]IPInfo, len(ips))
	const batch = 200

	for start := 0; start < len(ips); start += batch {
		end := start + batch
		if end > len(ips) {
			end = len(ips)
		}
		chunk := ips[start:end]
		args := make([]any, 0, len(chunk))
		marks := make([]string, 0, len(chunk))
		for _, ip := range chunk {
			ip = strings.TrimSpace(ip)
			if ip == "" {
				continue
			}
			args = append(args, ip)
			marks = append(marks, "?")
		}
		if len(args) == 0 {
			continue
		}

		rows, err := s.db.QueryContext(ctx, `
			SELECT ip, country, country_code, region, city, latitude, longitude,
			       asn, org, source, updated_at
			FROM ip_cache WHERE ip IN (`+strings.Join(marks, ",")+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("store: 批量读取 IP 归属地失败: %w", err)
		}
		for rows.Next() {
			i, err := scanIPInfo(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[i.IP] = *i
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SetIPInfoManual 手工填一条归属地。
//
// 存在的理由是：离线库对某些 IP（尤其是刚分配的地址段）查不到，而用户
// 可能恰好知道那台机器在哪。手填的条目不会被自动查询覆盖（见 UpsertIPInfo
// 的 WHERE 子句），否则下次刷新就白填了。
func (s *Store) SetIPInfoManual(ctx context.Context, info IPInfo) error {
	info.Source = "manual"
	return s.UpsertIPInfo(ctx, info)
}

// DeleteIPInfo 删掉一条缓存。用户手填错了要能撤。
func (s *Store) DeleteIPInfo(ctx context.Context, ip string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM ip_cache WHERE ip = ?`, strings.TrimSpace(ip))
	if err != nil {
		return fmt.Errorf("store: 删除 IP 归属地失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountIPInfo 返回缓存条数，供状态接口显示。
func (s *Store) CountIPInfo(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ip_cache`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: 统计 IP 归属地失败: %w", err)
	}
	return n, nil
}

func scanIPInfo(row rowScanner) (*IPInfo, error) {
	var (
		i         IPInfo
		updatedAt int64
	)
	err := row.Scan(&i.IP, &i.Country, &i.CountryCode, &i.Region, &i.City,
		&i.Latitude, &i.Longitude, &i.ASN, &i.Org, &i.Source, &updatedAt)
	if err != nil {
		return nil, err
	}
	// found 不落列：它有 source 或任何字段非空就为真。存一列布尔值会和
	// 实际内容脱节——手工清掉某个字段之后，found 还是 true。
	i.Found = i.Source != "" || i.Country != "" || i.City != "" || i.ASN != ""
	i.UpdatedAt = unixToTime(updatedAt)
	return &i, nil
}
