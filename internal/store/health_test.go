package store

import (
	"context"
	"testing"
	"time"
)

// seedHealthAccount 建一个最小可用的账号，供健康度测试挂靠。
func seedHealthAccount(t *testing.T, st *Store, alias, code string) *Account {
	t.Helper()
	// 代号 2–4 个字符且唯一：它和租户 OCID 一起构成唯一约束。
	acc, err := st.CreateAccount(context.Background(), testAccount(t, alias, code, alias))
	if err != nil {
		t.Fatalf("建账号失败: %v", err)
	}
	return acc
}

func TestAccountHealthRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	acc := seedHealthAccount(t, st, "东京", "TYO")

	// 没有记录时返回零值而不是错误："还没校验过"是正常状态。
	h, err := st.GetAccountHealth(ctx, acc.ID)
	if err != nil {
		t.Fatalf("读取不存在的健康度应返回零值，实际错误: %v", err)
	}
	if h.AccountID != acc.ID {
		t.Errorf("AccountID = %q，期望 %q", h.AccountID, acc.ID)
	}
	if h.Fatal || h.ConsecutiveFailures != 0 || h.LastOKAt != nil || h.LastErrorAt != nil {
		t.Errorf("零值记录不应带任何状态，实际 %+v", h)
	}

	okAt := time.Unix(1_800_000_000, 0).UTC()
	errAt := okAt.Add(5 * time.Minute)
	want := AccountHealth{
		AccountID:           acc.ID,
		LastOKAt:            &okAt,
		LastErrorClass:      "AuthFailed",
		LastErrorAt:         &errAt,
		LastErrorMessage:    "NotAuthenticated 签名不匹配",
		ConsecutiveFailures: 2,
		Fatal:               true,
	}
	if err := st.SaveAccountHealth(ctx, want); err != nil {
		t.Fatalf("写入健康度失败: %v", err)
	}

	got, err := st.GetAccountHealth(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConsecutiveFailures != 2 || !got.Fatal {
		t.Errorf("连续失败次数/fatal = %d/%v", got.ConsecutiveFailures, got.Fatal)
	}
	if got.LastErrorClass != "AuthFailed" {
		t.Errorf("错误分类 = %q", got.LastErrorClass)
	}
	if got.LastErrorMessage != want.LastErrorMessage {
		t.Errorf("错误消息 = %q", got.LastErrorMessage)
	}
	if got.LastOKAt == nil || !got.LastOKAt.Equal(okAt) {
		t.Errorf("LastOKAt = %v，期望 %v", got.LastOKAt, okAt)
	}
	if got.LastErrorAt == nil || !got.LastErrorAt.Equal(errAt) {
		t.Errorf("LastErrorAt = %v，期望 %v", got.LastErrorAt, errAt)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt 应被写入")
	}
}

// upsert 语义：第二次写入必须整体覆盖，不能残留上一次的值。
//
// 这是"恢复后必须归零"在存储层的保证——如果 upsert 退化成"只更新非零字段"，
// 已经清零的 consecutive_failures 就永远改不回去了。
func TestAccountHealthUpsertOverwrites(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	acc := seedHealthAccount(t, st, "东京", "TYO")

	errAt := time.Unix(1_800_000_000, 0).UTC()
	if err := st.SaveAccountHealth(ctx, AccountHealth{
		AccountID: acc.ID, LastErrorClass: "AuthFailed", LastErrorAt: &errAt,
		LastErrorMessage: "签名不匹配", ConsecutiveFailures: 2, Fatal: true,
	}); err != nil {
		t.Fatal(err)
	}

	okAt := errAt.Add(time.Minute)
	// 与 accountsvc.Next 的真实行为一致：成功时清空分类与消息、计数归零，
	// 但保留 LastErrorAt（"最后一次出错是什么时候"是历史事实）。
	if err := st.SaveAccountHealth(ctx, AccountHealth{
		AccountID: acc.ID, LastOKAt: &okAt, LastErrorAt: &errAt,
		LastErrorClass: "", LastErrorMessage: "", ConsecutiveFailures: 0, Fatal: false,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetAccountHealth(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fatal || got.ConsecutiveFailures != 0 {
		t.Errorf("恢复后应清零，实际 fatal=%v cf=%d", got.Fatal, got.ConsecutiveFailures)
	}
	if got.LastErrorClass != "" || got.LastErrorMessage != "" {
		t.Errorf("恢复后应清空错误分类与消息，实际 %q / %q",
			got.LastErrorClass, got.LastErrorMessage)
	}
	if got.LastOKAt == nil {
		t.Error("恢复后应记录成功时刻")
	}
	// LastErrorAt 保留：它是历史事实，下一次出错时会被覆盖。
	if got.LastErrorAt == nil {
		t.Error("LastErrorAt 不应被清空")
	}
}

func TestAccountHealthUnknownAccount(t *testing.T) {
	st := newTestStore(t)
	err := st.SaveAccountHealth(context.Background(), AccountHealth{AccountID: "nope"})
	if err != ErrNotFound {
		t.Errorf("给不存在的账号写健康度应返回 ErrNotFound，实际 %v", err)
	}
}

// 删除账号时健康度必须跟着走，否则会留下指向已删账号的孤儿行。
func TestAccountHealthCascadeOnDelete(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	acc := seedHealthAccount(t, st, "东京", "TYO")

	if err := st.SaveAccountHealth(ctx, AccountHealth{
		AccountID: acc.ID, ConsecutiveFailures: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAccount(ctx, acc.ID); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM account_health WHERE account_id = ?`, acc.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("账号删除后应级联删掉健康度，实际剩 %d 行", n)
	}
}

func TestAllAccountHealth(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	a := seedHealthAccount(t, st, "东京", "TYO")
	b := seedHealthAccount(t, st, "大阪", "OSA")
	// 第三个账号不写健康度：列表接口要能容忍"只有部分账号有记录"。

	if err := st.SaveAccountHealth(ctx, AccountHealth{AccountID: a.ID, Fatal: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAccountHealth(ctx, AccountHealth{AccountID: b.ID, ConsecutiveFailures: 1}); err != nil {
		t.Fatal(err)
	}

	all, err := st.AllAccountHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("应返回 2 条记录，实际 %d", len(all))
	}
	if !all[a.ID].Fatal {
		t.Error("a 应为 fatal")
	}
	if all[b.ID].Fatal || all[b.ID].ConsecutiveFailures != 1 {
		t.Errorf("b 的状态不对: %+v", all[b.ID])
	}
}

func TestClearAccountHealth(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	acc := seedHealthAccount(t, st, "东京", "TYO")

	if err := st.SaveAccountHealth(ctx, AccountHealth{AccountID: acc.ID, Fatal: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearAccountHealth(ctx, acc.ID); err != nil {
		t.Fatal(err)
	}

	h, err := st.GetAccountHealth(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if h.Fatal || h.ConsecutiveFailures != 0 {
		t.Errorf("清除后应为零值，实际 %+v", h)
	}

	// 清除不存在的记录不该报错：用户连点两次按钮不该看到错误。
	if err := st.ClearAccountHealth(ctx, acc.ID); err != nil {
		t.Errorf("重复清除应幂等，实际错误: %v", err)
	}
}

// nil 时间必须存 NULL 而不是 0。
//
// 0 会被 unixToTime 当成"零值时间"，与"从来没成功过"混在一起就分不开了
// ——而这两者在前端的显示完全不同（尚未校验 vs 正常）。
func TestAccountHealthNullTimes(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	acc := seedHealthAccount(t, st, "东京", "TYO")

	if err := st.SaveAccountHealth(ctx, AccountHealth{
		AccountID: acc.ID, LastErrorClass: "Transient", LastErrorMessage: "连接超时",
	}); err != nil {
		t.Fatal(err)
	}

	var okAt, errAt *int64
	if err := st.DB().QueryRow(
		`SELECT last_ok_at, last_error_at FROM account_health WHERE account_id = ?`,
		acc.ID).Scan(&okAt, &errAt); err != nil {
		t.Fatal(err)
	}
	if okAt != nil {
		t.Errorf("没成功过时 last_ok_at 应为 NULL，实际 %d", *okAt)
	}
	if errAt != nil {
		t.Errorf("没传 last_error_at 时应为 NULL，实际 %d", *errAt)
	}
}
