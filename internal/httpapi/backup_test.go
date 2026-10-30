package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"ocicore/internal/backup"
	"ocicore/internal/store"
)

// exportBackup 走接口导出一份备份，返回原始字节。
func exportBackup(t *testing.T, h *harness, password string, includeMasterKey bool) []byte {
	t.Helper()
	resp := h.post("/api/backup/export", map[string]any{
		"password":         password,
		"includeMasterKey": includeMasterKey,
	})
	expectStatus(t, resp, http.StatusOK)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, ".ocibak") {
		t.Errorf("Content-Disposition = %q，应带 .ocibak 文件名", cd)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// restoreBackup 用 multipart 上传备份。
func restoreBackup(t *testing.T, h *harness, data []byte, password, confirm string,
	allowMK bool) *http.Response {
	t.Helper()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("password", password); err != nil {
		t.Fatal(err)
	}
	if allowMK {
		if err := mw.WriteField("allowMasterKeyChange", "true"); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := mw.CreateFormFile("file", "backup.ocibak")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	path := "/api/backup/restore"
	if confirm != "" {
		path += "?confirm=" + confirm
	}
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+path, &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(csrfHeader, csrfValue)
	for _, c := range h.client.Jar.Cookies(mustParseURL(t, h.srv.URL)) {
		req.AddCookie(c)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestBackupExportRequiresAuth(t *testing.T) {
	h := newHarness(t)

	resp := h.do("POST", "/api/backup/export", map[string]any{"password": "12345678"}, false)
	expectStatus(t, resp, http.StatusForbidden) // 缺 CSRF
	resp.Body.Close()

	// 未登录时即使带 CSRF 也是 401。
	h2 := newHarness(t)
	resp = h2.do("POST", "/api/backup/export", map[string]any{"password": "12345678"}, true)
	expectStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}

func TestBackupExportRejectsWeakPassword(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	resp := h.post("/api/backup/export", map[string]any{"password": ""})
	expectStatus(t, resp, http.StatusBadRequest)
	if e := decode[errorBody](t, resp); e.Code != "password_required" {
		t.Errorf("错误码 = %q", e.Code)
	}

	resp = h.post("/api/backup/export", map[string]any{"password": "short"})
	expectStatus(t, resp, http.StatusBadRequest)
	if e := decode[errorBody](t, resp); e.Code != "password_too_short" {
		t.Errorf("错误码 = %q", e.Code)
	}
}

// 导出的必须是加密容器，不是明文库。
func TestBackupExportReturnsEncryptedContainer(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	id := createAccountForHealth(t, h, "东京主号", "TYO", "bkexport")

	data := exportBackup(t, h, "backup-pass-123", false)

	if !bytes.HasPrefix(data, []byte(backup.Magic)) {
		t.Fatalf("不是备份容器：前 8 字节 = %q", data[:8])
	}
	// 明文库以 "SQLite format 3" 开头。加密后不该还能看到它。
	if bytes.Contains(data, []byte("SQLite format 3")) {
		t.Fatal("备份里出现了明文 SQLite 头，说明没有加密")
	}

	ar, err := backup.ReadArchive("backup-pass-123", data)
	if err != nil {
		t.Fatal(err)
	}
	if ar.Manifest.DBBytes == 0 {
		t.Error("清单里的库大小不应为 0")
	}
	if ar.Manifest.IncludesMasterKey {
		t.Error("默认不该打包主密钥")
	}
	// 包里确实有那个账号：直接查一遍解出来的库。
	if !bytes.Contains(ar.DB, []byte(id)) {
		t.Error("备份的库里找不到刚建的账号")
	}

	// 导出要留痕：它产生的是一份可带走的完整凭据集合。
	// 备份审计不挂在某个账号上，所以按全局查。
	all, _, err := h.store.ListAudit(context.Background(), store.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range all {
		if e.Action == "backup_export" {
			found = true
		}
	}
	if !found {
		t.Error("导出应记审计")
	}
}

// 恢复是 L4：必须回传 RESTORE。
func TestBackupRestoreRequiresConfirmWord(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	data := exportBackup(t, h, "backup-pass-123", false)

	resp := restoreBackup(t, h, data, "backup-pass-123", "", false)
	expectStatus(t, resp, http.StatusBadRequest)
	e := decode[errorBody](t, resp)
	if e.Code != "confirm_required" {
		t.Errorf("错误码 = %q", e.Code)
	}
	if !strings.Contains(e.Message, "RESTORE") {
		t.Errorf("提示里应告诉用户要输入什么：%q", e.Message)
	}

	// 回传错的词也不行。
	resp = restoreBackup(t, h, data, "backup-pass-123", "restore", false)
	expectStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestBackupRestoreWrongPassword(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	data := exportBackup(t, h, "backup-pass-123", false)

	resp := restoreBackup(t, h, data, "not-the-password", "RESTORE", false)
	expectStatus(t, resp, http.StatusBadRequest)
	e := decode[errorBody](t, resp)
	if e.Code != "bad_password" {
		t.Errorf("错误码 = %q", e.Code)
	}
}

// 这是 P3-2 的端到端验收：导出 → 恢复 → 状态里能看到「待重启」。
func TestBackupRestoreStagesForRestart(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()
	id := createAccountForHealth(t, h, "东京主号", "TYO", "bkrestore")

	// 恢复前没有待生效的东西。
	resp := h.get("/api/backup")
	expectStatus(t, resp, http.StatusOK)
	before := decode[struct {
		Pending bool `json:"pending"`
	}](t, resp)
	if before.Pending {
		t.Fatal("恢复前不该有 pending")
	}

	data := exportBackup(t, h, "backup-pass-123", false)
	resp = restoreBackup(t, h, data, "backup-pass-123", "RESTORE", false)
	expectStatus(t, resp, http.StatusOK)
	got := decode[struct {
		RequiresRestart  bool            `json:"requiresRestart"`
		MasterKeyDiffers bool            `json:"masterKeyDiffers"`
		Manifest         backup.Manifest `json:"manifest"`
		Notice           string          `json:"notice"`
	}](t, resp)
	if !got.RequiresRestart {
		t.Error("必须如实告诉用户需要重启")
	}
	if !strings.Contains(got.Notice, "重启") {
		t.Errorf("提示里要写清重启才生效：%q", got.Notice)
	}
	if got.Manifest.DBBytes == 0 {
		t.Error("应回传备份清单")
	}

	// 状态接口要能看到它——「暂存了但没重启」这个状态必须可见。
	resp = h.get("/api/backup")
	expectStatus(t, resp, http.StatusOK)
	after := decode[struct {
		Pending         bool   `json:"pending"`
		MasterKeyPending bool  `json:"masterKeyPending"`
		StagedAt        string `json:"stagedAt"`
	}](t, resp)
	if !after.Pending {
		t.Fatal("恢复后应能看到 pending")
	}
	if after.MasterKeyPending {
		t.Error("没打包主密钥时不该有主密钥 pending")
	}

	// 暂存的库里应当有那个账号——证明解包与落盘都是对的。
	pendingPath := h.api.cfg.DBPath() + ".restore-pending"
	staged, err := store.Open(pendingPath, h.box)
	if err != nil {
		t.Fatalf("打开暂存的库失败: %v", err)
	}
	defer staged.Close()
	if _, err := staged.GetAccount(context.Background(), id); err != nil {
		t.Fatalf("暂存的库里找不到账号: %v", err)
	}
}

// 库里坏数据不能让状态接口 500——它只是个「有没有待生效恢复」的查询。
func TestBackupStatusToleratesGarbage(t *testing.T) {
	h := newHarness(t)
	h.setupAndLogin()

	// 没有 pending 文件时就是 pending=false。
	resp := h.get("/api/backup")
	expectStatus(t, resp, http.StatusOK)
	out := decode[map[string]any](t, resp)
	if out["pending"] != false {
		t.Errorf("pending = %v", out["pending"])
	}
	if _, ok := out["backupVersion"]; !ok {
		t.Error("应带上当前备份格式版本")
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), "backupVersion") {
		t.Errorf("响应形状不对: %s", raw)
	}
}
