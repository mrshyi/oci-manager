package cf

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ocicore/internal/cryptobox"
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
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

const testZoneID = "z0000000000000000000000000000001"

// seed 建一个 zone + 一条跟随 inst-1 的 A 记录，返回绑定 id。
func seed(t *testing.T, st *store.Store, token string) (zonePK, recordPK string) {
	t.Helper()
	ctx := t.Context()
	z, err := st.CreateCFZone(ctx, store.CFZone{
		ZoneID: testZoneID, ZoneName: "example.com",
	}, token)
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.CreateCFRecord(ctx, store.CFRecord{
		ZonePK: z.ID, RecordID: "rec1", Name: "a.example.com",
		Type: "A", TTL: 1, InstanceID: "inst-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return z.ID, r.ID
}

// fakeZoneServer 起一个只会「更新成功」的假 CF。
func fakeZoneServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`))
			return
		}
		buf, _ := json.Marshal(map[string]any{"success": true, "result": map[string]any{
			"id": "rec1", "type": "A", "name": "a.example.com", "content": "203.0.113.9",
		}})
		_, _ = w.Write(buf)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newSvc(st *store.Store, baseURL string) *Service {
	return New(Deps{Store: st, NewClient: func(token string) *Client {
		return NewClient(token, WithBaseURL(baseURL))
	}})
}

// 更新成功后 last_ip 要落库，状态是 updated。
func TestUpdateForInstanceSucceeds(t *testing.T) {
	st := newTestStore(t)
	srv := fakeZoneServer(t, "tok")
	seed(t, st, "tok")

	svc := newSvc(st, srv.URL)
	res := svc.UpdateForInstance(t.Context(), "", "inst-1", "203.0.113.9")

	if !res.Involved() {
		t.Fatal("有绑定时应标记为涉及 DNS")
	}
	if res.Status != StatusUpdated {
		t.Fatalf("状态 = %q，期望 %q（detail: %s）", res.Status, StatusUpdated, res.Detail)
	}
	if res.Records != 1 {
		t.Errorf("记录数 = %d，期望 1", res.Records)
	}

	recs, err := st.ListCFRecords(t.Context(), store.CFRecordFilter{InstanceID: "inst-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].LastIP != "203.0.113.9" {
		t.Fatalf("last_ip 没写回: %+v", recs)
	}
	if recs[0].LastError != "" {
		t.Errorf("成功时不该有错误: %s", recs[0].LastError)
	}
}

// 没有绑定时不涉及 DNS：调用方据此不往任务上写 dns_status，
// 否则每个换 IP 任务都会多出一行「DNS：跳过」的噪音。
func TestUpdateForInstanceWithoutBindingsIsNotInvolved(t *testing.T) {
	st := newTestStore(t)
	svc := newSvc(st, "http://127.0.0.1:1")

	res := svc.UpdateForInstance(t.Context(), "", "inst-1", "203.0.113.9")
	if res.Involved() {
		t.Fatalf("没有绑定不该涉及 DNS: %+v", res)
	}
}

// token 无效时状态是 failed，原因写进绑定行，且文案可操作。
func TestUpdateForInstanceWithBadToken(t *testing.T) {
	st := newTestStore(t)
	srv := fakeZoneServer(t, "right-token")
	_, recordPK := seed(t, st, "wrong-token")

	svc := newSvc(st, srv.URL)
	res := svc.UpdateForInstance(t.Context(), "", "inst-1", "203.0.113.9")

	if res.Status != StatusFailed {
		t.Fatalf("状态 = %q，期望 %q", res.Status, StatusFailed)
	}
	if !strings.Contains(res.Detail, "Zone.DNS") {
		t.Errorf("文案应告诉用户需要什么权限: %s", res.Detail)
	}

	rec, err := st.GetCFRecord(t.Context(), recordPK)
	if err != nil {
		t.Fatal(err)
	}
	if rec.LastError == "" {
		t.Error("失败原因应写回绑定行")
	}
	if rec.LastIP != "" {
		t.Errorf("失败时不该写入 last_ip: %q", rec.LastIP)
	}
}

// 一条成功一条失败时是 partial，且成功那条照样写回。
func TestUpdateForInstancePartial(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()

	z, err := st.CreateCFZone(ctx, store.CFZone{ZoneID: testZoneID, ZoneName: "example.com"}, "tok")
	if err != nil {
		t.Fatal(err)
	}
	// 两条绑定：一条记录 id 正确，一条在 Cloudflare 侧不存在。
	if _, err := st.CreateCFRecord(ctx, store.CFRecord{
		ZonePK: z.ID, RecordID: "rec1", Name: "a.example.com", Type: "A", TTL: 1, InstanceID: "inst-1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateCFRecord(ctx, store.CFRecord{
		ZonePK: z.ID, RecordID: "gone", Name: "b.example.com", Type: "A", TTL: 1, InstanceID: "inst-1",
	}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/gone") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":81044,"message":"Record does not exist."}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"result":{"id":"rec1"}}`))
	}))
	t.Cleanup(srv.Close)

	svc := newSvc(st, srv.URL)
	res := svc.UpdateForInstance(ctx, "", "inst-1", "203.0.113.9")

	if res.Status != StatusPartial {
		t.Fatalf("状态 = %q，期望 %q（detail: %s）", res.Status, StatusPartial, res.Detail)
	}
	if res.Records != 2 {
		t.Errorf("记录数 = %d，期望 2", res.Records)
	}
	if !strings.Contains(res.Detail, "b.example.com") {
		t.Errorf("detail 应点名失败的记录: %s", res.Detail)
	}

	recs, err := st.ListCFRecords(ctx, store.CFRecordFilter{InstanceID: "inst-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Name == "a.example.com" && r.LastIP != "203.0.113.9" {
			t.Errorf("成功的那条应写回 last_ip: %+v", r)
		}
		if r.Name == "b.example.com" && r.LastError == "" {
			t.Errorf("失败的那条应写回原因: %+v", r)
		}
	}
}

// 域名被停用后不再同步：用户停用就是「别再动它」。
func TestUpdateForInstanceSkipsDisabledZone(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	zonePK, _ := seed(t, st, "tok")

	off := false
	if _, err := st.UpdateCFZone(ctx, zonePK, store.CFZoneUpdate{Enabled: &off}); err != nil {
		t.Fatal(err)
	}

	svc := newSvc(st, "http://127.0.0.1:1")
	res := svc.UpdateForInstance(ctx, "", "inst-1", "203.0.113.9")
	if res.Involved() {
		t.Fatalf("停用的域名不该参与同步: %+v", res)
	}
}

// IP 为空时不能把空值写进 Cloudflare——那会把域名的 A 记录清空。
func TestUpdateForInstanceRejectsEmptyIP(t *testing.T) {
	st := newTestStore(t)
	seed(t, st, "tok")

	svc := newSvc(st, "http://127.0.0.1:1")
	res := svc.UpdateForInstance(t.Context(), "", "inst-1", "  ")
	if res.Status != StatusFailed {
		t.Fatalf("空 IP 应判失败，实际 %+v", res)
	}
}

// token 在库里是密文，明文只出现在解密接口里。
func TestTokenIsEncryptedAtRest(t *testing.T) {
	st := newTestStore(t)
	zonePK, _ := seed(t, st, "super-secret-token")

	var cipher, nonce []byte
	if err := st.DB().QueryRow(
		`SELECT token_ciphertext, token_nonce FROM cf_zones WHERE id = ?`, zonePK).
		Scan(&cipher, &nonce); err != nil {
		t.Fatal(err)
	}
	if len(cipher) == 0 || len(nonce) == 0 {
		t.Fatal("token 应该被加密存储")
	}
	if strings.Contains(string(cipher), "super-secret-token") {
		t.Fatal("密文里出现了明文 token")
	}

	got, err := st.CFZoneToken(t.Context(), zonePK)
	if err != nil {
		t.Fatal(err)
	}
	if got != "super-secret-token" {
		t.Fatalf("解密结果 = %q", got)
	}

	// 列表接口用的结构体不能带 token。
	zones, err := st.ListCFZones(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !zones[0].HasToken {
		t.Error("HasToken 应为 true")
	}
}

// 同一个 zone 不能录两次：两条 token 会让「用哪一条」取决于遍历顺序。
func TestDuplicateZoneRejected(t *testing.T) {
	st := newTestStore(t)
	seed(t, st, "tok")

	_, err := st.CreateCFZone(t.Context(), store.CFZone{ZoneID: testZoneID, ZoneName: "example.com"}, "tok2")
	if err == nil {
		t.Fatal("重复的 zone 应被拒绝")
	}
	if !strings.Contains(err.Error(), "已经添加过") {
		t.Errorf("错误文案不够明确: %v", err)
	}
}

// 删域名要连带删掉它的绑定，不留孤儿。
func TestDeleteZoneCascadesRecords(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	zonePK, recordPK := seed(t, st, "tok")

	if err := st.DeleteCFZone(ctx, zonePK); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetCFRecord(ctx, recordPK); err == nil {
		t.Fatal("绑定应随域名一起删除")
	}
}

// 绑定必须指向存在的域名。
func TestCreateRecordRequiresZone(t *testing.T) {
	st := newTestStore(t)
	_, err := st.CreateCFRecord(t.Context(), store.CFRecord{
		ZonePK: "nope", RecordID: "r", Name: "a.example.com", InstanceID: "inst-1",
	})
	if err == nil {
		t.Fatal("不存在的域名应被拒绝")
	}
}

// VerifyZone 会把域名回填到本地。
func TestVerifyZoneBackfillsName(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()

	z, err := st.CreateCFZone(ctx, store.CFZone{ZoneID: testZoneID}, "tok")
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"result":{"id":"` + testZoneID + `","name":"example.com","status":"active"}}`))
	}))
	t.Cleanup(srv.Close)

	svc := newSvc(st, srv.URL)
	got, err := svc.VerifyZone(ctx, z.ID)
	if err != nil {
		t.Fatalf("验证失败: %v", err)
	}
	if got.ZoneName != "example.com" {
		t.Fatalf("域名没回填: %q", got.ZoneName)
	}
	if got.LastSyncAt.IsZero() {
		t.Error("验证成功应推进 last_sync_at")
	}
}

// 验证失败时把原因写回域名行，且不推进 last_sync_at。
func TestVerifyZoneFailureRecordsError(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()

	z, err := st.CreateCFZone(ctx, store.CFZone{ZoneID: testZoneID}, "bad")
	if err != nil {
		t.Fatal(err)
	}
	srv := fakeZoneServer(t, "right")
	svc := newSvc(st, srv.URL)

	if _, err := svc.VerifyZone(ctx, z.ID); err == nil {
		t.Fatal("token 错误应验证失败")
	}
	got, err := st.GetCFZone(ctx, z.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastError == "" {
		t.Error("失败原因应写回")
	}
	if !got.LastSyncAt.IsZero() {
		t.Error("失败不该推进 last_sync_at")
	}
}
