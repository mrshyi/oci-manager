package ociclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxResponseBytes 限制单次响应的读取上限，避免异常响应耗尽内存。
const maxResponseBytes = 8 << 20 // 8 MiB

// UserAgent 使用真实的产品标识。不伪装成官方 SDK——伪造 UA 既无益于兼容性，
// 也会让异常流量更难被 Oracle 正确归因。
const UserAgent = "OCICore/0.1 (+https://github.com/ocicore)"

// Client 是一个绑定到单个 OCI 账号（租户 + 用户 + 密钥）的 API 客户端。
// 可安全地被多 goroutine 共用。
type Client struct {
	creds  *Credentials
	signer *Signer
	http   *http.Client

	// maxRetries 是瞬时错误的自动重试次数。认证、权限、配额类错误从不重试。
	maxRetries int

	// limiter 是跨账号共享的全局并发闸；tenancySlots 是本租户自己的闸。
	// 两者都可为 nil，表示不限——测试里用得上。
	limiter      *Limiter
	tenancySlots chan struct{}

	// backoff 计算第 n 次重试前的等待时长。
	//
	// 抽成字段是为了让测试能把等待压到 0：真实退避最短 5 秒（ClassTransient），
	// 一次「失败两次再成功」的重试测试就要等 15 秒以上，而这条路径恰恰是最
	// 需要反复测的。生产路径始终是 backoffFor。
	backoff func(error, int) time.Duration

	// onCall 在每次请求交给传输层后回调一次，用于调用预算计数。可为 nil。
	//
	// 不带参数：账号身份由注入方用闭包捕获，客户端本身只知道自己拿了哪份
	// 凭据，不该反过来去猜本地账号 ID。
	onCall func()

	// endpoint 解析某个服务在某区域的端点。生产路径永远是 Endpoint，
	// 抽成字段只为让测试能指向一个冒充 OCI 的本地服务端。
	endpoint func(Service, string) string
}

// NewTransport 返回一份带短拨号超时的传输层，供需要自定义 http.Client
// 的调用方复用（例如 ociconn 要挂代理）。
//
// 导出它是因为之前 ociconn 走的是 http.DefaultTransport.Clone()，
// 于是**配了代理的账号反而失去了这层保护**——而代理恰恰是最容易不通的
// 那一环，一挂就要等满 30 秒总超时才报错。
func NewTransport() *http.Transport { return defaultTransport() }

// defaultTransport 是带短拨号超时的传输层。
//
// 默认的 30 秒总超时挡不住"域名能解析但 TCP 连不上"这种情况——OCI 的
// 泛解析会让写错的服务域名照样解析出 IP，请求随后一路挂到总超时。
// 把拨号阶段单独限到 8 秒，配错域名会很快报错而不是让界面卡半分钟。
func defaultTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{
		Timeout:   8 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	t.TLSHandshakeTimeout = 8 * time.Second
	return t
}

// Option 用于定制 Client。
type Option func(*Client)

// WithHTTPClient 替换底层的 http.Client，用于设置代理或自定义超时。
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.http = hc }
}

// WithMaxRetries 设置瞬时错误的重试次数。
func WithMaxRetries(n int) Option {
	return func(c *Client) { c.maxRetries = n }
}

// WithCallObserver 注册一个「每次请求发出后」的回调，用于调用预算统计。
//
// 回调在**每次 attempt 之后**触发，包括重试与失败的那几次——预算统计的是
// 「打了多少次 OCI」，不是「成功了几次」。只统计成功会严重低估量级：
// 抢机失败的那几轮恰恰是调用最密的时候。
func WithCallObserver(fn func()) Option {
	return func(c *Client) { c.onCall = fn }
}

// WithEndpointResolver 替换端点解析函数。
//
// 存在的理由是测试：权限自检的验收是「缺 ListVolumes 权限时 storage 项为
// false、其余为 true」，这件事没法靠真实账号反复构造——去控制台删一条策略
// 再跑一次的成本太高。有了它就能起一个本地服务端按路径决定返回 200 还是 403。
//
// 也可用于对接 OCI 兼容端点。传 nil 不生效，避免把客户端变成"没有端点"的
// 状态——那会让所有请求拼出 "/20160918/xxx" 这种相对地址。
func WithEndpointResolver(fn func(Service, string) string) Option {
	return func(c *Client) {
		if fn != nil {
			c.endpoint = fn
		}
	}
}

// New 创建一个客户端。凭据会先经过离线校验，尽早暴露配置错误。
func New(creds *Credentials, opts ...Option) (*Client, error) {
	if err := creds.Validate(); err != nil {
		return nil, err
	}
	c := &Client{
		creds:      creds,
		signer:     NewSigner(creds),
		http:       &http.Client{Timeout: 30 * time.Second, Transport: defaultTransport()},
		maxRetries: 2,
		backoff:    backoffFor,
		endpoint:   Endpoint,
		// 默认就带上租户级限流。忘了配全局限流器最多是少一层保护，
		// 而默认不限流会让"同步 + 配额 + 批量同时触发"直接打到 Oracle。
		tenancySlots: make(chan struct{}, DefaultTenancyConcurrency),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Region 返回该客户端默认使用的区域。
func (c *Client) Region() string { return c.creds.Region }

// TenancyOCID 返回租户 OCID。
func (c *Client) TenancyOCID() string { return c.creds.TenancyOCID }

// UserOCID 返回 IAM 用户 OCID。
func (c *Client) UserOCID() string { return c.creds.UserOCID }

// Request 描述一次 OCI API 调用。
type Request struct {
	Method  string
	Service Service
	// Path 是版本段之后的路径，必须以 / 开头，例如 "/users/ocid1.user..."。
	Path string
	// Region 覆盖客户端默认区域。留空则用默认值。
	Region string
	// BaseURL 直接指定完整的基础地址（协议 + 主机 + 版本段），用于不遵循
	// {service}.{region} 命名规则的端点——Identity Domains 每个身份域
	// 有自己的 URL，形如 https://idcs-xxxx.identity.oraclecloud.com。
	//
	// 非空时 Service 与 Region 都被忽略。地址会先过一遍 oracleBaseURL 校验：
	// 它来自 ListDomains 的响应，虽然是 Oracle 给的，但仍是流进 URL 的数据，
	// 不该无条件相信（同 region.go 里对区域名的处理）。
	BaseURL string
	Query   url.Values
	// Body 会被序列化为 JSON。为 nil 表示无请求体。
	Body any
	// Idempotent 表示这次调用可以安全地重试而不产生重复副作用。
	//
	// 打开后会在重试循环**外**生成一个 opc-retry-token，本次调用的所有重试
	// 都携带同一个值，服务端据此保证只执行一次。创建类接口（建实例、建 VCN、
	// 建子网、建网关、申请保留 IP、挂载卷）必须打开——它们超时重试时若没有
	// 幂等键，就会造出第二份资源，而用户看到的只是"网络抖了一下"。
	//
	// 当前标记了的调用点见 retrytoken_test.go 的 TestCreationCallsCarryRetryToken，
	// 新增创建类接口时请一并加进那张表。
	//
	// 只读接口不需要打开：重复读没有副作用，带上反而多一个头，也让"哪些调用
	// 是幂等的"这条信息失真。
	Idempotent bool
}

// Response 携带调用方偶尔需要的响应元数据（分页游标、请求 ID）。
type Response struct {
	StatusCode   int
	OpcRequestID string
	// NextPage 是分页游标，为空表示已到最后一页。
	NextPage string
}

// Do 执行一次 API 调用，并把 JSON 响应体解码到 out（out 为 nil 时丢弃响应体）。
func (c *Client) Do(ctx context.Context, req Request, out any) (*Response, error) {
	var payload []byte
	if req.Body != nil {
		var err error
		payload, err = json.Marshal(req.Body)
		if err != nil {
			return nil, fmt.Errorf("ociclient: 序列化请求体失败: %w", err)
		}
	}

	// 走绝对地址的端点（Identity Domains）不参与区域拼接。
	if req.BaseURL != "" {
		base, err := oracleBaseURL(req.BaseURL)
		if err != nil {
			return nil, err
		}
		target := base + req.Path
		if len(req.Query) > 0 {
			target += "?" + req.Query.Encode()
		}
		return c.doTarget(ctx, req, target, payload, out)
	}

	region := req.Region
	if region == "" {
		region = c.creds.Region
	}

	// 在拼 URL 之前就把区域挡住。
	//
	// 区域名会被拼进主机名，NormalizeRegion 已经用字符集把注入挡在外面了
	// （见 region.go 的说明与 TestNormalizeRegionRejectsHostInjection）。
	// 但它对非法输入返回的是空串，于是这里会拼出 https://iaas..oraclecloud.com
	// 照常发出去——安全上无害（空 DNS 标签解析不了），可报错是一句莫名其妙的
	// DNS 失败，排障时要绕好大一圈才找得回是区域填错了。
	//
	// 在 sink 前显式判一次：既给出能看懂的错误，也让"主机名不受输入控制"
	// 这个性质在代码里直接可见，而不是只存在于另一个文件的正则里。
	if NormalizeRegion(region) == "" {
		return nil, fmt.Errorf("ociclient: 区域名不合法: %q", region)
	}

	target := c.endpoint(req.Service, region) + "/" + req.Service.Version + req.Path
	if len(req.Query) > 0 {
		target += "?" + req.Query.Encode()
	}
	return c.doTarget(ctx, req, target, payload, out)
}

// doTarget 对一个已经拼好的完整地址执行请求，带重试与退避。
//
// 幂等令牌在这里生成——**必须在循环外**。生成在 attempt 里是这套机制最典型
// 的错误写法：每次重试都是新 token，等于完全没做幂等，而现象上一切正常，
// 只在"第一次其实成功了但响应丢了"时才暴露成重复资源。
//
// 收整个 req 而不是 method + idempotent 两个参数，是为了让 Idempotent 只有
// 一处读取点：拆成布尔参数后，Do 的两个分支各传一次，写错一个就静默失效。
func (c *Client) doTarget(ctx context.Context, req Request, target string, payload []byte, out any) (*Response, error) {
	retryToken := ""
	if req.Idempotent {
		retryToken = newRetryToken()
	}

	for attempt := 0; ; attempt++ {
		resp, err := c.attempt(ctx, req.Method, target, payload, out, retryToken)
		if err == nil {
			return resp, nil
		}
		// 认证、权限、配额类错误重试多少次都是一样的结果，立刻返回。
		if attempt >= c.maxRetries || !IsRetryable(err) {
			return nil, err
		}
		// 服务端明确说这个 token 已失效时换一个再重试。此时上一次请求确定
		// 没有执行（token 根本没被接受），换 token 不会引入重复；而不换的话
		// 后面每一次重试都会以同样的错误失败，重试预算就白花了。
		if isInvalidatedRetryToken(err) {
			retryToken = newRetryToken()
		}
		// 退避期间必须尊重 context：用户关掉页面时应立刻停止。
		wait := c.backoff(err, attempt)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// isInvalidatedRetryToken 判断错误是否为"幂等键已失效"。
func isInvalidatedRetryToken(err error) bool {
	apiErr, ok := AsAPIError(err)
	return ok && apiErr.Code == "InvalidatedRetryToken"
}

// oracleBaseURL 校验并归一化一个绝对端点地址。
//
// 地址来自 ListDomains 的响应。虽然那是 Oracle 自己返回的，但它仍是一段
// 流进 URL 的数据，而流进去之后我们会**带着签名**向它发请求。不校验的话，
// 一个被污染的响应就能把带签名的请求引到别处去——这跟 region.go 里防的
// 是同一类事，只是入口换成了响应体而不是用户输入。
func oracleBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("ociclient: 端点地址无法解析: %w", err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("ociclient: 端点必须是 https: %q", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("ociclient: 端点地址含非预期成分: %q", raw)
	}
	host := strings.ToLower(u.Hostname())
	if !isOracleHost(host) {
		return "", fmt.Errorf("ociclient: 端点不在 Oracle 域内: %q", host)
	}
	// 去掉末尾斜杠，调用方的 Path 一律以 / 开头。
	return u.Scheme + "://" + u.Host + strings.TrimSuffix(u.Path, "/"), nil
}

// oracleHostSuffixes 是允许的端点域名后缀。
//
// 身份域的地址形如 idcs-xxxx.identity.oraclecloud.com，与常规服务端点
// 不是一个命名空间，所以单独列。
var oracleHostSuffixes = []string{
	".oraclecloud.com",
	".oraclegovcloud.com",
	".oraclegovcloud.uk",
	".oracle.com",
}

func isOracleHost(host string) bool {
	for _, suffix := range oracleHostSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// attempt 执行单次请求。每次尝试都重新构造并签名——Date 头必须是新鲜的，
// 陈旧超过 5 分钟的签名会被 OCI 直接拒绝。
//
// retryToken 非空时带上 opc-retry-token 头。它由调用方（doTarget）在重试
// 循环外生成并逐次传入，本函数不生成——生成逻辑放这里就会变成每次重试一个
// 新 token，幂等机制静默失效。
func (c *Client) attempt(ctx context.Context, method, target string, payload []byte, out any, retryToken string) (*Response, error) {
	var bodyReader io.Reader
	if payload != nil {
		bodyReader = bytes.NewReader(payload)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, target, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("ociclient: 构造请求失败: %w", err)
	}
	httpReq.Header.Set("User-Agent", UserAgent)
	httpReq.Header.Set("Accept", "application/json")
	if retryToken != "" {
		httpReq.Header.Set(retryTokenHeader, retryToken)
	}

	if err := c.signer.Sign(httpReq, payload); err != nil {
		return nil, err
	}

	release, err := c.acquireSlots(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	httpResp, err := c.http.Do(httpReq)

	// 计数放在错误处理之前：请求已经交给传输层了，无论结果如何都算一次
	// 对 OCI 的尝试。放在成功分支里会漏掉失败与超时——而那正是调用最密的时刻。
	//
	// 带 WithoutBudget 标记的调用（权限自检）不计数：它不是业务动作，
	// 不该占用用户本来留给自己操作的额度。
	if c.onCall != nil && !budgetSkipped(ctx) {
		c.onCall()
	}

	if err != nil {
		// 网络层失败没有 HTTP 状态码，归为瞬时错误交给重试逻辑。
		return nil, &APIError{
			StatusCode: 0,
			Code:       "NetworkError",
			Message:    err.Error(),
			Class:      ClassTransient,
			Method:     method,
			URL:        target,
		}
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, maxResponseBytes))
	if err != nil {
		return nil, &APIError{
			StatusCode: httpResp.StatusCode,
			Code:       "ReadBodyFailed",
			Message:    err.Error(),
			Class:      ClassTransient,
			Method:     method,
			URL:        target,
		}
	}

	resp := &Response{
		StatusCode:   httpResp.StatusCode,
		OpcRequestID: httpResp.Header.Get("opc-request-id"),
		NextPage:     httpResp.Header.Get("opc-next-page"),
	}

	if httpResp.StatusCode >= 400 {
		return nil, newAPIError(httpResp, body, method, target)
	}

	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return nil, fmt.Errorf("ociclient: 解析响应失败 (%s): %w", target, err)
		}
	}
	return resp, nil
}

// newAPIError 把错误响应解析成 APIError。OCI 的错误体格式是 {"code":…,"message":…}，
// 但网关层的错误未必遵守，因此解析失败时保留原始文本。
func newAPIError(httpResp *http.Response, body []byte, method, target string) *APIError {
	var parsed struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &parsed)

	message := parsed.Message
	if message == "" {
		message = strings.TrimSpace(string(body))
		if message == "" {
			message = httpResp.Status
		}
	}

	return &APIError{
		StatusCode:   httpResp.StatusCode,
		Code:         parsed.Code,
		Message:      message,
		OpcRequestID: httpResp.Header.Get("opc-request-id"),
		Class:        Classify(httpResp.StatusCode, parsed.Code, message),
		RetryAfter:   parseRetryAfter(httpResp.Header.Get("Retry-After")),
		Method:       method,
		URL:          target,
	}
}

// parseRetryAfter 解析 Retry-After 头，支持秒数与 HTTP 日期两种格式。
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// backoffFor 计算第 attempt 次重试前的等待时长：以分类的最小退避为基线，逐次翻倍，
// 再叠一层随机抖动。
//
// 抖动的必要性：同一个账号被限流后，若所有请求都按同样的 5s / 10s / 20s 精确
// 重试，重试就会自己排成一队整齐的脉冲，一波一波地打回去——服务端看到的是
// "刚安静下来又整齐地来了"，判定的频率特征反而更明显。±16.7% 的浮动把这一队
// 人打散，代价只是平均多等不到一秒。
//
// 上界仍然是硬上界：抖动后超过 2 分钟的一律压回 2 分钟，不会因为随机而突破。
func backoffFor(err error, attempt int) time.Duration {
	base := 2 * time.Second
	if apiErr, ok := AsAPIError(err); ok {
		if b := apiErr.Backoff(); b > 0 {
			base = b
		}
	}
	wait := withJitter(base << attempt)
	if max := 2 * time.Minute; wait > max {
		wait = max
	}
	return wait
}

// jitterRatio 是退避时长的随机浮动幅度，取 1/6 即 ±16.7%。
const jitterRatio = 1.0 / 6.0

// withJitter 在 d 上叠加 ±jitterRatio 的随机偏移。d <= 0 时原样返回。
func withJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	delta := float64(d) * jitterRatio
	offset := (rand.Float64()*2 - 1) * delta
	return time.Duration(float64(d) + offset)
}

// listPages 反复调用同一个接口直到分页结束，把每页结果追加进 collect。
//
// limit 是安全上限：OCI 的游标出错时可能永远返回 next page，不设上限会无限循环。
func listPages(ctx context.Context, c *Client, req Request, limit int, collect func(page []byte) error) error {
	if req.Query == nil {
		req.Query = url.Values{}
	}
	for i := 0; i < limit; i++ {
		var raw json.RawMessage
		resp, err := c.Do(ctx, req, &raw)
		if err != nil {
			return err
		}
		if err := collect(raw); err != nil {
			return err
		}
		if resp.NextPage == "" {
			return nil
		}
		req.Query.Set("page", resp.NextPage)
	}
	return fmt.Errorf("ociclient: 分页超过 %d 页上限，已中止 (%s)", limit, req.Path)
}
