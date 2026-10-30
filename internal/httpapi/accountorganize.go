package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"ocicore/internal/accountsvc"
	"ocicore/internal/store"
)

// ---- 标签 ----

// handleListTagUsage 返回所有标签及其使用次数，供筛选器与输入联想使用。
func (s *Server) handleListTagUsage(w http.ResponseWriter, r *http.Request) {
	usage, err := s.st.TagUsage(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": usage})
}

// handleGetAccountTags 返回某个账号的标签。
func (s *Server) handleGetAccountTags(w http.ResponseWriter, r *http.Request) {
	tags, err := s.st.AccountTags(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": tags})
}

// setTagsRequest 是标签的整体替换请求。
type setTagsRequest struct {
	Tags []string `json:"tags"`
}

// handleSetAccountTags 整体替换某个账号的标签。
//
// 用 PUT 而不是 PATCH：调用方给出完整的目标集合，服务端不猜「没提到的是
// 要删掉还是保持不动」。这样前端取消勾选一个标签不必额外发一个 DELETE，
// 也不会出现「两个页面同时改、后写的把前一个的改动吃掉」。
func (s *Server) handleSetAccountTags(w http.ResponseWriter, r *http.Request) {
	var req setTagsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tags, err := accountsvc.NormalizeTags(req.Tags)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_tag", err.Error())
		return
	}

	id := r.PathValue("id")
	if err := s.st.SetAccountTags(r.Context(), id, tags); err != nil {
		writeStoreError(w, err)
		return
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "account_tags", AccountID: id,
		Target: strings.Join(tags, ","), IP: s.clientIP(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{"tags": tags})
}

// ---- 附加信息 ----

// accountMetaRequest 是附加信息的整体替换请求。
//
// ExpiresAt / HuntAckAt 用指针：PUT 是整体替换，字段缺省等于清空，
// 所以「不设到期」和「没传这个字段」在这里是同一件事，都落成 NULL。
type accountMetaRequest struct {
	GroupName  string  `json:"groupName"`
	Note       string  `json:"note"`
	ExpiresAt  *string `json:"expiresAt"`
	SortWeight int     `json:"sortWeight"`
	HuntAckAt  *string `json:"huntAckAt"`
}

// handleGetAccountMeta 返回某个账号的附加信息。
func (s *Server) handleGetAccountMeta(w http.ResponseWriter, r *http.Request) {
	m, err := s.st.GetAccountMeta(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"meta": m})
}

// handleSetAccountMeta 整体替换某个账号的附加信息。
func (s *Server) handleSetAccountMeta(w http.ResponseWriter, r *http.Request) {
	var req accountMetaRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	expires, err := parseDayOrTime(req.ExpiresAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_time", "expiresAt 不是合法时间")
		return
	}
	ack, err := parseDayOrTime(req.HuntAckAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_time", "huntAckAt 不是合法时间")
		return
	}

	id := r.PathValue("id")
	m := store.AccountMeta{
		AccountID:  id,
		GroupName:  req.GroupName,
		Note:       req.Note,
		ExpiresAt:  expires,
		SortWeight: req.SortWeight,
		HuntAckAt:  ack,
	}
	if err := accountsvc.ValidateMeta(&m); err != nil {
		writeError(w, http.StatusBadRequest, "bad_meta", err.Error())
		return
	}
	if err := s.st.SetAccountMeta(r.Context(), m); err != nil {
		writeStoreError(w, err)
		return
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "account_meta", AccountID: id,
		Target: m.GroupName, IP: s.clientIP(r),
	})

	// 读回来再返回，让前端拿到落库后的真实值（updatedAt 等）。
	saved, err := s.st.GetAccountMeta(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"meta": saved})
}

// parseDayOrTime 解析两种时间写法：完整 RFC3339，或只有日期的 YYYY-MM-DD。
//
// 日期写法是必须支持的：前端用的是 <input type="date">，它只会给出
// "2026-11-30"。只认 RFC3339 的话，用户选完日期会收到一个 400。
func parseDayOrTime(raw *string) (*time.Time, error) {
	if raw == nil {
		return nil, nil
	}
	v := strings.TrimSpace(*raw)
	if v == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return &t, nil
		}
	}
	return nil, errors.New("无法解析时间")
}
