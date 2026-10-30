package ociclient

import (
	"crypto/rand"
	"encoding/base64"
)

// retryTokenHeader 是 OCI 的幂等键请求头。
//
// 语义：同一个 retry token 的重复 POST，服务端只会真正执行一次，后续请求
// 返回第一次的结果。这是"创建类接口在超时重试时不产生重复资源"的唯一
// 服务端保证——客户端的 findExisting 只是补偿控制，中间有竞态窗口。
const retryTokenHeader = "opc-retry-token"

// newRetryToken 生成一个 32 字节随机数，base64url 无填充编码。
//
// 长度取 32 字节是因为 OCI 文档要求 token 足够不可预测；base64url 是为了
// 让它可以安全地放进 HTTP 头（不含 +/= 等需要额外处理的字符）。
func newRetryToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败在 Go 里等同于系统随机源不可用。此时任何依赖
		// 随机性的安全机制都已失效，继续发请求比直接崩溃更危险——但这里
		// 不 panic，因为调用方只把 token 当作幂等键而非安全凭据，退回空串
		// 等于"这次不做幂等"，比中断用户操作更可接受。
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}
