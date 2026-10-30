package cf

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"ocicore/internal/store"
)

// 同步结果的状态。与 store.DNSStatus* 一一对应，这里再定义一份是为了
// 让 cf 包不被迫依赖 store 的那三个常量名（store 是持久层，不该决定
// 「同步结果有哪几种」这种业务词汇）。
const (
	StatusUpdated = "updated"
	StatusPartial = "partial"
	StatusFailed  = "failed"
)

// Result 是一次「把域名指向新 IP」的结果。
//
// Records 为 0 且 Status 为空表示这台实例没有绑定任何记录——不涉及，
// 调用方不该往任务上写任何 DNS 字段，否则每个换 IP 任务都会多出一行
// 「DNS：跳过」，纯噪音。
type Result struct {
	Status  string
	Records int
	Detail  string
}

// Involved 报告这次同步是否真的动过 DNS。
func (r Result) Involved() bool { return r.Status != "" }

// Deps 是构造依赖。
type Deps struct {
	Store *store.Store
	// NewClient 可替换，测试注入指向假服务端的客户端。
	NewClient func(token string) *Client
}

// Service 负责「换 IP 之后把域名指过去」。
type Service struct {
	st        *store.Store
	newClient func(string) *Client
}

// New 构造服务。
func New(d Deps) *Service {
	s := &Service{st: d.Store, newClient: d.NewClient}
	if s.newClient == nil {
		s.newClient = func(token string) *Client { return NewClient(token) }
	}
	return s
}

// UpdateForInstance 把跟随这台实例的全部 A 记录指向 ip。
//
// 返回的 error 永远为 nil——这是刻意的。这个方法被换 IP 任务的每一轮调用，
// 若它返回 error，调用方很容易顺手写成 `if err != nil { return failed(err) }`，
// 于是一次成功的换 IP 因为域名没更新上被判失败。DNS 的结果只通过 Result
// 表达，让「换 IP 成功、DNS 失败」这种组合在类型上就无法被混淆。
func (s *Service) UpdateForInstance(ctx context.Context, accountID, instanceID, ip string) Result {
	recs, err := s.st.CFRecordsForInstance(ctx, accountID, instanceID)
	if err != nil {
		slog.Warn("Cloudflare：查询实例的 DNS 绑定失败", "instance", instanceID, "err", err)
		return Result{Status: StatusFailed, Detail: "查询 DNS 绑定失败：" + err.Error()}
	}
	if len(recs) == 0 {
		return Result{}
	}
	if strings.TrimSpace(ip) == "" {
		return Result{Status: StatusFailed, Records: len(recs), Detail: "没有可写入的 IP 地址"}
	}
	return s.apply(ctx, recs, ip)
}

// SyncRecord 立刻把某一条绑定同步到 ip。供界面上的「立即同步」用。
func (s *Service) SyncRecord(ctx context.Context, recordPK, ip string) Result {
	rec, err := s.st.GetCFRecord(ctx, recordPK)
	if err != nil {
		return Result{Status: StatusFailed, Detail: "找不到这条绑定"}
	}
	return s.apply(ctx, []store.CFRecord{*rec}, ip)
}

// apply 逐条更新。返回汇总结果。
//
// 单条失败不影响其余记录：三条 A 记录里坏了一条（比如已被人在 Cloudflare
// 上删掉），另外两条仍然该指向新 IP。
func (s *Service) apply(ctx context.Context, recs []store.CFRecord, ip string) Result {
	var (
		okCount int
		failMsg []string
	)
	for i := range recs {
		rec := &recs[i]
		if err := s.applyOne(ctx, rec, ip); err != nil {
			msg := DescribeError(err)
			failMsg = append(failMsg, rec.Name+"："+msg)
			slog.Warn("Cloudflare：更新 A 记录失败",
				"record", rec.Name, "zone", rec.ZoneName, "err", err)
			if e := s.st.RecordCFRecordResult(ctx, rec.ID, "", msg); e != nil {
				slog.Warn("Cloudflare：写回失败原因失败", "record", rec.ID, "err", e)
			}
			continue
		}
		okCount++
		if e := s.st.RecordCFRecordResult(ctx, rec.ID, ip, ""); e != nil {
			slog.Warn("Cloudflare：写回同步结果失败", "record", rec.ID, "err", e)
		}
	}

	res := Result{Records: len(recs)}
	switch {
	case okCount == len(recs):
		res.Status = StatusUpdated
		res.Detail = fmt.Sprintf("%d 条记录已指向 %s", okCount, ip)
	case okCount == 0:
		res.Status = StatusFailed
		res.Detail = strings.Join(failMsg, "；")
	default:
		res.Status = StatusPartial
		res.Detail = fmt.Sprintf("%d 条已更新，%d 条失败：%s",
			okCount, len(recs)-okCount, strings.Join(failMsg, "；"))
	}
	return res
}

// applyOne 更新一条记录。
func (s *Service) applyOne(ctx context.Context, rec *store.CFRecord, ip string) error {
	zone, err := s.st.GetCFZone(ctx, rec.ZonePK)
	if err != nil {
		return fmt.Errorf("读取域名配置失败: %w", err)
	}
	if !zone.Enabled {
		return errors.New("这个域名已被停用")
	}
	token, err := s.st.CFZoneToken(ctx, rec.ZonePK)
	if err != nil {
		return err
	}
	if token == "" {
		return ErrNoToken
	}

	c := s.newClient(token)
	_, err = c.UpdateRecord(ctx, zone.ZoneID, rec.RecordID, RecordUpdate{
		Type:    rec.Type,
		Name:    rec.Name,
		Content: ip,
		TTL:     TTLFor(rec.TTL),
		Proxied: rec.Proxied,
	})
	if err != nil {
		return err
	}
	// zone 层面的成功时间也顺带推进一次，界面上「上次同步」才不会一直空着。
	if e := s.st.RecordCFZoneResult(ctx, zone.ID, ""); e != nil {
		slog.Warn("Cloudflare：写回域名同步时间失败", "zone", zone.ID, "err", e)
	}
	return nil
}

// ResolveZone 用给定 token 解析出一个 zone，供「添加域名」这一步使用。
//
// 走 Service 的客户端工厂而不是在 HTTP 层直接 NewClient：客户端构造逻辑
// 只该有一处，否则测试要指到假服务端时就得到处补参数。
func (s *Service) ResolveZone(ctx context.Context, token, raw string) (*Zone, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrNoToken
	}
	return s.newClient(token).ResolveZone(ctx, raw)
}

// VerifyZone 验证 token 能访问这个 zone，并把域名回填到本地。
//
// 用户填的往往只是一串 zone id，域名要问 Cloudflare 才知道。顺手验证
// 比让用户自己去控制台核对更省事，也能在 token 权限不够时立刻报出来。
func (s *Service) VerifyZone(ctx context.Context, zonePK string) (*store.CFZone, error) {
	zone, err := s.st.GetCFZone(ctx, zonePK)
	if err != nil {
		return nil, err
	}
	token, err := s.st.CFZoneToken(ctx, zonePK)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, ErrNoToken
	}

	c := s.newClient(token)
	remote, err := c.GetZone(ctx, zone.ZoneID)
	if err != nil {
		msg := DescribeError(err)
		if e := s.st.RecordCFZoneResult(ctx, zonePK, msg); e != nil {
			slog.Warn("Cloudflare：写回验证失败原因失败", "zone", zonePK, "err", e)
		}
		return nil, errors.New(msg)
	}
	if e := s.st.RecordCFZoneResult(ctx, zonePK, ""); e != nil {
		slog.Warn("Cloudflare：写回验证结果失败", "zone", zonePK, "err", e)
	}
	name := remote.Name
	if name != "" && name != zone.ZoneName {
		if _, err := s.st.UpdateCFZone(ctx, zonePK, store.CFZoneUpdate{ZoneName: &name}); err != nil {
			slog.Warn("Cloudflare：回填域名失败", "zone", zonePK, "err", err)
		}
	}
	return s.st.GetCFZone(ctx, zonePK)
}

// RemoteRecord 是从 Cloudflare 拉回来的记录，附带「是否已绑定」标记。
type RemoteRecord struct {
	Record
	Bound bool `json:"bound"`
}

// ListRemoteRecords 拉取 zone 下的 A 记录，并标出哪些已经绑定。
func (s *Service) ListRemoteRecords(ctx context.Context, zonePK string) ([]RemoteRecord, error) {
	zone, err := s.st.GetCFZone(ctx, zonePK)
	if err != nil {
		return nil, err
	}
	token, err := s.st.CFZoneToken(ctx, zonePK)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, ErrNoToken
	}

	c := s.newClient(token)
	recs, err := c.ListRecords(ctx, zone.ZoneID, "A")
	if err != nil {
		return nil, errors.New(DescribeError(err))
	}

	bound, err := s.st.ListCFRecords(ctx, store.CFRecordFilter{ZonePK: zonePK})
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(bound))
	for _, b := range bound {
		seen[b.RecordID] = true
	}

	out := make([]RemoteRecord, 0, len(recs))
	for _, r := range recs {
		out = append(out, RemoteRecord{Record: r, Bound: seen[r.ID]})
	}
	return out, nil
}
