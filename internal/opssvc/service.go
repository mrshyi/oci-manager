// Package opssvc 执行通用的运维任务（ops_tasks）。
//
// 与抢机（huntsvc）共用 internal/tasksched 的调度循环，差别只在「一轮做什么」：
// 抢机是反复尝试创建实例，运维任务是一次性的编排（放行端口、换 IP、救援……）。
// 所以这里不重复写 tick / 并发闸门 / 重启错峰，只按 kind 把任务分派给处理器。
//
// 状态约定：任务被捞到时若是 pending，先迁到 running（记录 started_at），
// 处理器返回后落到 succeeded / failed。处理器只关心业务，不碰状态机。
package opssvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"ocicore/internal/cf"
	"ocicore/internal/ociclient"
	"ocicore/internal/store"
	"ocicore/internal/tasksched"
)

const (
	// TickInterval 是调度循环的唤醒间隔。
	TickInterval = 10 * time.Second

	// MaxConcurrent 是同一时刻最多有几个运维任务在执行。
	//
	// 比抢机更保守：运维任务大多是「改配置」类写操作，同时改多个子网的
	// 安全列表既难排查也更容易触发限流。
	MaxConcurrent = 2

	// opTimeout 是单轮执行的超时。运维任务是有限步骤的编排，不该像抢机
	// 那样无限重试，卡住就判失败，让用户看到明确结果而不是一直转圈。
	opTimeout = 3 * time.Minute

	// drainTimeout 是停机时给在跑任务留的收尾窗口。
	drainTimeout = 5 * time.Second
)

// Connector 按账号 ID 产出 OCI 客户端。
//
// 与 huntsvc 一样收窄成只含 ForID 的接口：工厂还带着代理、缓存、限流一整套
// 装配逻辑，运维任务只关心「给我这个账号的客户端」。收窄后测试才能塞假实现。
type Connector interface {
	ForID(ctx context.Context, accountID string) (*ociclient.Client, *store.Account, error)
}

// Deps 是构造依赖。
type Deps struct {
	Store *store.Store
	Conns Connector
	// Instances 执行单台实例的开关机。只有批量任务需要它，留空时批量任务会
	// 直接判失败并说明未接线，而不是每轮 panic。
	Instances InstanceActioner
	// DNS 可选，用于换 IP 之后把绑定的 A 记录指向新地址。
	// 留空表示不联动 DNS，换 IP 照常执行。
	DNS DNSUpdater
	// OnEvent 可选，用于把任务结束推给通知渠道。
	OnEvent func(ctx context.Context, event, title, body string)
}

// DNSUpdater 在换到新 IP 之后把跟随这台实例的域名指过去。
//
// 返回值里没有 error，这是刻意的：这个方法的调用点在换 IP 的成功路径上，
// 只要签名里出现 error，就一定会有人写成「DNS 失败 → 任务失败」，而那时候
// IP 已经换完了、机器是通的，用户看到的却是「失败」，转头去重启机器。
// DNS 的结果只能通过 Result.Status 表达。
type DNSUpdater interface {
	UpdateForInstance(ctx context.Context, accountID, instanceID, ip string) cf.Result
}

// outcome 是一轮执行的结果。
//
// 用结构体而不是 (result, error)：换 IP 这类任务要区分三种结局——成功、
// 失败、以及「这一轮没命中但还要再来」。第三种既不是错误（不该判 failed），
// 也不是成功，只有 error 两态表达不了。
type outcome struct {
	// Done 为 true 表示任务到此终结：Err 为 nil 则成功，否则失败。
	Done bool
	// Result 是成功时给用户看的结果摘要。
	Result string
	// Progress 是可选的结构化进展（JSON），给前端画进度条。
	Progress string
	// NextAt 是 Done=false 时的下次执行时刻。零值会退回到任务间隔。
	NextAt time.Time
	// NoAttempt 为 true 时这一轮不计入尝试次数（限流退避、阶段推进）。
	NoAttempt bool
	// Stage 是要落库的阶段（分阶段任务用）。空值表示不改动 stage 列。
	Stage string
	// Pause 为 true 表示到达人工介入点：把任务置为 paused，等用户 resume。
	// 救援流程在「引导卷已挂到救援机、等用户修复」这一步就是这种情况。
	Pause bool
	// DNSStatus / DNSError 是顺带更新 DNS 的结果，与 Done/Err 完全无关。
	// 换 IP 成功而 DNS 失败时，Done=true、Err=nil，DNSStatus=failed。
	DNSStatus string
	DNSError  string
	// Err 是失败或本轮未命中的原因。
	Err error
}

// ok 表示任务成功结束。
func ok(result string) outcome { return outcome{Done: true, Result: result} }

// failed 表示任务失败结束。
func failed(err error) outcome { return outcome{Done: true, Err: err} }

// handler 执行一轮任务。处理器不负责改状态——那是 run 的事。
type handler func(ctx context.Context, t *store.OpsTask) outcome

// Service 是运维任务的调度器。
type Service struct {
	st        *store.Store
	conns     Connector
	instances InstanceActioner
	dns       DNSUpdater
	onEvent   func(context.Context, string, string, string)

	// handlers 按 kind 分派。没有登记的类型会直接判失败并说明原因，
	// 而不是让任务停在 pending 里永远没人处理。
	handlers map[string]handler

	sched *tasksched.Scheduler[store.OpsTask]
}

// New 构造服务并接好调度器。
func New(d Deps) *Service {
	s := &Service{
		st:        d.Store,
		conns:     d.Conns,
		instances: d.Instances,
		dns:       d.DNS,
		onEvent:   d.OnEvent,
	}
	// 只登记已实现的种类。没登记的类型会被判 failed 并说明原因，
	// 不会静默停在 pending。
	s.handlers = map[string]handler{
		store.OpsKindOpenPorts: s.handleOpenPorts,
		store.OpsKindChangeIP:  s.handleChangeIP,
		store.OpsKindRescue:    s.handleRescue,
		store.OpsKindBulk:      s.handleBulk,
		store.OpsKindEnable500: s.handleBandwidth,
	}

	s.sched = tasksched.New(tasksched.Options[store.OpsTask]{
		Tick:    TickInterval,
		Limit:   MaxConcurrent,
		Stagger: tasksched.DefaultStagger,
		Source:  opsSource{st: d.Store},
		Handle:  func(ctx context.Context, t store.OpsTask) { s.run(ctx, &t) },
		OnError: func(err error) {
			slog.Warn("运维任务：读取到期任务失败", "err", err)
		},
	})
	return s
}

// opsSource 把 store 的到期查询接到调度器上。
type opsSource struct{ st *store.Store }

func (o opsSource) Due(ctx context.Context, now time.Time, limit int) ([]store.OpsTask, error) {
	return o.st.DueOpsTasks(ctx, now, limit)
}

// Run 启动调度循环，直到 ctx 结束。
func (s *Service) Run(ctx context.Context) {
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
		defer cancel()
		if err := s.sched.Drain(dctx); err != nil {
			slog.Warn("运维任务：停止时仍有任务在执行，已放弃等待", "err", err)
		}
	}()
	s.sched.Run(ctx)
}

// run 执行一轮：置 running → 分派 → 落终态。
func (s *Service) run(ctx context.Context, t *store.OpsTask) {
	// 用独立 context：一次编排可能已经改了一半云端配置，用户在这期间做
	// 别的操作不该把它掐断，留下半成品。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
	defer cancel()

	// pending 才需要迁到 running。已经是 running 的是多轮任务被再次捞到，
	// 直接继续执行。迁移失败通常意味着用户刚取消/删除，或另一轮抢先推进了。
	//
	// 刻意不回写 t.State：处理器要靠它区分「新开一轮」（pending，含用户点
	// 重试）和「多轮任务的下一轮」（running）。救援流程据此决定是否重置
	// 阶段计时。
	if t.State == store.OpsPending {
		if _, err := s.st.TransitionOpsTask(ctx, t.ID, store.OpsRunning); err != nil {
			if errors.Is(err, store.ErrOpsTaskConflict) || errors.Is(err, store.ErrOpsTaskNotFound) {
				return
			}
			slog.Warn("运维任务：置为运行中失败", "task", t.ID, "err", err)
			return
		}
	}

	h, ok := s.handlers[t.Kind]
	if !ok {
		// 未知 kind 不静默卡住：直接判失败并说清楚。否则任务会一直 pending，
		// 界面显示「等待中」，实际永远不会有进展。
		s.finish(ctx, t, store.OpsFailed, outcome{
			Err: fmt.Errorf("未知的运维任务类型 %q，没有对应的处理器", t.Kind),
		})
		return
	}

	out := h(ctx, t)
	if out.Pause {
		s.pause(ctx, t, out)
		return
	}
	if !out.Done {
		s.reschedule(ctx, t, out)
		return
	}
	if out.Err != nil {
		// 失败时保留当前 stage，用户点「重试」会从这一步继续，而不是从头再来。
		s.finish(ctx, t, store.OpsFailed, out)
		return
	}
	s.finish(ctx, t, store.OpsSucceeded, out)
}

// reschedule 记录本轮进展并排下一轮，任务保持 running。
func (s *Service) reschedule(ctx context.Context, t *store.OpsTask, out outcome) {
	next := out.NextAt
	if next.IsZero() {
		// 必须给一个未来的时刻：0 表示立刻到期，会变成忙循环。
		next = time.Now().Add(time.Duration(t.IntervalSeconds) * time.Second)
	}
	if err := s.st.AdvanceOpsTask(ctx, t.ID, store.OpsTaskProgress{
		Stage:     out.Stage,
		Progress:  out.Progress,
		LastError: shortErr(out.Err),
		// 批量这类任务中途也要让人看到"做到第几台了"，所以把阶段性摘要写进
		// result；收尾时会被最终结果覆盖。
		Result:    out.Result,
		NextAt:    next,
		NoAttempt: out.NoAttempt,
		DNSStatus: out.DNSStatus,
		DNSError:  out.DNSError,
	}); err != nil && !errors.Is(err, store.ErrOpsTaskNotFound) {
		slog.Warn("运维任务：写回进展失败", "task", t.ID, "err", err)
	}
}

// pause 把任务停在人工介入点。
//
// 与 reschedule 的区别是状态：paused 的任务不会被 DueOpsTasks 捞到，直到用户
// 主动点「继续」。救援流程需要这个——引导卷挂到救援机之后，要等用户 SSH 进去
// 把文件改完，中间可能是几分钟也可能是几小时，不能一直空转。
func (s *Service) pause(ctx context.Context, t *store.OpsTask, out outcome) {
	if err := s.st.AdvanceOpsTask(ctx, t.ID, store.OpsTaskProgress{
		Stage:     out.Stage,
		Progress:  out.Progress,
		LastError: shortErr(out.Err),
		// 暂停时把提示语写进 result：任务列表只显示 result / last_error，
		// 写在这里用户才知道"为什么停了、下一步该干什么"。
		Result:    out.Result,
		NextAt:    time.Now(),
		NoAttempt: true,
	}); err != nil && !errors.Is(err, store.ErrOpsTaskNotFound) {
		slog.Warn("运维任务：写回暂停进展失败", "task", t.ID, "err", err)
	}
	if _, err := s.st.TransitionOpsTask(ctx, t.ID, store.OpsPaused); err != nil &&
		!errors.Is(err, store.ErrOpsTaskConflict) && !errors.Is(err, store.ErrOpsTaskNotFound) {
		slog.Warn("运维任务：暂停失败", "task", t.ID, "err", err)
	}
	if s.onEvent != nil {
		s.onEvent(ctx, "ops.paused", "运维任务等待人工处理："+t.Name, out.Result)
	}
}

// finish 写回进展并落到终态。
//
// 收 outcome 而不是一长串位置参数：换 IP 的 DNS 结果与任务结果必须各走
// 各的字段，而九个字符串参数里漏传一个不会编译报错，只会静默丢信息。
func (s *Service) finish(ctx context.Context, t *store.OpsTask, state string, out outcome) {
	lastErr, result := "", ""
	if state == store.OpsSucceeded {
		result = out.Result
	} else {
		lastErr = shortErr(out.Err)
	}

	// 先记进展再切状态：切换终态会把 next_at 归零，但 result / last_error
	// 只在 AdvanceOpsTask 里写。顺序反了的话，成功的结果就没地方落了。
	if err := s.st.AdvanceOpsTask(ctx, t.ID, store.OpsTaskProgress{
		Stage:     out.Stage,
		Progress:  out.Progress,
		LastError: lastErr,
		Result:    result,
		NextAt:    time.Time{},
		DNSStatus: out.DNSStatus,
		DNSError:  out.DNSError,
	}); err != nil && !errors.Is(err, store.ErrOpsTaskNotFound) {
		slog.Warn("运维任务：写回结果失败", "task", t.ID, "err", err)
	}
	if _, err := s.st.TransitionOpsTask(ctx, t.ID, state); err != nil &&
		!errors.Is(err, store.ErrOpsTaskConflict) && !errors.Is(err, store.ErrOpsTaskNotFound) {
		slog.Warn("运维任务：切换终态失败", "task", t.ID, "state", state, "err", err)
	}
	if s.onEvent != nil {
		s.onEvent(ctx, "ops."+state, "运维任务"+state+"："+t.Name, lastErr)
	}
	slog.Info("运维任务：结束",
		"task", t.ID, "kind", t.Kind, "state", state, "result", result, "err", lastErr,
		"dnsStatus", out.DNSStatus, "dnsError", out.DNSError)
}

// shortErr 截断过长的错误文本。这些字符串要落库并显示在列表一行里。
func shortErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	const max = 300
	if len(msg) > max {
		return msg[:max] + "…"
	}
	return msg
}
