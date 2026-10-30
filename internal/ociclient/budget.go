package ociclient

import "context"

// withoutBudgetKey 是 context 里「这次调用不计入调用预算」的标记。
//
// 用 context 而不是给每个方法加参数：需要跳过预算的调用点很少（目前只有
// 权限自检），而它们散落在多个客户端方法里。加参数意味着每个方法签名
// 都要多一个 bool，且调用方很容易在包装函数里把它丢掉——丢了不会报错，
// 只是预算悄悄多算了几次。
type withoutBudgetKey struct{}

// WithoutBudget 标记「这次调用不计入调用预算」。
//
// 权限自检发的探测请求是「为了搞清楚账号能不能干活」而发的，它本身不是
// 业务动作。把它算进预算，一个刚接入、还没做任何事的账号会先扣掉五次调用；
// 而预算的用意是约束业务操作量，不是惩罚用户点了一下「自检」。
//
// 只对挂在 Client 上的 onCall 回调生效。重试仍然照常——探测失败的账号
// 该退避还是要退避，否则限流期间自检会把情况弄得更糟。
func WithoutBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, withoutBudgetKey{}, true)
}

// budgetSkipped 判断这次调用是否应当跳过预算计数。
func budgetSkipped(ctx context.Context) bool {
	skip, _ := ctx.Value(withoutBudgetKey{}).(bool)
	return skip
}
