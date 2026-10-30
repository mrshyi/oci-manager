package opssvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

// 500Mbps 流程的两个动作。
//
// 它们共用 ops_tasks 里的同一个 kind（enable_500m），靠 spec 里的 action 区分。
// 分成两个 kind 也行，但那样「关闭」任务在列表里会和「开启」长得一模一样，
// 而这两件事的风险方向完全相反：一个开始花钱，一个停止花钱。
const (
	BandwidthEnable  = "enable"
	BandwidthDisable = "disable"
)

// 开启流程的阶段。
//
// 顺序就是计划里的 ①–⑤：预检 → NAT → 路由 → NLB → 后端集 → 监听。
// 每步单独落库，进程在任意一步被杀，重启后从库里那个 stage 继续。
const (
	BwPrecheck           = "PRECHECK"
	BwNatCreating        = "NAT_CREATING"
	BwRouteSwitching     = "ROUTE_SWITCHING"
	BwNLBCreating        = "NLB_CREATING"
	BwBackendSetCreating = "BACKEND_SET_CREATING"
	BwListenerCreating   = "LISTENER_CREATING"
	BwReady              = "DONE"
)

// 关闭流程的阶段。
//
// **必须是开启流程的严格逆序**，而且顺序里有一处是硬性的：路由必须先还原，
// NAT 才能删——OCI 不允许删除仍被路由表引用的 NAT 网关，顺序反了会在倒数
// 第二步拿到一个 409，然后用户看到「清理失败」但资源其实已经没救了。
//
// 计划里写的「删后端集 → 删 NLB → 删监听」按字面执行不了：删掉 NLB 时它的
// 监听会一起消失，再去删监听只能拿到 404。这里按真正的逆序写：监听 → 后端集
// → NLB → 路由 → NAT。
const (
	BwDiscover           = "DISCOVER"
	BwListenerDeleting   = "LISTENER_DELETING"
	BwBackendSetDeleting = "BACKEND_SET_DELETING"
	BwNLBDeleting        = "NLB_DELETING"
	BwRouteRestoring     = "ROUTE_RESTORING"
	BwNatDeleting        = "NAT_DELETING"
	BwCleaned            = "CLEANED"
)

// BandwidthStages 返回某个动作的合法阶段列表，供 HTTP 层与前端画流水线。
func BandwidthStages(action string) []string {
	if action == BandwidthDisable {
		return []string{
			BwDiscover, BwListenerDeleting, BwBackendSetDeleting,
			BwNLBDeleting, BwRouteRestoring, BwNatDeleting, BwCleaned,
		}
	}
	return []string{
		BwPrecheck, BwNatCreating, BwRouteSwitching,
		BwNLBCreating, BwBackendSetCreating, BwListenerCreating, BwReady,
	}
}

const (
	// BandwidthTagKey 是所有本功能创建的资源上的标记键，值是实例 OCID。
	//
	// 清理流程靠它找回资源，而不是靠「开启任务」的 progress：开启与关闭是两个
	// 独立的 ops_tasks，用户完全可能在开启失败之后、或者在另一台机器上点关闭。
	// 只认 tag 的话，即使开启任务的记录丢了也还能清干净。
	BandwidthTagKey = "ocim-bw"

	// BandwidthResourcePrefix 是资源显示名的前缀。
	BandwidthResourcePrefix = "ocim-bw"

	// bwBackendSetName / bwListenerName 是后端集与监听的固定名字。
	// 一个实例只允许一套，重名即冲突，正好当幂等键用。
	bwBackendSetName = "ocim-bw-bs"
	bwListenerName   = "ocim-bw-lsn"

	// bwStageTimeout 是单阶段的最长等待时间。NLB 的创建/删除都是分钟级。
	bwStageTimeout = 10 * time.Minute

	// bwPollInterval 是等云平台收敛时的轮询间隔。
	bwPollInterval = 15 * time.Second

	// bwDefaultPort 是默认监听端口。
	//
	// NLB 的监听是「一个端口一个监听」，没有「监听全部端口」这种写法——
	// 计划里写的「TCP 全端口」在 NLB 上不存在，只能选一个端口。默认 443
	// 是因为这个方案的实际用法是让实例的出站流量走 NLB，端口本身不参与
	// 带宽计算，选一个不冲突的即可，用户也可以在创建任务时改。
	bwDefaultPort = 443

	// DefaultBandwidthMinutes 是默认总时限。
	DefaultBandwidthMinutes = 60
	// MaxBandwidthMinutes 是总时限上限。
	MaxBandwidthMinutes = 240
)

// BandwidthSpec 是 500Mbps 任务的入参快照。
type BandwidthSpec struct {
	Action        string `json:"action"`
	Region        string `json:"region"`
	CompartmentID string `json:"compartmentId"`
	InstanceID    string `json:"instanceId"`
	// ListenerPort 可选，默认 443。
	ListenerPort int `json:"listenerPort,omitempty"`
}

// EncodeBandwidthSpec 把入参序列化成任务里存的那一列。
func EncodeBandwidthSpec(s BandwidthSpec) (string, error) {
	buf, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("opssvc: 序列化带宽参数失败: %w", err)
	}
	return string(buf), nil
}

// DecodeBandwidthSpec 从任务里取回入参。
func DecodeBandwidthSpec(raw string) (BandwidthSpec, error) {
	var s BandwidthSpec
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return s, fmt.Errorf("opssvc: 解析带宽参数失败: %w", err)
	}
	if s.Action == "" {
		s.Action = BandwidthEnable
	}
	if s.ListenerPort <= 0 || s.ListenerPort > 65535 {
		s.ListenerPort = bwDefaultPort
	}
	return s, nil
}

// ValidateBandwidthSpec 校验入参。HTTP 层在创建任务前调用。
func ValidateBandwidthSpec(s BandwidthSpec) error {
	switch s.Action {
	case BandwidthEnable, BandwidthDisable:
	default:
		return fmt.Errorf("未知的动作 %q，只支持 enable / disable", s.Action)
	}
	if strings.TrimSpace(s.Region) == "" {
		return errors.New("缺少区域")
	}
	if strings.TrimSpace(s.CompartmentID) == "" {
		return errors.New("缺少 compartmentId")
	}
	if strings.TrimSpace(s.InstanceID) == "" {
		return errors.New("缺少实例 ID")
	}
	if s.ListenerPort < 0 || s.ListenerPort > 65535 {
		return fmt.Errorf("监听端口 %d 不合法", s.ListenerPort)
	}
	return nil
}

// bwProgress 是落库的结构化进展。
//
// 它同时是「跨重启记忆」：NLB 的创建是异步的，进程如果在 CREATING 期间被
// 杀，重启后必须知道自己在等哪一个 NLB，否则会再建一个——而多出来的那个
// 不在任何人的视野里，会一直计费。
type bwProgress struct {
	Stage      string    `json:"stage"`
	Note       string    `json:"note,omitempty"`
	StageSince time.Time `json:"stageSince,omitempty"`

	Action        string `json:"action,omitempty"`
	InstanceID    string `json:"instanceId,omitempty"`
	InstanceName  string `json:"instanceName,omitempty"`
	CompartmentID string `json:"compartmentId,omitempty"`
	VcnID         string `json:"vcnId,omitempty"`
	SubnetID      string `json:"subnetId,omitempty"`
	PrivateIP     string `json:"privateIp,omitempty"`
	OriginalPubIP string `json:"originalPublicIp,omitempty"`

	RouteTableID string `json:"routeTableId,omitempty"`
	// OriginalRoutes 是切换前的静态路由规则。还原时按它原样写回。
	//
	// 不能只记「原来的默认路由指向哪个网关」：用户的路由表里可能还有别的
	// 静态规则（比如指向另一个 VCN 的 peering），整体替换语义下漏掉它们
	// 就等于把用户的网络配置删了。
	OriginalRoutes []ociclient.RouteRule `json:"originalRoutes,omitempty"`
	RoutesSaved    bool                  `json:"routesSaved,omitempty"`

	NatGatewayID   string `json:"natGatewayId,omitempty"`
	NLBID          string `json:"nlbId,omitempty"`
	BackendSetName string `json:"backendSetName,omitempty"`
	ListenerName   string `json:"listenerName,omitempty"`
	ListenerPort   int    `json:"listenerPort,omitempty"`
	PublicIP       string `json:"publicIp,omitempty"`

	// 清理阶段的发现结果：哪些资源本来就不存在。
	NatMissing bool `json:"natMissing,omitempty"`
	NLBMissing bool `json:"nlbMissing,omitempty"`
}

// bwClient 是带宽流程用到的 OCI 操作。
//
// 收窄成接口是为了测试：这套流程最难验证的是「第几步失败之后，后面几步有
// 没有停下」以及「逆序清理有没有漏」，用假实现可以精确驱动到任意一步。
type bwClient interface {
	GetInstance(ctx context.Context, region, instanceID string) (*ociclient.Instance, error)
	ListVnicAttachments(ctx context.Context, region, compartmentID, instanceID string) ([]ociclient.VnicAttachment, error)
	GetVnic(ctx context.Context, region, vnicID string) (*ociclient.Vnic, error)
	GetSubnet(ctx context.Context, region, subnetID string) (*ociclient.Subnet, error)
	GetVcn(ctx context.Context, region, vcnID string) (*ociclient.Vcn, error)
	GetRouteTable(ctx context.Context, region, routeTableID string) (*ociclient.RouteTable, error)
	UpdateRouteTable(ctx context.Context, region, routeTableID string, rules []ociclient.RouteRule) (*ociclient.RouteTable, error)
	ListInternetGateways(ctx context.Context, region, compartmentID, vcnID string) ([]ociclient.InternetGateway, error)

	ListNatGateways(ctx context.Context, region, compartmentID, vcnID string) ([]ociclient.NatGateway, error)
	CreateNatGateway(ctx context.Context, region string, req ociclient.CreateNatGatewayRequest) (*ociclient.NatGateway, error)
	GetNatGateway(ctx context.Context, region, natGatewayID string) (*ociclient.NatGateway, error)
	DeleteNatGateway(ctx context.Context, region, natGatewayID string) error

	ListNetworkLoadBalancers(ctx context.Context, region, compartmentID string) ([]ociclient.NetworkLoadBalancer, error)
	CreateNetworkLoadBalancer(ctx context.Context, region string, req ociclient.CreateNLBRequest) (*ociclient.NetworkLoadBalancer, error)
	GetNetworkLoadBalancer(ctx context.Context, region, nlbID string) (*ociclient.NetworkLoadBalancer, error)
	DeleteNetworkLoadBalancer(ctx context.Context, region, nlbID string) error
	CreateNLBBackendSet(ctx context.Context, region, nlbID string, req ociclient.CreateBackendSetRequest) error
	DeleteNLBBackendSet(ctx context.Context, region, nlbID, name string) error
	CreateNLBBackend(ctx context.Context, region, nlbID, backendSetName string, req ociclient.CreateBackendRequest) error
	CreateNLBListener(ctx context.Context, region, nlbID string, req ociclient.CreateListenerRequest) error
	DeleteNLBListener(ctx context.Context, region, nlbID, name string) error
}

// handleBandwidth 是 enable_500m 类型的处理器。
func (s *Service) handleBandwidth(ctx context.Context, t *store.OpsTask) outcome {
	spec, err := DecodeBandwidthSpec(t.Spec)
	if err != nil {
		return failed(err)
	}
	client, _, err := s.conns.ForID(ctx, t.AccountID)
	if err != nil {
		return failed(fmt.Errorf("读取账号凭据失败: %w", err))
	}
	return bandwidthStep(ctx, t, spec, client)
}

// bandwidthStep 执行一轮：判断当前阶段，做这一步该做的事。
//
// 与救援一样，一轮只推进一步。这里的代价比救援小（每步都是一个 HTTP 调用），
// 换来的是同样的东西：任何一步失败都停在原地，用户点「重试」从这一步继续。
func bandwidthStep(ctx context.Context, t *store.OpsTask, spec BandwidthSpec, c bwClient) outcome {
	prog := decodeBWProgress(t.Progress)
	prog.Action = spec.Action
	prog.InstanceID = spec.InstanceID
	prog.CompartmentID = spec.CompartmentID

	stages := BandwidthStages(spec.Action)
	stage := strings.TrimSpace(t.Stage)
	if stage == "" || !containsString(stages, stage) {
		// 阶段为空（新任务）或者库里是一个不属于本动作的阶段（用户改了 spec
		// 之后重试），都从第一步重来。不属于本动作的 stage 直接当未知处理，
		// 否则会在一个根本不存在的阶段上永远打转。
		stage = stages[0]
	}
	// 与救援相同的判断：t.State 是这一轮被捞到时的状态，pending 说明这是
	// 「新开一轮」（首次执行或用户点了重试），要重置阶段计时。
	if t.State == store.OpsPending || prog.StageSince.IsZero() {
		prog.StageSince = time.Now()
	}
	prog.Stage = stage

	if !t.ExpiresAt.IsZero() && time.Now().After(t.ExpiresAt) {
		return bwFail(stage, prog, fmt.Errorf(
			"已超过最长运行时间，流程停在 %s。已创建的资源不会被自动删除，请确认后点「重试」或手动清理", stage))
	}
	if elapsed := time.Since(prog.StageSince); elapsed > bwStageTimeout {
		return bwFail(stage, prog, fmt.Errorf(
			"%s 阶段已等待 %s 仍未完成，已停止推进。请到控制台确认资源状态后点「重试」",
			stage, elapsed.Round(time.Second)))
	}

	if spec.Action == BandwidthDisable {
		return bwDisableStep(ctx, spec, c, prog, stage)
	}
	return bwEnableStep(ctx, spec, c, prog, stage)
}

// ---- 开启 ----

func bwEnableStep(ctx context.Context, spec BandwidthSpec, c bwClient, prog bwProgress, stage string) outcome {
	switch stage {
	case BwPrecheck:
		return bwPrecheck(ctx, spec, c, prog)
	case BwNatCreating:
		return bwEnsureNat(ctx, spec, c, prog)
	case BwRouteSwitching:
		return bwSwitchRoute(ctx, spec, c, prog)
	case BwNLBCreating:
		return bwEnsureNLB(ctx, spec, c, prog)
	case BwBackendSetCreating:
		return bwEnsureBackendSet(ctx, spec, c, prog)
	case BwListenerCreating:
		return bwEnsureListener(ctx, spec, c, prog)
	case BwReady:
		// 曾经走到过 DONE 但没落到终态（进程刚好在 finish 之前被杀）。
		return bwSucceeded(prog)
	default:
		return bwFail(stage, prog, fmt.Errorf(
			"未知的阶段 %q，无法继续。请取消任务后重新创建", stage))
	}
}

// bwPrecheck 校验前提条件，并把后面几步要用的坐标一次查全。
//
// 这些查询本身不改任何东西，所以可以放心重复执行；但一旦开始建资源，
// 后面的阶段就不该再回到这里——回到这里意味着重新推导坐标，而那时
// 路由可能已经切到 NAT 上了，推导出来的东西会不一样。
func bwPrecheck(ctx context.Context, spec BandwidthSpec, c bwClient, prog bwProgress) outcome {
	inst, err := c.GetInstance(ctx, spec.Region, spec.InstanceID)
	if err != nil {
		return bwFail(BwPrecheck, prog, fmt.Errorf("读取实例失败: %w", err))
	}
	switch inst.LifecycleState {
	case ociclient.LifecycleTerminating, ociclient.LifecycleTerminated:
		return bwFail(BwPrecheck, prog, fmt.Errorf(
			"实例当前状态为 %s，已无法操作", inst.LifecycleState))
	}
	// 只做 AMD 免费实例。ARM 免费实例带宽本来就够，套一层 NLB 只会多一个
	// 故障点，还要多一份账单——这里直接拒绝，而不是"允许但没效果"。
	if !isAMDFreeTierShape(inst.Shape) {
		return bwFail(BwPrecheck, prog, fmt.Errorf(
			"这项优化只针对 AMD 免费实例（VM.Standard.E2.1.Micro），当前实例是 %s，无需也无法开启",
			inst.Shape))
	}
	if spec.CompartmentID != "" && inst.CompartmentID != "" && inst.CompartmentID != spec.CompartmentID {
		return bwFail(BwPrecheck, prog, fmt.Errorf(
			"实例不属于指定的 compartment（实例在 %s），已停止", inst.CompartmentID))
	}

	// 主网卡。NLB 的后端要填私网 IP，取错网卡会得到一个永远不健康的后端。
	atts, err := c.ListVnicAttachments(ctx, spec.Region, spec.CompartmentID, spec.InstanceID)
	if err != nil {
		return bwFail(BwPrecheck, prog, fmt.Errorf("列出网卡失败: %w", err))
	}
	vnicID := ""
	for _, a := range atts {
		if a.InstanceID != "" && a.InstanceID != spec.InstanceID {
			continue
		}
		if a.NicIndex == 0 && a.LifecycleState == "ATTACHED" && a.VnicID != "" {
			vnicID = a.VnicID
			break
		}
	}
	if vnicID == "" {
		return bwFail(BwPrecheck, prog, errors.New(
			"没有找到实例的主网卡（nicIndex=0 且已挂载），无法确定 NLB 后端地址"))
	}
	vnic, err := c.GetVnic(ctx, spec.Region, vnicID)
	if err != nil {
		return bwFail(BwPrecheck, prog, fmt.Errorf("读取网卡失败: %w", err))
	}
	if vnic.PrivateIP == "" {
		return bwFail(BwPrecheck, prog, errors.New("主网卡没有私网 IP，无法作为 NLB 后端"))
	}
	// 公网 IP 是「改坏了还能连上」的保险：切路由只影响出站，公网 IP 仍然
	// 可达。没有公网 IP 的机器一旦切过去就只剩 NLB 一条路，出事时无从下手。
	if vnic.PublicIP == "" {
		return bwFail(BwPrecheck, prog, errors.New(
			"实例没有公网 IP。切换路由后只能通过 NLB 访问，一旦失败将无法登录；请先分配公网 IP 再重试"))
	}

	subnet, err := c.GetSubnet(ctx, spec.Region, vnic.SubnetID)
	if err != nil {
		return bwFail(BwPrecheck, prog, fmt.Errorf("读取子网失败: %w", err))
	}
	if subnet.RouteTableID == "" {
		return bwFail(BwPrecheck, prog, errors.New("子网没有关联路由表，无法切换默认路由"))
	}
	vcn, err := c.GetVcn(ctx, spec.Region, subnet.VcnID)
	if err != nil {
		return bwFail(BwPrecheck, prog, fmt.Errorf("读取 VCN 失败: %w", err))
	}
	if vcn.CidrBlock == "" && len(vcn.CidrBlocks) == 0 {
		return bwFail(BwPrecheck, prog, errors.New("VCN 没有可用地址段"))
	}

	prog.InstanceName = inst.DisplayName
	prog.VcnID = subnet.VcnID
	prog.SubnetID = subnet.ID
	prog.PrivateIP = vnic.PrivateIP
	prog.OriginalPubIP = vnic.PublicIP
	prog.RouteTableID = subnet.RouteTableID
	prog.ListenerPort = spec.ListenerPort
	if prog.ListenerPort <= 0 {
		prog.ListenerPort = bwDefaultPort
	}

	return bwAdvance(BwNatCreating, prog, fmt.Sprintf(
		"预检通过：%s 是 AMD 免费实例，主网卡 %s 在子网 %s",
		inst.DisplayName, vnic.PrivateIP, subnet.DisplayName))
}

// bwEnsureNat 确保 NAT 网关存在且可用。
func bwEnsureNat(ctx context.Context, spec BandwidthSpec, c bwClient, prog bwProgress) outcome {
	// 已经建过的：查它的状态，不要重复建。
	if prog.NatGatewayID != "" {
		gw, err := c.GetNatGateway(ctx, spec.Region, prog.NatGatewayID)
		switch {
		case err != nil && ociclient.IsNotFound(err):
			// 被人在控制台删了。清掉 ID 重来一遍，比报错更符合预期。
			prog.NatGatewayID = ""
		case err != nil:
			return bwFail(BwNatCreating, prog, fmt.Errorf("读取 NAT 网关失败: %w", err))
		case gw.LifecycleState == ociclient.NatLifecycleAvailable:
			return bwAdvance(BwRouteSwitching, prog, "NAT 网关已就绪，准备切换默认路由")
		case gw.LifecycleState == ociclient.NatLifecycleTerminating ||
			gw.LifecycleState == ociclient.NatLifecycleTerminated:
			prog.NatGatewayID = ""
		default:
			return bwWait(BwNatCreating, prog, fmt.Sprintf("NAT 网关正在创建（%s），等待可用", gw.LifecycleState))
		}
	}

	// 没有 ID 时先按 tag 找一遍：可能上一次任务已经建好了，或者进程在
	// 创建成功后、写回 progress 之前被杀。找不到才真的建。
	if prog.NatGatewayID == "" {
		list, err := c.ListNatGateways(ctx, spec.Region, spec.CompartmentID, prog.VcnID)
		if err != nil {
			return bwFail(BwNatCreating, prog, fmt.Errorf("列出 NAT 网关失败: %w", err))
		}
		for _, gw := range list {
			if gw.FreeformTags[BandwidthTagKey] != spec.InstanceID {
				continue
			}
			if gw.LifecycleState == ociclient.NatLifecycleTerminating ||
				gw.LifecycleState == ociclient.NatLifecycleTerminated {
				continue
			}
			prog.NatGatewayID = gw.ID
			return bwWait(BwNatCreating, prog, "复用已有的 NAT 网关，等待可用")
		}

		gw, err := c.CreateNatGateway(ctx, spec.Region, ociclient.CreateNatGatewayRequest{
			CompartmentID: spec.CompartmentID,
			VcnID:         prog.VcnID,
			DisplayName:   bwResourceName("nat", prog),
			FreeformTags:  bwTags(spec.InstanceID),
		})
		if err != nil {
			return bwFail(BwNatCreating, prog, fmt.Errorf("创建 NAT 网关失败: %w", err))
		}
		prog.NatGatewayID = gw.ID
		if gw.LifecycleState == ociclient.NatLifecycleAvailable {
			// 少数情况下创建接口直接返回可用状态（复用的旧网关就是这种），
			// 这时候没必要白等一个 tick。
			return bwAdvance(BwRouteSwitching, prog, "NAT 网关已就绪，准备切换默认路由")
		}
		return bwWait(BwNatCreating, prog, "NAT 网关已提交创建，等待可用")
	}
	return bwWait(BwNatCreating, prog, "NAT 网关正在创建，等待可用")
}

// bwSwitchRoute 把子网路由表里 0.0.0.0/0 指向 NAT。
func bwSwitchRoute(ctx context.Context, spec BandwidthSpec, c bwClient, prog bwProgress) outcome {
	rt, err := c.GetRouteTable(ctx, spec.Region, prog.RouteTableID)
	if err != nil {
		return bwFail(BwRouteSwitching, prog, fmt.Errorf("读取路由表失败: %w", err))
	}
	if !prog.RoutesSaved {
		// 只存静态规则：LOCAL 规则是 OCI 自己维护的，写回去会被拒绝。
		prog.OriginalRoutes = staticRouteRules(rt.RouteRules)
		prog.RoutesSaved = true
	}
	if current := defaultRouteTarget(rt.RouteRules); current == prog.NatGatewayID {
		// 已经是 NAT 了：上一次已经切过，只是没来得及落库。
		return bwAdvance(BwNLBCreating, prog, "默认路由已指向 NAT 网关，准备创建 NLB")
	}

	// 整体替换语义：必须把原有规则原样带上，只改默认路由那一条。
	next := replaceDefaultRoute(rt.RouteRules, prog.NatGatewayID, "ocim-bw: 出站走 NAT 网关")
	if _, err := c.UpdateRouteTable(ctx, spec.Region, prog.RouteTableID, next); err != nil {
		return bwFail(BwRouteSwitching, prog, fmt.Errorf("切换默认路由失败: %w", err))
	}
	return bwAdvance(BwNLBCreating, prog, "默认路由已指向 NAT 网关，准备创建 NLB")
}

// bwEnsureNLB 确保 NLB 存在且处于 ACTIVE。
func bwEnsureNLB(ctx context.Context, spec BandwidthSpec, c bwClient, prog bwProgress) outcome {
	if prog.NLBID != "" {
		nlb, err := c.GetNetworkLoadBalancer(ctx, spec.Region, prog.NLBID)
		switch {
		case err != nil && ociclient.IsNotFound(err):
			prog.NLBID = ""
		case err != nil:
			return bwFail(BwNLBCreating, prog, fmt.Errorf("读取 NLB 失败: %w", err))
		case nlb.LifecycleState == ociclient.NLBLifecycleActive:
			prog.PublicIP = nlbPublicIP(nlb)
			return bwAdvance(BwBackendSetCreating, prog, "NLB 已就绪，准备建后端集")
		case nlb.LifecycleState == ociclient.NLBLifecycleFailed:
			return bwFail(BwNLBCreating, prog, fmt.Errorf(
				"NLB %s 创建失败（状态 FAILED），请到控制台确认后删除并重试", prog.NLBID))
		case nlb.LifecycleState == ociclient.NLBLifecycleDeleting ||
			nlb.LifecycleState == ociclient.NLBLifecycleDeleted:
			prog.NLBID = ""
		default:
			return bwWait(BwNLBCreating, prog, fmt.Sprintf("NLB 正在创建（%s），等待可用", nlb.LifecycleState))
		}
	}

	if prog.NLBID == "" {
		list, err := c.ListNetworkLoadBalancers(ctx, spec.Region, spec.CompartmentID)
		if err != nil {
			return bwFail(BwNLBCreating, prog, fmt.Errorf("列出 NLB 失败: %w", err))
		}
		for _, nlb := range list {
			if nlb.FreeformTags[BandwidthTagKey] != spec.InstanceID {
				continue
			}
			if nlb.LifecycleState == ociclient.NLBLifecycleDeleting ||
				nlb.LifecycleState == ociclient.NLBLifecycleDeleted {
				continue
			}
			prog.NLBID = nlb.ID
			return bwWait(BwNLBCreating, prog, "复用已有的 NLB，等待可用")
		}

		nlb, err := c.CreateNetworkLoadBalancer(ctx, spec.Region, ociclient.CreateNLBRequest{
			CompartmentID: spec.CompartmentID,
			SubnetID:      prog.SubnetID,
			DisplayName:   bwResourceName("nlb", prog),
			// 公网 NLB：只有公网 NLB 才会拿到公网 IP，也才有带宽上的意义。
			IsPrivate:    false,
			FreeformTags: bwTags(spec.InstanceID),
		})
		if err != nil {
			return bwFail(BwNLBCreating, prog, fmt.Errorf("创建 NLB 失败: %w", err))
		}
		prog.NLBID = nlb.ID
		if nlb.LifecycleState == ociclient.NLBLifecycleActive {
			prog.PublicIP = nlbPublicIP(nlb)
			return bwAdvance(BwBackendSetCreating, prog, "NLB 已就绪，准备建后端集")
		}
		return bwWait(BwNLBCreating, prog, "NLB 已提交创建，等待可用（这一步通常要几分钟）")
	}
	return bwWait(BwNLBCreating, prog, "NLB 正在创建，等待可用")
}

// bwEnsureBackendSet 建后端集，并把实例的私网 IP 加成唯一后端。
func bwEnsureBackendSet(ctx context.Context, spec BandwidthSpec, c bwClient, prog bwProgress) outcome {
	if prog.NLBID == "" {
		return bwFail(BwBackendSetCreating, prog, errors.New("NLB ID 丢失，无法建后端集；请取消后重新创建任务"))
	}
	if prog.BackendSetName == "" {
		prog.BackendSetName = bwBackendSetName
	}
	// 健康检查打在后端自己的端口上。用 TCP 而不是 HTTP：我们不知道这台机器
	// 上跑的是什么服务，HTTP 检查会让一台正常但没开 web 的机器被判不健康。
	// 端口填 0 表示"沿用后端端口"，由 OCI 决定。
	err := c.CreateNLBBackendSet(ctx, spec.Region, prog.NLBID, ociclient.CreateBackendSetRequest{
		Name:   prog.BackendSetName,
		Policy: ociclient.NLBPolicyFiveTuple,
		HealthChecker: ociclient.NLBHealthChecker{
			Protocol:         ociclient.NLBProtocolTCP,
			Port:             prog.ListenerPort,
			IntervalInMillis: 10000,
			TimeoutInMillis:  3000,
			Retries:          3,
		},
	})
	// 409 表示已经存在（上一轮建过但没落库），当成成功继续。
	if err != nil && !ociclient.IsConflict(err) {
		return bwFail(BwBackendSetCreating, prog, fmt.Errorf("创建后端集失败: %w", err))
	}

	if err := c.CreateNLBBackend(ctx, spec.Region, prog.NLBID, prog.BackendSetName, ociclient.CreateBackendRequest{
		IPAddress: prog.PrivateIP,
		Port:      prog.ListenerPort,
		Weight:    1,
	}); err != nil && !ociclient.IsConflict(err) {
		return bwFail(BwBackendSetCreating, prog, fmt.Errorf("添加后端失败: %w", err))
	}

	return bwAdvance(BwListenerCreating, prog, fmt.Sprintf(
		"后端集已就绪，后端为 %s:%d", prog.PrivateIP, prog.ListenerPort))
}

// bwEnsureListener 建监听，并读出 NLB 的公网 IP。
func bwEnsureListener(ctx context.Context, spec BandwidthSpec, c bwClient, prog bwProgress) outcome {
	if prog.ListenerName == "" {
		prog.ListenerName = bwListenerName
	}
	err := c.CreateNLBListener(ctx, spec.Region, prog.NLBID, ociclient.CreateListenerRequest{
		Name:                  prog.ListenerName,
		DefaultBackendSetName: prog.BackendSetName,
		Port:                  prog.ListenerPort,
		Protocol:              ociclient.NLBProtocolTCP,
	})
	if err != nil && !ociclient.IsConflict(err) {
		return bwFail(BwListenerCreating, prog, fmt.Errorf("创建监听失败: %w", err))
	}

	// 监听建完之后再读一次 NLB，把公网 IP 记下来给用户。读失败不算失败：
	// 资源已经全部建好，IP 只是展示信息，重试反而会把一个成功的流程拖成失败。
	if nlb, err := c.GetNetworkLoadBalancer(ctx, spec.Region, prog.NLBID); err == nil {
		prog.PublicIP = nlbPublicIP(nlb)
	}
	return bwSucceeded(prog)
}

// ---- 关闭 ----

func bwDisableStep(ctx context.Context, spec BandwidthSpec, c bwClient, prog bwProgress, stage string) outcome {
	switch stage {
	case BwDiscover:
		return bwDiscover(ctx, spec, c, prog)
	case BwListenerDeleting:
		return bwDeleteListener(ctx, spec, c, prog)
	case BwBackendSetDeleting:
		return bwDeleteBackendSet(ctx, spec, c, prog)
	case BwNLBDeleting:
		return bwDeleteNLB(ctx, spec, c, prog)
	case BwRouteRestoring:
		return bwRestoreRoute(ctx, spec, c, prog)
	case BwNatDeleting:
		return bwDeleteNat(ctx, spec, c, prog)
	case BwCleaned:
		return bwCleaned(prog)
	default:
		return bwFail(stage, prog, fmt.Errorf(
			"未知的清理阶段 %q，无法继续。请取消任务后重新创建", stage))
	}
}

// bwDiscover 找出这个实例名下的所有资源。
//
// 关闭是一个独立任务，拿不到开启任务的 progress，所以只能靠 tag 反查。
// 查不到不算失败：用户可能在控制台已经手工清理过了，那正是我们想要的结果。
func bwDiscover(ctx context.Context, spec BandwidthSpec, c bwClient, prog bwProgress) outcome {
	inst, err := c.GetInstance(ctx, spec.Region, spec.InstanceID)
	if err != nil && !ociclient.IsNotFound(err) {
		return bwFail(BwDiscover, prog, fmt.Errorf("读取实例失败: %w", err))
	}
	var notes []string
	if err == nil {
		prog.InstanceName = inst.DisplayName
	}

	// 子网与路由表。实例已经被终止时查不到网卡，就只能靠 progress 里
	// 已有的坐标；两样都没有就无法还原路由，宁可停下也不猜。
	if prog.SubnetID == "" {
		atts, err := c.ListVnicAttachments(ctx, spec.Region, spec.CompartmentID, spec.InstanceID)
		if err != nil {
			return bwFail(BwDiscover, prog, fmt.Errorf("列出网卡失败: %w", err))
		}
		for _, a := range atts {
			if a.NicIndex == 0 && a.VnicID != "" {
				prog.SubnetID = a.SubnetID
				if vnic, err := c.GetVnic(ctx, spec.Region, a.VnicID); err == nil {
					prog.PrivateIP = vnic.PrivateIP
				}
				break
			}
		}
	}
	if prog.SubnetID != "" {
		if subnet, err := c.GetSubnet(ctx, spec.Region, prog.SubnetID); err == nil {
			prog.VcnID = subnet.VcnID
			prog.RouteTableID = subnet.RouteTableID
		} else if !ociclient.IsNotFound(err) {
			return bwFail(BwDiscover, prog, fmt.Errorf("读取子网失败: %w", err))
		}
	}

	if prog.NLBID == "" {
		list, err := c.ListNetworkLoadBalancers(ctx, spec.Region, spec.CompartmentID)
		if err != nil {
			return bwFail(BwDiscover, prog, fmt.Errorf("列出 NLB 失败: %w", err))
		}
		for _, nlb := range list {
			if nlb.FreeformTags[BandwidthTagKey] != spec.InstanceID {
				continue
			}
			if nlb.LifecycleState == ociclient.NLBLifecycleDeleted {
				continue
			}
			prog.NLBID = nlb.ID
			prog.PublicIP = nlbPublicIP(&nlb)
			break
		}
	}
	if prog.NLBID == "" {
		prog.NLBMissing = true
		notes = append(notes, "没有找到本功能创建的 NLB")
	}

	if prog.NatGatewayID == "" {
		list, err := c.ListNatGateways(ctx, spec.Region, spec.CompartmentID, prog.VcnID)
		if err != nil {
			return bwFail(BwDiscover, prog, fmt.Errorf("列出 NAT 网关失败: %w", err))
		}
		for _, gw := range list {
			if gw.FreeformTags[BandwidthTagKey] != spec.InstanceID {
				continue
			}
			if gw.LifecycleState == ociclient.NatLifecycleTerminated {
				continue
			}
			prog.NatGatewayID = gw.ID
			break
		}
	}
	if prog.NatGatewayID == "" {
		prog.NatMissing = true
		notes = append(notes, "没有找到本功能创建的 NAT 网关")
	}

	if prog.ListenerName == "" {
		prog.ListenerName = bwListenerName
	}
	if prog.BackendSetName == "" {
		prog.BackendSetName = bwBackendSetName
	}

	note := "已定位要清理的资源"
	if len(notes) > 0 {
		note = strings.Join(notes, "；") + "，按已清理继续"
	}
	return bwAdvance(BwListenerDeleting, prog, note)
}

// bwDeleteListener 删监听。资源不存在视为已完成。
func bwDeleteListener(ctx context.Context, spec BandwidthSpec, c bwClient, prog bwProgress) outcome {
	if prog.NLBID == "" {
		return bwAdvance(BwBackendSetDeleting, prog, "没有 NLB，跳过删监听")
	}
	if err := c.DeleteNLBListener(ctx, spec.Region, prog.NLBID, prog.ListenerName); err != nil &&
		!ociclient.IsNotFound(err) && !ociclient.IsConflict(err) {
		return bwFail(BwListenerDeleting, prog, fmt.Errorf("删除监听失败: %w", err))
	}
	return bwAdvance(BwBackendSetDeleting, prog, "监听已删除")
}

// bwDeleteBackendSet 删后端集。
func bwDeleteBackendSet(ctx context.Context, spec BandwidthSpec, c bwClient, prog bwProgress) outcome {
	if prog.NLBID == "" {
		return bwAdvance(BwNLBDeleting, prog, "没有 NLB，跳过删后端集")
	}
	if err := c.DeleteNLBBackendSet(ctx, spec.Region, prog.NLBID, prog.BackendSetName); err != nil &&
		!ociclient.IsNotFound(err) && !ociclient.IsConflict(err) {
		return bwFail(BwBackendSetDeleting, prog, fmt.Errorf("删除后端集失败: %w", err))
	}
	return bwAdvance(BwNLBDeleting, prog, "后端集已删除")
}

// bwDeleteNLB 删 NLB，并等到真正消失。
//
// 这一步是整个流程里唯一在花钱的资源，所以必须等到 DELETED 才继续：
// 返回 202 只代表「已受理」，账单上的资源还在。
func bwDeleteNLB(ctx context.Context, spec BandwidthSpec, c bwClient, prog bwProgress) outcome {
	if prog.NLBID == "" {
		return bwAdvance(BwRouteRestoring, prog, "没有 NLB，跳过")
	}
	if err := c.DeleteNetworkLoadBalancer(ctx, spec.Region, prog.NLBID); err != nil &&
		!ociclient.IsNotFound(err) && !ociclient.IsConflict(err) {
		return bwFail(BwNLBDeleting, prog, fmt.Errorf("删除 NLB 失败: %w", err))
	}
	nlb, err := c.GetNetworkLoadBalancer(ctx, spec.Region, prog.NLBID)
	switch {
	case err != nil && ociclient.IsNotFound(err):
		return bwAdvance(BwRouteRestoring, prog, "NLB 已删除")
	case err != nil:
		return bwFail(BwNLBDeleting, prog, fmt.Errorf("确认 NLB 状态失败: %w", err))
	case nlb.LifecycleState == ociclient.NLBLifecycleDeleted:
		return bwAdvance(BwRouteRestoring, prog, "NLB 已删除")
	default:
		return bwWait(BwNLBDeleting, prog, fmt.Sprintf("NLB 正在删除（%s），等待释放", nlb.LifecycleState))
	}
}

// bwRestoreRoute 把默认路由改回去。
//
// 必须在删 NAT 之前做：OCI 不允许删除仍被路由表引用的 NAT 网关。
func bwRestoreRoute(ctx context.Context, spec BandwidthSpec, c bwClient, prog bwProgress) outcome {
	if prog.RouteTableID == "" {
		return bwFail(BwRouteRestoring, prog, errors.New(
			"找不到实例所在子网的路由表，无法还原默认路由。请到控制台手工把默认路由改回互联网网关后再点重试"))
	}
	rt, err := c.GetRouteTable(ctx, spec.Region, prog.RouteTableID)
	if err != nil {
		return bwFail(BwRouteRestoring, prog, fmt.Errorf("读取路由表失败: %w", err))
	}
	current := defaultRouteTarget(rt.RouteRules)
	// 已经不指向我们的 NAT 了：要么本来就没切过，要么用户手工改回去了。
	if prog.NatGatewayID == "" || current != prog.NatGatewayID {
		return bwAdvance(BwNatDeleting, prog, "默认路由已不指向 NAT 网关，无需还原")
	}

	// 优先用开启时存下来的原规则，那样连别的静态规则一起精确还原。
	var next []ociclient.RouteRule
	if prog.RoutesSaved {
		next = prog.OriginalRoutes
	} else {
		// 没有存过原规则（关闭任务拿不到开启任务的 progress）：只能去找
		// 互联网网关。只有一个才敢用，多个就是歧义，停下来让人判断。
		igws, err := c.ListInternetGateways(ctx, spec.Region, spec.CompartmentID, prog.VcnID)
		if err != nil {
			return bwFail(BwRouteRestoring, prog, fmt.Errorf("列出互联网网关失败: %w", err))
		}
		if len(igws) == 0 {
			// 没有互联网网关说明这个子网本来就不出公网，那把默认路由删掉即可。
			next = removeDefaultRoute(rt.RouteRules)
		} else if len(igws) == 1 {
			next = replaceDefaultRoute(rt.RouteRules, igws[0].ID, "ocim-bw: 还原默认路由")
		} else {
			return bwFail(BwRouteRestoring, prog, fmt.Errorf(
				"VCN 里有 %d 个互联网网关，无法确定默认路由该指回哪一个。请到控制台手工改回后再点重试",
				len(igws)))
		}
	}

	if _, err := c.UpdateRouteTable(ctx, spec.Region, prog.RouteTableID, next); err != nil {
		return bwFail(BwRouteRestoring, prog, fmt.Errorf("还原默认路由失败: %w", err))
	}
	return bwAdvance(BwNatDeleting, prog, "默认路由已还原")
}

// bwDeleteNat 删 NAT 网关，并等到真正消失。
func bwDeleteNat(ctx context.Context, spec BandwidthSpec, c bwClient, prog bwProgress) outcome {
	if prog.NatGatewayID == "" {
		return bwCleaned(prog)
	}
	if err := c.DeleteNatGateway(ctx, spec.Region, prog.NatGatewayID); err != nil &&
		!ociclient.IsNotFound(err) && !ociclient.IsConflict(err) {
		return bwFail(BwNatDeleting, prog, fmt.Errorf("删除 NAT 网关失败: %w", err))
	}
	gw, err := c.GetNatGateway(ctx, spec.Region, prog.NatGatewayID)
	switch {
	case err != nil && ociclient.IsNotFound(err):
		return bwCleaned(prog)
	case err != nil:
		return bwFail(BwNatDeleting, prog, fmt.Errorf("确认 NAT 网关状态失败: %w", err))
	case gw.LifecycleState == ociclient.NatLifecycleTerminated:
		return bwCleaned(prog)
	default:
		return bwWait(BwNatDeleting, prog, fmt.Sprintf("NAT 网关正在删除（%s），等待释放", gw.LifecycleState))
	}
}

// ---- 收尾与辅助 ----

// bwSucceeded 开启成功。
func bwSucceeded(prog bwProgress) outcome {
	prog.Stage = BwReady
	pub := prog.PublicIP
	if pub == "" {
		pub = "（NLB 公网 IP 待分配）"
	}
	prog.Note = "500Mbps 已开启"
	return outcome{
		Done:     true,
		Stage:    BwReady,
		Progress: encodeBWProgress(prog),
		Result: fmt.Sprintf(
			"500Mbps 已开启：NLB %s，公网入口 %s，后端 %s:%d。注意 NLB 会产生费用，不用时请点「关闭」",
			prog.NLBID, pub, prog.PrivateIP, prog.ListenerPort),
	}
}

// bwCleaned 清理完成。
func bwCleaned(prog bwProgress) outcome {
	prog.Stage = BwCleaned
	prog.Note = "清理完成"
	var parts []string
	if prog.NLBMissing {
		parts = append(parts, "NLB 本就不存在")
	} else {
		parts = append(parts, "NLB 已删除")
	}
	if prog.NatMissing {
		parts = append(parts, "NAT 网关本就不存在")
	} else {
		parts = append(parts, "NAT 网关已删除")
	}
	parts = append(parts, "默认路由已还原")
	return outcome{
		Done:     true,
		Stage:    BwCleaned,
		Progress: encodeBWProgress(prog),
		Result:   "500Mbps 已关闭：" + strings.Join(parts, "，"),
	}
}

// bwAdvance 推进到下一阶段：立刻执行下一步。
func bwAdvance(stage string, prog bwProgress, note string) outcome {
	prog.Stage = stage
	prog.StageSince = time.Now()
	prog.Note = note
	return outcome{
		Done:     false,
		Stage:    stage,
		Progress: encodeBWProgress(prog),
		Result:   note,
		NextAt:   time.Now(),
	}
}

// bwWait 留在当前阶段继续等。
func bwWait(stage string, prog bwProgress, note string) outcome {
	prog.Stage = stage
	prog.Note = note
	return outcome{
		Done:      false,
		Stage:     stage,
		Progress:  encodeBWProgress(prog),
		Result:    note,
		NextAt:    time.Now().Add(bwPollInterval),
		NoAttempt: true,
	}
}

// bwFail 在某个阶段失败。stage 原样保留，用户点「重试」从这一步继续。
func bwFail(stage string, prog bwProgress, err error) outcome {
	prog.Stage = stage
	prog.Note = err.Error()
	return outcome{
		Done:     true,
		Err:      err,
		Stage:    stage,
		Progress: encodeBWProgress(prog),
	}
}

// bwResourceName 拼资源显示名。带实例 ID 末段，方便在控制台一眼认出是谁的。
func bwResourceName(kind string, prog bwProgress) string {
	tail := prog.InstanceID
	if idx := strings.LastIndex(tail, "."); idx >= 0 && idx+1 < len(tail) {
		tail = tail[idx+1:]
	}
	if len(tail) > 12 {
		tail = tail[len(tail)-12:]
	}
	if tail == "" {
		tail = "unknown"
	}
	return fmt.Sprintf("%s-%s-%s", BandwidthResourcePrefix, kind, tail)
}

// bwTags 是本功能创建的所有资源上的标记。
func bwTags(instanceID string) map[string]string {
	return map[string]string{BandwidthTagKey: instanceID}
}

// isAMDFreeTierShape 判断是不是 AMD 免费实例。
//
// 只认 VM.Standard.E2.1.Micro：这是 Always Free 的 AMD 规格，也是唯一带宽
// 被限制在 50Mbps、值得绕一道的规格。
func isAMDFreeTierShape(shape string) bool {
	return strings.EqualFold(strings.TrimSpace(shape), "VM.Standard.E2.1.Micro")
}

// staticRouteRules 只留下 STATIC 规则。
//
// LOCAL 规则由 OCI 维护，写回去会被拒绝；把整个规则集原样 PUT 回去的写法
// 一旦带上 LOCAL 就会在还原阶段失败，而那时 NAT 还没删，用户会卡在一个
// 看起来莫名其妙的地方。
func staticRouteRules(rules []ociclient.RouteRule) []ociclient.RouteRule {
	out := make([]ociclient.RouteRule, 0, len(rules))
	for _, r := range rules {
		if strings.EqualFold(r.RouteType, "LOCAL") {
			continue
		}
		out = append(out, r)
	}
	return out
}

// defaultRouteTarget 返回 0.0.0.0/0 指向的网络实体 ID。
func defaultRouteTarget(rules []ociclient.RouteRule) string {
	for _, r := range rules {
		if r.Destination == "0.0.0.0/0" {
			return r.NetworkEntityID
		}
	}
	return ""
}

// replaceDefaultRoute 把 0.0.0.0/0 指向 target，其余规则原样保留。
func replaceDefaultRoute(rules []ociclient.RouteRule, target, description string) []ociclient.RouteRule {
	out := make([]ociclient.RouteRule, 0, len(rules)+1)
	replaced := false
	for _, r := range rules {
		if strings.EqualFold(r.RouteType, "LOCAL") {
			continue
		}
		if r.Destination == "0.0.0.0/0" {
			if replaced {
				// 重复的默认路由本身就是非法配置，保留一条即可。
				continue
			}
			replaced = true
			out = append(out, ociclient.RouteRule{
				Destination:     "0.0.0.0/0",
				DestinationType: "CIDR_BLOCK",
				NetworkEntityID: target,
				Description:     description,
			})
			continue
		}
		out = append(out, r)
	}
	if !replaced {
		out = append(out, ociclient.RouteRule{
			Destination:     "0.0.0.0/0",
			DestinationType: "CIDR_BLOCK",
			NetworkEntityID: target,
			Description:     description,
		})
	}
	return out
}

// removeDefaultRoute 删掉 0.0.0.0/0，其余规则原样保留。
func removeDefaultRoute(rules []ociclient.RouteRule) []ociclient.RouteRule {
	out := make([]ociclient.RouteRule, 0, len(rules))
	for _, r := range rules {
		if strings.EqualFold(r.RouteType, "LOCAL") {
			continue
		}
		if r.Destination == "0.0.0.0/0" {
			continue
		}
		out = append(out, r)
	}
	return out
}

// nlbPublicIP 从 NLB 的地址里挑出公网 IP。
func nlbPublicIP(nlb *ociclient.NetworkLoadBalancer) string {
	if nlb == nil {
		return ""
	}
	for _, ip := range nlb.IpAddresses {
		if ip.IsPublic && ip.IPAddress != "" {
			return ip.IPAddress
		}
	}
	return ""
}

// containsString 判断切片里有没有这个值。
func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func decodeBWProgress(raw string) bwProgress {
	var p bwProgress
	if strings.TrimSpace(raw) == "" {
		return p
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return bwProgress{}
	}
	return p
}

func encodeBWProgress(p bwProgress) string {
	buf, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(buf)
}
