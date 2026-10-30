package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"ocicore/internal/config"
)

// prefix 是测试用的网段构造器，写错就直接让测试失败。
func prefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("网段 %q 无效: %v", s, err)
	}
	return p
}

// TestClientIPIgnoresForgedForwardedFor 锁住「按可信网段逐跳回溯」这条规则。
//
// X-Forwarded-For 的格式是「客户端声称的值, 各层代理逐跳追加的值」，越靠右
// 越接近本服务。旧实现只判一个布尔开关：打开就无条件取最后一段，于是
// 任何直连的客户端都能自选限流桶——每个请求换一个伪造 IP，登录爆破的
// 次数限制就形同虚设，审计日志里留下的也全是编的地址。
//
// 这个失效没有任何可见症状——限流器照常工作，只是永远不会命中。
func TestClientIPIgnoresForgedForwardedFor(t *testing.T) {
	// 只信任本机代理：模拟 nginx 与面板同机部署。
	local := &Server{cfg: config.Config{TrustedProxies: []netip.Prefix{
		prefix(t, "127.0.0.0/8"), prefix(t, "::1/128"),
	}}}
	plain := &Server{cfg: config.Config{}}
	// 代理在私有网络里，且下游还有一层。
	chained := &Server{cfg: config.Config{TrustedProxies: []netip.Prefix{
		prefix(t, "10.0.0.0/8"),
	}}}

	cases := []struct {
		name   string
		srv    *Server
		xff    string
		remote string
		want   string
	}{
		{
			// 攻击者伪造在前，代理把真实 IP 追加在后。必须取后者。
			name: "伪造值在前，代理追加的真实值在后",
			srv:  local, xff: "1.2.3.4, 203.0.113.9", remote: "127.0.0.1:5000",
			want: "203.0.113.9",
		},
		{
			// 直连客户端伪造 XFF：来源不是可信代理，整个头都不能看。
			name: "来源不可信时完全忽略该头",
			srv:  local, xff: "1.2.3.4", remote: "203.0.113.9:5000",
			want: "203.0.113.9",
		},
		{
			// 没配可信代理：行为必须和上面那条一样，不给任何例外。
			name: "未配置可信代理时忽略该头",
			srv:  plain, xff: "1.2.3.4", remote: "203.0.113.9:5000",
			want: "203.0.113.9",
		},
		{
			name: "只有一段时直接采信——那就是代理填的",
			srv:  local, xff: "203.0.113.9", remote: "127.0.0.1:5000",
			want: "203.0.113.9",
		},
		{
			// 两层代理：右起第一个不可信的才是真实来源，左边全是声称。
			name: "多层代理逐跳回溯到第一个不可信地址",
			srv:  chained, xff: "9.9.9.9, 203.0.113.9, 10.1.2.3", remote: "10.0.0.1:5000",
			want: "203.0.113.9",
		},
		{
			// 整条链都在可信网段里（例如客户端本身也走内网），取最左起点。
			name: "整条链都可信时取最左",
			srv:  chained, xff: "10.9.9.9, 10.1.2.3", remote: "10.0.0.1:5000",
			want: "10.9.9.9",
		},
		{
			// 乱码段不能变成一个新的限流桶。
			name: "非 IP 的段被丢弃",
			srv:  local, xff: "not-an-ip, 203.0.113.9", remote: "127.0.0.1:5000",
			want: "203.0.113.9",
		},
		{
			name: "IPv6 的 RemoteAddr 要去掉方括号与端口",
			srv:  plain, xff: "", remote: "[2001:db8::1]:5000",
			want: "2001:db8::1",
		},
		{
			name: "IPv6 客户端经 IPv6 回环代理",
			srv:  local, xff: "2001:db8::9", remote: "[::1]:5000",
			want: "2001:db8::9",
		},
		{
			// 没有 XFF 但来源可信：退回 TCP 来源，不能返回空串。
			name: "可信来源但没有 XFF 时用来源地址",
			srv:  local, xff: "", remote: "127.0.0.1:5000",
			want: "127.0.0.1",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = c.remote
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := c.srv.clientIP(r); got != c.want {
				t.Errorf("clientIP = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestClientIPReadsAllForwardedForHeaders 验证多个同名头都被展开。
//
// 代理追加跳数时既可能改写同一个头，也可能再加一个同名头。只看第一个
// 会漏掉后面那几跳，回溯就会停在中途，把代理地址当成客户端。
func TestClientIPReadsAllForwardedForHeaders(t *testing.T) {
	srv := &Server{cfg: config.Config{TrustedProxies: []netip.Prefix{
		prefix(t, "10.0.0.0/8"),
	}}}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:5000"
	r.Header.Add("X-Forwarded-For", "203.0.113.9, 10.1.2.3")
	r.Header.Add("X-Forwarded-For", "10.2.3.4")

	if got := srv.clientIP(r); got != "203.0.113.9" {
		t.Errorf("clientIP = %q，期望 203.0.113.9", got)
	}
}

// TestSecureRequestOnlyTrustsForwardedProtoFromProxies 验证 X-Forwarded-Proto
// 与 X-Forwarded-For 共用同一条信任边界。
//
// 只信来源可信托管的协议，否则任何人都能声称自己是 https，从而骗到一个
// 带 Secure 标志的 Cookie——虽然 Secure 本身无害，但这个头的可信度一旦
// 松动，同源的 X-Forwarded-For 也就没有理由再被当真。
func TestSecureRequestOnlyTrustsForwardedProtoFromProxies(t *testing.T) {
	srv := &Server{cfg: config.Config{TrustedProxies: []netip.Prefix{
		prefix(t, "127.0.0.0/8"),
	}}}

	fromProxy := httptest.NewRequest(http.MethodGet, "/", nil)
	fromProxy.RemoteAddr = "127.0.0.1:5000"
	fromProxy.Header.Set("X-Forwarded-Proto", "https")
	if !srv.isSecureRequest(fromProxy) {
		t.Error("可信代理声称 https 时应判定为安全请求")
	}

	direct := httptest.NewRequest(http.MethodGet, "/", nil)
	direct.RemoteAddr = "203.0.113.9:5000"
	direct.Header.Set("X-Forwarded-Proto", "https")
	if srv.isSecureRequest(direct) {
		t.Error("非可信来源声称 https 时不应采信")
	}
}
