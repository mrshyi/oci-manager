// Package accountsvc 提供账号层面的业务操作：连通性校验、健康度、组织信息。
package accountsvc

import (
	"time"

	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

// FatalThreshold 是「翻红」所需的连续凭据类失败次数。
//
// 定在 2 而不是 1 是有原因的：OCI 的 401/403 会因服务器与本机时钟偏移
// （超过 5 分钟即拒绝签名）、出口代理抖动、并发限流误伤而偶发一次。
// 单次失败就翻红，十个账号的页面会长期挂着几个假红块，用户很快学会
// 无视它们——那时真正的红块也就没人看了。
const FatalThreshold = 2

// HealthLevel 是列表页色块的取值。
//
// 只有 red 与 orange 需要人动手：yellow 等一会儿会自己好，green 不用管。
// 这个区分是健康度存在的全部意义——把所有失败都画成一个红点，用户
// 就会把"被限流了"和"密钥被删了"当成同一件事。
type HealthLevel string

const (
	// HealthUnknown 表示还没校验过，或记录被手工清除了。
	HealthUnknown HealthLevel = "unknown"
	// HealthGreen 表示凭据可用。
	//
	// 注意"可用"不等于"这次调用成功了"：网络抖动、OCI 网关 5xx 都会让
	// 校验失败，但账号本身没有任何问题，仍然是绿。健康度回答的是
	// "这份凭据还能不能用"，不是"刚才那次请求成没成"。
	HealthGreen HealthLevel = "green"
	// HealthYellow 表示暂时性的问题：被限流，或凭据失败次数还没到阈值。
	HealthYellow HealthLevel = "yellow"
	// HealthOrange 表示配额用尽。账号是好的，但再操作也做不成。
	HealthOrange HealthLevel = "orange"
	// HealthRed 表示已确认凭据或权限有问题，必须去控制台处理。
	HealthRed HealthLevel = "red"
)

// 面向用户的文案。
//
// 红块的措辞是刻意的：「凭据或权限异常，请到控制台确认账号状态」。
// 不能写「已被封号」——OCI 对「无权限」和「资源不存在」返回同一个错误码
// （NotAuthorizedOrNotFound），从 API 侧根本区分不出来。账号可能只是
// IAM 策略被改了、用户被停用、或 OCID 填错了。写死成封号会让人直接
// 放弃这个账号，而实际上改一条策略就能救回来。
const (
	HealthLabelUnknown = "尚未校验"
	HealthLabelGreen   = "正常"
	HealthLabelYellow  = "暂时异常"
	HealthLabelOrange  = "配额已满"
	HealthLabelRed     = "凭据或权限异常"

	HealthAdviceGreen   = ""
	HealthAdviceUnknown = "还没有校验过这个账号，点「复检」跑一次。"
	HealthAdviceYellow  = "请求被限流或凭据校验偶发失败，稍后会自动复查。"
	HealthAdviceOrange  = "该账号的配额或服务限额已用尽，需在控制台调整限额或释放资源。"
	HealthAdviceRed     = "凭据或权限异常，请到控制台确认账号状态。"
)

// Health 是给前端的健康度视图：库里的事实 + 算出来的色块。
type Health struct {
	store.AccountHealth
	Level  HealthLevel `json:"level"`
	Label  string      `json:"label"`
	Advice string      `json:"advice"`
}

// LevelOf 把一个账号的健康度记录映射成色块。
//
// 判定顺序即优先级：已经确认坏掉（fatal）压倒一切；其次是"最近一次到底
// 是成功还是失败"；最后才按失败分类上色。
func LevelOf(h store.AccountHealth) HealthLevel {
	if h.Fatal {
		return HealthRed
	}
	// 从来没成功也没失败过——导入时跳过了校验，或记录被清了。
	if h.LastOKAt == nil && h.LastErrorAt == nil {
		return HealthUnknown
	}
	// 最近一次是成功：不管历史上连错过多少次，现在这份凭据是好的。
	if h.LastOKAt != nil && (h.LastErrorAt == nil || h.LastOKAt.After(*h.LastErrorAt)) {
		return HealthGreen
	}

	switch h.LastErrorClass {
	case ociclient.ClassAuthFailed.String(), ociclient.ClassNotAuthorized.String():
		// 走到这里说明失败次数还没到阈值（到了就是 fatal）。
		// 单次凭据失败给黄色而不是绿色：不能假装没事，但也不该报警。
		return HealthYellow
	case ociclient.ClassThrottled.String():
		return HealthYellow
	case ociclient.ClassQuotaExceeded.String():
		return HealthOrange
	}
	// 其余（网络抖动、OCI 5xx、参数错误）都说明账号本身是好的。
	return HealthGreen
}

// View 把库里的记录包装成前端要的形状。
func HealthView(h store.AccountHealth) Health {
	level := LevelOf(h)
	v := Health{AccountHealth: h, Level: level}
	switch level {
	case HealthGreen:
		v.Label, v.Advice = HealthLabelGreen, HealthAdviceGreen
	case HealthYellow:
		v.Label, v.Advice = HealthLabelYellow, HealthAdviceYellow
	case HealthOrange:
		v.Label, v.Advice = HealthLabelOrange, HealthAdviceOrange
	case HealthRed:
		v.Label, v.Advice = HealthLabelRed, HealthAdviceRed
	default:
		v.Label, v.Advice = HealthLabelUnknown, HealthAdviceUnknown
	}
	return v
}

// HealthEvent 是一次校验的归一化结果，是 Next 的输入。
//
// 不直接传 accountsvc.Result：健康度只关心四件事——成没成、错在哪一类、
// 错误码说了什么、是不是凭据问题。把整个 Result 传进来会让这个纯函数
// 依赖 Step 列表之类与健康度无关的东西。
type HealthEvent struct {
	OK bool
	// Class 是 ociclient.Class.String()。OK 时忽略。
	Class string
	// Message 面向用户，会原样存库并显示在 tooltip 里。
	Message string
	// AccountFatal 来自 ociclient.Class.AccountFatal()。
	AccountFatal bool
}

// Next 根据上一次的健康度与本次校验结果，算出新的健康度。
//
// 这是纯函数，不碰库也不看时钟以外的东西，所以"连续 2 次才翻红"这条
// 规则可以在表驱动测试里穷举。落库由调用方负责。
func Next(prev store.AccountHealth, ev HealthEvent, now time.Time) store.AccountHealth {
	next := prev
	next.UpdatedAt = now

	if ev.OK {
		next.LastOKAt = &now
		next.ConsecutiveFailures = 0
		next.Fatal = false
		// 清空错误分类，但保留 LastErrorAt：它是"最后一次出错是什么时候"，
		// 是历史事实。LevelOf 靠 LastOKAt 晚于 LastErrorAt 判绿，不靠这两个字段。
		next.LastErrorClass = ""
		next.LastErrorMessage = ""
		return next
	}

	next.LastErrorClass = ev.Class
	next.LastErrorAt = &now
	next.LastErrorMessage = ev.Message

	if ev.AccountFatal {
		next.ConsecutiveFailures = prev.ConsecutiveFailures + 1
	} else {
		// 非凭据类失败（限流、配额、网络抖动）说明不了密钥还能不能用，
		// 所以它既不累加也不保持——直接把连击打断。否则"连错 3 次限流
		// 再来 1 次 401"会翻红，而那次 401 很可能同样是代理抖动引起的。
		next.ConsecutiveFailures = 0
	}
	next.Fatal = next.ConsecutiveFailures >= FatalThreshold
	return next
}

// EventFromResult 把一次连通性校验的结论转成健康度事件。
func EventFromResult(res Result) HealthEvent {
	if res.OK {
		return HealthEvent{OK: true}
	}
	msg := res.ErrorText
	if res.ErrorCode != "" {
		msg = res.ErrorCode + " " + msg
	}
	return HealthEvent{
		OK:           false,
		Class:        res.ErrorClass,
		Message:      truncateHealthMessage(msg),
		AccountFatal: res.AccountFatal,
	}
}

// maxHealthMessageLen 是落库的错误摘要上限。
//
// 原始错误动辄上千字（OCI 会把整段策略文档塞进 message），列表页的
// tooltip 装不下，也没人读。留前 300 字足够定位问题，完整错误仍在
// 审计日志与账号状态里。
const maxHealthMessageLen = 300

func truncateHealthMessage(s string) string {
	runes := []rune(s)
	if len(runes) <= maxHealthMessageLen {
		return s
	}
	return string(runes[:maxHealthMessageLen]) + "…"
}
