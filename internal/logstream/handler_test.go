package logstream

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// newTestLogger 返回一个「输出进 buffer、同时进 ring」的 logger。
func newTestLogger(t *testing.T, ring *Ring) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(NewHandler(inner, ring)), &buf
}

// 脱敏必须在进环之前完成——环里存的就是明文的话，
// 展示端再脱敏已经晚了。
func TestHandlerRedactsBeforeRing(t *testing.T) {
	r := NewRing(10)
	logger, out := newTestLogger(t, r)

	logger.Info("登录失败", "password", "hunter2", "user", "admin")

	entries := r.Snapshot(0)
	if len(entries) != 1 {
		t.Fatalf("环里应有 1 条，实际 %d", len(entries))
	}
	if strings.Contains(entries[0].Attrs["password"], "hunter2") {
		t.Errorf("环里存了明文密码：%q", entries[0].Attrs["password"])
	}
	if entries[0].Attrs["password"] != mask {
		t.Errorf("password = %q，期望 %q", entries[0].Attrs["password"], mask)
	}
	// 非敏感字段不该被动。
	if entries[0].Attrs["user"] != "admin" {
		t.Errorf("user = %q", entries[0].Attrs["user"])
	}
	// 标准输出那份同样要脱敏。
	if strings.Contains(out.String(), "hunter2") {
		t.Errorf("stdout 里残留明文：%s", out.String())
	}
}

// 消息体本身也可能带凭据（错误信息里拼了 URL 或 token）。
func TestHandlerRedactsMessage(t *testing.T) {
	r := NewRing(10)
	logger, out := newTestLogger(t, r)

	logger.Error("代理连接失败 url=http://u:pa55w0rd@10.0.0.2:3128")

	entries := r.Snapshot(0)
	if strings.Contains(entries[0].Msg, "pa55w0rd") {
		t.Errorf("消息里残留密码：%q", entries[0].Msg)
	}
	if strings.Contains(out.String(), "pa55w0rd") {
		t.Errorf("stdout 里残留密码：%s", out.String())
	}
}

// WithAttrs 的属性不能在输出里出现两遍。
//
// inner 已经通过 WithAttrs 拿到了它们，Handle 里再往 record 上塞一次
// 就会重复——日志里每个字段出现两次，看起来像是两个不同的值。
func TestHandlerDoesNotDuplicateWithAttrs(t *testing.T) {
	ring := NewRing(10)
	logger, out := newTestLogger(t, ring)

	logger.With("account", "TYO").Info("同步完成", "count", 3)

	if n := strings.Count(out.String(), "account=TYO"); n != 1 {
		t.Fatalf("account 出现 %d 次，期望 1 次：%s", n, out.String())
	}
	// 环里也只该有一份。
	attrs := ring.Snapshot(0)[0].Attrs
	if attrs["account"] != "TYO" || attrs["count"] != "3" {
		t.Errorf("attrs = %+v", attrs)
	}
	if len(attrs) != 2 {
		t.Errorf("应有 2 个属性，实际 %d: %+v", len(attrs), attrs)
	}
}

// 分组用点号摊平，且先加后加分组的层次要还原对。
func TestHandlerFlattensGroups(t *testing.T) {
	ring := NewRing(10)
	logger, _ := newTestLogger(t, ring)

	// 先 With 再 WithGroup：account 属于外层。
	logger.With("account", "TYO").WithGroup("db").Info("查询完成", "host", "10.0.0.1")

	attrs := ring.Snapshot(0)[0].Attrs
	if attrs["account"] != "TYO" {
		t.Errorf("account 应在根层，实际键有 %v", keysOf(attrs))
	}
	if attrs["db.host"] != "10.0.0.1" {
		t.Errorf("db.host = %q，键有 %v", attrs["db.host"], keysOf(attrs))
	}
}

// 嵌套 Group 里的凭据也要脱敏：slog.Group("creds", "password", x)
// 是很自然的写法，跳过它等于漏掉。
func TestHandlerRedactsNestedGroupAttr(t *testing.T) {
	r := NewRing(10)
	logger, out := newTestLogger(t, r)

	logger.Info("轮换", slog.Group("creds", "password", "hunter2", "user", "admin"))

	attrs := r.Snapshot(0)[0].Attrs
	if attrs["creds.password"] != mask {
		t.Errorf("creds.password = %q，期望 %q", attrs["creds.password"], mask)
	}
	if attrs["creds.user"] != "admin" {
		t.Errorf("creds.user = %q", attrs["creds.user"])
	}
	if strings.Contains(out.String(), "hunter2") {
		t.Errorf("stdout 里残留明文：%s", out.String())
	}
}

// 低于阈值的日志不该进环——环是给人看的，不是全量归档。
func TestHandlerRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	ring := NewRing(10)
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(NewHandler(inner, ring))

	logger.Debug("这条不该出现")
	logger.Info("这条应该出现")

	entries := ring.Snapshot(0)
	if len(entries) != 1 || entries[0].Msg != "这条应该出现" {
		t.Fatalf("环内容 = %+v", entries)
	}
}

// ring 为 nil 时只脱敏、不缓冲，不能 panic。
func TestHandlerWithoutRing(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewHandler(
		slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}), nil))

	logger.Info("ok", "token", "abc123")
	if strings.Contains(buf.String(), "abc123") {
		t.Errorf("stdout 里残留 token：%s", buf.String())
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
