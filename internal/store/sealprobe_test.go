package store

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"ocicore/internal/cryptobox"
)

func probeBox(t *testing.T) *cryptobox.Box {
	t.Helper()
	key := make([]byte, cryptobox.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	box, err := cryptobox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

// 探测的三种结局：库不存在、库存在但没有密文、库里有密文。
// 第三种是启动时拒绝新建主密钥的依据，前两种必须放行——否则全新部署
// 会因为「数据库文件刚被建出来」而拒绝启动。
func TestHasSealedData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.db")
	ctx := context.Background()

	// 1) 库文件还不存在。
	found, err := HasSealedData(path)
	if err != nil {
		t.Fatalf("库不存在时不该报错: %v", err)
	}
	if found {
		t.Error("库不存在时应报告没有密文")
	}
	// 探测本身不能留下副作用：它若顺手建出一个空库，下一次判断就会被自己误导。
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("探测不应创建数据库文件，stat err = %v", err)
	}

	// 2) 库存在，但只是建好了表结构。
	st, err := Open(path, probeBox(t))
	if err != nil {
		t.Fatal(err)
	}
	if found, err := HasSealedData(path); err != nil || found {
		t.Fatalf("空库时 found=%v err=%v，期望 false/nil", found, err)
	}

	// 3) 写入一个账号，私钥密文落库。
	if _, err := st.CreateAccount(ctx, testAccount(t, "东京主号", "TYO", "probe")); err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	found, err = HasSealedData(path)
	if err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	if !found {
		t.Error("库里已有账号私钥密文，应报告存在加密数据")
	}
	if err := st.Close(); err != nil {
		t.Fatalf("关闭数据库失败: %v", err)
	}
}
