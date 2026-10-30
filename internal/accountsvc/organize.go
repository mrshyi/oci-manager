// Package accountsvc 提供账号层面的业务操作：连通性校验、凭据探测、组织信息。
package accountsvc

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"ocicore/internal/store"
)

// 标签与附加信息的边界。
//
// 这些上限不是为了省存储，是为了防呆：标签是自由文本，一旦允许任意长度
// 和任意数量，列表页的筛选器会被一个误粘贴的整段文本撑爆，而用户自己
// 也找不到想要的标签。
const (
	// MaxTagsPerAccount 是单个账号最多能打的标签数。
	MaxTagsPerAccount = 20
	// MaxTagLen 是单个标签的最大字符数（按 rune 计，中文也算一个）。
	MaxTagLen = 24
	// MaxGroupNameLen 是分组名的最大字符数。
	MaxGroupNameLen = 32
	// MaxNoteLen 是备注的最大字符数。
	MaxNoteLen = 200
	// MaxSortWeight 是排序权重的绝对值上限。
	MaxSortWeight = 1000
)

// View 是一个账号在列表页上的完整形态：账号本体 + 标签 + 附加信息 + 健康度。
//
// 内嵌 store.Account 而不是复制字段：账号的列还在增加，复制一份意味着
// 每加一列都要在两处同步，漏一处就是「接口少了字段」。
type View struct {
	store.Account
	Tags []string          `json:"tags"`
	Meta store.AccountMeta `json:"meta"`
	// Health 是算好的色块，不是库里的原始记录——level/label/advice 三条
	// 都由服务端给，前端不自己判颜色，否则「几天算快到期」「哪种错误算红」
	// 这类规则会散落在两边。
	Health Health `json:"health"`
	// Capability 是上次权限自检的结果。nil 表示还没自检过。
	Capability *CapabilityProbe `json:"capability"`
	// ExpiryDays 是距离到期的天数：负数表示已过期，0 表示今天到期，
	// nil 表示没设到期时间。算在服务端是因为「几天算快到期」这条规则
	// 只该有一份，否则前端每个用到的地方都要自己判一次。
	ExpiryDays *int `json:"expiryDays"`
}

// Merge 把账号、标签、附加信息、健康度合成列表页要的形状，并排好序。
//
// 排序规则：sortWeight 大的在前，同权重保持传入顺序（accounts 本身按
// 创建时间升序）。用稳定排序就是为了这一点——权重全为 0 时（默认情况）
// 列表顺序与没有这个功能之前完全一致。
func Merge(accounts []store.Account, tags map[string][]string,
	meta map[string]store.AccountMeta, health map[string]store.AccountHealth,
	now time.Time) []View {
	views := make([]View, 0, len(accounts))
	for _, a := range accounts {
		m := meta[a.ID]
		m.AccountID = a.ID
		h := health[a.ID]
		h.AccountID = a.ID
		v := View{
			Account:    a,
			Tags:       tags[a.ID],
			Meta:       m,
			Health:     HealthView(h),
			Capability: DecodeCapabilityProbe(a.CapabilityProbe),
			ExpiryDays: expiryDays(m.ExpiresAt, now),
		}
		if v.Tags == nil {
			// 空数组而不是 null：前端可以无脑 v-for，不必先判空。
			v.Tags = []string{}
		}
		views = append(views, v)
	}
	sort.SliceStable(views, func(i, j int) bool {
		return views[i].Meta.SortWeight > views[j].Meta.SortWeight
	})
	return views
}

// Groups 返回分组名的展示顺序：按组内最高权重降序，未分组（空串）永远最后。
//
// 「未分组」排最后而不是最前：它是兜底桶，不是用户主动划出来的分类。
// 排在最前面会把真正的分组挤到下面去。
func Groups(views []View) []string {
	best := make(map[string]int)
	for _, v := range views {
		name := v.Meta.GroupName
		if cur, ok := best[name]; !ok || v.Meta.SortWeight > cur {
			best[name] = v.Meta.SortWeight
		}
	}
	out := make([]string, 0, len(best))
	for name := range best {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool {
		// 未分组固定最后，无论权重。
		if (out[i] == "") != (out[j] == "") {
			return out[j] == ""
		}
		if best[out[i]] != best[out[j]] {
			return best[out[i]] > best[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// NormalizeTags 清洗并校验一组标签，返回可以直接落库的规范形式。
//
// 去重是不区分大小写的：用户连着两次输入 "JP" 和 "jp"，本意一定是同一个
// 标签，而不是想要两个只差大小写的分类。保留第一次出现的写法，让用户看到
// 的是自己先打的那个。
func NormalizeTags(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, t := range raw {
		tag := store.NormalizeTag(t)
		if tag == "" {
			continue
		}
		if !utf8.ValidString(tag) {
			return nil, errors.New("标签包含非法字符")
		}
		if n := utf8.RuneCountInString(tag); n > MaxTagLen {
			return nil, fmt.Errorf("标签 %q 过长（%d 字，上限 %d）", tag, n, MaxTagLen)
		}
		if hasControlChar(tag) {
			return nil, fmt.Errorf("标签 %q 含有控制字符", tag)
		}
		key := strings.ToLower(tag)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, tag)
	}
	if len(out) > MaxTagsPerAccount {
		return nil, fmt.Errorf("一个账号最多 %d 个标签，当前 %d 个", MaxTagsPerAccount, len(out))
	}
	return out, nil
}

// ValidateMeta 校验账号附加信息，并顺手把首尾空白去掉。
func ValidateMeta(m *store.AccountMeta) error {
	if m == nil {
		return errors.New("缺少账号信息")
	}
	m.GroupName = strings.TrimSpace(m.GroupName)
	m.Note = strings.TrimSpace(m.Note)

	if n := utf8.RuneCountInString(m.GroupName); n > MaxGroupNameLen {
		return fmt.Errorf("分组名过长（%d 字，上限 %d）", n, MaxGroupNameLen)
	}
	if n := utf8.RuneCountInString(m.Note); n > MaxNoteLen {
		return fmt.Errorf("备注过长（%d 字，上限 %d）", n, MaxNoteLen)
	}
	if hasControlChar(m.GroupName) || hasControlChar(m.Note) {
		return errors.New("分组名或备注含有控制字符")
	}
	if m.SortWeight > MaxSortWeight || m.SortWeight < -MaxSortWeight {
		return fmt.Errorf("排序权重需在 ±%d 之间", MaxSortWeight)
	}
	return nil
}

// expiryDays 把到期时刻换算成「还剩几天」。
//
// 按自然日算而不是按 24 小时：用户说「还有 3 天到期」指的是日历上的三天，
// 不是 72 小时。所以先把两个时刻截到当天零点再相减。
func expiryDays(expires *time.Time, now time.Time) *int {
	if expires == nil || expires.IsZero() {
		return nil
	}
	day := func(t time.Time) time.Time {
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
	}
	days := int(day(*expires).Sub(day(now)).Hours() / 24)
	return &days
}

// hasControlChar 判断字符串里有没有控制字符。
//
// 换行和制表符单独放行：备注是多行文本，用户会真的敲回车。
func hasControlChar(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\t' || r == '\r' {
			continue
		}
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
