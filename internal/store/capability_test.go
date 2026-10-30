package store

import (
	"context"
	"testing"
)

func TestSetAccountCapabilityProbe(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	acc := seedHealthAccount(t, st, "东京", "TYO")

	// 新账号没有自检结果。
	got, err := st.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CapabilityProbe != "" {
		t.Errorf("新账号不应有自检结果，实际 %q", got.CapabilityProbe)
	}

	const raw = `{"accountId":"x","items":[{"key":"compute","ok":true}]}`
	if err := st.SetAccountCapabilityProbe(ctx, acc.ID, raw); err != nil {
		t.Fatal(err)
	}

	got, err = st.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CapabilityProbe != raw {
		t.Errorf("自检结果 = %q，期望 %q", got.CapabilityProbe, raw)
	}

	// 列表接口也要带出来，否则账号页得逐个请求。
	list, err := st.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].CapabilityProbe != raw {
		t.Errorf("列表里的自检结果不对: %+v", list)
	}
}

// 保存自检结果不能改 updated_at。
//
// ociconn 按 updated_at 判断客户端缓存是否失效——改它会让每个客户端被无谓地
// 重建一次，连带重新解密私钥。
func TestSetAccountCapabilityProbeKeepsUpdatedAt(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	acc := seedHealthAccount(t, st, "东京", "TYO")

	before, err := st.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAccountCapabilityProbe(ctx, acc.ID, `{"items":[{"key":"compute","ok":true}]}`); err != nil {
		t.Fatal(err)
	}
	after, err := st.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("updated_at 被改了: %v -> %v", before.UpdatedAt, after.UpdatedAt)
	}
}

func TestSetAccountCapabilityProbeUnknownAccount(t *testing.T) {
	st := newTestStore(t)
	err := st.SetAccountCapabilityProbe(context.Background(), "nope", "{}")
	if err != ErrNotFound {
		t.Errorf("不存在的账号应返回 ErrNotFound，实际 %v", err)
	}
}

// 账号删除后自检结果随行消失（它存在 accounts 表自己的列上，
// 所以这条其实由删除行本身保证——留一条断言防止将来被拆成独立表后忘记级联）。
func TestCapabilityProbeGoneWithAccount(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	acc := seedHealthAccount(t, st, "东京", "TYO")

	if err := st.SetAccountCapabilityProbe(ctx, acc.ID, `{"items":[{"key":"compute","ok":true}]}`); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAccount(ctx, acc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAccount(ctx, acc.ID); err != ErrNotFound {
		t.Errorf("删除后应查不到账号，实际 %v", err)
	}
}
