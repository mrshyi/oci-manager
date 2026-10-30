package cf

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeCF 是一个最小的 Cloudflare API 假服务端。
//
// 只实现本包用到的四个端点。刻意把「token 校验」也做进来：真实世界里
// token 过期是最常见的失败，而客户端能不能把 401 翻成一句用户能照做的
// 话，是这套联动好不好用的关键。
type fakeCF struct {
	token  string
	zones  []Zone
	recs   map[string][]Record // zoneID -> records
	failOn string              // 非空时这个路径一律 500
	// lastBody 记录最后一次 PUT 的请求体，用来断言真的带上了新 IP。
	lastBody string
	calls    int
}

func (f *fakeCF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls++
	w.Header().Set("Content-Type", "application/json")

	if f.failOn != "" && strings.Contains(r.URL.Path, f.failOn) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":1000,"message":"internal"}]}`))
		return
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`))
		return
	}

	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/zones":
		writeEnv(w, f.zones)

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/zones/") &&
		strings.HasSuffix(path, "/dns_records"):
		zoneID := strings.TrimSuffix(strings.TrimPrefix(path, "/zones/"), "/dns_records")
		recs, ok := f.recs[zoneID]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":7003,"message":"no route"}]}`))
			return
		}
		writeEnv(w, recs)

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/zones/"):
		id := strings.TrimPrefix(path, "/zones/")
		for _, z := range f.zones {
			if z.ID == id {
				writeEnv(w, z)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":7003,"message":"zone not found"}]}`))

	case r.Method == http.MethodPut && strings.Contains(path, "/dns_records/"):
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		f.lastBody = string(buf)
		writeEnv(w, Record{ID: "r1", Type: "A", Name: "a.example.com", Content: "203.0.113.9"})

	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":7003,"message":"no route"}]}`))
	}
}

func writeEnv(w http.ResponseWriter, result any) {
	buf, _ := json.Marshal(map[string]any{"success": true, "result": result})
	_, _ = w.Write(buf)
}

func newFake(t *testing.T) (*fakeCF, *Client) {
	t.Helper()
	f := &fakeCF{
		token: "tok-123",
		zones: []Zone{{ID: strings.Repeat("a", 32), Name: "example.com", Status: "active"}},
		recs: map[string][]Record{
			strings.Repeat("a", 32): {
				{ID: "r1", Type: "A", Name: "a.example.com", Content: "198.51.100.1", TTL: 1},
			},
		},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, NewClient(f.token, WithBaseURL(srv.URL))
}

// 用域名解析 zone：客户端要自己去列 zone 再匹配。
func TestResolveZoneByName(t *testing.T) {
	_, c := newFake(t)
	z, err := c.ResolveZone(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("解析域名失败: %v", err)
	}
	if z.Name != "example.com" {
		t.Fatalf("解析到 %q，期望 example.com", z.Name)
	}
}

// 用 zone id 解析：直接打 /zones/{id}，不该去列全部 zone。
func TestResolveZoneByID(t *testing.T) {
	f, c := newFake(t)
	id := f.zones[0].ID
	z, err := c.ResolveZone(context.Background(), id)
	if err != nil {
		t.Fatalf("解析 zone id 失败: %v", err)
	}
	if z.ID != id {
		t.Fatalf("id = %q，期望 %q", z.ID, id)
	}
}

// 从控制台地址里取最后一段：用户经常直接粘浏览器地址栏。
func TestParseZoneIDFromConsoleURL(t *testing.T) {
	cases := map[string]string{
		"https://dash.cloudflare.com/abc123/example.com/dns": "dns",
		"example.com":                                        "example.com",
		"  example.com  ":                                    "example.com",
		"":                                                   "",
	}
	for in, want := range cases {
		if got := ParseZoneID(in); got != want {
			t.Errorf("ParseZoneID(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 找不到域名时要明确说 token 看不到它，而不是返回一个空 zone。
func TestResolveZoneUnknownName(t *testing.T) {
	_, c := newFake(t)
	_, err := c.ResolveZone(context.Background(), "nope.example")
	if err == nil {
		t.Fatal("未知域名应报错")
	}
	if !strings.Contains(err.Error(), "看不到") {
		t.Errorf("错误文案应说明 token 看不到该域名: %v", err)
	}
}

// token 错时给出一句用户能照做的话，且错误里不能出现 token 本身。
func TestAuthFailureIsActionableAndLeaksNothing(t *testing.T) {
	f, _ := newFake(t)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	bad := NewClient("wrong-token", WithBaseURL(srv.URL))
	_, err := bad.ListZones(context.Background())
	if err == nil {
		t.Fatal("token 错误应报错")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("应返回 *APIError，实际 %T", err)
	}
	if !apiErr.AuthFailed() {
		t.Error("401 应被判定为 token 问题")
	}
	if apiErr.Temporary() {
		t.Error("401 不该被判为可重试")
	}

	msg := DescribeError(err)
	if !strings.Contains(msg, "Zone.DNS") {
		t.Errorf("文案应告诉用户需要什么权限: %s", msg)
	}
	if strings.Contains(msg, "wrong-token") {
		t.Errorf("错误信息里出现了 token: %s", msg)
	}
}

// token 格式就不对时 Cloudflare 返回 400 + Invalid request headers，
// 这也必须被认成 token 问题——否则用户拿到的是一句无从下手的话。
func TestMalformedTokenIsAuthFailure(t *testing.T) {
	e := &APIError{StatusCode: 400, Code: 6003, Message: "Invalid request headers"}
	if !e.AuthFailed() {
		t.Error("6003 应被判为 token 问题")
	}
	if e.Temporary() {
		t.Error("400 不该被判为可重试")
	}
	msg := DescribeError(e)
	if !strings.Contains(msg, "Zone.DNS") {
		t.Errorf("文案应告诉用户需要什么权限: %s", msg)
	}
	if strings.Contains(msg, "Invalid request headers") {
		t.Errorf("不该把 Cloudflare 的原文直接丢给用户: %s", msg)
	}

	// 与 token 无关的 400（比如参数不合法）不能被误判成 token 问题，
	// 否则用户会一直去重建 token，而真正的问题在别处。
	other := &APIError{StatusCode: 400, Code: 9003, Message: "Invalid content type"}
	if other.AuthFailed() {
		t.Error("无关的 400 不该被判为 token 问题")
	}
}

// 429 与 5xx 可重试，4xx 其余不可。
func TestTemporaryClassification(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{429, true},
		{500, true},
		{502, true},
		{401, false},
		{403, false},
		{404, false},
	}
	for _, c := range cases {
		e := &APIError{StatusCode: c.status}
		if got := e.Temporary(); got != c.want {
			t.Errorf("status %d Temporary() = %v，期望 %v", c.status, got, c.want)
		}
	}
}

// 响应不是 JSON（比如被反代拦下返回 HTML）时要给出可读的错，而不是解析错误。
func TestNonJSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>blocked</html>"))
	}))
	t.Cleanup(srv.Close)

	c := NewClient("t", WithBaseURL(srv.URL))
	_, err := c.ListZones(context.Background())
	if err == nil {
		t.Fatal("非 JSON 响应应报错")
	}
	if !strings.Contains(err.Error(), "不是 JSON") {
		t.Errorf("错误应点明响应不是 JSON: %v", err)
	}
}

// 改 A 记录时要带上完整的记录字段：PUT 是覆盖语义，漏一个字段就会把它清掉。
func TestUpdateRecordSendsFullBody(t *testing.T) {
	f, c := newFake(t)
	zoneID := f.zones[0].ID

	if _, err := c.UpdateRecord(context.Background(), zoneID, "r1", RecordUpdate{
		Type: "A", Name: "a.example.com", Content: "203.0.113.9", TTL: 1, Proxied: false,
	}); err != nil {
		t.Fatalf("更新记录失败: %v", err)
	}
	for _, want := range []string{`"content":"203.0.113.9"`, `"type":"A"`, `"name":"a.example.com"`, `"proxied":false`} {
		if !strings.Contains(f.lastBody, want) {
			t.Errorf("请求体缺少 %s：%s", want, f.lastBody)
		}
	}
}

// 没配 token 时不该发请求。地址指向一个必然连不上的端口：真发了就会
// 得到连接错误而不是 ErrNoToken，测试自然失败。
func TestNoTokenDoesNotCallAPI(t *testing.T) {
	c := NewClient("", WithBaseURL("http://127.0.0.1:1"))
	_, err := c.ListZones(context.Background())
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("应返回 ErrNoToken，实际 %v", err)
	}
	if msg := DescribeError(err); !strings.Contains(msg, "token") {
		t.Errorf("文案应提到 token: %s", msg)
	}
}

func TestTTLFor(t *testing.T) {
	cases := map[int]int{
		1:     1,
		0:     1,
		30:    1,
		60:    60,
		3600:  3600,
		86400: 86400,
		99999: 1,
	}
	for in, want := range cases {
		if got := TTLFor(in); got != want {
			t.Errorf("TTLFor(%d) = %d，期望 %d", in, got, want)
		}
	}
}

func TestLooksLikeZoneID(t *testing.T) {
	if !looksLikeZoneID(strings.Repeat("a", 32)) {
		t.Error("32 位十六进制应被判为 zone id")
	}
	if looksLikeZoneID("example.com") {
		t.Error("域名不该被判为 zone id")
	}
	if looksLikeZoneID(strings.Repeat("z", 32)) {
		t.Error("含非十六进制字符不该被判为 zone id")
	}
}
