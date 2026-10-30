package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"ocicore/internal/cf"
	"ocicore/internal/store"
)

// cfTimeout 是 Cloudflare 相关接口的超时。
//
// 与换 IP 任务里的同步共用同一个量级：这些接口都是「点一下等一下」的
// 交互，卡太久不如直接报错让用户重试。
const cfTimeout = 20 * time.Second

// cfNotice 是面板上必须常驻的一句话。
//
// 它解释的是「为什么这里只有一个 token 输入框，而不是账号密码」——用户
// 会自然地把「Cloudflare 联动」理解成「登录我的 Cloudflare 账号」，而
// 那意味着要把账号密码交给一个自托管面板，风险远大于收益。
const cfNotice = "只接受 API Token，不接受账号密码。Token 请在 Cloudflare 后台" +
	"「我的个人资料 → API 令牌」里创建，权限只需 Zone.DNS 编辑 + Zone 读取，" +
	"并限定到具体域名。面板里存的是加密后的密文，界面上不会回显。" +
	"换 IP 任务会顺带把绑定的 A 记录指向新地址——DNS 更新失败不会让换 IP 判失败，" +
	"两者在任务详情里分开显示。"

func (s *Server) handleListCFZones(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	zones, err := s.st.ListCFZones(ctx)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	records, err := s.st.ListCFRecords(ctx, store.CFRecordFilter{})
	if err != nil {
		writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"zones":   zones,
		"records": records,
		"notice":  cfNotice,
	})
}

// cfZoneCreateRequest 是新增域名的入参。
type cfZoneCreateRequest struct {
	AccountID string `json:"accountId"`
	// Zone 可以是 zone id、域名，或直接粘控制台地址。
	Zone  string `json:"zone"`
	Token string `json:"token"`
}

// handleCreateCFZone 添加一个域名。
//
// 添加时立刻验证一次 token：不验的话，用户要等到第一次换 IP 之后才发现
// token 填错了，而那时候机器已经换完 IP、域名还指着旧地址。
func (s *Server) handleCreateCFZone(w http.ResponseWriter, r *http.Request) {
	var req cfZoneCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Zone) == "" {
		writeError(w, http.StatusBadRequest, "invalid_zone", "请填域名或 zone id")
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		writeError(w, http.StatusBadRequest, "invalid_token", "请填 API Token")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), cfTimeout)
	defer cancel()

	// 先用 token 解析出 zone，拿到真正的 zone id 与域名。
	zone, err := s.cf.ResolveZone(ctx, req.Token, req.Zone)
	if err != nil {
		writeError(w, http.StatusBadRequest, "cf_zone_unresolved", cf.DescribeError(err))
		return
	}

	created, err := s.st.CreateCFZone(ctx, store.CFZone{
		AccountID: req.AccountID,
		ZoneID:    zone.ID,
		ZoneName:  zone.Name,
	}, req.Token)
	if err != nil {
		if errors.Is(err, store.ErrCFZoneExists) {
			writeError(w, http.StatusConflict, "cf_zone_exists", err.Error())
			return
		}
		writeStoreError(w, err)
		return
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(ctx, store.AuditEntry{
		UserID: user.ID, Action: "cf_zone_create", AccountID: req.AccountID,
		Target: created.ZoneName, IP: s.clientIP(r),
		Detail: "zone " + created.ZoneID,
	})

	writeJSON(w, http.StatusOK, map[string]any{"zone": created})
}

// cfZoneUpdateRequest 修改域名配置。Token 非 nil 时重设，空串表示清除。
type cfZoneUpdateRequest struct {
	AccountID *string `json:"accountId"`
	Enabled   *bool   `json:"enabled"`
	Token     *string `json:"token"`
}

func (s *Server) handleUpdateCFZone(w http.ResponseWriter, r *http.Request) {
	var req cfZoneUpdateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	zone, err := s.st.UpdateCFZone(r.Context(), r.PathValue("id"), store.CFZoneUpdate{
		AccountID: req.AccountID, Enabled: req.Enabled, Token: req.Token,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"zone": zone})
}

func (s *Server) handleDeleteCFZone(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	zone, err := s.st.GetCFZone(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.st.DeleteCFZone(r.Context(), id); err != nil {
		writeStoreError(w, err)
		return
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "cf_zone_delete", Target: zone.Display(),
		IP: s.clientIP(r),
	})

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleVerifyCFZone 重新验证 token 并回填域名。
func (s *Server) handleVerifyCFZone(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), cfTimeout)
	defer cancel()

	zone, err := s.cf.VerifyZone(ctx, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cf_verify_failed", cf.DescribeError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"zone": zone})
}

// handleListCFRemoteRecords 拉取该域名下 Cloudflare 侧的 A 记录，供绑定选择。
func (s *Server) handleListCFRemoteRecords(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), cfTimeout)
	defer cancel()

	records, err := s.cf.ListRemoteRecords(ctx, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cf_list_failed", cf.DescribeError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": records})
}

// cfRecordCreateRequest 新增一条「记录跟随实例」的绑定。
type cfRecordCreateRequest struct {
	ZonePK     string `json:"zonePk"`
	RecordID   string `json:"recordId"`
	Name       string `json:"name"`
	TTL        int    `json:"ttl"`
	Proxied    bool   `json:"proxied"`
	AccountID  string `json:"accountId"`
	InstanceID string `json:"instanceId"`
}

func (s *Server) handleCreateCFRecord(w http.ResponseWriter, r *http.Request) {
	var req cfRecordCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.InstanceID) == "" {
		writeError(w, http.StatusBadRequest, "invalid_instance", "请选择要跟随的实例")
		return
	}
	if _, err := s.st.GetInstance(r.Context(), req.InstanceID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_instance", "找不到这台实例")
		return
	}

	rec, err := s.st.CreateCFRecord(r.Context(), store.CFRecord{
		ZonePK: req.ZonePK, RecordID: req.RecordID, Name: req.Name,
		Type: "A", TTL: req.TTL, Proxied: req.Proxied,
		AccountID: req.AccountID, InstanceID: req.InstanceID,
	})
	if err != nil {
		if errors.Is(err, store.ErrCFRecordExists) {
			writeError(w, http.StatusConflict, "cf_record_exists", err.Error())
			return
		}
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"record": rec})
}

func (s *Server) handleDeleteCFRecord(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteCFRecord(r.Context(), r.PathValue("id")); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSyncCFRecord 立刻把某条记录同步到实例当前公网 IP。
//
// 用途是「绑定之前先确认这条链路通」：用户刚填完 token 与绑定，不换 IP
// 也能验证一次，不必等到下一次换 IP 才发现配错了。
func (s *Server) handleSyncCFRecord(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), cfTimeout)
	defer cancel()

	rec, err := s.st.GetCFRecord(ctx, r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	inst, err := s.st.GetInstance(ctx, rec.InstanceID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "instance_missing",
			"这条绑定指向的实例已经不在面板里了，请重新选择实例")
		return
	}
	if strings.TrimSpace(inst.PublicIP) == "" {
		writeError(w, http.StatusBadRequest, "no_public_ip",
			"这台实例当前没有公网 IP，无法同步；请先分配公网 IP")
		return
	}

	res := s.cf.SyncRecord(ctx, rec.ID, inst.PublicIP)
	// 同步失败是用户当场要看到的结果，用 200 返回并带上结果，
	// 前端才能同时显示「IP 是多少」和「为什么没写上去」。
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  res.Status,
		"detail":  res.Detail,
		"records": res.Records,
		"ip":      inst.PublicIP,
	})
}
