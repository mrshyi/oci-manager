package ociclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newFastClient 造一个指向 srv 的客户端，并把退避压到 0。
//
// 真实退避最短 5 秒，一次「失败两次再成功」的测试要等 15 秒以上；
// 而重试路径恰恰是最需要反复测的。
func newFastClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(testCreds(t, testKey(t)), WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	c.backoff = func(error, int) time.Duration { return 0 }
	return c
}

// tokenRecorder 记录每次请求收到的 opc-retry-token，并按 failFirst 次返回 503。
type tokenRecorder struct {
	mu        sync.Mutex
	tokens    []string
	failFirst int
	calls     int
}

func (r *tokenRecorder) handler(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.calls <= r.failFirst {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"ServiceUnavailable","message":"try later"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

func (r *tokenRecorder) record(h http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens = append(r.tokens, h.Get(retryTokenHeader))
}

func (r *tokenRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tokens...)
}

// TestRetryTokenIsStableAcrossRetries 是 P0-1 的核心断言。
//
// 这套机制最典型的错误写法是在 attempt 里生成 token——那样每次重试都是新
// token，幂等完全失效，而现象上一切正常：请求成功、日志干净、单测若不看
// 请求头也照样通过。只有「第一次其实成功了但响应丢了」的场景才会暴露成
// 重复资源，而那时候用户已经在付两份钱了。
func TestRetryTokenIsStableAcrossRetries(t *testing.T) {
	rec := &tokenRecorder{failFirst: 2}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header)
		rec.handler(w, r)
	}))
	defer srv.Close()

	c := newFastClient(t, srv)
	if _, err := c.doTarget(context.Background(), Request{
		Method: http.MethodPost, Idempotent: true,
	}, srv.URL, []byte(`{"displayName":"x"}`), nil); err != nil {
		t.Fatalf("两次 503 后应当重试成功: %v", err)
	}

	tokens := rec.snapshot()
	if len(tokens) != 3 {
		t.Fatalf("期望 3 次请求（首发 + 2 次重试），实际 %d 次", len(tokens))
	}
	for i, got := range tokens {
		if got == "" {
			t.Fatalf("第 %d 次请求没有携带 %s", i+1, retryTokenHeader)
		}
		if got != tokens[0] {
			t.Fatalf("第 %d 次请求的 token 与首次不同：%q vs %q——"+
				"token 必须跨重试复用，否则幂等键形同虚设", i+1, got, tokens[0])
		}
	}
}

// TestRetryTokenDiffersBetweenCalls 验证两次独立调用拿到不同 token。
//
// 若退化成包级变量或固定值，同一个幂等键会跨调用复用，第二次创建会被
// 服务端当成"第一次的重复请求"直接返回旧结果——用户点两次只得到一台机器，
// 而且不会报错。
func TestRetryTokenDiffersBetweenCalls(t *testing.T) {
	rec := &tokenRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header)
		rec.handler(w, r)
	}))
	defer srv.Close()

	c := newFastClient(t, srv)
	for i := 0; i < 2; i++ {
		if _, err := c.doTarget(context.Background(), Request{
			Method: http.MethodPost, Idempotent: true,
		}, srv.URL, []byte(`{}`), nil); err != nil {
			t.Fatalf("第 %d 次调用失败: %v", i+1, err)
		}
	}

	tokens := rec.snapshot()
	if len(tokens) != 2 {
		t.Fatalf("期望 2 次请求，实际 %d 次", len(tokens))
	}
	if tokens[0] == tokens[1] {
		t.Fatalf("两次独立调用复用了同一个 token: %q", tokens[0])
	}
}

// TestRetryTokenAbsentWhenNotIdempotent 验证只读调用不带这个头。
func TestRetryTokenAbsentWhenNotIdempotent(t *testing.T) {
	rec := &tokenRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header)
		rec.handler(w, r)
	}))
	defer srv.Close()

	c := newFastClient(t, srv)
	if _, err := c.doTarget(context.Background(), Request{
		Method: http.MethodGet,
	}, srv.URL, nil, nil); err != nil {
		t.Fatal(err)
	}

	tokens := rec.snapshot()
	if len(tokens) != 1 {
		t.Fatalf("期望 1 次请求，实际 %d 次", len(tokens))
	}
	if tokens[0] != "" {
		t.Fatalf("非幂等调用不应携带 %s，实际 %q", retryTokenHeader, tokens[0])
	}
}

// TestRetryTokenRegeneratedWhenInvalidated 验证服务端说 token 失效时换一个。
//
// 不换的话，后续每次重试都会以同样的 InvalidatedRetryToken 失败，
// 重试预算白花，用户看到的是"重试了但还是报错"。
func TestRetryTokenRegeneratedWhenInvalidated(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens = append(tokens, r.Header.Get(retryTokenHeader))
		n := len(tokens)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"InvalidatedRetryToken","message":"token expired"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newFastClient(t, srv)
	if _, err := c.doTarget(context.Background(), Request{
		Method: http.MethodPost, Idempotent: true,
	}, srv.URL, []byte(`{}`), nil); err != nil {
		t.Fatalf("token 失效后换一个重试应当成功: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 2 {
		t.Fatalf("期望 2 次请求，实际 %d 次", len(tokens))
	}
	if tokens[0] == "" || tokens[1] == "" {
		t.Fatalf("两次请求都应携带 token，实际 %q / %q", tokens[0], tokens[1])
	}
	if tokens[0] == tokens[1] {
		t.Fatalf("InvalidatedRetryToken 之后必须换新 token，实际复用了 %q", tokens[0])
	}
}

// TestBackoffHasJitter 是 P0-2 的断言。
//
// 抖动写错的两种方式都不会有症状：取固定种子（值全一样，方差为 0），
// 或偏移量算错成 0（等于没加）。前者让重试排成整齐的脉冲，后者完全无效，
// 但两种情况单测若不看分布就都发现不了。
func TestBackoffHasJitter(t *testing.T) {
	err := &APIError{Class: ClassTransient, StatusCode: 503}

	// attempt=1 时基线是 5s<<1 = 10s，未触到 2 分钟上界。
	want := 10 * time.Second
	lo := time.Duration(float64(want) * (1 - jitterRatio))
	hi := time.Duration(float64(want) * (1 + jitterRatio))

	seen := map[time.Duration]struct{}{}
	for i := 0; i < 100; i++ {
		got := backoffFor(err, 1)
		if got < lo || got > hi {
			t.Fatalf("抖动后 %v 超出 [%v, %v]", got, lo, hi)
		}
		seen[got] = struct{}{}
	}
	if len(seen) < 50 {
		t.Fatalf("100 次只出现 %d 个不同取值，抖动可能被写成了固定值", len(seen))
	}
}

// TestBackoffCapIsHardCeiling 验证抖动不会突破 2 分钟硬上界。
func TestBackoffCapIsHardCeiling(t *testing.T) {
	err := &APIError{Class: ClassTransient, StatusCode: 503}
	for i := 0; i < 100; i++ {
		if got := backoffFor(err, 20); got > 2*time.Minute {
			t.Fatalf("退避 %v 突破了 2 分钟上界", got)
		}
	}
}

// TestWithJitterKeepsZero 验证 0 退避不会被抖成负数或正数。
//
// attempt 之前的路径里 d=0 表示"不需要等"，抖动若把它变成 -3s 会得到
// 一个立即返回的 time.After，看起来无害；但若变成 +3s，就把一次本该
// 立刻重试的操作拖慢了三秒，且难以归因。
func TestWithJitterKeepsZero(t *testing.T) {
	if got := withJitter(0); got != 0 {
		t.Fatalf("0 应原样返回，实际 %v", got)
	}
	if got := withJitter(-time.Second); got != -time.Second {
		t.Fatalf("负数应原样返回，实际 %v", got)
	}
}

// rewriteTransport 把发往任意主机的请求改投到本地测试服务器。
//
// 有了它才能测**公开方法**（LaunchInstance / CreateVcn 等）是否真的带上了
// 幂等令牌。只测 doTarget 是不够的：令牌生成逻辑对、但调用点忘了写
// Idempotent: true，症状和完全没实现一样，而且更难发现——代码看起来是做了的。
type rewriteTransport struct{ target *url.URL }

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	clone.Host = t.target.Host
	return http.DefaultTransport.RoundTrip(clone)
}

// clientFor 造一个所有请求都改投到 srv 的客户端。
func clientFor(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(testCreds(t, testKey(t)),
		WithHTTPClient(&http.Client{Transport: rewriteTransport{target: u}}))
	if err != nil {
		t.Fatal(err)
	}
	c.backoff = func(error, int) time.Duration { return 0 }
	return c
}

// TestCreationCallsCarryRetryToken 逐一验证创建类公开方法都带了令牌。
//
// 这张清单和 client.go 里 Idempotent 字段的注释是同一份约定：新增创建类
// 接口时若忘了标，这个测试不会自动发现——但至少现有的这些被钉住了，
// 而它们恰好是「重试一次就多一份资源」的那批。
func TestCreationCallsCarryRetryToken(t *testing.T) {
	cases := []struct {
		name string
		call func(*Client) error
	}{
		{"LaunchInstance", func(c *Client) error {
			_, err := c.LaunchInstance(context.Background(), "us-ashburn-1", LaunchInstanceRequest{
				Shape: "VM.Standard.E2.1.Micro", AvailabilityDomain: "AD-1",
			})
			return err
		}},
		{"CreateVcn", func(c *Client) error {
			_, err := c.CreateVcn(context.Background(), "us-ashburn-1", CreateVcnRequest{})
			return err
		}},
		{"CreateSubnet", func(c *Client) error {
			_, err := c.CreateSubnet(context.Background(), "us-ashburn-1", CreateSubnetRequest{})
			return err
		}},
		{"CreateInternetGateway", func(c *Client) error {
			_, err := c.CreateInternetGateway(context.Background(), "us-ashburn-1", "", "", "igw")
			return err
		}},
		{"CreatePublicIP", func(c *Client) error {
			_, err := c.CreatePublicIP(context.Background(), "us-ashburn-1", "", "RESERVED", "", "")
			return err
		}},
		{"AttachVolume", func(c *Client) error {
			_, err := c.AttachVolume(context.Background(), "us-ashburn-1", "ocid1.instance.x", "ocid1.volume.x", "", "")
			return err
		}},
		// 500Mbps 用到的四个创建动作。NLB 是计费资源，重试多建一个不会有人
		// 认领，账单上会一直挂着。
		{"CreateNatGateway", func(c *Client) error {
			_, err := c.CreateNatGateway(context.Background(), "us-ashburn-1", CreateNatGatewayRequest{
				VcnID: "ocid1.vcn.x",
			})
			return err
		}},
		{"CreateNetworkLoadBalancer", func(c *Client) error {
			_, err := c.CreateNetworkLoadBalancer(context.Background(), "us-ashburn-1", CreateNLBRequest{
				SubnetID: "ocid1.subnet.x",
			})
			return err
		}},
		{"CreateNLBBackendSet", func(c *Client) error {
			return c.CreateNLBBackendSet(context.Background(), "us-ashburn-1", "ocid1.nlb.x",
				CreateBackendSetRequest{Name: "bs"})
		}},
		{"CreateNLBListener", func(c *Client) error {
			return c.CreateNLBListener(context.Background(), "us-ashburn-1", "ocid1.nlb.x",
				CreateListenerRequest{Name: "lsn", Port: 443})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get(retryTokenHeader)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			if err := tc.call(clientFor(t, srv)); err != nil {
				t.Fatalf("%s 调用失败: %v", tc.name, err)
			}
			if got == "" {
				t.Fatalf("%s 没有携带 %s——重试时会产生重复资源", tc.name, retryTokenHeader)
			}
		})
	}
}

// TestReadOnlyCallsCarryNoRetryToken 验证只读方法不带令牌。
//
// 只读接口带令牌没有副作用，但会让「哪些调用是幂等的」这条信息失真：
// 排查重复资源时，一个到处都带令牌的日志等于没有信息。
func TestReadOnlyCallsCarryNoRetryToken(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(retryTokenHeader)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := clientFor(t, srv)
	if _, err := c.ListInstances(context.Background(), ListInstancesOptions{Region: "us-ashburn-1"}); err != nil {
		t.Fatalf("ListInstances 调用失败: %v", err)
	}
	if got != "" {
		t.Fatalf("只读调用不应携带 %s，实际 %q", retryTokenHeader, got)
	}
}

// TestCallObserverCountsEveryAttempt 验证预算回调按「尝试次数」触发。
//
// 只统计成功会严重低估调用量级：抢机失败的那几轮恰恰是调用最密的时候，
// 而失败一次往往还伴随重试。漏计的结果是预算永远用不满、降频永远不触发，
// 现象上却一切正常。
func TestCallObserverCountsEveryAttempt(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"ServiceUnavailable","message":"try later"}`))
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	var observed int32
	c, err := New(testCreds(t, testKey(t)),
		WithHTTPClient(&http.Client{Transport: rewriteTransport{target: u}}),
		WithMaxRetries(2),
		WithCallObserver(func() { atomic.AddInt32(&observed, 1) }))
	if err != nil {
		t.Fatal(err)
	}
	c.backoff = func(error, int) time.Duration { return 0 }

	// 服务端一直 503，三次尝试全部失败。
	if _, err := c.Do(context.Background(), Request{
		Method: http.MethodGet, Service: ServiceCore, Path: "/instances",
	}, nil); err == nil {
		t.Fatal("应当返回错误")
	}

	if got, want := atomic.LoadInt32(&calls), int32(3); got != want {
		t.Fatalf("服务端收到 %d 次请求，期望 %d 次", got, want)
	}
	if got := atomic.LoadInt32(&observed); got != atomic.LoadInt32(&calls) {
		t.Fatalf("预算回调触发 %d 次，服务端收到 %d 次——失败的重试没有被计数",
			got, atomic.LoadInt32(&calls))
	}
}
