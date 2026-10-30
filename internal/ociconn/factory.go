// Package ociconn 按账号构建 OCI 客户端。
//
// 独立成包是为了让 httpapi 与各个 service 共用同一条建连路径——
// 私钥解密只发生在这里一处，便于审计。
package ociconn

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"ocicore/internal/ociclient"
	"ocicore/internal/riskguard"
	"ocicore/internal/store"
)

// Factory 按账号 ID 产出 OCI 客户端，并缓存已建好的客户端。
//
// 缓存的意义不在于省内存，而在于避免每次请求都做一次 RSA 私钥解析——
// 那是毫秒级开销，跨账号聚合时会被放大几十倍。
type Factory struct {
	st *store.Store

	mu      sync.RWMutex
	cache   map[string]*cacheEntry
	timeout time.Duration

	// limiter 由所有账号的客户端共享，是唯一能拦住跨账号并发总量的地方。
	// 各操作内部的信号量只管自己那次操作的扇出，管不住彼此叠加。
	limiter *ociclient.Limiter

	// guard 统计每账号的调用量，供任务层降频。可为 nil（不统计）。
	//
	// 只在建客户端时读取，所以必须在服务开始接请求之前挂上，
	// 否则先建好的那批客户端不会计数。
	guard *riskguard.Guard

	// endpoint 覆盖端点解析，仅供测试把客户端指向一个冒充 OCI 的本地服务端。
	// 生产路径保持 nil，走 ociclient.Endpoint。
	endpoint func(ociclient.Service, string) string
}

type cacheEntry struct {
	client *ociclient.Client
	// updatedAt 取自账号行，账号被改过（例如轮换密钥）就作废重建。
	updatedAt time.Time
}

// New 创建工厂。
func New(st *store.Store) *Factory {
	return &Factory{
		st:      st,
		cache:   make(map[string]*cacheEntry),
		timeout: 30 * time.Second,
		limiter: ociclient.NewLimiter(ociclient.DefaultGlobalConcurrency),
	}
}

// SetEndpointResolver 覆盖端点解析。
//
// 只为测试而存在：权限自检这类功能的验收需要"某族返回 403、其余正常"的
// 服务端行为，真实账号上没法反复构造。与 SetRiskGuard 一样，必须在建客户端
// 之前调用——已经缓存下来的客户端不会补挂。
func (f *Factory) SetEndpointResolver(fn func(ociclient.Service, string) string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpoint = fn
}

// SetRiskGuard 挂上调用预算守卫。
//
// 必须在服务开始接请求之前调用：已经建好并缓存下来的客户端不会补挂回调，
// 那批账号在缓存失效前不计数。放在启动流程里（New 之后、Listen 之前）即可。
func (f *Factory) SetRiskGuard(g *riskguard.Guard) { f.guard = g }

// For 返回某账号的客户端。账号自上次建连后被修改过就会重建。
func (f *Factory) For(ctx context.Context, acc *store.Account) (*ociclient.Client, error) {
	f.mu.RLock()
	entry, ok := f.cache[acc.ID]
	f.mu.RUnlock()
	if ok && entry.updatedAt.Equal(acc.UpdatedAt) {
		return entry.client, nil
	}

	client, err := f.build(ctx, acc)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	f.cache[acc.ID] = &cacheEntry{client: client, updatedAt: acc.UpdatedAt}
	f.mu.Unlock()
	return client, nil
}

// ForID 按账号 ID 取客户端。
func (f *Factory) ForID(ctx context.Context, accountID string) (*ociclient.Client, *store.Account, error) {
	acc, err := f.st.GetAccount(ctx, accountID)
	if err != nil {
		return nil, nil, err
	}
	client, err := f.For(ctx, acc)
	if err != nil {
		return nil, acc, err
	}
	return client, acc, nil
}

// Invalidate 丢弃某账号的缓存客户端。账号被删除或密钥轮换后应调用。
func (f *Factory) Invalidate(accountID string) {
	f.mu.Lock()
	delete(f.cache, accountID)
	f.mu.Unlock()
}

func (f *Factory) build(ctx context.Context, acc *store.Account) (*ociclient.Client, error) {
	// 这是全项目唯一的私钥解密入口。返回的凭据只喂给 ociclient，
	// 绝不进入日志、错误信息或 HTTP 响应。
	creds, err := f.st.Credentials(ctx, acc.ID)
	if err != nil {
		return nil, err
	}

	opts := []ociclient.Option{ociclient.WithLimiter(f.limiter)}
	if f.endpoint != nil {
		opts = append(opts, ociclient.WithEndpointResolver(f.endpoint))
	}

	// 预算计数：账号 ID 用闭包捕获，客户端不需要知道本地账号是谁。
	if f.guard != nil {
		accountID := acc.ID
		guard := f.guard
		opts = append(opts, ociclient.WithCallObserver(func() {
			// 每次调用都新起一个带超时的 context：计数是旁路统计，
			// 不能让它的耗时拖住真正那次 API 调用。
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			guard.Record(ctx, accountID)
		}))
	}

	proxyURL, err := f.proxyFor(ctx, acc)
	if err != nil {
		return nil, err
	}
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("账号 %s 的代理地址无效: %w", acc.Alias, err)
		}
		// 用 ociclient 那份带 8 秒拨号超时的传输层，而不是
		// http.DefaultTransport —— 代理是最容易不通的一环，
		// 不设拨号超时会让它一路挂到 30 秒总超时。
		transport := ociclient.NewTransport()
		transport.Proxy = http.ProxyURL(u)
		opts = append(opts, ociclient.WithHTTPClient(&http.Client{
			Transport: transport,
			Timeout:   f.timeout,
		}))
	}
	return ociclient.New(creds, opts...)
}

// proxyFor 取该账号要走的代理地址，未绑定时返回空串（本机直连）。
//
// 这里**不做任何失败回落**：代理解不开或取不到就直接报错，让调用失败。
// 静默回落直连等于在用户不知情的时候把网络隔离撤了，而且恰好发生在
// 代理出问题、他最没在看的时候——用代理的全部目的就是不要那样。
func (f *Factory) proxyFor(ctx context.Context, acc *store.Account) (string, error) {
	proxyID, err := f.st.AccountProxyID(ctx, acc.ID)
	if err != nil {
		return "", err
	}
	if proxyID == "" {
		// 兜底：代理升级成独立实体之前，地址直接存在账号行上。
		// 启动迁移会把它搬进 proxies 表；迁不过去的（例如出口与别的账号冲突）
		// 以密文留在原字段，这里解密后照用，不静默改成直连。
		return f.st.LegacyProxyURL(ctx, acc.ID)
	}
	u, err := f.st.ProxyURL(ctx, proxyID)
	if err != nil {
		return "", fmt.Errorf("账号 %s 的代理不可用: %w", acc.Alias, err)
	}
	return u, nil
}
