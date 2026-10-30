package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"ocicore/internal/backup"
	"ocicore/internal/store"
)

// 备份相关的上限。
const (
	// maxRestoreBytes 是上传备份的大小上限。
	//
	// 200 MB：本工具的库是几十个账号加几千条审计，正常在几 MB 量级。
	// 给足余量，但不允许无限制——上传是唯一能把任意大小数据写进
	// 内存的入口。
	maxRestoreBytes = 200 << 20

	// restoreConfirmWord 是 L4 复核的回传词。
	//
	// 它由服务端定义，不接受前端传入——否则确认词就退化成
	// 「把前端传来的字符串再传回来一次」。
	restoreConfirmWord = "RESTORE"

	// backupTimeout 是导出/恢复的整体上限。
	//
	// argon2 派生一次 64 MB 要几百毫秒，VACUUM INTO 取决于库大小。
	// 两分钟足够，而更长的等待只会让用户以为界面卡死。
	backupTimeout = 2 * time.Minute
)

// handleExportBackup 导出一份加密备份。
//
// 门槛是 danger（L3）：这份文件里装着全部账号的私钥密文，拿到它
// 加上口令就等于拿到全部云账号。导出本身不改任何东西，但它产生的是
// 一份可离线带走的完整凭据集合。
func (s *Server) handleExportBackup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password         string `json:"password"`
		IncludeMasterKey bool   `json:"includeMasterKey"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Password) == "" {
		writeError(w, http.StatusBadRequest, "password_required", "备份必须设置口令")
		return
	}
	if len(in.Password) < 8 {
		writeError(w, http.StatusBadRequest, "password_too_short",
			"备份口令至少 8 位。这份文件会被拷到别处，口令是它唯一的防线。")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), backupTimeout)
	defer cancel()

	data, err := backup.Export(ctx, s.st, backup.ExportOptions{
		Password:         in.Password,
		DBPath:           s.cfg.DBPath(),
		MasterKeyPath:    s.cfg.MasterKeyPath(),
		IncludeMasterKey: in.IncludeMasterKey,
		AppVersion:       Version,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "export_failed", err.Error())
		return
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "backup_export",
		Target: "database", IP: s.clientIP(r),
		Detail: fmt.Sprintf("%d 字节，含主密钥=%v", len(data), in.IncludeMasterKey),
	})

	name := "oci-tools-" + time.Now().UTC().Format("20060102-150405") + ".ocibak"
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	// 备份不进任何缓存：中间层存一份等于多一处泄露面。
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleRestoreBackup 上传并暂存一份备份，重启后生效。
//
// 门槛是 L4：danger（TOTP）+ 回传 RESTORE。恢复会整体替换数据库，
// 错了之后连「用面板修回来」的入口都没了。
func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	if !s.requireNamedConfirm(w, r, restoreConfirmWord) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRestoreBytes)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "bad_upload",
			"无法解析上传内容（可能是文件过大）: "+err.Error())
		return
	}

	password := r.FormValue("password")
	if password == "" {
		writeError(w, http.StatusBadRequest, "password_required", "请输入备份口令")
		return
	}
	allowMK := r.FormValue("allowMasterKeyChange") == "true"

	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "file_required", "请选择备份文件")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_failed", "读取上传文件失败: "+err.Error())
		return
	}

	res, err := backup.Restore(backup.RestoreOptions{
		Password:             password,
		Data:                 data,
		DBPath:               s.cfg.DBPath(),
		MasterKeyPath:        s.cfg.MasterKeyPath(),
		AllowMasterKeyChange: allowMK,
	})
	if err != nil {
		// 口令错与内容损坏共用一句话，别告诉攻击者「口令对了」。
		if errors.Is(err, backup.ErrWrongPassword) {
			writeError(w, http.StatusBadRequest, "bad_password", err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "restore_failed", err.Error())
		return
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "backup_restore",
		Target: "database", IP: s.clientIP(r),
		Detail: fmt.Sprintf("备份创建于 %s，主密钥不同=%v，已暂存待重启",
			res.Manifest.CreatedAt.Format(time.RFC3339), res.MasterKeyDiffers),
	})

	slog.Warn("已暂存一份备份恢复，重启后生效",
		"createdAt", res.Manifest.CreatedAt, "masterKeyDiffers", res.MasterKeyDiffers)

	writeJSON(w, http.StatusOK, map[string]any{
		"manifest":         res.Manifest,
		"requiresRestart":  true,
		"masterKeyDiffers": res.MasterKeyDiffers,
		"notice": "备份已校验并暂存。**重启服务后生效**——数据库文件正被进程打开，" +
			"在运行中替换它会把库写坏。替换前会自动备份当前库。",
	})
}

// handleGetBackupStatus 返回是否有等待生效的恢复。
//
// 单独一个接口是因为「暂存了但没重启」这个状态要能看见：用户点完恢复
// 看到「成功」就去干别的了，几天后重启发现数据回去了，那时已经想不起
// 自己做过什么。
func (s *Server) handleGetBackupStatus(w http.ResponseWriter, r *http.Request) {
	info, err := backup.PendingRestore(s.cfg.DBPath())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "status_failed", err.Error())
		return
	}
	out := map[string]any{"pending": info != nil, "backupVersion": backup.Version}
	if info != nil {
		out["manifest"] = info.Manifest
		out["stagedAt"] = info.StagedAt
		out["masterKeyPending"] = info.MasterKeyPending
	}
	writeJSON(w, http.StatusOK, out)
}
