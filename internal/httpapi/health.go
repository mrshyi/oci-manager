package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"ocicore/internal/accountsvc"
	"ocicore/internal/store"
)

// applyAccountHealth 把一次校验的结论并进账号健康度。
//
// 写失败只记日志：健康度是"锦上添花"的信息，它坏掉不该让一次成功的
// 校验变成失败。账号状态（accounts.status）已经单独写过了，那才是
// 其它流程依赖的东西。
func (s *Server) applyAccountHealth(ctx context.Context, accountID string, res accountsvc.Result) {
	prev, err := s.st.GetAccountHealth(ctx, accountID)
	if err != nil {
		slog.Warn("读取账号健康度失败", "account", accountID, "err", err)
		return
	}
	next := accountsvc.Next(prev, accountsvc.EventFromResult(res), time.Now())
	if err := s.st.SaveAccountHealth(ctx, next); err != nil {
		slog.Warn("写入账号健康度失败", "account", accountID, "err", err)
	}
}

// handleGetAccountHealth 返回单个账号的健康度。
//
// 列表接口已经带上了每个账号的 health，这个接口是给"点进某个账号后
// 想刷新一下色块"用的——不必为了一个色块重拉整张列表。
func (s *Server) handleGetAccountHealth(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// 先确认账号存在，否则不存在的账号会返回一个"尚未校验"的绿色块，
	// 前端会以为账号是好的。
	if _, err := s.st.GetAccount(r.Context(), id); err != nil {
		writeStoreError(w, err)
		return
	}
	h, err := s.st.GetAccountHealth(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, accountsvc.HealthView(h))
}

// handleClearAccountHealth 清除一个账号的健康度记录，红块随之消失。
//
// 不清 accounts.status：那一条代表"最后一次校验的原始结论"，是审计意义上的
// 事实。这里清掉的只是"我们据此推断出的告警等级"。用户点了这个按钮，
// 下次自动校验若仍然失败，红块会再出现——这是对的，不能变成永久静音。
func (s *Server) handleClearAccountHealth(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	acc, err := s.st.GetAccount(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.st.ClearAccountHealth(r.Context(), id); err != nil {
		writeStoreError(w, err)
		return
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "account_health_clear", AccountID: id,
		Target: acc.Alias, IP: s.clientIP(r),
	})

	h, err := s.st.GetAccountHealth(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, accountsvc.HealthView(h))
}
