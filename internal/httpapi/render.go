package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"ocicore/internal/store"
)

// maxRequestBytes 限制请求体大小。表单里最大的字段是 PEM 私钥，几 KB 足够。
const maxRequestBytes = 256 << 10 // 256 KiB

// errorBody 是统一的错误响应格式。
//
// Code 是稳定的机器可读标识，Message 是给用户看的中文说明。
// 涉及 OCI 调用失败时额外带上 OciCode —— 设计稿要求原始错误码必须
// 原样可见、可复制，用户需要拿它去搜索。
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	OciCode string `json:"ociCode,omitempty"`
	Advice  string `json:"advice,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("写入响应失败", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Code: code, Message: message})
}

// writeStoreError 把持久层错误映射成合适的 HTTP 状态码。
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "记录不存在")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		slog.Error("数据库操作失败", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// decodeJSON 解析请求体。拒绝未知字段，让拼错的字段名立刻暴露而不是被静默忽略。
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		// 空 body 单独说清楚。io.EOF 的字面意思是"读到结尾"，套进
		// "请求内容格式有误: EOF" 之后完全看不出是"根本没发内容"——
		// 排查时会往 JSON 语法上想，而实际是前端漏传了参数。
		if errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "empty_body",
				"请求没有携带任何内容。这通常是前端漏传了参数，不是格式问题。")
			return false
		}
		writeError(w, http.StatusBadRequest, "bad_request", "请求内容格式有误: "+err.Error())
		return false
	}
	return true
}

// clientIP 提取客户端 IP。
//
// 只有当 TCP 来源落在 TrustedProxies 里时才采信 X-Forwarded-For —— 否则
// 任何人都能伪造该头来绕过登录失败限流。早先这里只判一个布尔开关，
// 打开就等于信任所有人：开关一旦为真，伪造者每次换一个 IP 就能无限次试密码，
// 而限流器看起来工作正常，只是永远不会命中。
func (s *Server) clientIP(r *http.Request) string {
	remote := hostOnly(r.RemoteAddr)
	if len(s.cfg.TrustedProxies) == 0 || !s.trustedProxy(remote) {
		return remote
	}

	// X-Forwarded-For 是「客户端声称的值, 各层代理逐跳追加的值」，越靠右
	// 越接近本服务。从右往左走，第一个不在可信列表里的就是真实来源：
	// 它左边的一切都只是上游的声称，可以随便写。
	hops := forwardedHops(r.Header.Values("X-Forwarded-For"))
	for i := len(hops) - 1; i >= 0; i-- {
		if !s.trustedProxy(hops[i]) {
			return hops[i]
		}
	}
	// 整条链都在可信网段内（例如客户端本身也走内网），取最左的起点。
	if len(hops) > 0 {
		return hops[0]
	}
	return remote
}

// trustedProxy 判断某个地址是否落在配置的可信代理网段内。
//
// 解析失败一律视为不可信：这是唯一安全的默认值——判错方向的代价是
// 一个伪造来源被当成真实 IP，而限流与审计都建立在它之上。
func (s *Server) trustedProxy(ip string) bool {
	addr, err := netip.ParseAddr(strings.Trim(ip, "[]"))
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// forwardedHops 展开全部 X-Forwarded-For 头并拆成逐跳地址。
//
// 无法解析成 IP 的段直接丢弃：留着它只会给伪造者一个免费的新限流桶
// （每个请求塞一段乱码，clientIP 就返回一个新字符串）。
func forwardedHops(values []string) []string {
	var out []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if _, err := netip.ParseAddr(strings.Trim(part, "[]")); err != nil {
				continue
			}
			out = append(out, part)
		}
	}
	return out
}

// hostOnly 去掉地址里的端口与 IPv6 方括号。
func hostOnly(addrport string) string {
	if host, _, err := net.SplitHostPort(addrport); err == nil {
		return host
	}
	// 没有端口的裸地址（例如 unix socket 或测试里直接赋的值）。
	return strings.Trim(addrport, "[]")
}

// isSecureRequest 判断连接是否是 HTTPS，用于决定 Cookie 的 Secure 标志。
func (s *Server) isSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	// 与 clientIP 同一条信任边界：只有来源本身可信时，它转述的协议才可信。
	if len(s.cfg.TrustedProxies) > 0 && s.trustedProxy(hostOnly(r.RemoteAddr)) {
		return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	}
	return false
}
