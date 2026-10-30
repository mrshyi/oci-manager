package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ocicore/internal/opssvc"
	"ocicore/internal/store"
)

// openPortsRequest 是「一键放行端口」的创建请求。
//
// 与任务里存的 spec 字段一一对应。单独定义而不是直接复用 opssvc.OpenPortsSpec：
// HTTP 契约与任务快照是两个东西，将来 spec 加字段不该顺带改变接口形状。
type openPortsRequest struct {
	AccountID      string            `json:"accountId"`
	Region         string            `json:"region"`
	SecurityListID string            `json:"securityListId"`
	Source         string            `json:"source"`
	Rules          []opssvc.PortRule `json:"rules"`
}

// handleCreateOpenPortsTask 创建一个「放行端口」运维任务。
//
// 这里只落库、不立即执行：执行由 opssvc 的后台调度器按 ops_tasks 的状态机
// 推进。好处是用户可以暂停/取消，失败后能重试，进程重启也不会丢——这些都是
// 同步执行一次 HTTP 请求给不了的。
func (s *Server) handleCreateOpenPortsTask(w http.ResponseWriter, r *http.Request) {
	var req openPortsRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	req.AccountID = strings.TrimSpace(req.AccountID)
	req.Region = strings.TrimSpace(req.Region)
	req.SecurityListID = strings.TrimSpace(req.SecurityListID)
	req.Source = strings.TrimSpace(req.Source)

	switch {
	case req.AccountID == "":
		writeError(w, http.StatusBadRequest, "bad_request", "缺少 accountId")
		return
	case req.Region == "":
		writeError(w, http.StatusBadRequest, "bad_request", "缺少 region")
		return
	case req.SecurityListID == "":
		writeError(w, http.StatusBadRequest, "bad_request", "缺少 securityListId")
		return
	case len(req.Rules) == 0:
		writeError(w, http.StatusBadRequest, "bad_request", "至少指定一个端口")
		return
	}
	for _, rule := range req.Rules {
		if err := opssvc.ValidatePortRule(rule); err != nil {
			writeError(w, http.StatusBadRequest, "bad_rule", err.Error())
			return
		}
	}

	acc, err := s.st.GetAccount(r.Context(), req.AccountID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "账号不存在")
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}

	spec, err := opssvc.EncodeOpenPortsSpec(opssvc.OpenPortsSpec{
		Region:         req.Region,
		SecurityListID: req.SecurityListID,
		Source:         req.Source,
		Rules:          req.Rules,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}

	task, err := s.st.CreateOpsTask(r.Context(), store.OpsTask{
		Kind:      store.OpsKindOpenPorts,
		AccountID: req.AccountID,
		Name:      "放行端口：" + acc.Alias + " " + portSummary(req.Rules),
		Spec:      spec,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "ops_open_ports", AccountID: req.AccountID,
		Target: task.Name, IP: s.clientIP(r),
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"task": toOpsDTO(*task, s.accountAliases(r.Context())),
		"notice": "任务已创建，后台会读取安全列表、追加规则后整体写回。" +
			"原有规则会原样保留；已经放行的端口不会重复添加。",
	})
}

// portSummary 把规则列表压成一行可读的标签，用于任务名。
func portSummary(rules []opssvc.PortRule) string {
	parts := make([]string, 0, len(rules))
	for _, r := range rules {
		if r.Port == 0 {
			parts = append(parts, protoLabel(r.Protocol)+"/*")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s/%d", protoLabel(r.Protocol), r.Port))
	}
	return strings.Join(parts, ",")
}

func protoLabel(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "6":
		return "tcp"
	case "17":
		return "udp"
	case "1":
		return "icmp"
	case "all":
		return "all"
	default:
		return p
	}
}

// changeIPRequest 是「按 CIDR 换公网 IP」的创建请求。
type changeIPRequest struct {
	// InstanceID 是可选的便捷入口。
	//
	// 前端拿不到 vnicId / compartmentId——它们是 OCI 的内部概念，前端的实例
	// 模型里根本没有（适配时就被丢掉了），用户更不该关心。传 instanceId 时
	// 由后端从实例缓存里补齐这三项；三项仍然可以显式传，用于本地库还没同步
	// 到网卡信息、但调用方自己知道的情况。
	InstanceID         string   `json:"instanceId"`
	AccountID          string   `json:"accountId"`
	Region             string   `json:"region"`
	VnicID             string   `json:"vnicId"`
	CompartmentID      string   `json:"compartmentId"`
	TargetCIDRs        []string `json:"targetCidrs"`
	MaxAttempts        int      `json:"maxAttempts"`
	MaxDurationMinutes int      `json:"maxDurationMinutes"`
}

const (
	// defaultChangeIPAttempts 与计划一致：30 次 × 20 秒 ≈ 10 分钟。
	defaultChangeIPAttempts = 30
	maxChangeIPAttempts     = 100
	// defaultChangeIPMinutes 是默认最长运行时间。
	defaultChangeIPMinutes = 30
	maxChangeIPMinutes     = 180
)

// handleCreateChangeIPTask 创建一个「按 CIDR 换 IP」运维任务。
func (s *Server) handleCreateChangeIPTask(w http.ResponseWriter, r *http.Request) {
	var req changeIPRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.InstanceID = strings.TrimSpace(req.InstanceID)

	// 传了 instanceId 就先补齐空缺的三项。这里只填空缺，不覆盖调用方显式给的值。
	if req.InstanceID != "" {
		inst, err := s.st.GetInstance(r.Context(), req.InstanceID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found",
				"实例不在本地库中（可能尚未同步），请先同步实例列表")
			return
		case err != nil:
			writeStoreError(w, err)
			return
		}
		if strings.TrimSpace(req.AccountID) == "" {
			req.AccountID = inst.AccountID
		}
		if strings.TrimSpace(req.Region) == "" {
			req.Region = inst.Region
		}
		if strings.TrimSpace(req.VnicID) == "" {
			req.VnicID = inst.VnicID
		}
		if strings.TrimSpace(req.CompartmentID) == "" {
			req.CompartmentID = inst.CompartmentID
		}
		// 缓存里没有网卡信息时，spec 校验会报「缺少网卡 ID」——那句话对用户
		// 没有意义（他根本没传过这个字段）。这里提前给一句能操作的话。
		if strings.TrimSpace(req.VnicID) == "" {
			writeError(w, http.StatusBadRequest, "vnic_unknown",
				"本地库里没有这台实例的网卡信息，请先同步实例列表再试")
			return
		}
	}

	req.AccountID = strings.TrimSpace(req.AccountID)
	req.Region = strings.TrimSpace(req.Region)
	req.VnicID = strings.TrimSpace(req.VnicID)
	req.CompartmentID = strings.TrimSpace(req.CompartmentID)

	spec := opssvc.ChangeIPSpec{
		Region:        req.Region,
		VnicID:        req.VnicID,
		CompartmentID: req.CompartmentID,
		TargetCIDRs:   req.TargetCIDRs,
	}
	if req.AccountID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "缺少 accountId")
		return
	}
	if err := opssvc.ValidateChangeIPSpec(spec); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	acc, err := s.st.GetAccount(r.Context(), req.AccountID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "账号不存在")
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}

	attempts := clampInt(req.MaxAttempts, 1, maxChangeIPAttempts, defaultChangeIPAttempts)
	minutes := clampInt(req.MaxDurationMinutes, 1, maxChangeIPMinutes, defaultChangeIPMinutes)

	encoded, err := opssvc.EncodeChangeIPSpec(spec)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	task, err := s.st.CreateOpsTask(r.Context(), store.OpsTask{
		Kind:        store.OpsKindChangeIP,
		AccountID:   req.AccountID,
		Name:        "换 IP：" + acc.Alias + " → " + strings.Join(req.TargetCIDRs, ","),
		Spec:        encoded,
		MaxAttempts: attempts,
		ExpiresAt:   time.Now().Add(time.Duration(minutes) * time.Minute),
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "ops_change_ip", AccountID: req.AccountID,
		Target: task.Name, IP: s.clientIP(r),
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"task": toOpsDTO(*task, s.accountAliases(r.Context())),
		"notice": fmt.Sprintf(
			"任务已创建：最多换 %d 次、最长 %d 分钟，命中目标网段即停止。"+
				"每次换 IP 会先删除旧 IP 再申请新 IP，SSH 连接会中断。",
			attempts, minutes),
	})
}

// rescueRequest 是「救援编排」的创建请求。
//
// 没有 bootVolumeId 字段：引导卷由 PRECHECK 从目标实例的挂载关系里推导。
// 让调用方传卷 OCID 会把"选错卷"的责任摊到前端，而选错卷的后果是实例
// 再也起不来。expectBootVolumeId 只是交叉校验用的回显值。
type rescueRequest struct {
	AccountID          string `json:"accountId"`
	Region             string `json:"region"`
	TargetInstanceID   string `json:"targetInstanceId"`
	RescueInstanceID   string `json:"rescueInstanceId"`
	CompartmentID      string `json:"compartmentId"`
	ExpectBootVolumeID string `json:"expectBootVolumeId"`
	MaxDurationMinutes int    `json:"maxDurationMinutes"`
}

// handleCreateRescueTask 创建一个「救援编排」运维任务。
//
// 只落库，不在这里执行：整条流程要跨「关机 → 分离 → 挂载 → 等人工修复 →
// 挂回 → 启动」六七个阶段，中间还要停下来等人。同步执行不可能做到，也没法
// 在进程重启后接着往下走。
func (s *Server) handleCreateRescueTask(w http.ResponseWriter, r *http.Request) {
	var req rescueRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.AccountID = strings.TrimSpace(req.AccountID)
	req.Region = strings.TrimSpace(req.Region)
	req.TargetInstanceID = strings.TrimSpace(req.TargetInstanceID)
	req.RescueInstanceID = strings.TrimSpace(req.RescueInstanceID)
	req.CompartmentID = strings.TrimSpace(req.CompartmentID)
	req.ExpectBootVolumeID = strings.TrimSpace(req.ExpectBootVolumeID)

	if req.AccountID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "缺少 accountId")
		return
	}
	spec := opssvc.RescueSpec{
		Region:             req.Region,
		TargetInstanceID:   req.TargetInstanceID,
		RescueInstanceID:   req.RescueInstanceID,
		CompartmentID:      req.CompartmentID,
		ExpectBootVolumeID: req.ExpectBootVolumeID,
	}
	if err := opssvc.ValidateRescueSpec(spec); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	acc, err := s.st.GetAccount(r.Context(), req.AccountID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "账号不存在")
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}

	minutes := clampInt(req.MaxDurationMinutes, 10, opssvc.MaxRescueMinutes, opssvc.DefaultRescueMinutes)

	encoded, err := opssvc.EncodeRescueSpec(spec)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	task, err := s.st.CreateOpsTask(r.Context(), store.OpsTask{
		Kind:        store.OpsKindRescue,
		AccountID:   req.AccountID,
		InstanceID:  req.TargetInstanceID,
		Name:        "救援：" + acc.Alias + " " + tailID(req.TargetInstanceID),
		Spec:        encoded,
		MaxAttempts: len(opssvc.RescueStages) * 3,
		ExpiresAt:   time.Now().Add(time.Duration(minutes) * time.Minute),
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "ops_rescue", AccountID: req.AccountID,
		Target: task.Name, IP: s.clientIP(r),
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"task": toOpsDTO(*task, s.accountAliases(r.Context())),
		// 阶段清单随创建结果一起给：前端不必自己维护一份，将来调整流程
		// 也不会出现"界面少画了一步"。
		"stages": opssvc.RescueStages,
		"notice": "任务已创建。流程会把目标实例关机、卸下引导卷、挂到救援机，" +
			"然后停下来等你修完——在任务列表里点「继续」才会挂回并启动。" +
			"这期间目标实例不可用，引导卷不属于任何一台运行中的机器。",
	})
}

// bandwidthRequest 是「500Mbps 带宽优化」的创建请求。
//
// action 决定走开启还是关闭流程，两者是同一套资源的两个方向：开启要建
// NAT + 路由 + NLB，关闭要把它们按逆序拆掉并还原路由。
type bandwidthRequest struct {
	AccountID          string `json:"accountId"`
	Region             string `json:"region"`
	CompartmentID      string `json:"compartmentId"`
	InstanceID         string `json:"instanceId"`
	Action             string `json:"action"`
	ListenerPort       int    `json:"listenerPort"`
	MaxDurationMinutes int    `json:"maxDurationMinutes"`
}

// handleCreateBandwidthTask 创建一个「500Mbps」运维任务。
//
// 开启和关闭都走这里。分开两个接口也可以，但关闭是一个必须能「在页面上
// 点一下就做完」的动作——资源在计费，让人去找另一个按钮是给自己挖坑。
func (s *Server) handleCreateBandwidthTask(w http.ResponseWriter, r *http.Request) {
	var req bandwidthRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.AccountID = strings.TrimSpace(req.AccountID)
	req.Region = strings.TrimSpace(req.Region)
	req.CompartmentID = strings.TrimSpace(req.CompartmentID)
	req.InstanceID = strings.TrimSpace(req.InstanceID)
	req.Action = strings.ToLower(strings.TrimSpace(req.Action))
	if req.Action == "" {
		req.Action = opssvc.BandwidthEnable
	}

	if req.AccountID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "缺少 accountId")
		return
	}
	spec := opssvc.BandwidthSpec{
		Action:        req.Action,
		Region:        req.Region,
		CompartmentID: req.CompartmentID,
		InstanceID:    req.InstanceID,
		ListenerPort:  req.ListenerPort,
	}
	if err := opssvc.ValidateBandwidthSpec(spec); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	acc, err := s.st.GetAccount(r.Context(), req.AccountID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "账号不存在")
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}

	minutes := clampInt(req.MaxDurationMinutes, 5, opssvc.MaxBandwidthMinutes, opssvc.DefaultBandwidthMinutes)

	encoded, err := opssvc.EncodeBandwidthSpec(spec)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	actionLabel := "开启"
	if spec.Action == opssvc.BandwidthDisable {
		actionLabel = "关闭"
	}
	stages := opssvc.BandwidthStages(spec.Action)

	task, err := s.st.CreateOpsTask(r.Context(), store.OpsTask{
		Kind:        store.OpsKindEnable500,
		AccountID:   req.AccountID,
		InstanceID:  req.InstanceID,
		Name:        actionLabel + " 500Mbps：" + acc.Alias + " " + tailID(req.InstanceID),
		Spec:        encoded,
		MaxAttempts: len(stages) * 3,
		ExpiresAt:   time.Now().Add(time.Duration(minutes) * time.Minute),
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "ops_bandwidth_" + spec.Action, AccountID: req.AccountID,
		Target: task.Name, IP: s.clientIP(r),
	})

	notice := "任务已创建。流程会依次创建 NAT 网关、切换默认路由、创建 NLB 与后端，" +
		"任一步失败都会停在原地，可以在任务列表里点「重试」从该步继续。"
	if spec.Action == opssvc.BandwidthDisable {
		notice = "关闭流程按逆序执行：删监听 → 删后端集 → 删 NLB → 还原默认路由 → 删 NAT 网关。" +
			"任一步失败会停在原地并保留提示，不会继续删后面的资源——在删除过程中越过失败点继续，" +
			"留下的半套配置比不删更难收拾。"
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"task":   toOpsDTO(*task, s.accountAliases(r.Context())),
		"stages": stages,
		"notice": notice,
	})
}

// tailID 取 OCID 的末段用于任务名。完整 OCID 太长，塞进列表里会把别的
// 信息挤没，而末段足以让人认出是哪台机器。
func tailID(ocid string) string {
	if i := strings.LastIndex(ocid, "."); i >= 0 && i+1 < len(ocid) {
		return ocid[i+1:]
	}
	return ocid
}

// clampInt 把 v 夹到 [lo, hi]；v 为 0（未指定）时用 def。
func clampInt(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// RunOps 启动运维任务的调度循环。由 main 在后台 goroutine 里调用。
func (s *Server) RunOps(ctx context.Context) { s.ops.Run(ctx) }
