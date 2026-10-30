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

// 救援流程的阶段。
//
// 一次救援是「把目标实例的引导卷卸下来、挂到救援机、人工修、再挂回去」。
// 每一步都单独落库（stage + progress），因为中间任何一步都可能失败，而失败
// 之后用户点「重试」必须从失败的那一步继续——从头再来会把已经卸下来的卷
// 再卸一次，或者在卷还挂在救援机上时去启动原实例。
//
// 这是全项目唯一一个「用户的数据挂在天上」的流程：DETACHING 之后到
// REATTACHING 完成之间，引导卷不属于任何一台运行中的机器。所有判断都按
// 「宁可停下让人看」来写，不做任何自动猜测。
const (
	RescuePrecheck        = "PRECHECK"
	RescueStopping        = "STOPPING"
	RescueDetaching       = "DETACHING"
	RescueAttaching       = "ATTACHING"
	RescueReadyForRepair  = "READY_FOR_REPAIR"
	RescueDetachingRescue = "DETACHING_RESCUE"
	RescueReattaching     = "REATTACHING"
	RescueStarting        = "STARTING"
	RescueDone            = "DONE"
	RescueFailed          = "FAILED"
)

// RescueStages 是合法阶段的有序列表，供 HTTP 层与前端校验/展示。
var RescueStages = []string{
	RescuePrecheck, RescueStopping, RescueDetaching, RescueAttaching,
	RescueReadyForRepair, RescueDetachingRescue, RescueReattaching,
	RescueStarting, RescueDone,
}

// 引导卷挂载关系的生命周期状态。ociclient 里没有为它单独定义常量
// （挂载关系不像实例那样被到处判断），这里就近定义。
const (
	bootAttachAttaching = "ATTACHING"
	bootAttachAttached  = "ATTACHED"
	bootAttachDetaching = "DETACHING"
)

const (
	// rescueStageTimeout 是单个阶段的最长等待时间。
	//
	// 关机/分离/挂载都是分钟级的操作，给 10 分钟足够。超时判失败而不是无限
	// 等下去：卡住的时候用户需要知道"卡在哪一步"，而不是看着转圈。
	rescueStageTimeout = 10 * time.Minute

	// rescuePollInterval 是等待云平台收敛时的轮询间隔。
	rescuePollInterval = 15 * time.Second

	// softStopGrace 是正常关机等多久之后升级为强制关机。
	//
	// 需要救援的机器往往已经关不掉了（引导坏了、内核挂了），一直等 SOFTSTOP
	// 只会等到阶段超时。超过这个时长就改发强制关机。
	softStopGrace = 2 * time.Minute

	// DefaultRescueMinutes 是救援任务的默认总时限。人工修复可能要很久，
	// 但流程本身（除 READY_FOR_REPAIR 等待）不该跑这么久。
	DefaultRescueMinutes = 120
	// MaxRescueMinutes 是总时限上限。给到 12 小时：跨夜排查也够用，
	// 又不至于让一个忘了收尾的任务挂上一整周。
	MaxRescueMinutes = 720
)

// RescueSpec 是救援任务的入参快照。
//
// 注意这里**没有** BootVolumeID 作为输入：引导卷必须由 PRECHECK 从目标实例的
// 挂载关系里推导出来。让用户填卷 OCID 看起来更灵活，实际上是把"选错卷"的
// 责任推给用户——而选错卷的后果是实例再也起不来。
type RescueSpec struct {
	Region           string `json:"region"`
	TargetInstanceID string `json:"targetInstanceId"`
	RescueInstanceID string `json:"rescueInstanceId"`
	CompartmentID    string `json:"compartmentId"`
	// ExpectBootVolumeID 是可选的前端回显值，只用于交叉校验。
	//
	// 它不参与决策：PRECHECK 推导出的卷 ID 与它不一致时直接失败，而不是
	// 采用它。这样即使用户看到的界面是旧的（卷已经被换过），也不会挂错卷。
	ExpectBootVolumeID string `json:"expectBootVolumeId,omitempty"`
}

// EncodeRescueSpec 把入参序列化成任务里存的那一列。
func EncodeRescueSpec(s RescueSpec) (string, error) {
	buf, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("opssvc: 序列化救援参数失败: %w", err)
	}
	return string(buf), nil
}

// DecodeRescueSpec 从任务里取回入参。
func DecodeRescueSpec(raw string) (RescueSpec, error) {
	var s RescueSpec
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return s, fmt.Errorf("opssvc: 解析救援参数失败: %w", err)
	}
	return s, nil
}

// ValidateRescueSpec 校验入参。HTTP 层在创建任务前调用。
func ValidateRescueSpec(s RescueSpec) error {
	if strings.TrimSpace(s.Region) == "" {
		return errors.New("缺少区域")
	}
	if strings.TrimSpace(s.TargetInstanceID) == "" {
		return errors.New("缺少目标实例 ID")
	}
	if strings.TrimSpace(s.RescueInstanceID) == "" {
		return errors.New("缺少救援机实例 ID")
	}
	if strings.TrimSpace(s.CompartmentID) == "" {
		return errors.New("缺少 compartmentId")
	}
	if s.TargetInstanceID == s.RescueInstanceID {
		return errors.New("目标实例与救援机不能是同一台")
	}
	return nil
}

// rescueProgress 是落库的结构化进展。
//
// 它同时承担另一个作用：跨轮次、跨进程重启保存"已经查到的卷 ID 和挂载关系
// ID"。重启后 stage 还在 DETACHING，但内存里什么都没有，只能靠这里恢复。
type rescueProgress struct {
	Stage string `json:"stage"`
	Note  string `json:"note,omitempty"`
	// StageSince 是本阶段的开始时刻，用于判断阶段超时。
	//
	// 不能用 tasks.updated_at：那个字段每写一次进展就变，量不出"在这个阶段
	// 停了多久"。
	StageSince time.Time `json:"stageSince,omitempty"`

	TargetInstanceID string `json:"targetInstanceId,omitempty"`
	TargetName       string `json:"targetName,omitempty"`
	RescueInstanceID string `json:"rescueInstanceId,omitempty"`
	RescueName       string `json:"rescueName,omitempty"`

	BootVolumeID string `json:"bootVolumeId,omitempty"`
	// AttachmentID 是目标实例名下的引导卷挂载关系。
	AttachmentID string `json:"attachmentId,omitempty"`
	// RescueAttachmentID 是救援机名下的挂载关系。
	RescueAttachmentID string `json:"rescueAttachmentId,omitempty"`

	// ForceStop 记录是否已经把正常关机升级为强制关机，避免每轮重复发。
	ForceStop bool `json:"forceStop,omitempty"`
	// RepairResumed 记录"已经在 READY_FOR_REPAIR 停过一次"。
	//
	// 用它区分「刚走到等待修复」和「用户点了继续」——两者在库里都是
	// READY_FOR_REPAIR 阶段，唯一差别是前者没暂停过。没有这个标记的话，
	// 用户一点「继续」就会立刻又被暂停回去，永远走不到下一步。
	RepairResumed bool `json:"repairResumed,omitempty"`
}

// rescueClient 是救援流程用到的 OCI 操作。
//
// 收窄成接口是为了测试：救援流程最需要被验证的恰恰是"哪一步在什么状态下
// 该做什么"，而这些东西用真账号根本没法复现（不能真的去拔一台机器的盘）。
type rescueClient interface {
	GetInstance(ctx context.Context, region, instanceID string) (*ociclient.Instance, error)
	InstanceAction(ctx context.Context, region, instanceID, action string) (*ociclient.Instance, error)
	ListBootVolumeAttachments(ctx context.Context, region, compartmentID, availabilityDomain, instanceID string) ([]ociclient.BootVolumeAttachment, error)
	DetachBootVolume(ctx context.Context, region, attachmentID string) error
	AttachBootVolume(ctx context.Context, region, instanceID, bootVolumeID, displayName string) (*ociclient.BootVolumeAttachment, error)
}

// handleRescue 是 rescue 类型的处理器。
func (s *Service) handleRescue(ctx context.Context, t *store.OpsTask) outcome {
	spec, err := DecodeRescueSpec(t.Spec)
	if err != nil {
		return failed(err)
	}
	client, _, err := s.conns.ForID(ctx, t.AccountID)
	if err != nil {
		return failed(fmt.Errorf("读取账号凭据失败: %w", err))
	}
	return rescueStep(ctx, t, spec, client)
}

// rescueStep 执行一轮：判断当前阶段，做这一步该做的事。
//
// 一轮只推进一步。看起来慢（每步至少一个 tick），但换来的是：进程在任意
// 时刻被杀，重启后从库里那个 stage 继续，不会重复已经做过的破坏性操作。
func rescueStep(ctx context.Context, t *store.OpsTask, spec RescueSpec, c rescueClient) outcome {
	prog := decodeRescueProgress(t.Progress)
	stage := strings.TrimSpace(t.Stage)
	if stage == "" {
		stage = RescuePrecheck
	}
	// t.State 是这一轮被捞到时的状态（run 把它改成 running 之后没有再回写
	// 内存里的值）。pending 说明这是「新开一轮」：首次执行，或者用户点了
	// 重试。两种情况都要重置阶段计时——否则重试后第一轮就会撞上上一轮残留
	// 的阶段超时，用户点了重试却看到「已等待 12 分钟」，永远推不动。
	if t.State == store.OpsPending || prog.StageSince.IsZero() {
		prog.StageSince = time.Now()
	}
	prog.Stage = stage

	// 总时限。零值表示不限（手工补建的任务）。
	if !t.ExpiresAt.IsZero() && time.Now().After(t.ExpiresAt) {
		return rescueFail(stage, prog, fmt.Errorf(
			"已超过最长运行时间，流程停在 %s。引导卷当前状态请到块存储页面人工确认", stage))
	}
	// 单阶段超时。
	if elapsed := time.Since(prog.StageSince); elapsed > rescueStageTimeout {
		return rescueFail(stage, prog, fmt.Errorf(
			"%s 阶段已等待 %s 仍未完成，已停止推进。引导卷当前状态请人工确认后点「重试」",
			stage, elapsed.Round(time.Second)))
	}

	switch stage {
	case RescuePrecheck:
		return rescuePrecheck(ctx, spec, c, prog)
	case RescueStopping:
		return rescueStopping(ctx, spec, c, prog)
	case RescueDetaching:
		return rescueDetaching(ctx, spec, c, prog)
	case RescueAttaching:
		return rescueAttaching(ctx, spec, c, prog)
	case RescueReadyForRepair:
		return rescueWaitRepair(prog)
	case RescueDetachingRescue:
		return rescueDetachingRescue(ctx, spec, c, prog)
	case RescueReattaching:
		return rescueReattaching(ctx, spec, c, prog)
	case RescueStarting:
		return rescueStarting(ctx, spec, c, prog)
	case RescueDone:
		// 走到这里说明任务曾被标成 DONE 但没有落到终态（比如进程刚好在
		// finish 之前被杀）。直接收尾，不要重新跑一遍流程。
		return rescueSucceeded(prog)
	default:
		return rescueFail(stage, prog, fmt.Errorf(
			"未知的救援阶段 %q，无法继续。请取消任务后重新创建", stage))
	}
}

// rescuePrecheck 校验前提条件并推导出要操作的引导卷。
//
// 这里是整个流程唯一允许"确定卷是哪一块"的地方，因此校验写得比别处啰嗦：
// 一旦从这里带着错误的卷 ID 出去，后面每一步都在照着错的做。
func rescuePrecheck(ctx context.Context, spec RescueSpec, c rescueClient, prog rescueProgress) outcome {
	target, err := c.GetInstance(ctx, spec.Region, spec.TargetInstanceID)
	if err != nil {
		return rescueFail(RescuePrecheck, prog, fmt.Errorf("读取目标实例失败: %w", err))
	}
	switch target.LifecycleState {
	case ociclient.LifecycleTerminating, ociclient.LifecycleTerminated:
		return rescueFail(RescuePrecheck, prog, fmt.Errorf(
			"目标实例当前状态为 %s，已无法救援", target.LifecycleState))
	}
	// 归属校验：目标实例必须落在用户指定的 compartment 里。否则可能是在
	// 一个 compartment 下创建任务、实际动到另一个 compartment 的机器。
	if spec.CompartmentID != "" && target.CompartmentID != "" &&
		target.CompartmentID != spec.CompartmentID {
		return rescueFail(RescuePrecheck, prog, fmt.Errorf(
			"目标实例不属于指定的 compartment（实例在 %s），已停止", target.CompartmentID))
	}

	rescue, err := c.GetInstance(ctx, spec.Region, spec.RescueInstanceID)
	if err != nil {
		return rescueFail(RescuePrecheck, prog, fmt.Errorf("读取救援机失败: %w", err))
	}
	switch rescue.LifecycleState {
	case ociclient.LifecycleTerminating, ociclient.LifecycleTerminated:
		return rescueFail(RescuePrecheck, prog, fmt.Errorf(
			"救援机当前状态为 %s，请换一台", rescue.LifecycleState))
	}
	// 引导卷是可用域级资源，跨可用域挂不上。提前拦下来，别等到 ATTACHING
	// 才让用户看到一句 OCI 的 400。
	if target.AvailabilityDomain != "" && rescue.AvailabilityDomain != "" &&
		target.AvailabilityDomain != rescue.AvailabilityDomain {
		return rescueFail(RescuePrecheck, prog, fmt.Errorf(
			"救援机在 %s，目标实例在 %s，不在同一可用域，引导卷无法跨域挂载",
			rescue.AvailabilityDomain, target.AvailabilityDomain))
	}

	att, err := findBootAttachment(ctx, c, spec.Region, spec.CompartmentID, spec.TargetInstanceID, "")
	if err != nil {
		return rescueFail(RescuePrecheck, prog, err)
	}
	if att == nil {
		// 这是最该停手的一种情况：没有挂载关系就没有可信的卷 ID，而后面每
		// 一步都照着这个 ID 走。绝不能退化成"用用户填的卷 ID"。
		return rescueFail(RescuePrecheck, prog, fmt.Errorf(
			"在实例 %s 名下没有找到处于 ATTACHED 的引导卷挂载关系，无法确定要操作哪一块卷，已停止",
			target.DisplayName))
	}

	// 交叉校验用户界面上的值。不一致说明界面已经过期，宁可停下。
	if spec.ExpectBootVolumeID != "" && att.BootVolumeID != spec.ExpectBootVolumeID {
		return rescueFail(RescuePrecheck, prog, fmt.Errorf(
			"界面上显示的引导卷是 %s，但实例当前实际挂载的是 %s；为防止挂错卷已停止，请刷新后重试",
			spec.ExpectBootVolumeID, att.BootVolumeID))
	}

	prog.TargetInstanceID = spec.TargetInstanceID
	prog.TargetName = target.DisplayName
	prog.RescueInstanceID = spec.RescueInstanceID
	prog.RescueName = rescue.DisplayName
	prog.BootVolumeID = att.BootVolumeID
	prog.AttachmentID = att.ID

	return rescueAdvance(RescueStopping, prog, fmt.Sprintf(
		"预检通过：引导卷 %s 来自实例 %s 的引导卷挂载关系",
		att.BootVolumeID, target.DisplayName))
}

// rescueStopping 把目标实例停到 STOPPED。
//
// 必须先停：OCI 不允许对运行中的实例分离引导卷。
func rescueStopping(ctx context.Context, spec RescueSpec, c rescueClient, prog rescueProgress) outcome {
	inst, err := c.GetInstance(ctx, spec.Region, spec.TargetInstanceID)
	if err != nil {
		return rescueFail(RescueStopping, prog, fmt.Errorf("读取目标实例失败: %w", err))
	}
	switch inst.LifecycleState {
	case ociclient.LifecycleStopped:
		return rescueAdvance(RescueDetaching, prog, "目标实例已停止，准备分离引导卷")
	case ociclient.LifecycleStopping:
		// 正常关机迟迟不完成就升级为强制关机。需要救援的机器常常已经关不掉，
		// 一直等 SOFTSTOP 只会等到阶段超时。
		if time.Since(prog.StageSince) <= softStopGrace {
			return rescueWait(RescueStopping, prog, "目标实例正在关机，等待 STOPPED")
		}
		if prog.ForceStop {
			return rescueWait(RescueStopping, prog, "已发强制关机，等待 STOPPED")
		}
		if _, err := c.InstanceAction(ctx, spec.Region, spec.TargetInstanceID, ociclient.ActionStop); err != nil {
			return rescueFail(RescueStopping, prog, fmt.Errorf("强制关机失败: %w", err))
		}
		prog.ForceStop = true
		return rescueWait(RescueStopping, prog, "正常关机超过 2 分钟仍未完成，已改为强制关机")
	case ociclient.LifecycleRunning, ociclient.LifecycleStarting:
		// 先软关机：强制断电可能丢文件系统里尚未落盘的元数据，而救援流程
		// 恰恰是为了把这块盘的数据救回来。
		if _, err := c.InstanceAction(ctx, spec.Region, spec.TargetInstanceID, ociclient.ActionSoftStop); err != nil {
			return rescueFail(RescueStopping, prog, fmt.Errorf("关机请求失败: %w", err))
		}
		return rescueWait(RescueStopping, prog, "已请求正常关机，等待 STOPPED")
	default:
		return rescueFail(RescueStopping, prog, fmt.Errorf(
			"目标实例状态为 %s，无法继续关机", inst.LifecycleState))
	}
}

// rescueDetaching 分离目标实例的引导卷。
func rescueDetaching(ctx context.Context, spec RescueSpec, c rescueClient, prog rescueProgress) outcome {
	inst, err := c.GetInstance(ctx, spec.Region, spec.TargetInstanceID)
	if err != nil {
		return rescueFail(RescueDetaching, prog, fmt.Errorf("读取目标实例失败: %w", err))
	}
	if inst.LifecycleState != ociclient.LifecycleStopped {
		return rescueWait(RescueDetaching, prog, fmt.Sprintf(
			"目标实例状态为 %s，需先回到 STOPPED 才能分离引导卷", inst.LifecycleState))
	}

	att, err := findBootAttachment(ctx, c, spec.Region, spec.CompartmentID, spec.TargetInstanceID, prog.BootVolumeID)
	if err != nil {
		return rescueFail(RescueDetaching, prog, err)
	}
	if att == nil {
		// 挂载关系已经不存在：分离完成了。这正是幂等的关键——重试时若上一轮
		// 已经分离成功，这里直接放行，不会再发一次 DELETE。
		prog.AttachmentID = ""
		return rescueAdvance(RescueAttaching, prog, "引导卷已从目标实例分离")
	}
	prog.AttachmentID = att.ID
	switch att.LifecycleState {
	case bootAttachDetaching:
		return rescueWait(RescueDetaching, prog, "引导卷正在分离，等待挂载关系消失")
	case bootAttachAttached:
		if err := c.DetachBootVolume(ctx, spec.Region, att.ID); err != nil {
			return rescueFail(RescueDetaching, prog, fmt.Errorf("分离引导卷失败: %w", err))
		}
		return rescueWait(RescueDetaching, prog, "已请求分离引导卷，等待挂载关系消失")
	default:
		return rescueWait(RescueDetaching, prog, fmt.Sprintf(
			"引导卷挂载关系状态为 %s，等待其变为可分离", att.LifecycleState))
	}
}

// rescueAttaching 把引导卷挂到救援机。
func rescueAttaching(ctx context.Context, spec RescueSpec, c rescueClient, prog rescueProgress) outcome {
	if prog.BootVolumeID == "" {
		// 只有 progress 丢失（手工改库、早期版本）才会走到这里。没有卷 ID
		// 就绝对不能猜，退回 PRECHECK 重新推导。
		return rescueAdvance(RescuePrecheck, prog, "缺少引导卷 ID，回到预检重新确认")
	}

	rescue, err := c.GetInstance(ctx, spec.Region, spec.RescueInstanceID)
	if err != nil {
		return rescueFail(RescueAttaching, prog, fmt.Errorf("读取救援机失败: %w", err))
	}
	switch rescue.LifecycleState {
	case ociclient.LifecycleStopped:
		// 挂载引导卷要求实例处于 STOPPED，继续往下走。
	case ociclient.LifecycleRunning, ociclient.LifecycleStarting:
		if _, err := c.InstanceAction(ctx, spec.Region, spec.RescueInstanceID, ociclient.ActionSoftStop); err != nil {
			return rescueFail(RescueAttaching, prog, fmt.Errorf("停止救援机失败: %w", err))
		}
		return rescueWait(RescueAttaching, prog, "已请求停止救援机，挂载引导卷前它必须是 STOPPED")
	default:
		return rescueWait(RescueAttaching, prog, fmt.Sprintf(
			"救援机状态为 %s，等待其变为 STOPPED", rescue.LifecycleState))
	}

	// 幂等：救援机名下已经有这块卷的挂载关系，就不再重复创建。
	if att, err := findBootAttachment(ctx, c, spec.Region, spec.CompartmentID, spec.RescueInstanceID, prog.BootVolumeID); err != nil {
		return rescueFail(RescueAttaching, prog, err)
	} else if att != nil {
		prog.RescueAttachmentID = att.ID
		if att.LifecycleState == bootAttachAttached {
			return rescueAdvance(RescueReadyForRepair, prog, "引导卷已挂到救援机")
		}
		return rescueWait(RescueAttaching, prog, fmt.Sprintf(
			"引导卷正在挂到救援机（%s）", att.LifecycleState))
	}

	// 分离可能还没在服务端完全生效：卷仍挂在原实例上时不能挂到救援机。
	// 这一层是 DETACHING 之后的兜底，避免两处都"看到"已分离的假象。
	holder, err := bootVolumeHolder(ctx, c, spec.Region, spec.CompartmentID, prog.BootVolumeID)
	if err != nil {
		return rescueFail(RescueAttaching, prog, err)
	}
	if holder != "" && holder != spec.RescueInstanceID {
		return rescueWait(RescueAttaching, prog, fmt.Sprintf(
			"引导卷仍挂在实例 %s 名下，等待分离完成", holder))
	}

	att, err := c.AttachBootVolume(ctx, spec.Region, spec.RescueInstanceID, prog.BootVolumeID, "ocicore-rescue")
	if err != nil {
		return rescueFail(RescueAttaching, prog, fmt.Errorf("挂载引导卷到救援机失败: %w", err))
	}
	prog.RescueAttachmentID = att.ID
	return rescueWait(RescueAttaching, prog, "已请求挂载引导卷到救援机，等待 ATTACHED")
}

// rescueWaitRepair 是人工介入点：卷已经挂在救援机上，等用户修完。
//
// 这里必须是 pause 而不是"一直 reschedule 等"：修复可能花几十分钟也可能
// 花几小时，空转既没意义又占着并发槽（MaxConcurrent 只有 2）。
func rescueWaitRepair(prog rescueProgress) outcome {
	// 已经在这一点暂停过，又跑起来了 —— 只可能是用户点了「继续」。
	if prog.RepairResumed {
		return rescueAdvance(RescueDetachingRescue, prog, "已确认修复完成，开始从救援机卸下引导卷")
	}
	prog.RepairResumed = true
	vol := prog.BootVolumeID
	if vol == "" {
		vol = "未知"
	}
	msg := fmt.Sprintf(
		"引导卷 %s 已挂到救援机 %s，可以 SSH 登录救援机修复。修完后点「继续」，流程会卸下引导卷并挂回原实例。",
		vol, rescueMachineLabel(prog))
	return outcome{
		Done:     false,
		Pause:    true,
		Stage:    RescueReadyForRepair,
		Progress: encodeRescueProgress(prog),
		Result:   msg,
	}
}

// rescueDetachingRescue 把引导卷从救援机上卸下来。
func rescueDetachingRescue(ctx context.Context, spec RescueSpec, c rescueClient, prog rescueProgress) outcome {
	rescue, err := c.GetInstance(ctx, spec.Region, spec.RescueInstanceID)
	if err != nil {
		return rescueFail(RescueDetachingRescue, prog, fmt.Errorf("读取救援机失败: %w", err))
	}
	if rescue.LifecycleState != ociclient.LifecycleStopped {
		if rescue.LifecycleState == ociclient.LifecycleRunning ||
			rescue.LifecycleState == ociclient.LifecycleStarting {
			if _, err := c.InstanceAction(ctx, spec.Region, spec.RescueInstanceID, ociclient.ActionSoftStop); err != nil {
				return rescueFail(RescueDetachingRescue, prog, fmt.Errorf("停止救援机失败: %w", err))
			}
			return rescueWait(RescueDetachingRescue, prog, "已请求停止救援机，分离引导卷前它必须是 STOPPED")
		}
		return rescueWait(RescueDetachingRescue, prog, fmt.Sprintf(
			"救援机状态为 %s，等待其变为 STOPPED", rescue.LifecycleState))
	}

	att, err := findBootAttachment(ctx, c, spec.Region, spec.CompartmentID, spec.RescueInstanceID, prog.BootVolumeID)
	if err != nil {
		return rescueFail(RescueDetachingRescue, prog, err)
	}
	if att == nil {
		prog.RescueAttachmentID = ""
		return rescueAdvance(RescueReattaching, prog, "引导卷已从救援机分离")
	}
	prog.RescueAttachmentID = att.ID
	if att.LifecycleState == bootAttachDetaching {
		return rescueWait(RescueDetachingRescue, prog, "引导卷正在从救援机分离")
	}
	if err := c.DetachBootVolume(ctx, spec.Region, att.ID); err != nil {
		return rescueFail(RescueDetachingRescue, prog, fmt.Errorf("从救援机分离引导卷失败: %w", err))
	}
	return rescueWait(RescueDetachingRescue, prog, "已请求从救援机分离引导卷")
}

// rescueReattaching 把引导卷挂回原实例。
func rescueReattaching(ctx context.Context, spec RescueSpec, c rescueClient, prog rescueProgress) outcome {
	target, err := c.GetInstance(ctx, spec.Region, spec.TargetInstanceID)
	if err != nil {
		return rescueFail(RescueReattaching, prog, fmt.Errorf("读取目标实例失败: %w", err))
	}
	switch target.LifecycleState {
	case ociclient.LifecycleStopped:
		// 正常情况：STOPPING 阶段停下来的，之后没人动过。
	case ociclient.LifecycleStopping:
		return rescueWait(RescueReattaching, prog, "目标实例正在关机，等待 STOPPED 后再挂回引导卷")
	default:
		// 用户在修复期间手动启动过原实例（此时它没有引导卷，属于异常状态）。
		// 停掉再继续，而不是直接挂——挂载要求 STOPPED。
		if _, err := c.InstanceAction(ctx, spec.Region, spec.TargetInstanceID, ociclient.ActionSoftStop); err != nil {
			return rescueFail(RescueReattaching, prog, fmt.Errorf(
				"目标实例处于 %s，尝试关机失败: %w", target.LifecycleState, err))
		}
		return rescueWait(RescueReattaching, prog, fmt.Sprintf(
			"目标实例被手动启动为 %s，已请求关机，等待 STOPPED", target.LifecycleState))
	}

	if att, err := findBootAttachment(ctx, c, spec.Region, spec.CompartmentID, spec.TargetInstanceID, prog.BootVolumeID); err != nil {
		return rescueFail(RescueReattaching, prog, err)
	} else if att != nil {
		if att.LifecycleState == bootAttachAttached {
			prog.AttachmentID = att.ID
			return rescueAdvance(RescueStarting, prog, "引导卷已挂回原实例")
		}
		return rescueWait(RescueReattaching, prog, fmt.Sprintf(
			"引导卷正在挂回原实例（%s）", att.LifecycleState))
	}

	holder, err := bootVolumeHolder(ctx, c, spec.Region, spec.CompartmentID, prog.BootVolumeID)
	if err != nil {
		return rescueFail(RescueReattaching, prog, err)
	}
	if holder != "" && holder != spec.TargetInstanceID {
		return rescueWait(RescueReattaching, prog, fmt.Sprintf(
			"引导卷仍挂在实例 %s 名下，等待分离完成", holder))
	}

	att, err := c.AttachBootVolume(ctx, spec.Region, spec.TargetInstanceID, prog.BootVolumeID, "")
	if err != nil {
		return rescueFail(RescueReattaching, prog, fmt.Errorf("把引导卷挂回原实例失败: %w", err))
	}
	prog.AttachmentID = att.ID
	return rescueWait(RescueReattaching, prog, "已请求把引导卷挂回原实例，等待 ATTACHED")
}

// rescueStarting 启动原实例，等它回到 RUNNING。
func rescueStarting(ctx context.Context, spec RescueSpec, c rescueClient, prog rescueProgress) outcome {
	inst, err := c.GetInstance(ctx, spec.Region, spec.TargetInstanceID)
	if err != nil {
		return rescueFail(RescueStarting, prog, fmt.Errorf("读取目标实例失败: %w", err))
	}
	switch inst.LifecycleState {
	case ociclient.LifecycleRunning:
		return rescueSucceeded(prog)
	case ociclient.LifecycleStarting, ociclient.LifecycleStopping:
		return rescueWait(RescueStarting, prog, fmt.Sprintf(
			"目标实例状态为 %s，等待 RUNNING", inst.LifecycleState))
	case ociclient.LifecycleStopped:
		if _, err := c.InstanceAction(ctx, spec.Region, spec.TargetInstanceID, ociclient.ActionStart); err != nil {
			return rescueFail(RescueStarting, prog, fmt.Errorf("启动目标实例失败: %w", err))
		}
		return rescueWait(RescueStarting, prog, "已请求启动目标实例，等待 RUNNING")
	default:
		return rescueFail(RescueStarting, prog, fmt.Errorf(
			"目标实例状态为 %s，无法启动", inst.LifecycleState))
	}
}

// rescueSucceeded 收尾。
func rescueSucceeded(prog rescueProgress) outcome {
	prog.Stage = RescueDone
	prog.Note = "救援完成"
	return outcome{
		Done:     true,
		Stage:    RescueDone,
		Progress: encodeRescueProgress(prog),
		Result: fmt.Sprintf("救援完成：引导卷 %s 已挂回 %s 并启动",
			prog.BootVolumeID, rescueLabel(prog)),
	}
}

// rescueAdvance 推进到下一阶段：立刻执行下一步。
func rescueAdvance(stage string, prog rescueProgress, note string) outcome {
	prog.Stage = stage
	prog.StageSince = time.Now()
	prog.Note = note
	prog.ForceStop = false
	return outcome{
		Done:     false,
		Stage:    stage,
		Progress: encodeRescueProgress(prog),
		// 推进阶段是真实进展，计入 attempts：任务列表里"尝试次数"就等于
		// 已完成的步骤数，比"轮询了多少次"有意义得多。
		NextAt: time.Now(),
	}
}

// rescueWait 留在当前阶段继续等。
func rescueWait(stage string, prog rescueProgress, note string) outcome {
	prog.Stage = stage
	prog.Note = note
	return outcome{
		Done:     false,
		Stage:    stage,
		Progress: encodeRescueProgress(prog),
		NextAt:   time.Now().Add(rescuePollInterval),
		// 等云平台收敛不是一次失败的尝试，不计次。
		NoAttempt: true,
	}
}

// rescueFail 在某个阶段失败。stage 原样保留，用户点「重试」会从这一步继续。
func rescueFail(stage string, prog rescueProgress, err error) outcome {
	prog.Stage = stage
	prog.Note = err.Error()
	return outcome{
		Done:     true,
		Err:      err,
		Stage:    stage,
		Progress: encodeRescueProgress(prog),
	}
}

// findBootAttachment 找出实例名下的引导卷挂载关系。
//
// bootVolumeID 为空表示"不限定哪块卷"，此时若找到多条会报错而不是随便挑一条：
// 一个实例有多块引导卷挂载关系是异常状态，随便挑一条去分离就是赌运气。
//
// 返回 (nil, nil) 表示没有匹配的挂载关系——调用方据此判断"已分离/已挂好"。
func findBootAttachment(ctx context.Context, c rescueClient, region, compartmentID, instanceID, bootVolumeID string) (*ociclient.BootVolumeAttachment, error) {
	atts, err := c.ListBootVolumeAttachments(ctx, region, compartmentID, "", instanceID)
	if err != nil {
		return nil, fmt.Errorf("列出引导卷挂载关系失败: %w", err)
	}
	var found []ociclient.BootVolumeAttachment
	for _, a := range atts {
		// 服务端已经按 instanceId 过滤过，这里再筛一遍：过滤条件是安全关键，
		// 不能只依赖对端行为（代理、兼容层、将来重构都可能悄悄丢掉参数）。
		if a.InstanceID != "" && a.InstanceID != instanceID {
			continue
		}
		if bootVolumeID != "" && a.BootVolumeID != bootVolumeID {
			continue
		}
		if a.LifecycleState == "DETACHED" {
			continue
		}
		found = append(found, a)
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		return &found[0], nil
	default:
		if bootVolumeID == "" {
			return nil, fmt.Errorf(
				"实例 %s 名下有 %d 条引导卷挂载关系，无法确定该操作哪一块；请人工确认后再试",
				instanceID, len(found))
		}
		return nil, fmt.Errorf(
			"实例 %s 名下有 %d 条指向引导卷 %s 的挂载关系，状态异常；请人工确认后再试",
			instanceID, len(found), bootVolumeID)
	}
}

// bootVolumeHolder 返回当前挂着这块引导卷的实例 ID，没有则返回空串。
func bootVolumeHolder(ctx context.Context, c rescueClient, region, compartmentID, bootVolumeID string) (string, error) {
	atts, err := c.ListBootVolumeAttachments(ctx, region, compartmentID, "", "")
	if err != nil {
		return "", fmt.Errorf("列出引导卷挂载关系失败: %w", err)
	}
	for _, a := range atts {
		if a.BootVolumeID != bootVolumeID {
			continue
		}
		if a.LifecycleState == "DETACHED" {
			continue
		}
		return a.InstanceID, nil
	}
	return "", nil
}

// decodeRescueProgress 解析落库的进展。坏数据按空处理：宁可重新确认一遍，
// 也不要因为一段 JSON 解析失败就卡死整个流程。
func decodeRescueProgress(raw string) rescueProgress {
	var p rescueProgress
	if strings.TrimSpace(raw) == "" {
		return p
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return rescueProgress{}
	}
	return p
}

// encodeRescueProgress 序列化进展。失败时返回空串（等于"不改动这一列"），
// 比把半截 JSON 写进库里好。
func encodeRescueProgress(p rescueProgress) string {
	buf, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(buf)
}

// rescueLabel 是给用户看的目标实例标识，名字优先，没有名字就退回 ID。
func rescueLabel(p rescueProgress) string {
	if p.TargetName != "" {
		return p.TargetName
	}
	if p.TargetInstanceID != "" {
		return p.TargetInstanceID
	}
	return "目标实例"
}

// rescueMachineLabel 是给用户看的救援机标识。
func rescueMachineLabel(p rescueProgress) string {
	if p.RescueName != "" {
		return p.RescueName
	}
	if p.RescueInstanceID != "" {
		return p.RescueInstanceID
	}
	return "救援机"
}
