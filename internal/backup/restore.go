package backup

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// pendingFile 记录「有一份恢复已经就绪，等重启生效」。
//
// 为什么恢复不能立刻生效：SQLite 的库文件被进程打开着，WAL 里可能还有
// 未合并的页。在连接活着的时候把文件换掉，轻则读到半截数据，重则把库
// 写坏——而且是在「用户以为已经恢复成功」之后才发现。
//
// 所以恢复分两步：先把包解开、校验、落成 pending 文件，重启时由
// ApplyPendingRestore 在 store.Open 之前完成替换。界面必须如实告诉
// 用户「需要重启」，不能假装已经生效。
const pendingFile = "restore-pending.json"

// PendingInfo 是 pending 文件的元信息。
type PendingInfo struct {
	Manifest Manifest `json:"manifest"`
	// StagedAt 是解包时间。
	StagedAt time.Time `json:"stagedAt"`
	// MasterKeyPending 表示同时暂存了主密钥。
	MasterKeyPending bool `json:"masterKeyPending"`
	// PreviousBackup 是替换前自动备份的路径（替换时才填）。
	PreviousBackup string `json:"previousBackup,omitempty"`
}

// RestoreOptions 是恢复参数。
type RestoreOptions struct {
	Password string
	Data     []byte
	// DBPath 与 MasterKeyPath 来自配置，决定 pending 文件落在哪里。
	DBPath        string
	MasterKeyPath string
	// AllowMasterKeyChange 是「包里带了主密钥且与当前不同」时的二次确认。
	//
	// 这个开关不能由前端默认打开：换掉主密钥会让**当前库里其他账号**
	// 的私钥再也解不开。恢复备份的直觉是「回到过去」，但换主密钥
	// 是另一个方向的不可逆操作。
	AllowMasterKeyChange bool
	Now                  time.Time
}

// RestoreResult 是恢复的结果。
type RestoreResult struct {
	Manifest Manifest
	// MasterKeyDiffers 表示包里的主密钥与当前不同。
	MasterKeyDiffers bool
	// Applied 为 false 表示已就绪但需要重启。
	RequiresRestart bool
}

// Restore 校验并暂存一份备份，等重启时生效。
func Restore(opts RestoreOptions) (RestoreResult, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	archive, err := ReadArchive(opts.Password, opts.Data)
	if err != nil {
		return RestoreResult{}, err
	}

	res := RestoreResult{Manifest: archive.Manifest, RequiresRestart: true}

	if len(archive.MasterKey) > 0 {
		current, err := os.ReadFile(opts.MasterKeyPath)
		switch {
		case err == nil:
			if subtle.ConstantTimeCompare(current, archive.MasterKey) != 1 {
				res.MasterKeyDiffers = true
				if !opts.AllowMasterKeyChange {
					return res, errors.New(
						"backup: 备份内的主密钥与当前不同。恢复它会让当前库中其他账号的私钥无法解密。" +
							"如确认要用备份里的主密钥，请显式勾选「允许替换主密钥」")
				}
			}
		case os.IsNotExist(err):
			// 当前没有主密钥文件（全新机器）。直接落下去即可。
			res.MasterKeyDiffers = true
		default:
			return res, fmt.Errorf("backup: 读取当前主密钥失败: %w", err)
		}
	}

	dir := filepath.Dir(opts.DBPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return res, fmt.Errorf("backup: 创建数据目录失败: %w", err)
	}
	if err := writeFileAtomic(opts.DBPath+".restore-pending", archive.DB); err != nil {
		return res, err
	}

	info := PendingInfo{Manifest: archive.Manifest, StagedAt: now}
	if len(archive.MasterKey) > 0 {
		if err := writeFileAtomic(opts.MasterKeyPath+".restore-pending", archive.MasterKey); err != nil {
			return res, err
		}
		info.MasterKeyPending = true
	}

	raw, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return res, fmt.Errorf("backup: 序列化 pending 信息失败: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, pendingFile), raw); err != nil {
		return res, err
	}
	return res, nil
}

// ApplyPendingRestore 在打开数据库之前执行：把暂存的库换成正式库。
//
// 返回 applied 表示确实换过文件，调用方据此打日志。
//
// 必须在 store.Open 之前调用。打开之后再换文件等于换掉一个正被使用的库。
func ApplyPendingRestore(dbPath, masterKeyPath string) (applied bool, err error) {
	dir := filepath.Dir(dbPath)
	raw, err := os.ReadFile(filepath.Join(dir, pendingFile))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("backup: 读取 pending 信息失败: %w", err)
	}
	var info PendingInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return false, fmt.Errorf("backup: pending 信息损坏: %w", err)
	}

	staged := dbPath + ".restore-pending"
	if _, err := os.Stat(staged); err != nil {
		return false, fmt.Errorf("backup: 暂存的数据库不存在: %w", err)
	}

	// 先把当前库备份一份。恢复本身是「回退」，而回退错了就再也回不去，
	// 所以这一步不做成可选项。
	backupPath := ""
	if _, err := os.Stat(dbPath); err == nil {
		backupPath = dbPath + ".before-restore-" + time.Now().UTC().Format("20060102-150405")
		if err := copyFile(dbPath, backupPath); err != nil {
			return false, fmt.Errorf("backup: 备份当前库失败: %w", err)
		}
	}

	// 删掉 -wal / -shm。留着它们是最隐蔽的坑：主库换成了旧数据，
	// 而旧的 WAL 会在下次打开时被回放上去，得到一个两边都不是的库。
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			return false, fmt.Errorf("backup: 清理 WAL 失败: %w", err)
		}
	}
	if err := os.Rename(staged, dbPath); err != nil {
		return false, fmt.Errorf("backup: 替换数据库失败: %w", err)
	}

	if info.MasterKeyPending {
		mkStaged := masterKeyPath + ".restore-pending"
		if _, err := os.Stat(mkStaged); err == nil {
			if _, err := os.Stat(masterKeyPath); err == nil {
				if err := copyFile(masterKeyPath, masterKeyPath+".before-restore-"+
					time.Now().UTC().Format("20060102-150405")); err != nil {
					return false, fmt.Errorf("backup: 备份当前主密钥失败: %w", err)
				}
			}
			if err := os.Rename(mkStaged, masterKeyPath); err != nil {
				return false, fmt.Errorf("backup: 替换主密钥失败: %w", err)
			}
		}
	}

	// 记下这次替换前的备份路径，供事后追查。
	info.PreviousBackup = backupPath
	if raw, err := json.MarshalIndent(info, "", "  "); err == nil {
		_ = writeFileAtomic(filepath.Join(dir, pendingFile+".applied"), raw)
	}
	if err := os.Remove(filepath.Join(dir, pendingFile)); err != nil && !os.IsNotExist(err) {
		return true, fmt.Errorf("backup: 清理 pending 信息失败: %w", err)
	}
	return true, nil
}

// PendingRestore 返回当前是否有等待生效的恢复。
func PendingRestore(dbPath string) (*PendingInfo, error) {
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(dbPath), pendingFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var info PendingInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("backup: pending 信息损坏: %w", err)
	}
	return &info, nil
}

// writeFileAtomic 写临时文件再 rename，避免中途失败留下半截文件。
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("backup: 创建临时文件失败: %w", err)
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("backup: 写入失败: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("backup: 关闭临时文件失败: %w", err)
	}
	// 备份里有全部凭据的密文，权限必须是 0600。
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("backup: 设置权限失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("backup: 落盘失败: %w", err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, in); err != nil {
		return err
	}
	return writeFileAtomic(dst, buf.Bytes())
}
