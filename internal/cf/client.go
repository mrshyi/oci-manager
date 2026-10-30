// Package cf 是 Cloudflare DNS API 的最小客户端与「换 IP 后同步 A 记录」的编排。
//
// 只做三件事：列 zone、列记录、改一条 A 记录的内容。刻意不实现「建 zone」
// 「建记录」「改 NS」——这个工具的定位是「机器换了地址，把域名指过去」，
// 不是 Cloudflare 控制台。功能面越小，token 需要的最小权限就越小，
// 用户也就越敢把 token 填进来。
//
// # token 不进日志
//
// token 等价于该域名全部 DNS 写权限。它只出现在 Authorization 头里，
// 不进错误信息、不进 API 响应、不进日志。logstream 的脱敏规则认得
// `Bearer xxx` 这一种形态，但那是最后一道网，不是唯一一道。
package cf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL 是 Cloudflare API v4 的根地址。
const DefaultBaseURL = "https://api.cloudflare.com/client/v4"

// apiTimeout 是单次 HTTP 调用的超时。
//
// 比 OCI 的调用短得多：换 IP 任务的每一轮都夹着一次 DNS 同步，DNS 卡住
// 会拖住整轮。DNS 是「顺带做的事」，不该占用换 IP 的时间预算。
const apiTimeout = 15 * time.Second

// ErrNoToken 表示这个 zone 没配 token，无法调用。
var ErrNoToken = errors.New("cf: 未配置 API token")

// Client 是一个 zone 维度的 Cloudflare API 客户端。
type Client struct {
	token string
	base  string
	http  *http.Client
}

// Option 用于在测试里换掉根地址与传输层。
type Option func(*Client)

// WithBaseURL 换掉 API 根地址。测试指向 httptest 服务端用。
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if strings.TrimSpace(u) != "" {
			c.base = strings.TrimRight(u, "/")
		}
	}
}

// WithHTTPClient 换掉传输层。测试里也可以直接指向假服务端。
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// NewClient 构造客户端。token 为空时所有方法返回 ErrNoToken。
func NewClient(token string, opts ...Option) *Client {
	c := &Client{
		token: strings.TrimSpace(token),
		base:  DefaultBaseURL,
		// 不设 Timeout 而由每次调用带 context：调用方的 context 已经
		// 带了整轮任务的预算，再叠一层固定超时只会让两个都失去意义。
		http: &http.Client{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Zone 是一个域名区。
type Zone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Record 是一条 DNS 记录。只关心 A 记录需要的字段。
type Record struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

// RecordUpdate 描述要改成的样子。
type RecordUpdate struct {
	Type    string
	Name    string
	Content string
	TTL     int
	Proxied bool
}

// APIError 是 Cloudflare 明确返回的业务错误。
//
// 与网络层错误分开：前者说明「请求到了、被拒了」，重试没有意义；
// 后者可能是抖动，重试有意义。把两者混成一个 error，调用方只能盲目重试。
type APIError struct {
	StatusCode int
	Code       int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("Cloudflare API 返回 %d", e.StatusCode)
	}
	return fmt.Sprintf("Cloudflare API 返回 %d：%s", e.StatusCode, e.Message)
}

// Temporary 报告这个错误是否值得重试。
//
// 429 与 5xx 是限流/服务端抖动，重试有意义；401/403 是 token 或权限问题，
// 重试一万次也是同一个结果。
func (e *APIError) Temporary() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// AuthFailed 报告这个错误是不是 token 本身的问题。
//
// 界面要据此给出「token 无效或权限不足」这种可操作的话，而不是把
// Cloudflare 的原文（"Authentication error"）直接丢给用户。
//
// 注意 Cloudflare 并不总是用 401 表达这件事：token 格式就不对（少一段、
// 多了空格、粘贴时带了引号）时它返回 400 + code 6003「Invalid request
// headers」。只认 401 的话，用户拿到的是「Invalid request headers」——
// 一句既不说哪里错、也不说该改什么的话，而这恰恰是最常见的一种失败。
func (e *APIError) AuthFailed() bool {
	switch e.Code {
	// 10000 Authentication error / 9103 未知的 key / 9109 无权访问该资源
	// 6003 Invalid request headers / 6111 X-Auth-Key 格式错误
	case 10000, 9103, 9109, 6003, 6111:
		return true
	}
	if e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden {
		return true
	}
	if e.StatusCode == http.StatusBadRequest {
		msg := strings.ToLower(e.Message)
		return strings.Contains(msg, "invalid request headers") ||
			strings.Contains(msg, "authentication")
	}
	return false
}

// envelope 是 Cloudflare 的统一响应外壳。
type envelope struct {
	Success bool            `json:"success"`
	Errors  []envelopeError `json:"errors"`
	Result  json.RawMessage `json:"result"`
	// ResultInfo 只在分页接口上有值。
	ResultInfo struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

type envelopeError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// do 发一次请求并把 result 解到 out。
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	if c.token == "" {
		return ErrNoToken
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, apiTimeout)
		defer cancel()
	}

	var payload io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("cf: 序列化请求失败: %w", err)
		}
		payload = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, payload)
	if err != nil {
		return fmt.Errorf("cf: 构造请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// 不包装 err 的原文之外再加东西：url.Error 里已经带了地址，
		// 而地址里没有 token（token 在头里）。
		return fmt.Errorf("cf: 请求失败: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("cf: 读取响应失败: %w", err)
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// 非 JSON 响应通常意味着打到了错误的地方（比如被反代拦下返回 HTML）。
		// 带上状态码，别带正文——正文可能很长，且对排障没有额外信息。
		return &APIError{StatusCode: resp.StatusCode, Message: "响应不是 JSON，请检查网络与 API 地址"}
	}
	if !env.Success || resp.StatusCode >= 300 {
		e := &APIError{StatusCode: resp.StatusCode}
		if len(env.Errors) > 0 {
			e.Code = env.Errors[0].Code
			msgs := make([]string, 0, len(env.Errors))
			for _, it := range env.Errors {
				if strings.TrimSpace(it.Message) != "" {
					msgs = append(msgs, it.Message)
				}
			}
			e.Message = strings.Join(msgs, "; ")
		}
		if e.Message == "" {
			e.Message = http.StatusText(resp.StatusCode)
		}
		return e
	}
	if out == nil {
		return nil
	}
	if len(env.Result) == 0 || string(env.Result) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("cf: 解析响应失败: %w", err)
	}
	return nil
}

// ListZones 列出 token 可见的全部 zone。
func (c *Client) ListZones(ctx context.Context) ([]Zone, error) {
	var out []Zone
	if err := c.do(ctx, http.MethodGet, "/zones?per_page=50", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetZone 取单个 zone 的信息。用来验证 token 是否有权访问它。
func (c *Client) GetZone(ctx context.Context, zoneID string) (*Zone, error) {
	var out Zone
	if err := c.do(ctx, http.MethodGet, "/zones/"+url.PathEscape(zoneID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListRecords 列出某个 zone 下指定类型的记录。recordType 为空表示全部。
func (c *Client) ListRecords(ctx context.Context, zoneID, recordType string) ([]Record, error) {
	path := "/zones/" + url.PathEscape(zoneID) + "/dns_records?per_page=100"
	if recordType != "" {
		path += "&type=" + url.QueryEscape(recordType)
	}
	var out []Record
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateRecord 覆盖一条记录。
//
// 用 PUT 而不是 PATCH：PATCH 只改给出的字段，剩下的保持原样——听起来更安全，
// 但对 A 记录而言「保持原样」意味着 content 也保持原样，那就什么都没改。
// PUT 要求带上完整记录，正好逼调用方把 ttl / proxied 一起想清楚。
func (c *Client) UpdateRecord(ctx context.Context, zoneID, recordID string, u RecordUpdate) (*Record, error) {
	body := map[string]any{
		"type":    u.Type,
		"name":    u.Name,
		"content": u.Content,
		"ttl":     u.TTL,
		"proxied": u.Proxied,
	}
	var out Record
	path := "/zones/" + url.PathEscape(zoneID) + "/dns_records/" + url.PathEscape(recordID)
	if err := c.do(ctx, http.MethodPut, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DescribeError 把 Cloudflare 的错误翻成一句用户能照做的话。
//
// 原始文案（"Authentication error"）只说"不行"，不说"该改什么"。
func DescribeError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrNoToken) {
		return "这个域名还没配 API token"
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.AuthFailed():
			return "API token 无效或权限不足：需要在 Cloudflare 建一个带 Zone.DNS 编辑权限的 token"
		case apiErr.StatusCode == http.StatusTooManyRequests:
			return "被 Cloudflare 限流，稍后再试"
		case apiErr.StatusCode == http.StatusNotFound:
			return "记录或域名不存在：可能已在 Cloudflare 上被删除"
		}
	}
	return err.Error()
}

// TTLFor 把界面上的 TTL 归一到 Cloudflare 接受的值。
//
// 1 是 Cloudflare 的 "automatic"。合法区间是 60–86400，其余一律按自动处理：
// 猜一个具体秒数比交给 Cloudflare 决定更容易出错。
func TTLFor(ttl int) int {
	if ttl == 1 {
		return 1
	}
	if ttl < 60 || ttl > 86400 {
		return 1
	}
	return ttl
}

// ParseZoneID 从用户可能粘进来的各种形态里取出 zone id。
//
// 用户经常直接粘控制台地址：dash.cloudflare.com/<account>/<zone-name>/dns
// 那里没有 zone id，只有域名。所以这里同时接受「32 位十六进制 zone id」
// 和「域名」，后者由调用方拿去 ListZones 里匹配。
func ParseZoneID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		// 从控制台地址里取路径最后一段。可能是 zone id，也可能是域名。
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) > 0 {
			s = parts[len(parts)-1]
		}
	}
	return strings.TrimSpace(s)
}

// looksLikeZoneID 报告 s 是否像 Cloudflare 的 zone id（32 位十六进制）。
func looksLikeZoneID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, r := range s {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

// ResolveZone 把用户填的东西解析成一个 zone。
//
// 接受 zone id、域名、以及控制台地址。域名与控制台地址会去 ListZones 里
// 找同名的那一个——不猜、不拼，找不到就报错让用户自己核对。
func (c *Client) ResolveZone(ctx context.Context, raw string) (*Zone, error) {
	v := ParseZoneID(raw)
	if v == "" {
		return nil, errors.New("cf: zone 不能为空")
	}
	if looksLikeZoneID(v) {
		return c.GetZone(ctx, v)
	}
	zones, err := c.ListZones(ctx)
	if err != nil {
		return nil, err
	}
	want := strings.ToLower(strings.TrimSuffix(v, "."))
	for i := range zones {
		if strings.ToLower(zones[i].Name) == want {
			return &zones[i], nil
		}
	}
	return nil, fmt.Errorf("cf: 这个 token 看不到域名 %q（token 需要 Zone 读权限，且范围要覆盖它）", want)
}
