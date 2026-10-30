package logstream

import (
	"strings"
	"testing"
)

// 8 类模式的脱敏。每一条都断言两件事：该藏的被藏了，**原文不能残留**。
//
// 只断言"输出里有 ***"是不够的——原文可能还在后面某处。
func TestRedactCoversEightPatterns(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// leak 是绝不该出现在输出里的片段
		leak string
	}{
		{
			name: "1 PEM 私钥块",
			in: "启动失败 err=load key: -----BEGIN RSA PRIVATE KEY-----\n" +
				"MIIEowIBAAKCAQEA1234567890abcdef\n-----END RSA PRIVATE KEY-----",
			leak: "MIIEowIBAAKCAQEA1234567890abcdef",
		},
		{
			name: "2 privateKeyPem",
			in:   `{"privateKeyPem":"-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBg\n-----END PRIVATE KEY-----\n"}`,
			leak: "MIIEvQIBADANBg",
		},
		{
			name: "3 password",
			in:   "login attempt password=hunter2 from 10.0.0.1",
			leak: "hunter2",
		},
		{
			name: "4 token",
			in:   "刷新会话失败 token=abc123def456ghi789",
			leak: "abc123def456ghi789",
		},
		{
			name: "5 secret",
			in:   `{"clientSecret":"s3cr3t-value-here"}`,
			leak: "s3cr3t-value-here",
		},
		{
			name: "6 apiKey",
			in:   "上游返回 401 api_key=AKIAIOSFODNN7EXAMPLE",
			leak: "AKIAIOSFODNN7EXAMPLE",
		},
		{
			name: "7 Authorization 头",
			in:   "GET /v1 头 Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0",
			leak: "eyJhbGciOiJIUzI1NiJ9",
		},
		{
			name: "8 长十六进制串",
			in:   "主密钥 3f8a2c1d4e5b6a7f8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b 已加载",
			leak: "3f8a2c1d4e5b6a7f",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := Redact(c.in)
			if strings.Contains(out, c.leak) {
				t.Fatalf("原文残留：\n输入 %q\n输出 %q", c.in, out)
			}
			if !strings.Contains(out, mask) {
				t.Errorf("输出里没有脱敏标记：%q", out)
			}
		})
	}
}

// OCID 不是凭据，是排障时最需要看到的标识符，必须原样保留。
//
// 它的尾段常常整段都是十六进制字符，无差别替换会把每条带 OCID 的
// 日志糊成 ocid1.tenancy.oc1..***。
func TestRedactKeepsOCID(t *testing.T) {
	ocid := "ocid1.tenancy.oc1..aaaaaaaabbbbbbbbccccccccddddddddeeeeeeeeffffffff"
	in := "账号 " + ocid + " 校验失败"
	out := Redact(in)
	if out != in {
		t.Fatalf("OCID 被改动了：\n输入 %q\n输出 %q", in, out)
	}
}

func TestRedactKeepsFingerprint(t *testing.T) {
	in := "指纹 3f:8a:2c:1d:4e:5b:6a:7f:8c:9d:0e:1f:2a:3b:4c:5d"
	if out := Redact(in); out != in {
		t.Fatalf("指纹被改动了：%q", out)
	}
}

// 普通日志一个字都不该动。过度脱敏会让日志读不懂，而读不懂等于没有日志。
func TestRedactLeavesOrdinaryTextAlone(t *testing.T) {
	for _, in := range []string{
		"服务已启动 addr=127.0.0.1:8080 version=dev",
		"已清理过期会话 count=12",
		"同步实例完成 account=TYO instances=3 elapsed=1.2s",
		"",
	} {
		if out := Redact(in); out != in {
			t.Errorf("普通日志被改动了：\n输入 %q\n输出 %q", in, out)
		}
	}
}

// 代理地址里的密码：这类值不在 key=value 形式里，靠单独的规则处理。
func TestRedactProxyCredentials(t *testing.T) {
	in := "通过代理 http://user:p%40ssw0rd@10.0.0.2:3128 连接"
	out := Redact(in)
	if strings.Contains(out, "p%40ssw0rd") {
		t.Fatalf("代理密码残留：%q", out)
	}
	if !strings.Contains(out, "10.0.0.2:3128") {
		t.Errorf("代理主机不该被抹掉：%q", out)
	}
}

// 被引号包住的值脱敏后引号要留着。
//
// 否则 JSON 日志脱敏后就不是合法 JSON 了，而它本来是要被解析的。
func TestRedactKeepsQuotes(t *testing.T) {
	out := Redact(`{"password":"hunter2"}`)
	if out != `{"password":"***"}` {
		t.Fatalf("输出 = %q", out)
	}
}

// 太短的十六进制串不处理：颜色值、短 ID 都是这个形状，
// 抹掉它们只会让日志变得没法读。
func TestRedactIgnoresShortHex(t *testing.T) {
	in := "color=#a1b2c3 id=deadbeef"
	if out := Redact(in); out != in {
		t.Fatalf("短十六进制串被改动了：%q", out)
	}
}

// 属性级脱敏：键和值是分开的，键名必须参与判定。
//
// `slog.String("password", "hunter2")` 交给 Redact 时只剩 "hunter2"，
// 任何正则都认不出它是密码——所以有一条独立于字符串内容的规则。
func TestRedactValueUsesKey(t *testing.T) {
	cases := []struct {
		key, val string
		wantMask bool
	}{
		{"password", "hunter2", true},
		{"dbPassword", "hunter2", true},
		{"private_key_pem", "-----BEGIN...", true},
		{"clientSecret", "s3cr3t", true},
		{"csrfToken", "abc", true},
		{"apiKey", "AKIA...", true},
		{"Authorization", "Bearer x", true},
		{"credentials", "x", true},
		// 非敏感键：值原样保留（除非值自身命中字符串规则）。
		{"account", "TYO", false},
		{"count", "3", false},
		{"region", "ap-tokyo-1", false},
	}
	for _, c := range cases {
		got := RedactValue(c.key, c.val)
		if c.wantMask && got != mask {
			t.Errorf("RedactValue(%q, %q) = %q，期望 %q", c.key, c.val, got, mask)
		}
		if !c.wantMask && got != c.val {
			t.Errorf("RedactValue(%q, %q) = %q，不该改动", c.key, c.val, got)
		}
	}
}

// 非敏感键的值里如果自带凭据，仍然要被字符串规则抓到。
func TestRedactValueStillScansNonSensitiveValues(t *testing.T) {
	got := RedactValue("url", "http://u:pa55w0rd@10.0.0.2:3128")
	if strings.Contains(got, "pa55w0rd") {
		t.Fatalf("值里的密码残留：%q", got)
	}
}
