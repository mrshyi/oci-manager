package httpapi

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"ocicore/internal/logstream"
)

// openLogStream 建立一条日志流连接并返回按行读取器。
func openLogStream(t *testing.T, h *harness) (*http.Response, *bufio.Reader) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/api/logs/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(csrfHeader, csrfValue)
	for _, c := range h.client.Jar.Cookies(mustParseURL(t, h.srv.URL)) {
		req.AddCookie(c)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("建立日志流失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		resp.Body.Close()
		t.Fatalf("Content-Type = %q", ct)
	}
	return resp, bufio.NewReader(resp.Body)
}

// readLogEvents 读 n 条 event: log，返回解析后的条目。
//
// 带超时：SSE 卡住时测试应该失败并说清楚，而不是挂到 go test 的 10 分钟。
func readLogEvents(t *testing.T, r *bufio.Reader, n int) []logstream.Entry {
	t.Helper()
	type result struct {
		entries []logstream.Entry
		err     error
	}
	done := make(chan result, 1)

	go func() {
		out := make([]logstream.Entry, 0, n)
		var data string
		for len(out) < n {
			line, err := r.ReadString('\n')
			if err != nil {
				done <- result{out, err}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
				var e logstream.Entry
				if err := json.Unmarshal([]byte(data), &e); err != nil {
					done <- result{out, err}
					return
				}
				out = append(out, e)
			}
		}
		done <- result{out, nil}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("读取日志流失败: %v（已读 %d 条）", r.err, len(r.entries))
		}
		return r.entries
	case <-time.After(3 * time.Second):
		t.Fatalf("等待 %d 条日志事件超时", n)
		return nil
	}
}

// 建立连接时先补发最近的日志，之后继续推新的。
//
// 补发是必须的：用户打开这一页往往正是因为刚刚出了事，而 SSE 只能推
// 「之后」的日志。
func TestLogStreamReplaysThenStreams(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	h.logs.Push(logstream.Entry{Msg: "启动完成", Level: "INFO"})
	h.logs.Push(logstream.Entry{Msg: "同步实例 account=TYO", Level: "INFO"})

	resp, reader := openLogStream(t, h)
	defer resp.Body.Close()

	replay := readLogEvents(t, reader, 2)
	if replay[0].Msg != "启动完成" || replay[1].Msg != "同步实例 account=TYO" {
		t.Fatalf("补发内容不对: %+v", replay)
	}

	// 连接建立之后写进来的日志要继续推。
	h.logs.Push(logstream.Entry{Msg: "新日志", Level: "WARN"})
	live := readLogEvents(t, reader, 1)
	if live[0].Msg != "新日志" || live[0].Level != "WARN" {
		t.Fatalf("实时推送不对: %+v", live[0])
	}
}

// 「先订阅再快照」的重叠部分要按 Seq 去重，否则用户会看到重复的几行。
func TestLogStreamDoesNotDuplicateReplay(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	for i := 0; i < 3; i++ {
		h.logs.Push(logstream.Entry{Msg: "replay"})
	}

	resp, reader := openLogStream(t, h)
	defer resp.Body.Close()

	got := readLogEvents(t, reader, 3)
	seen := map[uint64]bool{}
	for _, e := range got {
		if seen[e.Seq] {
			t.Fatalf("Seq %d 重复出现", e.Seq)
		}
		seen[e.Seq] = true
	}

	// 再推一条，确认流还活着且不会把补发的那几条重发一遍。
	h.logs.Push(logstream.Entry{Msg: "after"})
	next := readLogEvents(t, reader, 1)
	if next[0].Msg != "after" {
		t.Fatalf("期望 after，实际 %+v", next[0])
	}
}

// 这是 P3-1 的验收：日志页不出现明文私钥。
//
// 走完整链路——slog 打日志 → logstream.Handler 脱敏 → 环形缓冲 →
// SSE 输出——断言私钥的 base64 主体在任何一环都没留下来。
func TestLogStreamNeverCarriesPrivateKey(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	const keyBody = "MIIEowIBAAKCAQEAveryLongPrivateKeyBodyThatMustNeverAppear"
	// 用真实的 handler 写入，而不是直接 Push——否则测的只是 SSE 输出，
	// 测不到"脱敏发生在写入端"这件事。
	logger := slog.New(logstream.NewHandler(
		slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelInfo}),
		h.logs,
	))
	logger.Error("导入账号失败",
		"privateKeyPem", "-----BEGIN RSA PRIVATE KEY-----\n"+keyBody+"\n-----END RSA PRIVATE KEY-----",
		"password", "hunter2",
		"account", "TYO",
	)

	resp, reader := openLogStream(t, h)
	defer resp.Body.Close()

	got := readLogEvents(t, reader, 1)
	e := got[0]

	// 整个 JSON 序列化后再查一遍：字段值、消息体都算上。
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	payload := string(raw)
	for _, leak := range []string{keyBody, "hunter2", "BEGIN RSA PRIVATE KEY"} {
		if strings.Contains(payload, leak) {
			t.Fatalf("日志流里残留 %q：%s", leak, payload)
		}
	}
	// 非敏感字段要留着，否则日志没法用来排障。
	if e.Attrs["account"] != "TYO" {
		t.Errorf("account 应保留，实际 %+v", e.Attrs)
	}
	if e.Msg != "导入账号失败" {
		t.Errorf("消息被改坏了: %q", e.Msg)
	}
}

func TestLogStreamRequiresAuth(t *testing.T) {
	h := newHarness(t)

	resp := h.get("/api/logs/stream")
	expectStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}

// 没接日志缓冲时返回 503 而不是 500：这是"这个部署没开这项功能"，
// 不是服务出错。
func TestLogStreamUnavailableWithoutRing(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	h.api.logs = nil

	resp := h.get("/api/logs/stream")
	expectStatus(t, resp, http.StatusServiceUnavailable)
	if e := decode[errorBody](t, resp); e.Code != "logs_unavailable" {
		t.Errorf("错误码 = %q", e.Code)
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
