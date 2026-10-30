package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"ocicore/internal/accountsvc"
	"ocicore/internal/store"
)

// handleGetCapabilityProbe 返回上次权限自检的结果。
//
// 没自检过时返回 items 为 null 而不是 404：这不是"资源不存在"，
// 而是"还没查过"，前端据此显示"尚未自检"并给一个按钮。
func (s *Server) handleGetCapabilityProbe(w http.ResponseWriter, r *http.Request) {
	acc, err := s.st.GetAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	probe := accountsvc.DecodeCapabilityProbe(acc.CapabilityProbe)
	if probe == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"probe":   nil,
			"notice":  "这个账号还没有做过权限自检。",
			"specs":   capabilitySpecsForUI(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"probe": probe})
}

// handleRunCapabilityProbe 立刻跑一次权限自检。
//
// 这是只读操作：五个接口全是 list / summarize，不改任何资源。所以只走
// guard 不走 danger——要求 TOTP 会让用户为了看一眼权限而去翻手机。
func (s *Server) handleRunCapabilityProbe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	acc, err := s.st.GetAccount(ctx, r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !acc.Enabled {
		// 停用的账号不该被拿去调 API：用户停用它可能正是因为不想再产生
		// 任何流量。想看权限就先启用。
		writeError(w, http.StatusBadRequest, "account_disabled",
			"该账号已停用，启用后才能做权限自检。")
		return
	}

	client, err := s.conns.For(ctx, acc)
	if err != nil {
		// 连客户端都建不出来（私钥解不开），五族全都没法测。
		// 直接报错而不是返回一个全是 false 的结果——后者看起来像"缺权限"，
		// 会把人引向错误的方向。
		writeError(w, http.StatusBadGateway, "credentials", err.Error())
		return
	}

	// 单次自检最多五个请求，给足时间但不要挂太久。
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	probe := accountsvc.ProbeCapabilities(ctx, client, accountsvc.CapabilityOptions{
		Region:        acc.DefaultRegion,
		CompartmentID: acc.CompartmentOCID,
		TenancyID:     acc.TenancyOCID,
	})
	probe.AccountID = acc.ID

	if raw := accountsvc.EncodeCapabilityProbe(probe); raw != "" {
		if err := s.st.SetAccountCapabilityProbe(ctx, acc.ID, raw); err != nil {
			// 存不下不影响这次结果：用户当场就能看到，下次自检会重写。
			slog.Warn("保存权限自检结果失败", "account", acc.ID, "err", err)
		}
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "account_capability_probe", AccountID: acc.ID,
		Target: acc.Alias, IP: s.clientIP(r),
		Detail: probe.Summary,
	})

	writeJSON(w, http.StatusOK, map[string]any{"probe": probe})
}

// capabilitySpecsForUI 把五族的静态描述下发给前端。
//
// 前端不该自己维护一份「有哪五族、缺权限该加哪条策略」的清单：
// 后端加了第六族，前端不跟着改就会少显示一项，而且没有任何报错。
func capabilitySpecsForUI() []map[string]string {
	out := make([]map[string]string, 0, 5)
	for _, spec := range accountsvc.CapabilitySpecs() {
		out = append(out, map[string]string{
			"key": spec.Key, "label": spec.Label, "policy": spec.Policy,
		})
	}
	return out
}
