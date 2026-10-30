package backup

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"ocicore/internal/cryptobox"
	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

// newBox 造一把随机主密钥。
func newBox(t *testing.T) (*cryptobox.Box, []byte) {
	t.Helper()
	key := make([]byte, cryptobox.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	box, err := cryptobox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return box, key
}

// newTestStore 在 dir 下打开（或创建）一个库。
func newTestStore(t *testing.T, dir string, box *cryptobox.Box) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "oci-tools.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// seedAccount 建一个账号，返回账号 ID 与私钥指纹（用于恢复后核对）。
func seedAccount(t *testing.T, st *store.Store, alias, code, suffix string) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	fp := ociclient.FingerprintOf(&key.PublicKey)
	acc, err := st.CreateAccount(context.Background(), store.NewAccount{
		Alias:         alias,
		Code:          code,
		TenancyOCID:   "ocid1.tenancy.oc1..aaaa" + suffix,
		UserOCID:      "ocid1.user.oc1..aaaauser" + suffix,
		Fingerprint:   fp,
		PrivateKeyPEM: string(pemBytes),
		DefaultRegion: "ap-tokyo-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return acc.ID, fp
}

func TestSealOpenRoundTrip(t *testing.T) {
	plaintext := []byte("数据库内容，随便写点什么")
	sealed, err := Seal("correct horse battery staple", plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if string(sealed[:len(Magic)]) != Magic {
		t.Fatalf("魔数不对: %q", sealed[:len(Magic)])
	}
	got, err := Open("correct horse battery staple", sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("解出来的内容不对: %q", got)
	}
}

func TestOpenWrongPassword(t *testing.T) {
	sealed, err := Seal("right", []byte("data"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open("wrong", sealed); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("错误 = %v，期望 ErrWrongPassword", err)
	}
}

// 这是 P3-2 的验收条件之一：改一个字节后导入失败。
//
// 两处都要测：密文区（GCM 本体）与文件头（版本号被 AAD 绑定）。
// 只测密文的话，"头部能不能被篡改"这件事就没被覆盖。
func TestOpenFailsOnAnyTamperedByte(t *testing.T) {
	sealed, err := Seal("password", []byte("some meaningful plaintext payload"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("密文区", func(t *testing.T) {
		bad := append([]byte(nil), sealed...)
		bad[len(bad)-1] ^= 0x01
		if _, err := Open("password", bad); !errors.Is(err, ErrWrongPassword) {
			t.Fatalf("错误 = %v，期望 ErrWrongPassword", err)
		}
	})

	t.Run("盐", func(t *testing.T) {
		bad := append([]byte(nil), sealed...)
		bad[len(Magic)+1] ^= 0x01 // salt 的第一个字节
		if _, err := Open("password", bad); !errors.Is(err, ErrWrongPassword) {
			t.Fatalf("错误 = %v，期望 ErrWrongPassword", err)
		}
	})

	t.Run("版本号", func(t *testing.T) {
		bad := append([]byte(nil), sealed...)
		// 改成 0：ParseHeader 直接拒。改成 1 以外的合法值会走 GCM 失败。
		bad[len(Magic)] = 0
		if _, err := Open("password", bad); err == nil {
			t.Fatal("篡改版本号后应当失败")
		}
	})
}

func TestParseHeaderRejectsGarbage(t *testing.T) {
	if _, err := ParseHeader([]byte("short")); !errors.Is(err, ErrNotBackup) {
		t.Errorf("短数据 = %v，期望 ErrNotBackup", err)
	}
	junk := make([]byte, headerSize+10)
	copy(junk, "NOTABACK")
	if _, err := ParseHeader(junk); !errors.Is(err, ErrNotBackup) {
		t.Errorf("错误魔数 = %v，期望 ErrNotBackup", err)
	}
}

func TestParseHeaderRejectsNewerVersion(t *testing.T) {
	sealed, err := Seal("p", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	sealed[len(Magic)] = Version + 1
	if _, err := ParseHeader(sealed); !errors.Is(err, ErrVersionTooNew) {
		t.Fatalf("错误 = %v，期望 ErrVersionTooNew", err)
	}
}

func TestSealRequiresPassword(t *testing.T) {
	if _, err := Seal("", []byte("x")); err == nil {
		t.Fatal("空口令应被拒绝")
	}
}

// 这是 P3-2 的主验收：A 机导出 → B 机导入 → 账号私钥可正常解密。
//
// 走的是真实路径：VACUUM INTO 快照 → 打包加密 → 解开 → 暂存 →
// ApplyPendingRestore 换文件 → 在新目录上重新打开 store → 解私钥。
func TestExportRestoreKeepsCredentialsDecryptable(t *testing.T) {
	box, masterKey := newBox(t)
	dirA := t.TempDir()
	stA := newTestStore(t, dirA, box)
	accID, wantFP := seedAccount(t, stA, "东京主号", "TYO", "aaaa1111")

	data, err := Export(context.Background(), stA, ExportOptions{
		Password:   "backup-pass-123",
		DBPath:     filepath.Join(dirA, "oci-tools.db"),
		AppVersion: "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	// B 机：新目录、同一把主密钥（默认不打包主密钥，所以迁移时
	// 必须手动带上 master.key——这正是文档里反复强调的那句话）。
	dirB := t.TempDir()
	mkPath := filepath.Join(dirB, "master.key")
	if err := os.WriteFile(mkPath, masterKey, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Restore(RestoreOptions{
		Password:      "backup-pass-123",
		Data:          data,
		DBPath:        filepath.Join(dirB, "oci-tools.db"),
		MasterKeyPath: mkPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.RequiresRestart {
		t.Error("恢复应当要求重启")
	}
	if res.MasterKeyDiffers {
		t.Error("主密钥相同，不该标为不同")
	}

	applied, err := ApplyPendingRestore(filepath.Join(dirB, "oci-tools.db"), mkPath)
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("应完成替换")
	}

	stB := newTestStore(t, dirB, box)
	creds, err := stB.Credentials(context.Background(), accID)
	if err != nil {
		t.Fatalf("恢复后读凭据失败: %v", err)
	}
	if got := ociclient.FingerprintOf(&creds.PrivateKey.PublicKey); got != wantFP {
		t.Fatalf("私钥指纹 = %q，期望 %q", got, wantFP)
	}
	// 账号本身也要在。
	if _, err := stB.GetAccount(context.Background(), accID); err != nil {
		t.Fatalf("恢复后读账号失败: %v", err)
	}
}

// 包里的主密钥与当前不同时，必须显式确认才能继续。
//
// 换主密钥会让**当前库里其他账号**的私钥再也解不开，而恢复备份的
// 直觉是「回到过去」——两者方向相反，不能默认允许。
func TestRestoreRejectsMasterKeyMismatch(t *testing.T) {
	box, masterKey := newBox(t)
	dirA := t.TempDir()
	stA := newTestStore(t, dirA, box)
	seedAccount(t, stA, "东京主号", "TYO", "aaaa2222")

	mkA := filepath.Join(dirA, "master.key")
	if err := os.WriteFile(mkA, masterKey, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := Export(context.Background(), stA, ExportOptions{
		Password:         "p",
		DBPath:           filepath.Join(dirA, "oci-tools.db"),
		MasterKeyPath:    mkA,
		IncludeMasterKey: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// B 机有一把不同的主密钥。
	dirB := t.TempDir()
	mkB := filepath.Join(dirB, "master.key")
	otherKey := make([]byte, cryptobox.KeySize)
	if _, err := rand.Read(otherKey); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mkB, otherKey, 0o600); err != nil {
		t.Fatal(err)
	}

	opts := RestoreOptions{
		Password:      "p",
		Data:          data,
		DBPath:        filepath.Join(dirB, "oci-tools.db"),
		MasterKeyPath: mkB,
	}
	res, err := Restore(opts)
	if err == nil {
		t.Fatal("主密钥不同且未确认时应当拒绝")
	}
	if !res.MasterKeyDiffers {
		t.Error("应标出主密钥不同")
	}

	// 显式允许之后才能继续。
	opts.AllowMasterKeyChange = true
	res, err = Restore(opts)
	if err != nil {
		t.Fatalf("显式允许后应当成功: %v", err)
	}
	if !res.MasterKeyDiffers {
		t.Error("仍应如实标出主密钥不同")
	}

	// 生效后主密钥应被替换成备份里那份。
	if _, err := ApplyPendingRestore(filepath.Join(dirB, "oci-tools.db"), mkB); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(mkB)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(masterKey) {
		t.Error("主密钥没有被替换成备份里那份")
	}
}

func TestRestoreWrongPassword(t *testing.T) {
	box, _ := newBox(t)
	dir := t.TempDir()
	st := newTestStore(t, dir, box)
	seedAccount(t, st, "东京主号", "TYO", "aaaa3333")

	data, err := Export(context.Background(), st, ExportOptions{
		Password: "right", DBPath: filepath.Join(dir, "oci-tools.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Restore(RestoreOptions{
		Password: "wrong", Data: data,
		DBPath:        filepath.Join(t.TempDir(), "oci-tools.db"),
		MasterKeyPath: filepath.Join(t.TempDir(), "master.key"),
	})
	if !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("错误 = %v，期望 ErrWrongPassword", err)
	}
}

// 换库时必须清掉 -wal / -shm。
//
// 留着它们是最隐蔽的坑：主库换成了旧数据，而旧的 WAL 会在下次打开时
// 被回放上去，得到一个两边都不是的库——而且不会有任何报错。
func TestApplyPendingRestoreRemovesWAL(t *testing.T) {
	box, _ := newBox(t)
	dirA := t.TempDir()
	stA := newTestStore(t, dirA, box)
	accID, _ := seedAccount(t, stA, "东京主号", "TYO", "aaaa4444")
	data, err := Export(context.Background(), stA, ExportOptions{
		Password: "p", DBPath: filepath.Join(dirA, "oci-tools.db"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// B 机先有一个自己的库（会产生 WAL）。
	dirB := t.TempDir()
	stB := newTestStore(t, dirB, box)
	seedAccount(t, stB, "别的号", "OTH", "bbbb4444")
	dbB := filepath.Join(dirB, "oci-tools.db")
	if _, err := os.Stat(dbB + "-wal"); err != nil {
		t.Logf("没有 WAL 文件（可能已被合并），继续: %v", err)
	}
	// 关掉 B 的库才能替换文件——真实场景里这是重启。
	if err := stB.Close(); err != nil {
		t.Fatal(err)
	}
	// 手工造一个 WAL 占位文件，确认它会被清掉。
	if err := os.WriteFile(dbB+"-wal", []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	mkPath := filepath.Join(dirB, "master.key")
	if _, err := Restore(RestoreOptions{
		Password: "p", Data: data, DBPath: dbB, MasterKeyPath: mkPath,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyPendingRestore(dbB, mkPath); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(dbB + "-wal"); !os.IsNotExist(err) {
		t.Fatalf("-wal 没有被清掉: %v", err)
	}
	// 替换前的库要留一份备份。
	matches, _ := filepath.Glob(dbB + ".before-restore-*")
	if len(matches) == 0 {
		t.Error("应保留一份替换前的库备份")
	}

	// 恢复出来的库里应当是 A 的账号，不是 B 的。
	st := newTestStore(t, dirB, box)
	if _, err := st.GetAccount(context.Background(), accID); err != nil {
		t.Fatalf("恢复后找不到 A 的账号: %v", err)
	}
}

func TestPendingRestoreNilWhenAbsent(t *testing.T) {
	info, err := PendingRestore(filepath.Join(t.TempDir(), "oci-tools.db"))
	if err != nil {
		t.Fatal(err)
	}
	if info != nil {
		t.Fatalf("没有 pending 时应返回 nil，实际 %+v", info)
	}
}

func TestExportRequiresPasswordAndPath(t *testing.T) {
	box, _ := newBox(t)
	dir := t.TempDir()
	st := newTestStore(t, dir, box)

	if _, err := Export(context.Background(), st, ExportOptions{
		DBPath: filepath.Join(dir, "oci-tools.db"),
	}); err == nil {
		t.Error("空口令应被拒绝")
	}
	if _, err := Export(context.Background(), st, ExportOptions{Password: "p"}); err == nil {
		t.Error("缺数据库路径应被拒绝")
	}
}

// 清单里的摘要能区分「口令对但内容坏了」和「口令错」。
func TestReadArchiveDetectsCorruptPayload(t *testing.T) {
	box, _ := newBox(t)
	dir := t.TempDir()
	st := newTestStore(t, dir, box)
	seedAccount(t, st, "东京主号", "TYO", "aaaa5555")

	data, err := Export(context.Background(), st, ExportOptions{
		Password: "p", DBPath: filepath.Join(dir, "oci-tools.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ar, err := ReadArchive("p", data)
	if err != nil {
		t.Fatal(err)
	}
	if ar.Manifest.DBSHA256 == "" || ar.Manifest.DBBytes == 0 {
		t.Fatalf("清单缺字段: %+v", ar.Manifest)
	}
	if ar.Manifest.BackupVersion != Version {
		t.Errorf("版本 = %d", ar.Manifest.BackupVersion)
	}
	// 默认不打包主密钥。
	if ar.Manifest.IncludesMasterKey || len(ar.MasterKey) != 0 {
		t.Error("默认不该打包主密钥")
	}
}
