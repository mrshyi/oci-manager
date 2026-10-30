package httpapi

import (
	"net/http"

	"ocicore/internal/accountsvc"
)

// handleGetPolicyTemplate 下发云侧策略模板。
//
// 只读、无副作用、不碰 OCI。这一阶段刻意没有后端逻辑——方案 ADR-2 的
// 结论是「不需要策略生成」，让面板持有 IAM 写权限去生成策略，
// 等于为了限制权限而先给一个更大的权限。
func (s *Server) handleGetPolicyTemplate(w http.ResponseWriter, r *http.Request) {
	// group 是可选的：不传就保留占位符，让用户自己替换。
	writeJSON(w, http.StatusOK, accountsvc.BuildPolicyTemplate(r.URL.Query().Get("group")))
}
