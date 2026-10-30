// Package httpapi 是面板的 HTTP 接口层。
//
// 所有接口都在 /api 下，前端 SPA 构建产物由 cmd/server 挂在根路径。
// 认证使用 HttpOnly Cookie 会话，配合自定义请求头做 CSRF 防护。
package httpapi

import (
	"context"
	"net/http"
	"sync"
	"time"

	"ocicore/internal/billingsvc"
	"ocicore/internal/capacitysvc"
	"ocicore/internal/cf"
	"ocicore/internal/config"
	"ocicore/internal/geoip"
	"ocicore/internal/huntsvc"
	"ocicore/internal/instancesvc"
	"ocicore/internal/jobsvc"
	"ocicore/internal/logstream"
	"ocicore/internal/notify"
	"ocicore/internal/ociconn"
	"ocicore/internal/opssvc"
	"ocicore/internal/riskguard"
	"ocicore/internal/store"
)

const (
	sessionCookie = "oci_session"

	// csrfHeader 是所有写操作必须携带的自定义头。
	//
	// 浏览器不允许跨源请求携带自定义头（除非目标站点显式开启 CORS 预检，
	// 而本服务从不开启），因此"存在该头"就等价于"请求来自本站脚本"。
	// 这比双提交 Cookie 更简单，且没有令牌同步问题。
	csrfHeader = "X-OCI-Tools"
	csrfValue  = "1"
)

// Deps 是构造 Server 所需的依赖。
//
// 用结构体而非位置参数：这一层会持续接入新的 service，
// 每加一个就改一次函数签名会让所有调用点跟着抖动。
type Deps struct {
	Store     *store.Store
	Config    config.Config
	Conns     *ociconn.Factory
	Instances *instancesvc.Service
	// Budget 是调用预算，传给容量守候的调度器用于超预算降频。可为 nil。
	Budget *riskguard.Guard
	// Logs 是进程日志的环形缓冲。为 nil 时日志流接口返回 503，
	// 其余功能不受影响——日志是排障工具，不该成为启动的前置条件。
	Logs *logstream.Ring
	// GeoIP 是离线 IP 库。为 nil 表示没配库，此时分布图退回按区域估算，
	// 而不是报错——这个库要用户自己去 MaxMind 下载，缺失是常态。
	GeoIP *geoip.DB
}

// Server 持有依赖并注册路由。
type Server struct {
	st        *store.Store
	cfg       config.Config
	conns     *ociconn.Factory
	instances *instancesvc.Service
	mux       *http.ServeMux
	logins    *attemptLimiter
	quotas    *quotaCache
	resources *resourcesCache
	notifier  *notify.Dispatcher
	budget    *riskguard.Guard
	hunter    *huntsvc.Service
	ops       *opssvc.Service
	capacity  *capacitysvc.Service
	billing   *billingsvc.Service
	logs      *logstream.Ring
	cf        *cf.Service
	geo       *geoip.DB
	jobs      *jobsvc.Service
	started   time.Time

	// elevations 记录通过了危险操作复核的会话，见 danger.go。
	elevations *dangerElevations
}

// New 构造服务并注册全部路由。
func New(deps Deps) *Server {
	s := &Server{
		st:        deps.Store,
		cfg:       deps.Config,
		conns:     deps.Conns,
		instances: deps.Instances,
		budget:    deps.Budget,
		logs:      deps.Logs,
		geo:       deps.GeoIP,
		mux:       http.NewServeMux(),
		logins:    newAttemptLimiter(5, 15*time.Minute),
		quotas:    newQuotaCache(),
		resources: newResourcesCache(),
		notifier:  notify.NewDispatcher(deps.Store, notify.WithAllowedNets(deps.Config.NotifyAllowedNets)),
		started:   time.Now(),

		elevations: newDangerElevations(),
	}
	if s.conns == nil {
		s.conns = ociconn.New(deps.Store)
	}
	// 让后台的生命周期轮询也能推送通知（新实例就绪、账号凭据失效）。
	if s.instances != nil {
		s.instances.SetNotifier(s.notifier)
	}
	// 容量守候的调度器建在这里而不是 main：它要用到 notifier 与实例缓存，
	// 两者都只在 Server 里组装完整。
	s.capacity = capacitysvc.New(capacitysvc.Deps{
		Store: s.st, Conns: s.conns, OnChange: s.onCapacityChange,
	})
	// 账单只是按需查询，没有后台轮询，所以构造得比其余 service 简单。
	s.billing = billingsvc.New(billingsvc.Deps{Store: s.st, Conns: s.conns})
	// 长耗时操作（创建实例、换 IP、改配置……）放到后台跑，进度经 SSE 推送。
	s.jobs = jobsvc.New(jobsvc.Deps{Store: s.st, OnChange: s.onJobChange})
	s.jobs.Recover(context.Background())
	s.hunter = huntsvc.New(huntsvc.Deps{
		Store:    s.st,
		Conns:    s.conns,
		Budget:   deps.Budget,
		OnLaunch: s.onHuntLaunched,
		OnEvent:  s.onHuntEvent,
	})
	// 运维任务调度器。与抢机共用 tasksched，但按 kind 分派到各自处理器。
	// 批量任务要通过 instancesvc 执行开关机（它会校验过渡态、刷缓存、起轮询），
	// 所以把实例服务一并注入。
	//
	// 先判 nil 再赋值：把 (*instancesvc.Service)(nil) 塞进接口会得到一个
	// 非 nil 接口，opssvc 里的 "== nil" 判断就失效了，调用时才 panic。
	//
	// Cloudflare 联动也在这里接进去：换 IP 命中后要更新 A 记录。它没有后台
	// 循环，只是一个被 opssvc 调用的纯函数集合，所以构造得比其余 service 简单。
	s.cf = cf.New(cf.Deps{Store: s.st})

	opsDeps := opssvc.Deps{Store: s.st, Conns: s.conns, DNS: s.cf}
	if s.instances != nil {
		opsDeps.Instances = s.instances
	}
	s.ops = opssvc.New(opsDeps)
	s.routes()
	return s
}

func (s *Server) routes() {
	// 无需认证：首次设置与登录。
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("GET /api/risk-budget", s.requireAuth(s.handleRiskBudget))
	s.mux.HandleFunc("POST /api/setup", s.handleSetup)
	s.mux.HandleFunc("POST /api/auth/login", s.handleLogin)

	// 半登录即可访问：仅用于完成第二因子。
	s.mux.HandleFunc("POST /api/auth/totp/verify", s.requireSession(s.handleTOTPVerify))
	s.mux.HandleFunc("POST /api/auth/logout", s.requireSession(s.handleLogout))

	// 需要完整会话。
	//
	// 套了 s.danger 的接口对应前端 L2 / L3 确认框，「危险操作需重新验证 TOTP」
	// 打开时要求附带验证码。只在部分参数下才危险的接口（强制关机、批量操作、
	// 启停账号与轮换密钥、放宽策略的设置）在处理器里自己判断。
	s.mux.HandleFunc("GET /api/auth/me", s.requireAuth(s.handleMe))
	s.mux.HandleFunc("POST /api/auth/totp/setup", s.requireAuth(s.handleTOTPSetup))
	s.mux.HandleFunc("POST /api/auth/totp/enable", s.requireAuth(s.handleTOTPEnable))
	s.mux.HandleFunc("POST /api/auth/totp/disable", s.requireAuth(s.handleTOTPDisable))
	s.mux.HandleFunc("POST /api/auth/password", s.requireAuth(s.handleChangePassword))

	s.mux.HandleFunc("GET /api/accounts", s.requireAuth(s.handleListAccounts))
	s.mux.HandleFunc("POST /api/accounts", s.requireAuth(s.handleCreateAccount))
	s.mux.HandleFunc("GET /api/accounts/{id}", s.requireAuth(s.handleGetAccount))
	s.mux.HandleFunc("PATCH /api/accounts/{id}", s.requireAuth(s.handleUpdateAccount))
	s.mux.HandleFunc("DELETE /api/accounts/{id}", s.requireAuth(s.danger(s.handleDeleteAccount)))
	s.mux.HandleFunc("POST /api/accounts/{id}/check", s.requireAuth(s.handleCheckAccount))
	s.mux.HandleFunc("GET /api/accounts/{id}/regions", s.requireAuth(s.handleAccountRegions))
	// 资源页：按账号汇总卷、公网 IP、控制台连接、VCN 与免费额度用量。只读。
	s.mux.HandleFunc("GET /api/accounts/{id}/resources", s.requireAuth(s.handleAccountResources))
	// 资源页的清理操作。删卷、释放保留 IP 不可恢复，走 danger 并要求回传名称；
	// 删控制台连接不丢数据，只要 confirm=true。
	s.mux.HandleFunc("DELETE /api/accounts/{id}/resources/volumes/{volumeId}", s.requireAuth(s.danger(s.handleDeleteVolume)))
	s.mux.HandleFunc("DELETE /api/accounts/{id}/resources/public-ips/{ipId}", s.requireAuth(s.danger(s.handleReleaseReservedIP)))
	s.mux.HandleFunc("DELETE /api/accounts/{id}/resources/console-connections/{connId}", s.requireAuth(s.handleDeleteConsoleConnection))

	// 账号组织（P2-1）：标签与附加信息。
	//
	// 都不走 danger：改一个标签、填一条备注不改动任何云端资源，也不扩大
	// 攻击面。要求 TOTP 只会让用户把验证码背下来，反而削弱了真正危险操作
	// 那一步的仪式感。写操作仍然记审计。
	//
	// 注意 /api/accounts/tags 与 /api/accounts/{id} 共存：Go 1.22 的 mux 里
	// 字面量段优先于通配段，所以 tags 不会被当成某个账号的 ID。
	s.mux.HandleFunc("GET /api/accounts/tags", s.requireAuth(s.handleListTagUsage))
	s.mux.HandleFunc("GET /api/accounts/{id}/tags", s.requireAuth(s.handleGetAccountTags))
	s.mux.HandleFunc("PUT /api/accounts/{id}/tags", s.requireAuth(s.handleSetAccountTags))
	s.mux.HandleFunc("GET /api/accounts/{id}/meta", s.requireAuth(s.handleGetAccountMeta))
	s.mux.HandleFunc("PUT /api/accounts/{id}/meta", s.requireAuth(s.handleSetAccountMeta))

	// 健康度：色块由服务端算好，前端只负责画。清除记录是写操作但不危险
	// ——它不碰云端资源，只是把"我们据此推断的告警等级"抹掉，下次校验
	// 会重新写。所以不走 danger。
	s.mux.HandleFunc("GET /api/accounts/{id}/health", s.requireAuth(s.handleGetAccountHealth))
	s.mux.HandleFunc("DELETE /api/accounts/{id}/health", s.requireAuth(s.handleClearAccountHealth))

	// 权限自检。五个接口全是 list / summarize，只读、不改任何资源，
	// 所以不走 danger。探测请求不计入调用预算（ociclient.WithoutBudget）。
	s.mux.HandleFunc("GET /api/accounts/{id}/capability-probe", s.requireAuth(s.handleGetCapabilityProbe))
	s.mux.HandleFunc("POST /api/accounts/{id}/capability-probe", s.requireAuth(s.handleRunCapabilityProbe))

	// 计费风险。只读（六路取数全是 list / summarize），所以不走 danger。
	s.mux.HandleFunc("GET /api/accounts/{id}/billing-risk", s.requireAuth(s.handleGetBillingRisk))

	// 添加账号抽屉的两个辅助接口：粘贴解析与保存前测试。
	s.mux.HandleFunc("POST /api/accounts/parse-config", s.requireAuth(s.handleParseConfig))
	s.mux.HandleFunc("POST /api/accounts/check-draft", s.requireAuth(s.handleCheckDraft))

	s.mux.HandleFunc("GET /api/audit", s.requireAuth(s.handleListAudit))
	s.mux.HandleFunc("GET /api/audit/export", s.requireAuth(s.handleExportAudit))
	s.mux.HandleFunc("GET /api/regions", s.requireAuth(s.handleListRegions))

	// 云侧策略模板。纯文案，不碰 OCI，也不生成策略——方案 ADR-2 的结论是
	// 「不需要策略生成」：让面板持有 IAM 写权限去生成策略，等于为了限制
	// 权限而先给一个更大的权限。
	s.mux.HandleFunc("GET /api/iam/policy-template", s.requireAuth(s.handleGetPolicyTemplate))

	// 实例管理。
	s.mux.HandleFunc("GET /api/instances", s.requireAuth(s.handleListInstances))
	s.mux.HandleFunc("GET /api/instances/{id}", s.requireAuth(s.handleGetInstance))
	s.mux.HandleFunc("POST /api/instances/sync", s.requireAuth(s.handleSyncInstances))
	s.mux.HandleFunc("POST /api/instances/{id}/actions/{action}", s.requireAuth(s.handleInstanceAction))
	s.mux.HandleFunc("PATCH /api/instances/{id}", s.requireAuth(s.handleRenameInstance))
	s.mux.HandleFunc("PATCH /api/instances/{id}/note", s.requireAuth(s.handleSetInstanceNote))
	s.mux.HandleFunc("POST /api/instances/{id}/reshape", s.requireAuth(s.danger(s.handleReshapeInstance)))
	s.mux.HandleFunc("DELETE /api/instances/{id}", s.requireAuth(s.danger(s.handleTerminateInstance)))
	s.mux.HandleFunc("POST /api/instances/{id}/dismiss-error", s.requireAuth(s.handleDismissInstanceError))

	s.mux.HandleFunc("POST /api/instances/{id}/console", s.requireAuth(s.handleCreateConsole))
	s.mux.HandleFunc("POST /api/instances/bulk", s.requireAuth(s.handleBulkAction))
	s.mux.HandleFunc("POST /api/auth/sessions/revoke-all", s.requireAuth(s.danger(s.handleRevokeSessions)))

	// 后台任务。长耗时操作提交后立刻返回 202 与任务快照，进度看这里。
	s.mux.HandleFunc("GET /api/jobs", s.requireAuth(s.handleListJobs))
	s.mux.HandleFunc("GET /api/jobs/{id}", s.requireAuth(s.handleGetJob))
	s.mux.HandleFunc("POST /api/jobs/{id}/cancel", s.requireAuth(s.handleCancelJob))
	s.mux.HandleFunc("DELETE /api/jobs/{id}", s.requireAuth(s.handleDeleteJob))
	s.mux.HandleFunc("POST /api/jobs/clear", s.requireAuth(s.handleClearJobs))

	// 实时事件流。SSE 单向推送足够，不需要 WebSocket 的双向能力。
	s.mux.HandleFunc("GET /api/events", s.requireAuth(s.handleEvents))

	// 进程日志流。只读，内容已在写入端脱敏（logstream.Redact），
	// 所以这里不再做二次处理——展示端脱敏意味着明文已经在内存里了。
	s.mux.HandleFunc("GET /api/logs/stream", s.requireAuth(s.handleLogStream))

	// 备份。导出是 L3：文件里装着全部账号的私钥密文，拿到它加口令
	// 就等于拿到全部云账号。恢复是 L4：整体替换数据库，错了之后
	// 连「用面板修回来」的入口都没了，所以还要回传 RESTORE。
	s.mux.HandleFunc("GET /api/backup", s.requireAuth(s.handleGetBackupStatus))
	s.mux.HandleFunc("POST /api/backup/export", s.requireAuth(s.danger(s.handleExportBackup)))
	s.mux.HandleFunc("POST /api/backup/restore", s.requireAuth(s.danger(s.handleRestoreBackup)))

	// 网络。
	s.mux.HandleFunc("GET /api/network/vcns", s.requireAuth(s.handleListVcns))
	s.mux.HandleFunc("GET /api/network/subnets", s.requireAuth(s.handleListSubnets))
	s.mux.HandleFunc("GET /api/network/security-lists", s.requireAuth(s.handleListSecurityLists))
	s.mux.HandleFunc("PUT /api/network/security-lists/{id}", s.requireAuth(s.danger(s.handleUpdateSecurityList)))
	s.mux.HandleFunc("GET /api/network/rule-templates", s.requireAuth(s.handleRuleTemplates))
	s.mux.HandleFunc("GET /api/network/public-ips", s.requireAuth(s.handleListPublicIPs))
	s.mux.HandleFunc("POST /api/network/ensure", s.requireAuth(s.handleEnsureNetwork))
	s.mux.HandleFunc("POST /api/instances/{id}/change-ip", s.requireAuth(s.danger(s.handleChangePublicIP)))
	s.mux.HandleFunc("POST /api/instances/{id}/enable-ipv6", s.requireAuth(s.danger(s.handleEnableIPv6)))
	s.mux.HandleFunc("DELETE /api/instances/{id}/ipv6", s.requireAuth(s.danger(s.handleRemoveIPv6)))
	// 添加附属网卡只是多分配资源，不走 danger；分离会释放网卡上的公网 IP，不可找回，走 danger。
	s.mux.HandleFunc("POST /api/instances/{id}/vnics", s.requireAuth(s.handleAttachVnic))
	s.mux.HandleFunc("DELETE /api/instances/{id}/vnics/{attachmentId}", s.requireAuth(s.danger(s.handleDetachVnic)))

	// 存储。
	s.mux.HandleFunc("GET /api/storage/boot-volumes", s.requireAuth(s.handleListBootVolumes))
	s.mux.HandleFunc("PATCH /api/storage/boot-volumes/{id}", s.requireAuth(s.danger(s.handleUpdateBootVolume)))
	s.mux.HandleFunc("GET /api/storage/volumes", s.requireAuth(s.handleListVolumes))
	s.mux.HandleFunc("PATCH /api/storage/volumes/{id}", s.requireAuth(s.danger(s.handleUpdateVolume)))
	s.mux.HandleFunc("POST /api/storage/boot-volume-attachments/detach", s.requireAuth(s.danger(s.handleDetachBootVolume)))
	s.mux.HandleFunc("POST /api/storage/boot-volume-attachments/attach", s.requireAuth(s.danger(s.handleAttachBootVolume)))
	// 数据盘挂载。救援模式要靠它把坏机器的引导卷挂到好机器上当普通盘改文件——
	// 上面那对 boot-volume-attachments 只能让卷当引导卷，改不了里面的东西。
	s.mux.HandleFunc("POST /api/storage/volume-attachments/attach", s.requireAuth(s.danger(s.handleAttachVolume)))
	s.mux.HandleFunc("POST /api/storage/volume-attachments/detach", s.requireAuth(s.danger(s.handleDetachVolume)))

	// 创建实例。
	s.mux.HandleFunc("GET /api/launch/shapes", s.requireAuth(s.handleListShapes))
	s.mux.HandleFunc("GET /api/launch/images", s.requireAuth(s.handleListImages))
	s.mux.HandleFunc("GET /api/launch/availability-domains", s.requireAuth(s.handleListADs))
	s.mux.HandleFunc("GET /api/launch/presets", s.requireAuth(s.handleLaunchPresets))
	s.mux.HandleFunc("POST /api/launch", s.requireAuth(s.handleLaunchInstance))

	// 通用运维任务。查询是只读的；状态切换（暂停/恢复/取消/重试）只改本地
	// 记录、不碰云端资源，所以不走 danger。删除只允许终态任务。
	s.mux.HandleFunc("GET /api/tasks", s.requireAuth(s.handleListOpsTasks))
	s.mux.HandleFunc("GET /api/tasks/{id}", s.requireAuth(s.handleGetOpsTask))
	s.mux.HandleFunc("POST /api/tasks/{id}/{action}", s.requireAuth(s.handleOpsTaskAction))
	s.mux.HandleFunc("DELETE /api/tasks/{id}", s.requireAuth(s.danger(s.handleDeleteOpsTask)))

	// 运维作业入口。放行端口会改安全列表（整体替换语义），属于会扩大攻击面
	// 的写操作，走 danger 复核；创建本身只落库，真正执行在后台调度器里。
	s.mux.HandleFunc("POST /api/ops/open-ports", s.requireAuth(s.danger(s.handleCreateOpenPortsTask)))
	// 换 IP 会删掉旧公网 IP 并立刻断开 SSH，不可逆，走 danger 复核。
	s.mux.HandleFunc("POST /api/ops/change-ip", s.requireAuth(s.danger(s.handleCreateChangeIPTask)))
	// 救援编排会关机、分离引导卷——这是全项目最危险的操作（分离后实例
	// 起不来、引导卷暂时不属于任何运行中的机器），必须走 danger 复核。
	s.mux.HandleFunc("POST /api/ops/rescue", s.requireAuth(s.danger(s.handleCreateRescueTask)))
	// 500Mbps 开启会建 NAT 与 NLB，NLB 不在 Always Free 范围内、开着就一直在
	// 计费；关闭会删资源并改路由。两个方向都改网络拓扑，走 danger 复核。
	s.mux.HandleFunc("POST /api/ops/bandwidth", s.requireAuth(s.danger(s.handleCreateBandwidthTask)))

	// 容量守候（抢机）。创建走 danger：它会持续调用 Oracle，
	// 属于需要二次确认的操作，和普通只读接口不是一个量级。
	s.mux.HandleFunc("GET /api/hunt", s.requireAuth(s.handleListHuntTasks))
	s.mux.HandleFunc("POST /api/hunt", s.requireAuth(s.danger(s.handleCreateHuntTask)))
	s.mux.HandleFunc("POST /api/hunt/{id}/{action}", s.requireAuth(s.handleSetHuntState))
	s.mux.HandleFunc("DELETE /api/hunt/{id}", s.requireAuth(s.danger(s.handleDeleteHuntTask)))

	// 容量监控。查询本身是只读的（Oracle 的容量报告接口，不创建任何资源），
	// 所以 probe 不走 danger；增删监控项只改本地记录，影响的是后台轮询频率。
	s.mux.HandleFunc("GET /api/capacity", s.requireAuth(s.handleListCapacityWatches))
	s.mux.HandleFunc("POST /api/capacity/probe", s.requireAuth(s.handleProbeCapacity))
	s.mux.HandleFunc("POST /api/capacity", s.requireAuth(s.handleCreateCapacityWatch))
	s.mux.HandleFunc("POST /api/capacity/{id}/{action}", s.requireAuth(s.handleSetCapacityWatchEnabled))
	s.mux.HandleFunc("DELETE /api/capacity/{id}", s.requireAuth(s.danger(s.handleDeleteCapacityWatch)))

	// 身份域密码策略。读是只读；写会改变整个租户的登录行为，走 danger 复核。
	s.mux.HandleFunc("GET /api/accounts/{id}/password-policy", s.requireAuth(s.handleGetPasswordPolicy))
	s.mux.HandleFunc("PATCH /api/accounts/{id}/password-policy", s.requireAuth(s.danger(s.handleUpdatePasswordPolicy)))

	// 代理池。读操作只走普通鉴权；导入、绑定、改删都会影响所有账号出网，但只改本地配置，
	// 不走 danger 复核。检测是纯出网请求，不打 Oracle 接口。
	s.mux.HandleFunc("GET /api/proxies", s.requireAuth(s.handleListProxies))
	s.mux.HandleFunc("POST /api/proxies/import", s.requireAuth(s.handleImportProxies))
	s.mux.HandleFunc("PATCH /api/proxies/{id}", s.requireAuth(s.handleUpdateProxy))
	s.mux.HandleFunc("DELETE /api/proxies/{id}", s.requireAuth(s.danger(s.handleDeleteProxy)))
	s.mux.HandleFunc("POST /api/proxies/bind", s.requireAuth(s.handleBindProxy))
	s.mux.HandleFunc("POST /api/proxies/check", s.requireAuth(s.handleCheckProxies))
	s.mux.HandleFunc("POST /api/proxies/{id}/check", s.requireAuth(s.handleCheckProxies))

	// Cloudflare 联动。域名与绑定都是本地配置，读只走普通鉴权；写会改变
	// 真实域名的解析结果（删域名还会连带删掉它的绑定），改删走 danger 复核。
	//
	// 删除域名走 danger：它同时删掉这个域名下全部记录绑定，而"域名解析
	// 指着哪台机器"这件事，用户往往要过几天才发现没了。
	s.mux.HandleFunc("GET /api/cf/zones", s.requireAuth(s.handleListCFZones))
	s.mux.HandleFunc("POST /api/cf/zones", s.requireAuth(s.handleCreateCFZone))
	s.mux.HandleFunc("PATCH /api/cf/zones/{id}", s.requireAuth(s.handleUpdateCFZone))
	s.mux.HandleFunc("DELETE /api/cf/zones/{id}", s.requireAuth(s.danger(s.handleDeleteCFZone)))
	s.mux.HandleFunc("POST /api/cf/zones/{id}/verify", s.requireAuth(s.handleVerifyCFZone))
	s.mux.HandleFunc("GET /api/cf/zones/{id}/records", s.requireAuth(s.handleListCFRemoteRecords))
	s.mux.HandleFunc("POST /api/cf/records", s.requireAuth(s.handleCreateCFRecord))
	s.mux.HandleFunc("DELETE /api/cf/records/{id}", s.requireAuth(s.handleDeleteCFRecord))
	s.mux.HandleFunc("POST /api/cf/records/{id}/sync", s.requireAuth(s.handleSyncCFRecord))

	// IP 情报与分布。纯只读；手填归属地只写本地库。
	// 这条路径只在本地查离线库，不出网，所以没有超时与限流的额外处理。
	s.mux.HandleFunc("GET /api/geo/status", s.requireAuth(s.handleGetGeoStatus))
	s.mux.HandleFunc("GET /api/geo/distribution", s.requireAuth(s.handleGetGeoDistribution))
	s.mux.HandleFunc("PUT /api/geo/manual", s.requireAuth(s.handleSetGeoManual))
	s.mux.HandleFunc("DELETE /api/geo/manual/{ip}", s.requireAuth(s.handleDeleteGeoManual))

	// 账单。纯只读查询，不走 danger——它不改变任何东西。
	s.mux.HandleFunc("GET /api/billing", s.requireAuth(s.handleBilling))
	s.mux.HandleFunc("GET /api/billing/{id}", s.requireAuth(s.handleBillingDetail))
	s.mux.HandleFunc("GET /api/billing/{id}/egress", s.requireAuth(s.handleBillingEgress))

	// 配额与监控。
	s.mux.HandleFunc("GET /api/quota", s.requireAuth(s.handleQuota))
	s.mux.HandleFunc("GET /api/instances/{id}/metrics", s.requireAuth(s.handleInstanceMetrics))
	s.mux.HandleFunc("GET /api/instances/{id}/traffic", s.requireAuth(s.handleInstanceTraffic))

	// 通知与设置。
	s.mux.HandleFunc("GET /api/notifications/channels", s.requireAuth(s.handleListChannels))
	s.mux.HandleFunc("POST /api/notifications/channels", s.requireAuth(s.handleCreateChannel))
	s.mux.HandleFunc("PATCH /api/notifications/channels/{id}", s.requireAuth(s.handleUpdateChannel))
	s.mux.HandleFunc("DELETE /api/notifications/channels/{id}", s.requireAuth(s.danger(s.handleDeleteChannel)))
	s.mux.HandleFunc("POST /api/notifications/channels/{id}/test", s.requireAuth(s.handleTestChannel))
	s.mux.HandleFunc("GET /api/notifications/events", s.requireAuth(s.handleNotificationEvents))
	s.mux.HandleFunc("GET /api/settings", s.requireAuth(s.handleGetSettings))
	s.mux.HandleFunc("PATCH /api/settings", s.requireAuth(s.handleUpdateSettings))

	// 总览页数据。
	s.mux.HandleFunc("GET /api/overview", s.requireAuth(s.handleOverview))
}

// Handler 返回带全局中间件的处理器。
//
// 顺序：安全响应头 → CSRF → 路由。CSRF 放在路由之前、且由中间件统一执行，
// 而不是给每条路由包一层——见 csrfGuard 的说明。
func (s *Server) Handler() http.Handler {
	return securityHeaders(csrfGuard(s.mux))
}

// ServeHTTP 让 Server 本身可以作为 http.Handler 使用。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Handler().ServeHTTP(w, r)
}

// securityHeaders 设置基础安全响应头。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		// 面板是纯本地资源的 SPA，不需要加载任何外部资源。
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
				"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// csrfGuard 是 CSRF 防护中间件：所有写请求必须带上自定义请求头。
//
// 浏览器不允许跨源请求携带自定义头（除非目标站点显式开启 CORS 预检，
// 而本服务从不开启），因此"存在该头"就等价于"请求来自本站脚本"。
// 这比双提交 Cookie 更简单，且没有令牌同步问题。
//
// 做成中间件而不是逐条路由包一层，是因为后者漏一个就是一处缺口：
// 本项目曾漏掉 5 个 POST 接口（容量探测、代理检测、CF 校验/同步），
// 全靠 SameSite=Strict 兜底才没有真的可利用。放在这里之后，新增路由
// 默认就是受保护的，忘记加防护这个错误在结构上不再可能发生。
func csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// 读请求不改变状态；OPTIONS 留给未来的 CORS 预检（当前不开启）。
		default:
			if r.Header.Get(csrfHeader) != csrfValue {
				writeError(w, http.StatusForbidden, "csrf",
					"缺少 "+csrfHeader+" 请求头。跨站请求已被拒绝。")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---- 会话上下文 ----

type ctxKey int

const (
	ctxKeySession ctxKey = iota
	ctxKeyUser
	ctxKeyToken
)

func sessionFrom(ctx context.Context) *store.Session {
	v, _ := ctx.Value(ctxKeySession).(*store.Session)
	return v
}

func userFrom(ctx context.Context) *store.User {
	v, _ := ctx.Value(ctxKeyUser).(*store.User)
	return v
}

func tokenFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyToken).(string)
	return v
}

// loadSession 从 Cookie 还原会话与用户。
func (s *Server) loadSession(r *http.Request) (*store.Session, *store.User, string, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return nil, nil, "", false
	}
	sess, err := s.st.GetSession(r.Context(), cookie.Value)
	if err != nil {
		return nil, nil, "", false
	}
	user, err := s.st.GetUser(r.Context(), sess.UserID)
	if err != nil {
		return nil, nil, "", false
	}
	return sess, user, cookie.Value, true
}

// requireSession 只要求会话存在，不要求已完成第二因子。
// 仅用于 TOTP 校验与登出这两个必须在"半登录"状态下可用的接口。
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, user, token, ok := s.loadSession(r)
		if !ok {
			s.clearSessionCookie(w, r)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "请先登录")
			return
		}
		next(w, r.WithContext(withSession(r.Context(), sess, user, token)))
	}
}

// requireAuth 要求完整会话：已通过口令，且在启用了 TOTP 时也已通过第二因子。
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, user, token, ok := s.loadSession(r)
		if !ok {
			s.clearSessionCookie(w, r)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "请先登录")
			return
		}
		if user.TOTPEnabled && !sess.TOTPVerified {
			writeError(w, http.StatusUnauthorized, "totp_required", "请完成两步验证")
			return
		}

		// 滑动续期：活跃用户不会在操作中途被踢下线。
		if err := s.st.TouchSession(r.Context(), token, s.cfg.SessionTTL); err != nil {
			// 续期失败不影响本次请求，交给下一次重试。
			_ = err
		}
		next(w, r.WithContext(withSession(r.Context(), sess, user, token)))
	}
}

func withSession(ctx context.Context, sess *store.Session, user *store.User, token string) context.Context {
	ctx = context.WithValue(ctx, ctxKeySession, sess)
	ctx = context.WithValue(ctx, ctxKeyUser, user)
	return context.WithValue(ctx, ctxKeyToken, token)
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.isSecureRequest(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(s.cfg.SessionTTL.Seconds()),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.isSecureRequest(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// ---- 登录失败限流 ----

// attemptLimiter 是按来源 IP 的固定窗口计数器。
//
// 单机部署、单用户场景下，固定窗口足够挡住暴力破解，
// 不值得为此引入更复杂的滑动窗口或外部存储。
type attemptLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	buckets map[string]*bucket
}

type bucket struct {
	count     int
	resetAt   time.Time
	lastTouch time.Time
}

func newAttemptLimiter(max int, window time.Duration) *attemptLimiter {
	return &attemptLimiter{max: max, window: window, buckets: make(map[string]*bucket)}
}

// Reserve 在请求进入时预占一次尝试额度，返回是否放行与预占后的剩余次数。
//
// 额度必须在校验之前扣：以前是先 Allow 只看计数、校验失败后才 Fail 加一，
// 进行中的请求不占额度，首批 Argon2 算完之前同一 IP 能放进任意多个请求。
// 现在每次进入都算一次尝试，成功后由 Reset 清零。
func (l *attemptLimiter) Reserve(key string) (allowed bool, remaining int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	l.pruneLocked(now)

	b, ok := l.buckets[key]
	if !ok || now.After(b.resetAt) {
		b = &bucket{resetAt: now.Add(l.window)}
		l.buckets[key] = b
	}
	b.lastTouch = now

	if b.count >= l.max {
		return false, 0
	}
	b.count++
	return true, l.max - b.count
}

// Reset 在登录成功后清除计数。
func (l *attemptLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

// pruneLocked 清理长期不活跃的条目，防止 map 无限增长。
func (l *attemptLimiter) pruneLocked(now time.Time) {
	if len(l.buckets) < 128 {
		return
	}
	for key, b := range l.buckets {
		if now.Sub(b.lastTouch) > 2*l.window {
			delete(l.buckets, key)
		}
	}
}
