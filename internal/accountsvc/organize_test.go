package accountsvc

import (
	"strings"
	"testing"
	"time"

	"ocicore/internal/store"
)

func TestNormalizeTagsTrimsAndDropsEmpty(t *testing.T) {
	got, err := NormalizeTags([]string{"  东京 ", "", "   ", "主力"})
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if len(got) != 2 || got[0] != "东京" || got[1] != "主力" {
		t.Fatalf("去空白与丢空值不对：%v", got)
	}
}

// 大小写不同的同名标签必须合并：用户连着打 "JP" 和 "jp" 是想表达同一个
// 标签，而不是想要两个只差大小写的分类。保留第一次出现的写法。
func TestNormalizeTagsDedupesCaseInsensitively(t *testing.T) {
	got, err := NormalizeTags([]string{"JP", "jp", "Jp"})
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if len(got) != 1 || got[0] != "JP" {
		t.Fatalf("应当只留第一次出现的写法「JP」，实际 %v", got)
	}
}

func TestNormalizeTagsLimits(t *testing.T) {
	// 单标签超长。
	long := strings.Repeat("东", MaxTagLen+1)
	if _, err := NormalizeTags([]string{long}); err == nil {
		t.Fatal("超长标签应当被拒绝")
	}
	// 刚好到上限。
	exact := strings.Repeat("东", MaxTagLen)
	if _, err := NormalizeTags([]string{exact}); err != nil {
		t.Fatalf("刚好到上限应当通过: %v", err)
	}
	// 数量超限。
	many := make([]string, MaxTagsPerAccount+1)
	for i := range many {
		many[i] = string(rune('a' + i%26))
	}
	if _, err := NormalizeTags(many); err == nil {
		t.Fatal("超过数量上限应当被拒绝")
	}
}

func TestNormalizeTagsRejectsControlChars(t *testing.T) {
	if _, err := NormalizeTags([]string{"东京\x00"}); err == nil {
		t.Fatal("控制字符应当被拒绝")
	}
}

func TestValidateMeta(t *testing.T) {
	ok := store.AccountMeta{GroupName: "  日本  ", Note: "主力号\n第二行"}
	if err := ValidateMeta(&ok); err != nil {
		t.Fatalf("应当通过: %v", err)
	}
	// 顺带把首尾空白去掉。
	if ok.GroupName != "日本" {
		t.Errorf("分组名应当被 trim，实际 %q", ok.GroupName)
	}
	// 换行是合法的：备注是多行文本。
	if ok.Note != "主力号\n第二行" {
		t.Errorf("备注不该被动过，实际 %q", ok.Note)
	}

	bad := []struct {
		name string
		m    store.AccountMeta
	}{
		{"分组名过长", store.AccountMeta{GroupName: strings.Repeat("组", MaxGroupNameLen+1)}},
		{"备注过长", store.AccountMeta{Note: strings.Repeat("备", MaxNoteLen+1)}},
		{"权重越界", store.AccountMeta{SortWeight: MaxSortWeight + 1}},
		{"权重负越界", store.AccountMeta{SortWeight: -MaxSortWeight - 1}},
		{"分组名含控制字符", store.AccountMeta{GroupName: "日\x00本"}},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			m := c.m
			if err := ValidateMeta(&m); err == nil {
				t.Fatal("应当被拒绝")
			}
		})
	}

	if err := ValidateMeta(nil); err == nil {
		t.Fatal("nil 应当被拒绝")
	}
}

func TestExpiryDays(t *testing.T) {
	now := time.Date(2026, 10, 30, 14, 30, 0, 0, time.UTC)

	day := func(y int, m time.Month, d int) *time.Time {
		t := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		return &t
	}

	cases := []struct {
		name string
		at   *time.Time
		want int
	}{
		// 按自然日算，不是按 24 小时：现在 14:30，到明天零点算 1 天。
		{"明天", day(2026, 10, 31), 1},
		{"今天", day(2026, 10, 30), 0},
		{"昨天", day(2026, 10, 29), -1},
		{"一周后", day(2026, 11, 6), 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := expiryDays(c.at, now)
			if got == nil {
				t.Fatal("不该返回 nil")
			}
			if *got != c.want {
				t.Fatalf("= %d，期望 %d", *got, c.want)
			}
		})
	}

	if got := expiryDays(nil, now); got != nil {
		t.Fatalf("没设到期时间应当返回 nil，实际 %d", *got)
	}
	zero := time.Time{}
	if got := expiryDays(&zero, now); got != nil {
		t.Fatalf("零值时间应当当成没设置，实际 %d", *got)
	}
}

func TestMergeSortsByWeight(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	accounts := []store.Account{
		{ID: "a", Alias: "A", CreatedAt: base},
		{ID: "b", Alias: "B", CreatedAt: base.Add(time.Hour)},
		{ID: "c", Alias: "C", CreatedAt: base.Add(2 * time.Hour)},
	}
	meta := map[string]store.AccountMeta{
		"a": {AccountID: "a", SortWeight: 0},
		"b": {AccountID: "b", SortWeight: 10},
		"c": {AccountID: "c", SortWeight: 0},
	}

	views := Merge(accounts, nil, meta, nil, base)
	// b 权重最高排第一；a 与 c 同权重，保持传入顺序（创建时间升序）。
	if views[0].ID != "b" || views[1].ID != "a" || views[2].ID != "c" {
		t.Fatalf("排序不对：%v %v %v", views[0].ID, views[1].ID, views[2].ID)
	}
}

// 权重全为 0 时（默认情况）顺序必须与没有这个功能之前完全一致：
// 按创建时间升序。用稳定排序就是为了这一点。
func TestMergeKeepsCreationOrderWhenNoWeight(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	accounts := []store.Account{
		{ID: "a", CreatedAt: base},
		{ID: "b", CreatedAt: base.Add(time.Hour)},
		{ID: "c", CreatedAt: base.Add(2 * time.Hour)},
	}
	views := Merge(accounts, nil, nil, nil, base)
	for i, want := range []string{"a", "b", "c"} {
		if views[i].ID != want {
			t.Fatalf("第 %d 个应当是 %s，实际 %s", i, want, views[i].ID)
		}
	}
}

func TestMergeAlwaysReturnsTagSlice(t *testing.T) {
	views := Merge([]store.Account{{ID: "a"}}, nil, nil, nil, time.Now())
	// 空数组而不是 nil：前端可以无脑 v-for，不必先判空。
	if views[0].Tags == nil {
		t.Fatal("没有标签时应当是空数组而不是 nil")
	}
	if len(views[0].Tags) != 0 {
		t.Fatalf("不该凭空造出标签：%v", views[0].Tags)
	}
}

func TestGroupsOrdering(t *testing.T) {
	views := []View{
		{Account: store.Account{ID: "a"}, Meta: store.AccountMeta{GroupName: "日本"}},
		{Account: store.Account{ID: "b"}, Meta: store.AccountMeta{GroupName: "美国", SortWeight: 5}},
		{Account: store.Account{ID: "c"}, Meta: store.AccountMeta{GroupName: ""}},
		{Account: store.Account{ID: "d"}, Meta: store.AccountMeta{GroupName: "日本"}},
	}
	got := Groups(views)
	want := []string{"美国", "日本", ""}
	if len(got) != len(want) {
		t.Fatalf("分组数量不对：%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("分组顺序 = %v，期望 %v", got, want)
		}
	}
}

// 未分组是兜底桶，不是用户划出来的分类，必须永远排最后——
// 哪怕组内某个账号的权重很高。
func TestGroupsUngroupedAlwaysLast(t *testing.T) {
	views := []View{
		{Account: store.Account{ID: "a"}, Meta: store.AccountMeta{GroupName: "", SortWeight: 999}},
		{Account: store.Account{ID: "b"}, Meta: store.AccountMeta{GroupName: "日本", SortWeight: -5}},
	}
	got := Groups(views)
	if len(got) != 2 || got[0] != "日本" || got[1] != "" {
		t.Fatalf("未分组应当排最后，实际 %v", got)
	}
}
