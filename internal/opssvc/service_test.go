package opssvc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"path/filepath"
	"strings"
	"testing"

	"ocicore/internal/cryptobox"
	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	key := make([]byte, cryptobox.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	box, err := cryptobox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// seedTestAccount 建一个满足外键与凭据校验的账号。ops_tasks.account_id 有
// 外键，测试必须先有账号。
func seedTestAccount(t *testing.T, st *store.Store) *store.Account {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	acc, err := st.CreateAccount(t.Context(), store.NewAccount{
		Alias:         "测试号",
		Code:          "T1",
		TenancyOCID:   "ocid1.tenancy.oc1..aaaa",
		UserOCID:      "ocid1.user.oc1..aaaauser",
		Fingerprint:   ociclient.FingerprintOf(&key.PublicKey),
		PrivateKeyPEM: string(keyPEM),
		DefaultRegion: "ap-tokyo-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

// 未知 kind 不能静默停在 pending：必须判 failed 并说明原因，否则任务会一直
// 显示「等待中」，用户无从知道它永远不会被执行。
func TestRunUnknownKindFails(t *testing.T) {
	st := newTestStore(t)
	acc := seedTestAccount(t, st)
	svc := New(Deps{Store: st})

	task, err := st.CreateOpsTask(t.Context(), store.OpsTask{
		Kind: "no_such_kind", AccountID: acc.ID, Name: "未知任务",
	})
	if err != nil {
		t.Fatal(err)
	}

	svc.run(context.Background(), task)

	got, err := st.GetOpsTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.OpsFailed {
		t.Fatalf("state = %s，期望 failed", got.State)
	}
	if !strings.Contains(got.LastError, "未知的运维任务类型") {
		t.Fatalf("lastError = %q，期望说明未知类型", got.LastError)
	}
}

// 每个已实现的 kind 都必须真的登记在分派表里。
//
// 忘了登记的表现很隐蔽：任务一直停在 pending，界面显示「等待中」，实际上
// 永远不会被执行，也没有任何错误可看。
func TestImplementedKindsRegistered(t *testing.T) {
	svc := New(Deps{Store: newTestStore(t)})
	for _, kind := range []string{
		store.OpsKindOpenPorts, store.OpsKindChangeIP, store.OpsKindRescue,
		store.OpsKindBulk, store.OpsKindEnable500,
	} {
		if _, ok := svc.handlers[kind]; !ok {
			t.Errorf("%s 没有登记处理器", kind)
		}
	}
}

// 救援任务走到 run 时应当被认成"救援"而不是"未知类型"。
//
// 用一个坏的 spec 来验证：它会在解析阶段失败，不会碰账号凭据，因此不需要
// 假 connector 也能证明分派是通的。
func TestRunRescueDispatched(t *testing.T) {
	st := newTestStore(t)
	acc := seedTestAccount(t, st)
	svc := New(Deps{Store: st})

	task, err := st.CreateOpsTask(t.Context(), store.OpsTask{
		Kind: store.OpsKindRescue, AccountID: acc.ID, Name: "救援", Spec: "{不是 JSON",
	})
	if err != nil {
		t.Fatal(err)
	}

	svc.run(context.Background(), task)

	got, err := st.GetOpsTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.OpsFailed {
		t.Fatalf("state = %s，期望 failed", got.State)
	}
	if strings.Contains(got.LastError, "未知的运维任务类型") {
		t.Fatalf("rescue 未被识别为已实现的类型：%q", got.LastError)
	}
	if !strings.Contains(got.LastError, "解析救援参数失败") {
		t.Fatalf("lastError = %q，期望说明参数解析失败", got.LastError)
	}
}

// 500Mbps 任务走到 run 时也应当被认成已实现的类型。
func TestRunBandwidthDispatched(t *testing.T) {
	st := newTestStore(t)
	acc := seedTestAccount(t, st)
	svc := New(Deps{Store: st})

	task, err := st.CreateOpsTask(t.Context(), store.OpsTask{
		Kind: store.OpsKindEnable500, AccountID: acc.ID, Name: "开启 500Mbps", Spec: "{不是 JSON",
	})
	if err != nil {
		t.Fatal(err)
	}

	svc.run(context.Background(), task)

	got, err := st.GetOpsTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.OpsFailed {
		t.Fatalf("state = %s，期望 failed", got.State)
	}
	if strings.Contains(got.LastError, "未知的运维任务类型") {
		t.Fatalf("enable_500m 未被识别为已实现的类型：%q", got.LastError)
	}
	if !strings.Contains(got.LastError, "解析带宽参数失败") {
		t.Fatalf("lastError = %q，期望说明参数解析失败", got.LastError)
	}
}

// 已取消的任务不该再被执行：run 第一步的 pending→running 会冲突并直接返回。
func TestRunSkipsCanceledTask(t *testing.T) {
	st := newTestStore(t)
	acc := seedTestAccount(t, st)
	svc := New(Deps{Store: st})

	task, err := st.CreateOpsTask(t.Context(), store.OpsTask{
		Kind: "no_such_kind", AccountID: acc.ID, Name: "已取消",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionOpsTask(t.Context(), task.ID, store.OpsCanceled); err != nil {
		t.Fatal(err)
	}
	// 重新读一次，拿到 canceled 状态；调度器捞到的也是这个状态。
	cur, err := st.GetOpsTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}

	svc.run(context.Background(), cur)

	got, err := st.GetOpsTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.OpsCanceled {
		t.Fatalf("state = %s，期望保持 canceled", got.State)
	}
}
