package opssvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"ocicore/internal/netsvc"
	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

// OpenPortsSpec 是「放行端口」任务的入参快照。
//
// 任务在创建时快照一次，之后每轮原样重放——与抢机任务同样的理由：参数在
// 任务生命周期内不变是刻意的，否则用户改一次表单就改变了正在跑的任务语义。
type OpenPortsSpec struct {
	Region         string `json:"region"`
	SecurityListID string `json:"securityListId"`
	// Source 是来源 CIDR。留空由 BuildIngressRule 兜底成 0.0.0.0/0。
	Source string     `json:"source,omitempty"`
	Rules  []PortRule `json:"rules"`
}

// PortRule 是一条要放行的规则。
type PortRule struct {
	// Protocol 取 "6"=TCP "17"=UDP "1"=ICMP "all"。
	Protocol string `json:"protocol"`
	// Port 为 0 表示不限端口。
	Port        int    `json:"port"`
	Description string `json:"description,omitempty"`
}

// EncodeOpenPortsSpec 把入参序列化成任务里存的那一列。
func EncodeOpenPortsSpec(s OpenPortsSpec) (string, error) {
	buf, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("opssvc: 序列化放行端口参数失败: %w", err)
	}
	return string(buf), nil
}

// DecodeOpenPortsSpec 从任务里取回入参。
func DecodeOpenPortsSpec(raw string) (OpenPortsSpec, error) {
	var s OpenPortsSpec
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return s, fmt.Errorf("opssvc: 解析放行端口参数失败: %w", err)
	}
	return s, nil
}

// networkClient 是放行端口用到的两个 OCI 调用。
//
// 收窄成接口而不是直接依赖 *ociclient.Client：测试要能断言「PUT 的规则数
// 只增不减」这条最要紧的性质，而这不需要真的走签名与 HTTP。
// *ociclient.Client 天然满足这个接口。
type networkClient interface {
	GetSecurityList(ctx context.Context, region, securityListID string) (*ociclient.SecurityList, error)
	UpdateSecurityList(ctx context.Context, region, securityListID string,
		ingress []ociclient.IngressSecurityRule, egress []ociclient.EgressSecurityRule) (*ociclient.SecurityList, error)
}

// handleOpenPorts 是 open_ports 类型的处理器：取凭据 → 应用规则。
func (s *Service) handleOpenPorts(ctx context.Context, t *store.OpsTask) outcome {
	spec, err := DecodeOpenPortsSpec(t.Spec)
	if err != nil {
		return failed(err)
	}
	client, _, err := s.conns.ForID(ctx, t.AccountID)
	if err != nil {
		return failed(fmt.Errorf("读取账号凭据失败: %w", err))
	}
	msg, err := applyOpenPorts(ctx, client, spec)
	if err != nil {
		return failed(err)
	}
	return ok(msg)
}

// applyOpenPorts 把请求的端口追加到安全列表，并保证原有规则一条不丢。
//
// 安全列表的 UpdateSecurityList 是**整体替换**语义：PUT 里没带上的规则会被
// 静默删除。所以这里的做法是「读全量 → 在副本上追加 → 全量写回」，并且
// 在写回之后校验规则数没有减少。漏一条入站规则的表现是某个服务突然不通，
// 而用户根本不会想到是「放行端口」这个操作干的。
func applyOpenPorts(ctx context.Context, net networkClient, spec OpenPortsSpec) (string, error) {
	if strings.TrimSpace(spec.Region) == "" {
		return "", errors.New("缺少区域")
	}
	if strings.TrimSpace(spec.SecurityListID) == "" {
		return "", errors.New("缺少安全列表 ID")
	}
	if len(spec.Rules) == 0 {
		return "", errors.New("没有指定要放行的端口")
	}

	list, err := net.GetSecurityList(ctx, spec.Region, spec.SecurityListID)
	if err != nil {
		return "", fmt.Errorf("读取安全列表失败: %w", err)
	}
	if list == nil {
		return "", errors.New("安全列表返回为空")
	}

	existing := list.IngressSecurityRules
	// 全量拷贝原有规则。用 copy 而不是直接 append 到 existing：existing 可能
	// 是存储层返回的切片，直接 append 在容量足够时会改写它背后的数组。
	merged := make([]ociclient.IngressSecurityRule, len(existing), len(existing)+len(spec.Rules))
	copy(merged, existing)

	// 已有规则的指纹集合，用于幂等跳过：同一条规则重复放行不该堆出第二条。
	seen := make(map[string]bool, len(merged)+len(spec.Rules))
	for _, r := range merged {
		seen[ingressKey(r)] = true
	}

	added, skipped := 0, 0
	for _, r := range spec.Rules {
		if err := ValidatePortRule(r); err != nil {
			return "", err
		}
		tpl := netsvc.RuleTemplate{
			Direction:   netsvc.DirIngress,
			Protocol:    r.Protocol,
			Port:        r.Port,
			Description: r.Description,
		}
		rule := netsvc.BuildIngressRule(tpl, spec.Source, r.Description)
		key := ingressKey(rule)
		if seen[key] {
			skipped++
			continue
		}
		seen[key] = true
		merged = append(merged, rule)
		added++
	}

	// 全部已存在时不动云端：一次多余的 PUT 就是一次整体替换的机会，
	// 没有任何收益，只有风险。
	if added == 0 {
		return fmt.Sprintf("所需端口均已放行，未做改动（现有 %d 条入站规则）", len(existing)), nil
	}

	// 关键：egress 原样带回。安全列表是白名单，漏掉出站规则等于掐断该子网
	// 的全部对外流量，而实例本身看起来一切正常——这种故障最难排查。
	updated, err := net.UpdateSecurityList(ctx, spec.Region, spec.SecurityListID,
		merged, list.EgressSecurityRules)
	if err != nil {
		return "", fmt.Errorf("更新安全列表失败: %w", err)
	}

	total := len(merged)
	if updated != nil {
		// 防御性校验：更新后入站规则数不能少于更新前。少一条就是被整体替换
		// 吞掉了，必须立刻报错而不是让用户以为成功了。
		if len(updated.IngressSecurityRules) < len(existing) {
			return "", fmt.Errorf("更新后入站规则数从 %d 降到 %d，疑似规则丢失，请立即到控制台核对",
				len(existing), len(updated.IngressSecurityRules))
		}
		total = len(updated.IngressSecurityRules)
	}

	msg := fmt.Sprintf("新增 %d 条，跳过 %d 条（已存在），现共 %d 条入站规则", added, skipped, total)
	if updated != nil && !netsvc.HasEgress(*updated) {
		msg += "。警告：该安全列表没有出站规则，子网内实例将无法访问外网，建议补一条出站全放行"
	}
	return msg, nil
}

// ValidatePortRule 校验单条规则，避免把明显写错的参数发到云端。
//
// 导出给 HTTP 层在创建任务前做一次前置校验：任务一旦落库就会在后台执行，
// 让用户等到执行阶段才发现端口写错，体验上等于「提交成功但什么也没发生」。
func ValidatePortRule(r PortRule) error {
	switch strings.ToLower(strings.TrimSpace(r.Protocol)) {
	case "6", "17", "all", "1":
	default:
		return fmt.Errorf("不支持的协议 %q（只接受 6 / 17 / 1 / all）", r.Protocol)
	}
	if r.Port < 0 || r.Port > 65535 {
		return fmt.Errorf("端口 %d 超出 0-65535", r.Port)
	}
	return nil
}

// ingressKey 把一条入站规则归约成可比较的指纹。
//
// 只比较决定「是否同一条规则」的三要素：协议、来源、目的端口。描述与
// isStateless 不参与——用户把描述改了不该被当成一条新规则再加一遍。
func ingressKey(r ociclient.IngressSecurityRule) string {
	var port string
	switch strings.ToLower(r.Protocol) {
	case "6":
		if r.TCPOptions != nil && r.TCPOptions.DestinationPortRange != nil {
			pr := r.TCPOptions.DestinationPortRange
			port = fmt.Sprintf("%d-%d", pr.Min, pr.Max)
		}
	case "17":
		if r.UDPOptions != nil && r.UDPOptions.DestinationPortRange != nil {
			pr := r.UDPOptions.DestinationPortRange
			port = fmt.Sprintf("%d-%d", pr.Min, pr.Max)
		}
	case "1":
		if r.ICMPOptions != nil {
			port = fmt.Sprintf("icmp:%d", r.ICMPOptions.Type)
		}
	}
	return strings.ToLower(r.Protocol) + "|" + r.Source + "|" + port
}
