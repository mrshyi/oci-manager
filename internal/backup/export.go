package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"ocicore/internal/store"
)

// 归档里的固定文件名。恢复时按名字找，改名字等于让旧备份失效。
const (
	EntryManifest  = "manifest.json"
	EntryDB        = "oci-tools.db"
	EntryMasterKey = "master.key"
)

// Manifest 是备份的自描述。它在密文里，不是明文头的一部分——
// 创建时间、版本、账号数量这些信息本身也不该随便暴露。
type Manifest struct {
	CreatedAt time.Time `json:"createdAt"`
	// AppVersion 是导出时的程序版本，仅供排障参考。
	AppVersion string `json:"appVersion"`
	// BackupVersion 与文件头的版本号一致，用于交叉校验。
	BackupVersion int `json:"backupVersion"`
	DBBytes       int64 `json:"dbBytes"`
	// DBSHA256 是数据库明文的摘要。恢复时校验，用来区分
	// 「口令对但包内容坏了」和「口令错」——虽然对外都是同一个错。
	DBSHA256 string `json:"dbSha256"`
	// IncludesMasterKey 表示包里带了主密钥。
	//
	// 默认不带：主密钥泄露等于所有账号的私钥泄露，而它本来就该
	// 和数据库分开保管。带上它是给「整机迁移」这种场景的便利，
	// 代价必须让用户知情。
	IncludesMasterKey bool `json:"includesMasterKey"`
	Notice            string `json:"notice"`
}

// ExportOptions 是导出参数。
type ExportOptions struct {
	// Password 用于加密整份备份。空口令直接拒绝。
	Password string
	// DBPath 是当前库文件路径，用于生成同目录下的临时快照。
	DBPath string
	// MasterKeyPath 是主密钥文件路径；IncludeMasterKey 为 true 时打包它。
	MasterKeyPath    string
	IncludeMasterKey bool
	AppVersion       string
	// Now 可注入，测试用。
	Now time.Time
}

// Export 生成一份加密备份。
//
// 步骤：VACUUM INTO 出一份自洽的库快照 → 打成 tar → 整体加密。
// 快照落在库文件同目录（临时文件），导完即删。
func Export(ctx context.Context, st *store.Store, opts ExportOptions) ([]byte, error) {
	if opts.Password == "" {
		return nil, fmt.Errorf("backup: 必须设置备份口令")
	}
	if opts.DBPath == "" {
		return nil, fmt.Errorf("backup: 缺少数据库路径")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	dir := filepath.Dir(opts.DBPath)
	tmp, err := os.CreateTemp(dir, ".snapshot-*.db")
	if err != nil {
		return nil, fmt.Errorf("backup: 创建临时快照失败: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	// VACUUM INTO 要求目标不存在，CreateTemp 已经建了一个空文件。
	if err := os.Remove(tmpPath); err != nil {
		return nil, fmt.Errorf("backup: 清理临时文件失败: %w", err)
	}
	defer os.Remove(tmpPath)

	if err := st.SnapshotTo(ctx, tmpPath); err != nil {
		return nil, err
	}
	dbBytes, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("backup: 读取快照失败: %w", err)
	}

	var masterKey []byte
	if opts.IncludeMasterKey {
		if opts.MasterKeyPath == "" {
			return nil, fmt.Errorf("backup: 选择打包主密钥但没有提供路径")
		}
		masterKey, err = os.ReadFile(opts.MasterKeyPath)
		if err != nil {
			return nil, fmt.Errorf("backup: 读取主密钥失败: %w", err)
		}
	}

	man := Manifest{
		CreatedAt:         now,
		AppVersion:        opts.AppVersion,
		BackupVersion:     Version,
		DBBytes:           int64(len(dbBytes)),
		DBSHA256:          SHA256(dbBytes),
		IncludesMasterKey: len(masterKey) > 0,
		Notice: "备份内含全部账号的私钥密文。未打包主密钥时，恢复到新机器需要同时带上原 master.key，" +
			"否则账号私钥无法解密。",
	}
	if man.IncludesMasterKey {
		man.Notice = "备份内已包含主密钥。主密钥泄露等于所有账号的私钥泄露，请把这份文件当作最高敏感数据保管。"
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := writeTarEntry(tw, EntryManifest, mustJSON(man)); err != nil {
		return nil, err
	}
	if err := writeTarEntry(tw, EntryDB, dbBytes); err != nil {
		return nil, err
	}
	if len(masterKey) > 0 {
		if err := writeTarEntry(tw, EntryMasterKey, masterKey); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("backup: 归档失败: %w", err)
	}

	return Seal(opts.Password, buf.Bytes())
}

func writeTarEntry(tw *tar.Writer, name string, data []byte) error {
	hdr := &tar.Header{
		Name: name,
		Mode: 0o600,
		Size: int64(len(data)),
		// 固定时间戳：同样的内容每次导出得到同样的归档，
		// 便于对比两份备份是否等价。
		ModTime: time.Unix(0, 0).UTC(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("backup: 写入归档头失败: %w", err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("backup: 写入归档内容失败: %w", err)
	}
	return nil
}

func mustJSON(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		// 结构体里没有 chan / func，序列化不可能失败。
		panic("backup: manifest 序列化失败: " + err.Error())
	}
	return b
}

// Archive 是解开后的备份内容。
type Archive struct {
	Manifest  Manifest
	DB        []byte
	MasterKey []byte
}

// ReadArchive 解密并解析一份备份。不改动任何文件。
//
// 恢复前的「先看清包里是什么」也走这里：用户应该在按下恢复之前
// 就知道这份备份是哪天导的、有没有带主密钥。
func ReadArchive(password string, data []byte) (Archive, error) {
	plaintext, err := Open(password, data)
	if err != nil {
		return Archive{}, err
	}

	var out Archive
	tr := tar.NewReader(bytes.NewReader(plaintext))
	for {
		hdr, err := tr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return Archive{}, fmt.Errorf("backup: 解析归档失败: %w", err)
		}
		switch hdr.Name {
		case EntryManifest:
			var man Manifest
			if err := json.NewDecoder(tr).Decode(&man); err != nil {
				return Archive{}, fmt.Errorf("backup: 解析清单失败: %w", err)
			}
			out.Manifest = man
		case EntryDB:
			buf, err := readAll(tr, hdr.Size)
			if err != nil {
				return Archive{}, err
			}
			out.DB = buf
		case EntryMasterKey:
			buf, err := readAll(tr, hdr.Size)
			if err != nil {
				return Archive{}, err
			}
			out.MasterKey = buf
		}
	}

	if len(out.DB) == 0 {
		return Archive{}, fmt.Errorf("backup: 归档里没有数据库文件")
	}
	if out.Manifest.BackupVersion > Version {
		return Archive{}, ErrVersionTooNew
	}
	// 摘要对不上说明包内容坏了。口令已经过了 GCM，所以这不是口令问题。
	if out.Manifest.DBSHA256 != "" && SHA256(out.DB) != out.Manifest.DBSHA256 {
		return Archive{}, fmt.Errorf("backup: 数据库内容与清单不符，备份可能已损坏")
	}
	return out, nil
}

func readAll(r *tar.Reader, size int64) ([]byte, error) {
	if size < 0 || size > 1<<30 {
		return nil, fmt.Errorf("backup: 归档条目大小异常: %d", size)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("backup: 读取归档条目失败: %w", err)
	}
	return buf, nil
}
