package store

import (
	"context"
	"testing"
)

// 输入错误必须带上显式类型：HTTP 层靠它决定「400 + 原文」还是「500 + 通用文案」。
// 早先是靠错误串里有没有 store: 前缀来猜的，这个测试把分类钉死在类型上。
func TestCreateAccountClassifiesInputErrors(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*NewAccount)
	}{
		{"私钥无法解析", func(a *NewAccount) { a.PrivateKeyPEM = "这不是私钥" }},
		{"别名为空", func(a *NewAccount) { a.Alias = "   " }},
		{"代号格式错", func(a *NewAccount) { a.Code = "too-long-code" }},
		{"身份色越界", func(a *NewAccount) { a.ColorIndex = 99 }},
		{"指纹与私钥不匹配", func(a *NewAccount) { a.Fingerprint = "aa:bb:cc:dd" }},
		{"代理地址无效", func(a *NewAccount) { a.ProxyURL = "这不是代理" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			in := testAccount(t, "东京主号", "TYO", "cls")
			tc.mutate(&in)
			if _, err := st.CreateAccount(context.Background(), in); !IsInvalid(err) {
				t.Errorf("应归类为输入错误，实际 err = %v", err)
			}
		})
	}
}

// 内部错误不能被当成输入错误：那会把 500 伪装成 400，并把 SQL 细节回显给前端。
func TestInternalErrorIsNotInvalid(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := st.CreateAccount(ctx, testAccount(t, "东京主号", "TYO", "closed"))
	if err == nil {
		t.Fatal("库已关闭，创建账号应当失败")
	}
	if IsInvalid(err) {
		t.Errorf("内部错误被误判为输入错误: %v", err)
	}
}

func TestIsInvalidOnPlainAndNil(t *testing.T) {
	if IsInvalid(nil) {
		t.Error("nil 不应被判定为输入错误")
	}
	if IsInvalid(ErrNotFound) {
		t.Error("ErrNotFound 不应被判定为输入错误")
	}
}
