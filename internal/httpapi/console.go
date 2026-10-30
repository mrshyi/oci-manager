package httpapi

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ocicore/internal/jobsvc"
	"ocicore/internal/ociclient"
	"ocicore/internal/opssvc"
	"ocicore/internal/store"
)

type consoleRequest struct {
	/** SSH 公钥。Oracle 用它鉴权，只有持有对应私钥的人才连得上控制台。 */
	PublicKey string `json:"publicKey"`
}

// handleCreateConsole 为实例建立串行控制台连接。
//
// 本工具不代管任何 SSH 私钥：公钥由用户提交，返回的连接命令也由用户
// 在自己的终端里执行。面板只负责把 Oracle 那套繁琐的连接串拼好。
func (s *Server) handleCreateConsole(w http.ResponseWriter, r *http.Request) {
	inst, err := s.st.GetInstance(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}

	var req consoleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	key := strings.TrimSpace(req.PublicKey)
	if key == "" {
		writeError(w, http.StatusBadRequest, "missing_key",
			"请提供 SSH 公钥。Oracle 用它鉴权，本工具不会代管你的私钥。")
		return
	}
	if !strings.HasPrefix(key, "ssh-") && !strings.HasPrefix(key, "ecdsa-") {
		writeError(w, http.StatusBadRequest, "invalid_key",
			"这看起来不是 SSH 公钥，应以 ssh-rsa / ssh-ed25519 之类开头")
		return
	}
	// Oracle 的串行控制台只接受 RSA。
	//
	// 提前拦下来是为了给一句能照着做的话——直接发过去的话，Oracle 回的是
	// Invalid ssh public key type "ssh-ed25519"，用户既不知道该换成什么，
	// 也不知道这个限制只针对控制台（实例登录用的密钥没有这个限制）。
	if !strings.HasPrefix(key, "ssh-rsa") {
		writeError(w, http.StatusBadRequest, "console_needs_rsa",
			"Oracle 的串行控制台只接受 RSA 公钥，不支持 ed25519 / ecdsa。"+
				"请另生成一把：ssh-keygen -t rsa -b 4096 -f ~/.ssh/oci_console"+
				"（这个限制只针对控制台，实例登录用的密钥不受影响）")
		return
	}

	client, acc, err := s.conns.ForID(r.Context(), inst.AccountID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "credentials", err.Error())
		return
	}

	// 一台实例只能有一个活跃的控制台连接，否则 Oracle 会返回 409 而用户完全
	// 不知道发生了什么。已有连接时：
	//   - 公钥一致就直接复用；
	//   - 公钥不一致就删掉旧的再建。先前一律复用，换了公钥的人拿到的仍是绑着
	//     旧公钥的连接，照着命令连只会 Permission denied，界面上看不出原因。
	replaced := false
	existing, err := client.ListConsoleConnections(r.Context(), inst.Region, acc.CompartmentOCID, inst.ID)
	if err == nil {
		want := sshKeyMD5Fingerprint(key)
		for i := range existing {
			if existing[i].LifecycleState != "ACTIVE" {
				continue
			}
			if want == "" || strings.EqualFold(existing[i].Fingerprint, want) {
				writeJSON(w, http.StatusOK, consoleResponse(&existing[i]))
				return
			}
			if err := s.replaceConsoleConnection(r.Context(), client, inst.Region, acc.CompartmentOCID,
				inst.ID, existing[i].ID); err != nil {
				writeError(w, http.StatusBadGateway, "console_replace",
					"已有一条用另一把公钥建立的控制台连接，删除它时失败："+shortOCIError(err))
				return
			}
			replaced = true
			break
		}
	}

	conn, err := client.CreateConsoleConnection(r.Context(), inst.Region, acc.CompartmentOCID, inst.ID, key)
	if err != nil {
		writeOCIError(w, err)
		return
	}

	user := userFrom(r.Context())
	detail := ""
	if replaced {
		detail = "替换了用另一把公钥建立的旧连接"
	}
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "console_create", AccountID: inst.AccountID,
		Target: inst.DisplayName, Detail: detail, IP: s.clientIP(r),
	})
	resp := consoleResponse(conn)
	if replaced {
		resp["replaced"] = true
	}
	writeJSON(w, http.StatusOK, resp)
}

// sshKeyMD5Fingerprint 按 OCI 控制台连接记录的格式（ssh-keygen -E md5 的
// aa:bb:… 形式）计算 OpenSSH 公钥的指纹。解析不了时返回空串，调用方应当
// 退回「直接复用」，宁可多复用一次也不要误删别人的连接。
func sshKeyMD5Fingerprint(publicKey string) string {
	fields := strings.Fields(publicKey)
	if len(fields) < 2 {
		return ""
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return ""
	}
	sum := md5.Sum(blob)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = hex.EncodeToString([]byte{b})
	}
	return strings.Join(parts, ":")
}

// replaceConsoleConnection 删除旧连接并等它真正消失。
//
// 删除是异步的：连接会先停在 DELETING，这期间再建新连接 Oracle 仍会回 409。
func (s *Server) replaceConsoleConnection(ctx context.Context, client *ociclient.Client,
	region, compartment, instanceID, connID string) error {

	if err := client.DeleteConsoleConnection(ctx, region, connID); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for {
		conns, err := client.ListConsoleConnections(ctx, region, compartment, instanceID)
		if err == nil {
			busy := false
			for _, c := range conns {
				if c.ID == connID && c.LifecycleState != "DELETED" {
					busy = true
				}
			}
			if !busy {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("等待旧连接删除超时，请稍后重试")
		case <-time.After(3 * time.Second):
		}
	}
}

func consoleResponse(conn *ociclient.InstanceConsoleConnection) map[string]any {
	return map[string]any{
		"id":                   conn.ID,
		"instanceId":           conn.InstanceID,
		"lifecycleState":       conn.LifecycleState,
		"serialConsoleCommand": conn.ConnectionString,
		"vncConsoleCommand":    conn.VncConnectionString,
		// Oracle 返回的命令串里没有 -i，它假设你的默认密钥就是注册的那把。
		// 而控制台要求 RSA，多数人的默认密钥是 ed25519——照抄必然失败，
		// 且失败信息是代理主机的 Permission denied，看起来像控制台本身的问题。
		"notice": "串行控制台适合排查「机器起不来」的场景——改坏 fstab、" +
			"防火墙把自己关在门外时都能救回来。",
		// 两层 ssh 都要显式 -i：只给外层加，内层的 ProxyCommand 仍然会拿默认密钥
		// 去试，然后被拒。写进 ~/.ssh/config 比每次记着加两个 -i 靠谱。
		"keyHint": "命令里是两层 ssh：外层连实例，内层的 ProxyCommand 先连 Oracle 的代理主机，" +
			"两层都要用你注册的那把 RSA 私钥。写进 ~/.ssh/config 最省事：\n\n" +
			`Host instance-console.*.oci.oraclecloud.com
    IdentityFile ~/.ssh/你的私钥
    IdentitiesOnly yes

Host ocid1.instance.*
    IdentityFile ~/.ssh/你的私钥
    IdentitiesOnly yes`,
	}
}

// ---- 引导卷分离与挂载（救援模式）----

func (s *Server) handleDetachBootVolume(w http.ResponseWriter, r *http.Request) {
	client, _, region, ok := s.resolveTarget(w, r)
	if !ok {
		return
	}
	attachmentID := r.URL.Query().Get("attachmentId")
	if attachmentID == "" {
		writeError(w, http.StatusBadRequest, "missing_attachment", "缺少 attachmentId 参数")
		return
	}

	if err := client.DetachBootVolume(r.Context(), region, attachmentID); err != nil {
		writeOCIError(w, err)
		return
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "boot_volume_detach",
		AccountID: r.URL.Query().Get("accountId"), Target: attachmentID, IP: s.clientIP(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"notice": "引导卷已分离。该实例在重新挂载引导卷之前无法启动。",
	})
}

func (s *Server) handleAttachBootVolume(w http.ResponseWriter, r *http.Request) {
	client, _, region, ok := s.resolveTarget(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	instanceID, bootVolumeID := q.Get("instanceId"), q.Get("bootVolumeId")
	if instanceID == "" || bootVolumeID == "" {
		writeError(w, http.StatusBadRequest, "missing_params",
			"需要 instanceId 与 bootVolumeId 两个参数")
		return
	}

	attachment, err := client.AttachBootVolume(r.Context(), region, instanceID, bootVolumeID, "")
	if err != nil {
		writeOCIError(w, err)
		return
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "boot_volume_attach",
		AccountID: q.Get("accountId"), Target: instanceID,
		Detail: bootVolumeID, IP: s.clientIP(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "attachment": attachment})
}

// handleAttachVolume 把一块卷挂到实例上当数据盘。
//
// 救援流程的中间一环：坏机器的引导卷分离之后，挂到一台能 SSH 的机器上，
// mount 起来改里面的文件（补 authorized_keys、还原 fstab……），再挂回去。
// 没有这个接口的话这一步只能去 Oracle 控制台点，救援模式就是半截的。
func (s *Server) handleAttachVolume(w http.ResponseWriter, r *http.Request) {
	client, _, region, ok := s.resolveTarget(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	instanceID, volumeID := q.Get("instanceId"), q.Get("volumeId")
	if instanceID == "" || volumeID == "" {
		writeError(w, http.StatusBadRequest, "missing_params",
			"需要 instanceId 与 volumeId 两个参数")
		return
	}

	// 只允许半虚拟化。iSCSI 挂完还要在客机里跑 iscsiadm 才看得到设备，
	// 而面板没法替用户跑那串命令，给了选项只会让人挂上之后发现 lsblk 里什么都没有。
	attachment, err := client.AttachVolume(r.Context(), region, instanceID, volumeID,
		q.Get("displayName"), ociclient.AttachmentTypeParavirtualized)
	if err != nil {
		writeOCIError(w, err)
		return
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "volume_attach",
		AccountID: q.Get("accountId"), Target: instanceID,
		Detail: volumeID, IP: s.clientIP(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"attachment": attachment,
		"notice": "已挂为数据盘。登录目标实例后用 lsblk 找到新增的设备（通常是 /dev/sdb），" +
			"mount 它的根分区就能改里面的文件。",
	})
}

// handleDetachVolume 分离数据盘挂载。
func (s *Server) handleDetachVolume(w http.ResponseWriter, r *http.Request) {
	client, _, region, ok := s.resolveTarget(w, r)
	if !ok {
		return
	}
	attachmentID := r.URL.Query().Get("attachmentId")
	if attachmentID == "" {
		writeError(w, http.StatusBadRequest, "missing_attachment", "缺少 attachmentId 参数")
		return
	}

	if err := client.DetachVolume(r.Context(), region, attachmentID); err != nil {
		writeOCIError(w, err)
		return
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "volume_detach",
		AccountID: r.URL.Query().Get("accountId"), Target: attachmentID, IP: s.clientIP(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"notice": "已分离。分离前记得先在实例里 umount，否则卷上可能有没落盘的写入。" +
			"接下来可以把它挂回原实例当引导卷。",
	})
}

// ---- 批量操作 ----

type bulkRequest struct {
	InstanceIDs []string `json:"instanceIds"`
	Action      string   `json:"action"`
	Force       bool     `json:"force"`
}

type bulkResult struct {
	InstanceID string `json:"instanceId"`
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
}

// maxBulkSize 是同步执行的上限。
//
// 上限的意义不在于性能，而在于防呆：一次误选把三十台机器全关掉的代价太大。
// 超过这个数不再直接拒绝，而是转成 ops_tasks 后台逐个执行——既保留防呆，
// 又不会因为「太多了」把正常的大批量运维挡在门外。
const maxBulkSize = 20

// maxBulkTaskSize 是单个批量任务的上限。
//
// 200 台按每轮 5 台算约 40 轮，几分钟内能跑完；再往上就该分批提交，否则
// 中途失败时"到底改了哪几台"很难核对。
const maxBulkTaskSize = 200

// bulkTaskMinutes 是批量任务的默认总时限。
const bulkTaskMinutes = 30

// bulkConfirmThreshold 是触发确认词的台数门槛。
//
// 取 5 是因为 1–4 台通常是「点着看」的规模，误选的代价肉眼可辨；从 5 台起
// 列表开始需要滚动，选中状态在视口外，误选很难被发现。
const bulkConfirmThreshold = 5

// bulkCrossAccountWord 是跨账号批量操作要求的确认词。
const bulkCrossAccountWord = "ALL"

// bulkConfirmScope 汇总一批实例涉及哪些账号（去重）。
//
// 查不到的实例直接跳过：本地库尚未同步到的新实例本来就查不到，把它当成
// 异常只会让正常用法也被拦住。
func (s *Server) bulkConfirmScope(ctx context.Context, ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	aliases := make([]string, 0, len(ids))
	for _, id := range ids {
		inst, err := s.st.GetInstance(ctx, id)
		if err != nil {
			continue
		}
		acc, err := s.st.GetAccount(ctx, inst.AccountID)
		if err != nil {
			continue
		}
		if _, ok := seen[acc.Alias]; ok {
			continue
		}
		seen[acc.Alias] = struct{}{}
		aliases = append(aliases, acc.Alias)
	}
	return aliases
}

// requireBulkConfirm 判定批量操作是否需要回传确认词。
//
// 台数一律用请求体里的 instanceIds 长度，不读任何前端传来的 count 字段——
// 那等于让被约束方自己申报该受多大约束。
//
// 查不到所属账号的实例（本地库还没同步到）不单独升级门槛：新建实例在下次
// 同步前就是查不到的，为这个要求 ALL 会把正常用法也挡住。它只在批量够大、
// 又恰好一个别名都算不出来时才退化成 ALL——那时确实没有任何可用于确认的词。
func (s *Server) requireBulkConfirm(w http.ResponseWriter, r *http.Request, ids []string) bool {
	aliases := s.bulkConfirmScope(r.Context(), ids)

	need := ""
	reason := ""
	switch {
	case len(aliases) >= 2:
		need = bulkCrossAccountWord
		reason = "这批实例分属 " + itoa(len(aliases)) + " 个账号（" +
			strings.Join(aliases, "、") + "）"
	case len(ids) >= bulkConfirmThreshold:
		reason = "这次操作会同时影响 " + itoa(len(ids)) + " 台实例"
		if len(aliases) == 1 {
			need = aliases[0]
		} else {
			need = bulkCrossAccountWord
			reason += "，且无法确定它们所属的账号"
		}
	}
	if need == "" {
		return true
	}

	if strings.TrimSpace(r.URL.Query().Get("confirm")) == need {
		return true
	}
	writeError(w, http.StatusBadRequest, "confirm_required",
		reason+"。如确认执行，请在 confirm 参数中回传 "+strconv.Quote(need))
	return false
}

// handleBulkAction 批量执行开关机。
//
// 刻意不支持批量终止：那是 L3 操作，必须逐台输名确认。
func (s *Server) handleBulkAction(w http.ResponseWriter, r *http.Request) {
	var req bulkRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	policy, err := s.st.Settings(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !policy.AllowBulkActions {
		writeError(w, http.StatusForbidden, "bulk_disabled", "批量操作已在设置中禁用")
		return
	}

	action := strings.ToUpper(strings.TrimSpace(req.Action))
	if err := ociclient.ValidateAction(action); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_action", err.Error())
		return
	}
	if isForcefulAction(action) && !req.Force {
		writeError(w, http.StatusBadRequest, "confirm_required",
			"批量强制操作会直接切断电源，请附带 force=true 确认")
		return
	}
	if len(req.InstanceIDs) == 0 {
		writeError(w, http.StatusBadRequest, "empty_selection", "没有选中任何实例")
		return
	}
	if len(req.InstanceIDs) > maxBulkTaskSize {
		writeError(w, http.StatusBadRequest, "too_many",
			"单次批量最多 "+itoa(maxBulkTaskSize)+" 台，请分批提交")
		return
	}
	// 规模与范围的门槛放在 TOTP 之前：先让用户确认「这一下会影响多少台」，
	// 再要验证码。反过来的话，用户输完验证码才被告知还要回传确认词，
	// 就得再输一次验证码（同一个 30 秒窗口未必还在）。
	if !s.requireBulkConfirm(w, r, req.InstanceIDs) {
		return
	}
	// 批量开机不会丢数据也不会断业务，其余批量动作一次波及多台机器，算危险操作。
	if action != "START" && !s.requireDangerTOTP(w, r) {
		return
	}
	// 超过同步上限就转成后台任务：HTTP 请求不该挂在那儿等几十次串行调用，
	// 而且转成任务后进度可查、进程重启也能接着跑。
	if len(req.InstanceIDs) > maxBulkSize {
		s.createBulkTasks(w, r, req.InstanceIDs, action)
		return
	}

	verb := bulkVerb(action)
	who := s.actorOf(r)
	ids := append([]string(nil), req.InstanceIDs...)

	s.submitJob(w, jobsvc.Spec{
		Kind: jobBulkAction, Title: "批量" + verb + " · " + itoa(len(ids)) + " 台实例",
		Timeout: 20 * time.Minute,
	}, func(ctx context.Context, p *jobsvc.Progress) (jobsvc.Outcome, error) {
		results := make([]bulkResult, len(ids))
		var (
			wg   sync.WaitGroup
			done atomic.Int32
		)
		// 并发度压到 3：批量操作往往集中在同一个租户上，
		// 并发太高很容易把这个账号打进限流，反而拖慢所有人。
		sem := make(chan struct{}, 3)

		for i, id := range ids {
			wg.Add(1)
			go func(idx int, instanceID string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				entry := bulkResult{InstanceID: instanceID, Name: instanceID}
				if cached, err := s.st.GetInstance(ctx, instanceID); err == nil {
					entry.Name = cached.DisplayName
				}

				if _, err := s.instances.Action(ctx, instanceID, action); errors.Is(err, store.ErrNotFound) {
					entry.Error = "实例不存在或尚未同步"
					p.Step(entry.Name + "：失败 · " + entry.Error)
				} else if err != nil {
					entry.Error = errorTextOf(err)
					p.Step(entry.Name + "：失败 · " + entry.Error)
				} else {
					entry.OK = true
					p.Step(entry.Name + "：已提交" + verb)
				}
				results[idx] = entry
				p.Set(int(done.Add(1)) * 30 / len(ids))
			}(i, id)
		}
		wg.Wait()

		succeeded := 0
		for _, res := range results {
			if res.OK {
				succeeded++
			}
		}
		s.audit(who, store.AuditEntry{
			Action: "instance_bulk_" + strings.ToLower(action),
			Target: itoa(len(ids)) + " 台实例",
			Detail: "成功 " + itoa(succeeded) + " 台",
		})

		// 逐台等状态落定，进度条按台数推进。
		if succeeded > 0 {
			p.Step("等待实例状态落定")
		}
		waited := 0
		for _, res := range results {
			if !res.OK {
				continue
			}
			from := 30 + 69*waited/succeeded
			to := 30 + 69*(waited+1)/succeeded
			if inst, err := s.waitInstanceSettled(ctx, p, res.InstanceID, from, to, 0); err != nil {
				return jobsvc.Outcome{}, err
			} else if inst != nil {
				p.Step(res.Name + "：" + inst.LifecycleState)
			}
			waited++
		}

		out := jobsvc.Outcome{
			Summary: "成功 " + itoa(succeeded) + " 台，失败 " + itoa(len(ids)-succeeded) + " 台",
			Result: map[string]any{
				"results":   results,
				"succeeded": succeeded,
				"failed":    len(ids) - succeeded,
			},
		}
		if succeeded == 0 {
			return out, errors.New("全部 " + itoa(len(ids)) + " 台都没能提交" + verb)
		}
		return out, nil
	})
}

// createBulkTasks 把超过同步上限的批量操作拆成按账号的运维任务。
//
// 为什么按账号拆：ops_tasks.account_id 是单值且有外键，而每台实例的凭据都
// 挂在它自己的账号上。硬塞进一行会让"这个任务用谁的凭据"变得没有答案。
func (s *Server) createBulkTasks(w http.ResponseWriter, r *http.Request, ids []string, action string) {
	// 去重。同一个实例出现两次会让第二遍撞上「实例正在转换中」，
	// 用户看到一条莫名其妙的失败，还会以为批量只成功了一半。
	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}

	groups := make(map[string][]string, 4)
	order := make([]string, 0, 4)
	var unknown []string
	for _, id := range unique {
		inst, err := s.st.GetInstance(r.Context(), id)
		if err != nil {
			unknown = append(unknown, id)
			continue
		}
		if _, ok := groups[inst.AccountID]; !ok {
			order = append(order, inst.AccountID)
		}
		groups[inst.AccountID] = append(groups[inst.AccountID], id)
	}
	if len(unknown) > 0 {
		// 静默跳过比报错更糟：用户以为 25 台都关了，实际有几台根本没被处理。
		shown := unknown
		if len(shown) > 5 {
			shown = shown[:5]
		}
		writeError(w, http.StatusBadRequest, "unknown_instance",
			"有 "+itoa(len(unknown))+" 台实例不在本地库中（可能尚未同步），"+
				"请先同步实例列表再批量操作："+strings.Join(shown, "、"))
		return
	}

	aliases := s.accountAliases(r.Context())
	deadline := time.Now().Add(bulkTaskMinutes * time.Minute)
	tasks := make([]opsTaskDTO, 0, len(order))
	for _, accountID := range order {
		list := groups[accountID]
		spec, err := opssvc.EncodeBulkSpec(opssvc.BulkSpec{Action: action, InstanceIDs: list})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		// 每轮 BulkChunkSize 台，留一倍余量给重试。
		chunks := (len(list) + opssvc.BulkChunkSize - 1) / opssvc.BulkChunkSize
		alias := aliases[accountID]
		if alias == "" {
			alias = accountID
		}
		task, err := s.st.CreateOpsTask(r.Context(), store.OpsTask{
			Kind:        store.OpsKindBulk,
			AccountID:   accountID,
			Name:        "批量" + bulkActionLabel(action) + "：" + alias + " " + itoa(len(list)) + " 台",
			Spec:        spec,
			MaxAttempts: chunks*2 + 5,
			ExpiresAt:   deadline,
		})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		tasks = append(tasks, toOpsDTO(*task, aliases))
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "instance_bulk_task_" + strings.ToLower(action),
		Target: itoa(len(unique)) + " 台实例",
		Detail: "拆成 " + itoa(len(tasks)) + " 个后台任务", IP: s.clientIP(r),
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"tasks": tasks,
		"notice": "超过 " + itoa(maxBulkSize) + " 台，已转为后台任务逐个执行：" +
			"每 " + itoa(opssvc.BulkChunkSize) + " 台一轮，可在任务页查看进度；" +
			"进程重启后会接着跑。失败的那几台可以点「重试」只重试它们。",
	})
}

// bulkActionLabel 是动作的中文名，用于任务标题。
func bulkActionLabel(action string) string {
	switch action {
	case ociclient.ActionStart:
		return "开机"
	case ociclient.ActionSoftStop:
		return "关机"
	case ociclient.ActionStop:
		return "强制关机"
	case ociclient.ActionSoftReset:
		return "重启"
	case ociclient.ActionReset:
		return "强制重启"
	default:
		return action
	}
}

// bulkVerb 是批量操作在任务标题里的说法。
//
// 与 bulkActionLabel 并存是合并上游 v0.5.x 的过渡状态：两者服务不同的
// 任务系统——bulkActionLabel 给 opssvc 的 ops_tasks 用（超过同步上限的
// 批量），bulkVerb 给 jobsvc 用（同步上限内、异步执行并推进度）。
// 等任务入口统一后收敛成一个，现在不能删任何一个。
func bulkVerb(action string) string {
	switch action {
	case ociclient.ActionStart:
		return "开机"
	case ociclient.ActionStop:
		return "强制关机"
	case ociclient.ActionReset:
		return "强制重启"
	case ociclient.ActionSoftReset:
		return "重启"
	default:
		return "关机"
	}
}

func errorTextOf(err error) string {
	if apiErr, ok := ociclient.AsAPIError(err); ok {
		if apiErr.Code != "" {
			return apiErr.Code + " · " + apiErr.Message
		}
		return apiErr.Message
	}
	return err.Error()
}

// ---- 会话管理 ----

// handleRevokeSessions 强制该用户的全部会话下线（含当前会话）。
//
// 对应设计规格里「会话管理与强制下线」。怀疑凭据泄露时这是第一件该做的事。
func (s *Server) handleRevokeSessions(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if err := s.st.DeleteUserSessions(r.Context(), user.ID); err != nil {
		writeStoreError(w, err)
		return
	}
	s.clearSessionCookie(w, r)
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "sessions_revoke_all", IP: s.clientIP(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "所有会话已下线，请重新登录",
	})
}
