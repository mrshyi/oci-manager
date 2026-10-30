package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// seedOrgAccount 建一个账号，返回它的 ID。
func seedOrgAccount(t *testing.T, st *Store, alias, code, suffix string) string {
	t.Helper()
	acc, err := st.CreateAccount(context.Background(), testAccount(t, alias, code, suffix))
	if err != nil {
		t.Fatalf("建账号失败: %v", err)
	}
	return acc.ID
}

func TestAccountTagsReplaceSemantics(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id := seedOrgAccount(t, st, "东京号", "TYO", "tag1")

	if err := st.SetAccountTags(ctx, id, []string{"东京", "主力"}); err != nil {
		t.Fatalf("写标签失败: %v", err)
	}
	got, err := st.AccountTags(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("标签数量 = %d，期望 2：%v", len(got), got)
	}

	// 整体替换：只留一个，另一个必须消失。这是 PUT 语义的核心，
	// 写成"追加"的话前端取消勾选就永远删不掉标签。
	if err := st.SetAccountTags(ctx, id, []string{"备用"}); err != nil {
		t.Fatalf("替换标签失败: %v", err)
	}
	got, err = st.AccountTags(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "备用" {
		t.Fatalf("替换后应当只剩「备用」，实际 %v", got)
	}

	// 清空。
	if err := st.SetAccountTags(ctx, id, nil); err != nil {
		t.Fatalf("清空标签失败: %v", err)
	}
	got, _ = st.AccountTags(ctx, id)
	if len(got) != 0 {
		t.Fatalf("清空后应当为空，实际 %v", got)
	}
}

func TestAccountTagsUnknownAccount(t *testing.T) {
	st := newTestStore(t)
	err := st.SetAccountTags(context.Background(), "nope", []string{"x"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("给不存在的账号打标签应当返回 ErrNotFound，实际 %v", err)
	}
}

func TestAccountTagsCascadeOnDelete(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id := seedOrgAccount(t, st, "东京号", "TYO", "tag2")

	if err := st.SetAccountTags(ctx, id, []string{"东京"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAccountMeta(ctx, AccountMeta{AccountID: id, GroupName: "日本"}); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteAccount(ctx, id); err != nil {
		t.Fatalf("删账号失败: %v", err)
	}

	// 外键带 ON DELETE CASCADE：账号没了，标签和信息也该没了。
	// 否则标签统计里会出现一堆挂不到任何账号上的幽灵标签。
	usage, err := st.TagUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 0 {
		t.Fatalf("账号删除后标签应当一并清除，实际 %v", usage)
	}
	meta, err := st.GetAccountMeta(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if meta.GroupName != "" {
		t.Fatalf("账号删除后附加信息应当一并清除，实际 %+v", meta)
	}
}

func TestTagUsageOrdering(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	a := seedOrgAccount(t, st, "A号", "AAA", "u1")
	b := seedOrgAccount(t, st, "B号", "BBB", "u2")
	c := seedOrgAccount(t, st, "C号", "CCC", "u3")

	_ = st.SetAccountTags(ctx, a, []string{"东京", "主力"})
	_ = st.SetAccountTags(ctx, b, []string{"东京"})
	_ = st.SetAccountTags(ctx, c, []string{"大阪", "东京"})

	usage, err := st.TagUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 3 {
		t.Fatalf("应当有 3 个标签，实际 %d：%v", len(usage), usage)
	}
	// 用得多的排前面：筛选器里用户最可能点的是最常用的那个。
	if usage[0].Tag != "东京" || usage[0].Count != 3 {
		t.Fatalf("排第一的应当是「东京」(3)，实际 %+v", usage[0])
	}
	// 次数相同的按标签名升序，保证顺序稳定（否则每次刷新顺序都在跳）。
	if usage[1].Tag != "主力" || usage[2].Tag != "大阪" {
		t.Fatalf("同次数的标签应当按名字升序，实际 %+v", usage[1:])
	}
}

func TestAllAccountTagsGroupsByAccount(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	a := seedOrgAccount(t, st, "A号", "AAA", "g1")
	b := seedOrgAccount(t, st, "B号", "BBB", "g2")

	_ = st.SetAccountTags(ctx, a, []string{"东京", "主力"})
	_ = st.SetAccountTags(ctx, b, []string{"大阪"})

	all, err := st.AllAccountTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all[a]) != 2 || len(all[b]) != 1 {
		t.Fatalf("按账号分组的结果不对：%v", all)
	}
}

func TestAccountMetaUpsert(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id := seedOrgAccount(t, st, "东京号", "TYO", "meta1")

	// 没填过时返回零值而不是错误：「没设置」是正常状态。
	m, err := st.GetAccountMeta(ctx, id)
	if err != nil {
		t.Fatalf("读取未设置的附加信息不该报错: %v", err)
	}
	if m.AccountID != id || m.GroupName != "" || m.ExpiresAt != nil {
		t.Fatalf("未设置时应当是零值，实际 %+v", m)
	}

	expires := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	if err := st.SetAccountMeta(ctx, AccountMeta{
		AccountID: id, GroupName: "日本", Note: "主力号",
		ExpiresAt: &expires, SortWeight: 10,
	}); err != nil {
		t.Fatalf("写附加信息失败: %v", err)
	}

	m, err = st.GetAccountMeta(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.GroupName != "日本" || m.Note != "主力号" || m.SortWeight != 10 {
		t.Fatalf("读回来的值不对: %+v", m)
	}
	if m.ExpiresAt == nil || !m.ExpiresAt.Equal(expires) {
		t.Fatalf("到期时间不对: %v", m.ExpiresAt)
	}

	// 再写一次走 upsert，不能变成两行，也不能报主键冲突。
	if err := st.SetAccountMeta(ctx, AccountMeta{
		AccountID: id, GroupName: "关西", SortWeight: 5,
	}); err != nil {
		t.Fatalf("第二次写入失败: %v", err)
	}
	m, _ = st.GetAccountMeta(ctx, id)
	if m.GroupName != "关西" || m.SortWeight != 5 {
		t.Fatalf("upsert 后应当是新值，实际 %+v", m)
	}
	// 整体替换语义：这次没传 expiresAt，到期时间应当被清掉。
	if m.ExpiresAt != nil {
		t.Fatalf("未传的字段应当被清空，实际 %v", m.ExpiresAt)
	}
}

func TestAccountMetaUnknownAccount(t *testing.T) {
	st := newTestStore(t)
	err := st.SetAccountMeta(context.Background(), AccountMeta{AccountID: "nope"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("给不存在的账号写附加信息应当返回 ErrNotFound，实际 %v", err)
	}
}

// 到期时间存 NULL 而不是 0：0 会被 unixToTime 当成零值时间，
// 与「没设置」混在一起就分不开了。
func TestAccountMetaNullExpiryRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id := seedOrgAccount(t, st, "东京号", "TYO", "meta2")

	if err := st.SetAccountMeta(ctx, AccountMeta{AccountID: id, Note: "x"}); err != nil {
		t.Fatal(err)
	}
	var raw *int64
	row := st.DB().QueryRowContext(ctx, `SELECT expires_at FROM account_meta WHERE account_id = ?`, id)
	var v any
	if err := row.Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != nil {
		if n, ok := v.(int64); ok {
			raw = &n
		}
		t.Fatalf("未设置到期时间时列应当是 NULL，实际 %v", v)
	}
	_ = raw
}
