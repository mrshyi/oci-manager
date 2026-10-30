package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"strings"
	"testing"

	"ocicore/internal/config"
	"ocicore/internal/cryptobox"
	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

func testBox(t *testing.T) *cryptobox.Box {
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

// 主密钥文件缺失时，只有「库里没有密文」才允许新建。
//
// 反过来的情形是灾难：库里全是密文，新建的密钥会让它们永久解不开，
// 而且新密钥立刻占住了用户还可能找回旧密钥的那个路径。
func TestEnsureMasterKeyMayBeCreated(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}

	// 1) 全新目录：库和密钥都不存在，允许新建。
	if err := ensureMasterKeyMayBeCreated(cfg); err != nil {
		t.Fatalf("全新目录应允许新建主密钥: %v", err)
	}

	// 2) 有库、有账号，但没有 master.key——模拟密钥丢失或只恢复了数据库。
	st, err := store.Open(cfg.DBPath(), testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if _, err := st.CreateAccount(context.Background(), store.NewAccount{
		Alias:         "东京主号",
		Code:          "TYO",
		TenancyOCID:   "ocid1.tenancy.oc1..aaaamaster",
		UserOCID:      "ocid1.user.oc1..aaaauser",
		Fingerprint:   ociclient.FingerprintOf(&key.PublicKey),
		PrivateKeyPEM: string(keyPEM),
		DefaultRegion: "ap-tokyo-1",
	}); err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	err = ensureMasterKeyMayBeCreated(cfg)
	if err == nil {
		t.Fatal("库里有密文而主密钥文件缺失时应当拒绝启动")
	}
	// 拒绝的同时必须给出可执行的出路，否则用户只会看到"启动失败"。
	if !strings.Contains(err.Error(), "OCICORE_ALLOW_NEW_MASTER_KEY") ||
		!strings.Contains(err.Error(), cfg.MasterKeyPath()) {
		t.Errorf("错误信息应给出恢复路径与显式放行的办法，实际: %v", err)
	}

	// 3) 显式放行后照旧新建。
	cfg.AllowNewMasterKey = true
	if err := ensureMasterKeyMayBeCreated(cfg); err != nil {
		t.Errorf("显式放行后应允许新建: %v", err)
	}

	// 4) 主密钥文件存在时直接放行，不必探测库。
	if err := os.WriteFile(cfg.MasterKeyPath(), []byte(strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.AllowNewMasterKey = false
	if err := ensureMasterKeyMayBeCreated(cfg); err != nil {
		t.Errorf("主密钥文件存在时应直接放行: %v", err)
	}
}
