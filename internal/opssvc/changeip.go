package opssvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"ocicore/internal/cf"
	"ocicore/internal/netsvc"
	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

// ChangeIPSpec 是「按 CIDR 换公网 IP」任务的入参快照。
//
// 目标不是"换一次 IP"，而是"反复换，直到换到一个落在目标网段里的 IP"。
// 典型用途是换到某个地区/运营商的地址段，或避开被墙的段。
type ChangeIPSpec struct {
	Region        string `json:"region"`
	VnicID        string `json:"vnicId"`
	CompartmentID string `json:"compartmentId"`
	// TargetCIDRs 是目标网段，命中任意一个即停止。
	TargetCIDRs []string `json:"targetCidrs"`
}

// EncodeChangeIPSpec 把入参序列化成任务里存的那一列。
func EncodeChangeIPSpec(s ChangeIPSpec) (string, error) {
	buf, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("opssvc: 序列化换 IP 参数失败: %w", err)
	}
	return string(buf), nil
}

// DecodeChangeIPSpec 从任务里取回入参。
func DecodeChangeIPSpec(raw string) (ChangeIPSpec, error) {
	var s ChangeIPSpec
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return s, fmt.Errorf("opssvc: 解析换 IP 参数失败: %w", err)
	}
	return s, nil
}

// ValidateChangeIPSpec 校验入参。HTTP 层在创建任务前调用。
func ValidateChangeIPSpec(s ChangeIPSpec) error {
	if strings.TrimSpace(s.Region) == "" {
		return errors.New("缺少区域")
	}
	if strings.TrimSpace(s.VnicID) == "" {
		return errors.New("缺少网卡 ID")
	}
	if strings.TrimSpace(s.CompartmentID) == "" {
		return errors.New("缺少 compartmentId")
	}
	if len(s.TargetCIDRs) == 0 {
		return errors.New("至少指定一个目标网段")
	}
	for _, c := range s.TargetCIDRs {
		if _, err := netip.ParsePrefix(strings.TrimSpace(c)); err != nil {
			return fmt.Errorf("目标网段 %q 无效: %w", c, err)
		}
	}
	return nil
}

// ipChanger 抽象一次换 IP。生产实现走 netsvc，测试注入假实现。
type ipChanger interface {
	ChangePublicIP(ctx context.Context, region, vnicID, compartmentID string) (*netsvc.ChangePublicIPResult, error)
}

// ociIPChanger 把 *ociclient.Client 适配成 ipChanger。
type ociIPChanger struct{ client *ociclient.Client }

func (o ociIPChanger) ChangePublicIP(ctx context.Context, region, vnicID, compartmentID string) (*netsvc.ChangePublicIPResult, error) {
	return netsvc.ChangePublicIP(ctx, o.client, region, vnicID, compartmentID)
}

// handleChangeIP 是 change_ip_cidr 类型的处理器。
func (s *Service) handleChangeIP(ctx context.Context, t *store.OpsTask) outcome {
	spec, err := DecodeChangeIPSpec(t.Spec)
	if err != nil {
		return failed(err)
	}
	client, _, err := s.conns.ForID(ctx, t.AccountID)
	if err != nil {
		return failed(fmt.Errorf("读取账号凭据失败: %w", err))
	}
	return changeIPTowards(ctx, t, spec, ociIPChanger{client}, s.dns)
}

// changeIPTowards 执行一轮换 IP，并判断是否命中目标网段。
//
// 一次换 IP 是「删旧 + 建新」，不可逆：旧 IP 删掉就再也拿不回来。所以这里
// 宁可少打一次，也不在已经超限之后再多换一次——那不是"多花一次调用"，而是
// 多一次让用户永久失去某个 IP 的机会。
//
// upd 可为 nil（没配 Cloudflare）。DNS 的结果只进 outcome 的 DNSStatus /
// DNSError，绝不参与 Done / Err 的判定。
func changeIPTowards(ctx context.Context, t *store.OpsTask, spec ChangeIPSpec, ch ipChanger, upd DNSUpdater) outcome {
	if t.MaxAttempts > 0 && t.Attempts >= t.MaxAttempts {
		return failed(fmt.Errorf(
			"已达最大尝试次数 %d，未命中目标网段；当前公网 IP 保留不变", t.MaxAttempts))
	}
	if !t.ExpiresAt.IsZero() && time.Now().After(t.ExpiresAt) {
		return failed(errors.New("已超过最长运行时间，未命中目标网段；当前公网 IP 保留不变"))
	}

	res, err := ch.ChangePublicIP(ctx, spec.Region, spec.VnicID, spec.CompartmentID)
	if err != nil {
		// 保留 IP 是独立计费资源，删了就永久释放。直接判失败，交给用户手动处理。
		if errors.Is(err, netsvc.ErrReservedIP) {
			return failed(errors.New("当前公网 IP 是保留 IP，不能自动更换；请在网络页面手动处理"))
		}
		// 429 不计入尝试次数：被限流不是任务的一次失败，只退避。
		var apiErr *ociclient.APIError
		if errors.As(err, &apiErr) && apiErr.Class == ociclient.ClassThrottled {
			wait := throttleWait(t.Attempts, apiErr)
			return outcome{
				Done:      false,
				NextAt:    time.Now().Add(wait),
				NoAttempt: true,
				Err:       fmt.Errorf("被 Oracle 限流，%s 后重试", wait.Round(time.Second)),
			}
		}
		return failed(err)
	}
	if res == nil || res.NewIP == "" {
		return failed(errors.New("换 IP 返回结果为空"))
	}

	round := t.Attempts + 1
	progress := fmt.Sprintf(`{"oldIp":%q,"newIp":%q,"attempt":%d}`, res.OldIP, res.NewIP, round)
	dnsStatus, dnsErr := syncDNS(ctx, upd, t, res.NewIP)

	if matchesAnyCIDR(res.NewIP, spec.TargetCIDRs) {
		return outcome{
			Done:      true,
			Result:    fmt.Sprintf("第 %d 轮命中目标网段：%s → %s", round, res.OldIP, res.NewIP),
			DNSStatus: dnsStatus,
			DNSError:  dnsErr,
		}
	}

	// 未命中：排下一轮。任务保持 running，DueOpsTasks 会在 next_at 到点后再捞。
	wait := time.Duration(t.IntervalSeconds) * time.Second
	if wait <= 0 {
		wait = 20 * time.Second
	}
	return outcome{
		Done:      false,
		NextAt:    time.Now().Add(wait),
		Progress:  progress,
		DNSStatus: dnsStatus,
		DNSError:  dnsErr,
		Err:       fmt.Errorf("第 %d 轮换到 %s，未命中目标网段", round, res.NewIP),
	}
}

// syncDNS 把域名指向刚换到的 IP。
//
// 每一轮都同步，不只是命中那一轮：换 IP 是「删旧建新」，旧 IP 一旦释放，
// 域名指着它就是一个打不开的站。中间轮的地址虽然最终会被换掉，但它至少是
// 活的——让域名指向一个活着的地址，比指向一个已经释放的地址好。
//
// 返回值只有 status / detail 两样，没有 error：调用点就在换 IP 的成功路径上，
// 只要这里能返回 error，就一定会有人把它当任务失败处理。
func syncDNS(ctx context.Context, upd DNSUpdater, t *store.OpsTask, ip string) (status, detail string) {
	if upd == nil || strings.TrimSpace(t.InstanceID) == "" {
		return "", ""
	}
	res := upd.UpdateForInstance(ctx, t.AccountID, t.InstanceID, ip)
	if !res.Involved() {
		return "", ""
	}
	if res.Status == cf.StatusUpdated {
		// 成功时不写原因列：那一列只在失败时有意义，写进去会让界面
		// 显示「已更新」外加一句看起来像错误的话。
		return res.Status, ""
	}
	return res.Status, res.Detail
}

// matchesAnyCIDR 报告 ip 是否落在任一 CIDR 内。
func matchesAnyCIDR(ip string, cidrs []string) bool {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(strings.TrimSpace(c))
		if err != nil {
			continue
		}
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// throttleWait 计算限流后的等待时长。
//
// 尊重服务端给的 Retry-After；没有则指数退避 5s → 10s → … 封顶 5 分钟。
// 不用 APIError.Backoff()：它对 Throttled 类固定返回 5 分钟，不是指数。
func throttleWait(attempts int, apiErr *ociclient.APIError) time.Duration {
	if apiErr != nil && apiErr.RetryAfter > 0 {
		return apiErr.RetryAfter
	}
	wait := 5 * time.Second
	for i := 0; i < attempts && wait < 5*time.Minute; i++ {
		wait *= 2
	}
	if wait > 5*time.Minute {
		wait = 5 * time.Minute
	}
	return wait
}
