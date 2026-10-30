package accountsvc

import (
	"strings"
	"testing"
	"time"

	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

// at 返回一个固定时刻，让 Next 的断言不受真实时钟影响。
func at(offset time.Duration) time.Time {
	return time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC).Add(offset)
}

func failEvent(class string) HealthEvent {
	return HealthEvent{OK: false, Class: class, Message: class + " 出错了",
		AccountFatal: class == ociclient.ClassAuthFailed.String() ||
			class == ociclient.ClassNotAuthorized.String()}
}

// 这是 P2-2 的核心验收：连续 2 次凭据失败才翻红。
//
// 单次 401 可能只是时钟偏移了几分钟或出口代理抖了一下。第一次就给红块，
// 用户会学会无视红块，真正的故障也就淹没在里面了。
func TestNextNeedsTwoAuthFailuresToGoRed(t *testing.T) {
	authFail := failEvent(ociclient.ClassAuthFailed.String())

	var h store.AccountHealth
	// 先成功一次，建立"这个账号本来是好的"这个前提。
	h = Next(h, HealthEvent{OK: true}, at(0))
	if LevelOf(h) != HealthGreen {
		t.Fatalf("成功后应为绿，实际 %s", LevelOf(h))
	}

	// 第一次失败：黄，不红。
	h = Next(h, authFail, at(time.Minute))
	if h.ConsecutiveFailures != 1 {
		t.Errorf("首次失败后连续失败次数 = %d，期望 1", h.ConsecutiveFailures)
	}
	if h.Fatal {
		t.Error("首次失败不应标记为 fatal")
	}
	if got := LevelOf(h); got != HealthYellow {
		t.Errorf("首次失败后等级 = %s，期望 yellow", got)
	}

	// 第二次失败：翻红。
	h = Next(h, authFail, at(2*time.Minute))
	if h.ConsecutiveFailures != 2 {
		t.Errorf("第二次失败后连续失败次数 = %d，期望 2", h.ConsecutiveFailures)
	}
	if !h.Fatal {
		t.Error("连续两次失败后应标记为 fatal")
	}
	if got := LevelOf(h); got != HealthRed {
		t.Errorf("连续两次失败后等级 = %s，期望 red", got)
	}
}

// 恢复成功后计数必须归零，否则"红过一次"会永远粘在账号上：
// 下次再失败一次就又红了，等于阈值退化成 1。
func TestNextResetsAfterRecovery(t *testing.T) {
	authFail := failEvent(ociclient.ClassAuthFailed.String())

	h := store.AccountHealth{ConsecutiveFailures: 1, Fatal: true,
		LastErrorClass: ociclient.ClassAuthFailed.String(), LastErrorAt: timePtr(at(0))}

	h = Next(h, HealthEvent{OK: true}, at(time.Minute))
	if h.ConsecutiveFailures != 0 {
		t.Errorf("恢复后连续失败次数 = %d，期望 0", h.ConsecutiveFailures)
	}
	if h.Fatal {
		t.Error("恢复后不应仍是 fatal")
	}
	if h.LastErrorClass != "" {
		t.Errorf("恢复后应清空错误分类，实际 %q", h.LastErrorClass)
	}
	if h.LastOKAt == nil || !h.LastOKAt.Equal(at(time.Minute)) {
		t.Errorf("恢复后应记录成功时刻，实际 %v", h.LastOKAt)
	}
	if got := LevelOf(h); got != HealthGreen {
		t.Errorf("恢复后等级 = %s，期望 green", got)
	}

	// 恢复之后再失败一次，必须重新从 1 数起。
	h = Next(h, authFail, at(2*time.Minute))
	if h.ConsecutiveFailures != 1 || h.Fatal {
		t.Errorf("恢复后首次失败应重新计数为 1 且不 fatal，实际 %d / %v",
			h.ConsecutiveFailures, h.Fatal)
	}
}

// 非凭据类失败必须打断连击。
//
// 否则"连续 3 次限流 + 1 次 401"会翻红，而那一次 401 很可能同样是代理
// 抖动引起的——限流期间的重试本来就更易失败。
func TestNextNonCredentialFailureBreaksStreak(t *testing.T) {
	authFail := failEvent(ociclient.ClassAuthFailed.String())
	throttled := failEvent(ociclient.ClassThrottled.String())

	h := Next(store.AccountHealth{}, authFail, at(0))
	if h.ConsecutiveFailures != 1 {
		t.Fatalf("连续失败次数 = %d，期望 1", h.ConsecutiveFailures)
	}

	h = Next(h, throttled, at(time.Minute))
	if h.ConsecutiveFailures != 0 {
		t.Errorf("限流应打断连击，实际 %d", h.ConsecutiveFailures)
	}
	if h.Fatal {
		t.Error("限流不应标记 fatal")
	}

	// 再失败一次只是新一轮的第 1 次，不该红。
	h = Next(h, authFail, at(2*time.Minute))
	if h.Fatal || LevelOf(h) == HealthRed {
		t.Error("打断后的首次凭据失败不应翻红")
	}
}

func TestLevelOfMapping(t *testing.T) {
	ok := at(0)
	errAt := at(time.Minute)

	cases := []struct {
		name string
		h    store.AccountHealth
		want HealthLevel
	}{
		{
			// 从来没校验过：导入时跳过校验，或记录被手工清了。
			name: "无任何记录",
			h:    store.AccountHealth{},
			want: HealthUnknown,
		},
		{
			name: "最近一次成功",
			h:    store.AccountHealth{LastOKAt: &errAt, LastErrorAt: &ok},
			want: HealthGreen,
		},
		{
			// 成功之后又失败，最近一次是失败，按分类上色。
			name: "成功后失败-限流",
			h: store.AccountHealth{
				LastOKAt: &ok, LastErrorAt: &errAt,
				LastErrorClass: ociclient.ClassThrottled.String(),
			},
			want: HealthYellow,
		},
		{
			name: "成功后失败-配额",
			h: store.AccountHealth{
				LastOKAt: &ok, LastErrorAt: &errAt,
				LastErrorClass: ociclient.ClassQuotaExceeded.String(),
			},
			want: HealthOrange,
		},
		{
			// 单次凭据失败但没到阈值。
			name: "成功后失败-凭据一次",
			h: store.AccountHealth{
				LastOKAt: &ok, LastErrorAt: &errAt, ConsecutiveFailures: 1,
				LastErrorClass: ociclient.ClassAuthFailed.String(),
			},
			want: HealthYellow,
		},
		{
			name: "成功后失败-凭据两次",
			h: store.AccountHealth{
				LastOKAt: &ok, LastErrorAt: &errAt, ConsecutiveFailures: 2, Fatal: true,
				LastErrorClass: ociclient.ClassAuthFailed.String(),
			},
			want: HealthRed,
		},
		{
			// 网络抖动、OCI 5xx：账号本身没问题。
			name: "成功后失败-瞬时",
			h: store.AccountHealth{
				LastOKAt: &ok, LastErrorAt: &errAt,
				LastErrorClass: ociclient.ClassTransient.String(),
			},
			want: HealthGreen,
		},
		{
			// 没有任何分类（非 API 错误，如 DNS 失败）。
			name: "成功后失败-未分类",
			h:    store.AccountHealth{LastOKAt: &ok, LastErrorAt: &errAt},
			want: HealthGreen,
		},
		{
			// fatal 压倒一切：即使 LastOKAt 更晚（比如手工改过库），
			// 也不该显示成绿。
			name: "fatal 优先",
			h: store.AccountHealth{
				LastOKAt: &errAt, LastErrorAt: &ok, Fatal: true,
				LastErrorClass: ociclient.ClassAuthFailed.String(),
			},
			want: HealthRed,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LevelOf(c.h); got != c.want {
				t.Errorf("LevelOf = %s，期望 %s", got, c.want)
			}
		})
	}
}

// 红块文案必须写成"凭据或权限异常"，不能写"已被封号"。
//
// OCI 对「无权限」和「资源不存在」返回同一个错误码，从 API 侧区分不出来。
// 断言成"封号"会让用户直接放弃一个改条策略就能救回来的账号。
func TestRedAdviceDoesNotClaimAccountBanned(t *testing.T) {
	v := HealthView(store.AccountHealth{Fatal: true})
	if v.Level != HealthRed {
		t.Fatalf("等级 = %s，期望 red", v.Level)
	}
	if v.Advice != HealthAdviceRed {
		t.Errorf("建议文案 = %q", v.Advice)
	}
	for _, banned := range []string{"封号", "被封", "封禁", "停用"} {
		if strings.Contains(v.Advice, banned) {
			t.Errorf("红块文案不应断言账号被%s: %q", banned, v.Advice)
		}
	}
	if !strings.Contains(v.Advice, "控制台") {
		t.Errorf("红块文案应引导用户去控制台确认，实际 %q", v.Advice)
	}
}

func TestHealthViewLabels(t *testing.T) {
	cases := []struct {
		h     store.AccountHealth
		level HealthLevel
		label string
	}{
		{store.AccountHealth{}, HealthUnknown, HealthLabelUnknown},
		{store.AccountHealth{Fatal: true}, HealthRed, HealthLabelRed},
	}
	for _, c := range cases {
		v := HealthView(c.h)
		if v.Level != c.level || v.Label != c.label {
			t.Errorf("HealthView(%+v) = %s/%q，期望 %s/%q",
				c.h, v.Level, v.Label, c.level, c.label)
		}
		// AccountID 之类的字段要原样带出去。
		if v.AccountHealth.AccountID != c.h.AccountID {
			t.Errorf("AccountID 丢失: %q", v.AccountHealth.AccountID)
		}
	}
}

func TestEventFromResult(t *testing.T) {
	ok := EventFromResult(Result{OK: true})
	if !ok.OK {
		t.Error("成功结果应转成 OK 事件")
	}

	res := Result{
		OK:           false,
		ErrorCode:    "NotAuthenticated",
		ErrorText:    "签名不匹配",
		ErrorClass:   ociclient.ClassAuthFailed.String(),
		AccountFatal: true,
	}
	ev := EventFromResult(res)
	if ev.OK {
		t.Error("失败结果不应转成 OK 事件")
	}
	if ev.Class != ociclient.ClassAuthFailed.String() {
		t.Errorf("分类 = %q", ev.Class)
	}
	if !ev.AccountFatal {
		t.Error("应保留凭据问题标记")
	}
	// 错误码要拼进消息里：用户排障时第一眼要找的就是那个码。
	if !strings.Contains(ev.Message, "NotAuthenticated") {
		t.Errorf("消息里应包含原始错误码，实际 %q", ev.Message)
	}
}

// 超长错误要截断。OCI 会把整段策略文档塞进 message，列表页的 tooltip 装不下。
func TestEventFromResultTruncatesLongMessage(t *testing.T) {
	long := ""
	for i := 0; i < 1000; i++ {
		long += "错"
	}
	ev := EventFromResult(Result{OK: false, ErrorText: long})
	n := len([]rune(ev.Message))
	if n > maxHealthMessageLen+1 {
		t.Errorf("消息长度 = %d，应截断到 %d 附近", n, maxHealthMessageLen)
	}
	if !strings.Contains(ev.Message, "…") {
		t.Error("截断后应有省略号")
	}
}

func timePtr(t time.Time) *time.Time { return &t }

