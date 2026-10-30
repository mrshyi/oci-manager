/**
 * 后端返回的原始数据结构（DTO）。字段名与 Go 侧的 json tag 一一对应。
 *
 * 刻意与 @/types 里的视图模型分开：视图模型是为了渲染方便而设计的
 * （预格式化的 uptime、扁平的 quota），DTO 是后端的契约。
 * 两者之间的转换集中在 @/lib/adapt。
 */

export type LifecycleState =
  | 'RUNNING' | 'STOPPED' | 'PROVISIONING' | 'STARTING' | 'STOPPING' | 'TERMINATING' | 'TERMINATED'

export type AccountStatusDTO = 'unchecked' | 'ok' | 'error'

export interface StatusDTO {
  setupRequired: boolean
  authenticated: boolean
  totpRequired: boolean
  totpEnabled: boolean
  username?: string
  version: string
}

export interface MeDTO {
  username: string
  totpEnabled: boolean
  createdAt: string
}

export interface LoginResultDTO {
  totpRequired: boolean
  totpEnabled: boolean
}

export interface TotpSetupDTO {
  secret: string
  uri: string
}

/** 账号的附加信息：分组、备注、到期时间、排序权重 */
export interface AccountMetaDTO {
  accountId: string
  /** 空串表示未分组，前端归到最后一组 */
  groupName: string
  note: string
  /** 到期时间（ISO）。null 表示不设提醒 */
  expiresAt: string | null
  /** 大的排前面。全为 0 时退回创建时间顺序 */
  sortWeight: number
  /** 抢机页的「已知悉」标记，当前只做存取 */
  huntAckAt: string | null
  updatedAt: string
}

/**
 * 账号健康度：最近一次校验的结论。
 *
 * level 由后端算好，前端不自己判颜色——「连续几次算坏」「哪种错误算红」
 * 这类规则只该有一份。
 */
export interface AccountHealthDTO {
  accountId: string
  /** 最近一次成功。null 表示从来没成功过 */
  lastOkAt: string | null
  /** 最近一次失败的分类名（AuthFailed / Throttled / QuotaExceeded …） */
  lastErrorClass: string
  /** 最后一次失败的时刻。成功不清空，它是历史事实 */
  lastErrorAt: string | null
  lastErrorMessage: string
  /** 连续凭据类失败次数。非凭据类结果会把它归零 */
  consecutiveFailures: number
  /** 已确认凭据不可用（连续失败达到阈值） */
  fatal: boolean
  updatedAt: string
  /** unknown / green / yellow / orange / red */
  level: HealthLevelDTO
  label: string
  /** 面向用户的一句话说明。红块写「凭据或权限异常，请到控制台确认账号状态」 */
  advice: string
}

export type HealthLevelDTO = 'unknown' | 'green' | 'yellow' | 'orange' | 'red'

/** 一族能力的探测结论 */
export interface CapabilityDTO {
  /** compute / network / storage / usage / monitoring */
  key: string
  label: string
  /** 接口调通了、拿到了数据 */
  ok: boolean
  /** 不可用的原因是缺权限。与「接口报错」必须分开：前者去加策略，后者重试可能就好 */
  denied: boolean
  class?: string
  detail?: string
  /** 缺权限时该补的 IAM 策略 */
  policy?: string
  /** 探测到的资源数量，只有 compute / network / storage 有意义 */
  count: number
  durationMs: number
}

/** 一次权限自检的结果 */
export interface CapabilityProbeDTO {
  accountId: string
  region: string
  probedAt: string
  items: CapabilityDTO[]
  /** 一句话结论，列表页直接显示 */
  summary: string
}

/** 五族的静态描述。由后端下发，前端不自己维护清单 */
export interface CapabilitySpecDTO {
  key: string
  label: string
  policy: string
}

/** GET/POST /api/accounts/{id}/capability-probe 的响应 */
export interface CapabilityProbeResponseDTO {
  /** 没自检过时为 null */
  probe: CapabilityProbeDTO | null
  notice?: string
  /** 只在没自检过时下发 */
  specs?: CapabilitySpecDTO[]
}

/** 一个标签及其被多少个账号使用 */
export interface TagUsageDTO {
  tag: string
  count: number
}

/** GET /api/accounts 的完整响应 */
export interface AccountsResponseDTO {
  accounts: AccountDTO[]
  /** 分组展示顺序，未分组（空串）固定最后。由后端排好，前端不重算 */
  groups: string[]
  /** 筛选器与输入联想用的标签清单，按使用次数降序 */
  tags: TagUsageDTO[]
}

export interface AccountDTO {
  id: string
  alias: string
  code: string
  colorIndex: number
  tenancyOcid: string
  userOcid: string
  fingerprint: string
  defaultRegion: string
  compartmentOcid: string
  enabled: boolean
  status: AccountStatusDTO
  statusMessage: string
  lastCheckedAt: string | null
  /** FREE_TRIAL / PAY_AS_YOU_GO，取不到时为空 */
  paymentModel?: string
  subscriptionState?: string
  /** 试用订阅上就是试用到期日 */
  /** 甲骨文账号开户时刻。注意与 createdAt（接进本面板的时刻）不是一回事 */
  subscriptionStartsAt?: string | null
  subscriptionEndsAt?: string | null
  subscribedRegions: string[] | null
  homeRegion: string
  email: string
  tenancyName: string
  createdAt: string
  updatedAt: string
  /** 自由文本标签，按字母序。没有标签时是空数组 */
  tags: string[]
  meta: AccountMetaDTO
  /** 健康度色块，由后端算好 */
  health: AccountHealthDTO
  /** 上次权限自检的结果。null 表示还没自检过 */
  capability: CapabilityProbeDTO | null
  /** 距离到期的天数：负数已过期，0 表示今天，null 表示不设到期 */
  expiryDays: number | null
}

export interface InstanceDTO {
  id: string
  accountId: string
  region: string
  compartmentId: string
  displayName: string
  availabilityDomain: string
  faultDomain: string
  shape: string
  ocpus: number
  memoryGb: number
  /** 突发基线；空串或 BASELINE_1_1 表示不突发 */
  baselineOcpuUtilization: string
  lifecycleState: LifecycleState
  imageId: string
  /** 镜像的操作系统名，如 "Canonical Ubuntu"；取不到为空串 */
  imageOs: string
  publicIp: string
  privateIp: string
  /** 主网卡上的全部 IPv6，逗号分隔；未启用为空串 */
  ipv6: string
  /** 附属网卡上的公网 IPv4，逗号分隔 */
  secondaryIpv4?: string
  /** 附属网卡上的全部 IPv6，逗号分隔 */
  secondaryIpv6?: string
  vnicId: string
  subnetId: string
  bootVolumeId: string
  bootVolumeGb: number
  bootVolumeVpus: number
  timeCreated: string
  /** 面板观测到进入 RUNNING 的时刻，null 表示首次同步时它就已经在跑了 */
  runningSince: string | null
  /** 用户手写的备注，不来自 Oracle */
  note: string
  syncedAt: string
  /** 最近一次操作失败的原因，非空时该行浮出错误条 */
  lastError: string
  accountAlias: string
  accountCode: string
  accountColorIndex: number
}

export interface SyncStatusDTO {
  syncing: boolean
  lastSync: string
}

export interface RegionErrorDTO {
  accountId: string
  accountAlias: string
  region: string
  message: string
  ociCode?: string
  advice?: string
}

export interface SyncReportDTO {
  startedAt: string
  durationMs: number
  accounts: number
  regions: number
  instances: number
  /** 已终止实例数。界面各处不显示它们，因此不计入 instances */
  terminated: number
  pruned: number
  errors: RegionErrorDTO[] | null
}

export interface CheckStepDTO {
  key: string
  label: string
  ok: boolean
  detail: string
}

export interface CheckResultDTO {
  ok: boolean
  steps: CheckStepDTO[]
  userName?: string
  userEmail?: string
  tenancyName?: string
  regions?: string[]
  homeRegion?: string
  errorCode?: string
  errorText?: string
  advice?: string
  /** ociclient.Class 的名字，健康度据此上色 */
  errorClass?: string
  accountFatal: boolean
}

export interface ConfigProfileDTO {
  name: string
  userOcid: string
  fingerprint: string
  tenancyOcid: string
  region: string
  keyFile: string
  hasPassPhrase: boolean
  complete: boolean
  missing: string[] | null
  suggestedCode: string
}

export interface QuotaItemDTO {
  /** 稳定的语义标识，UI 按它取值——不要匹配 name，那是会变的 OCI 限额名 */
  key: 'ocpu' | 'memory' | 'block' | 'micro' | string
  name: string
  label: string
  used: number
  limit: number
  /** false 表示这项没查到，UI 必须显示"未知"而不是 0 */
  known: boolean
  /** 没查到时的原因，供排障用 */
  error?: string
  /** true 表示没有实际上限（升级号的 ARM 限额 Oracle 返回一亿，那是哨兵值） */
  unlimited?: boolean
}

export interface AccountQuotaDTO {
  accountId: string
  region: string
  items: QuotaItemDTO[] | null
  error?: string
  fetchedAt: string
}

export interface VnicInfoDTO {
  attachmentId: string
  vnicId: string
  displayName: string
  isPrimary: boolean
  nicIndex: number
  privateIp: string
  publicIp: string
  publicIpId?: string
  publicIpType?: string
  privateIpId?: string
  ipv6: string[] | null
  macAddress: string
  subnetId: string
  subnetName?: string
  vcnId?: string
  vcnName?: string
}

export interface VolumeInfoDTO {
  volumeId: string
  attachmentId: string
  displayName: string
  sizeInGbs: number
  vpusPerGb: number
  device: string
  isReadOnly: boolean
  state: string
}

export interface BootVolumeDTO {
  id: string
  compartmentId: string
  displayName: string
  availabilityDomain: string
  sizeInGBs: number
  vpusPerGB: number
  lifecycleState: string
  imageId: string
  isHydrated: boolean
  timeCreated: string
}

export interface InstanceDetailDTO {
  instance: InstanceDTO
  vnics: VnicInfoDTO[] | null
  bootVolume: BootVolumeDTO | null
  /** 挂载关系 OCID，不是卷 OCID。分离引导卷用的是这个 */
  bootVolumeAttachmentId: string
  blockVolumes: VolumeInfoDTO[] | null
  metadata: Record<string, string> | null
  /** 当前规格与配置允许的网卡数上限；0 表示读不到 */
  maxVnics?: number
  warnings?: string[]
}

export interface AttachVnicRequest {
  subnetId: string
  displayName?: string
  assignPublicIp: boolean
  assignIpv6: boolean
}

export interface AttachVnicResultDTO {
  attachmentId: string
  state: string
  warnings?: string[]
}

export interface MetricSeriesDTO {
  metric: string
  aggregation: string
  datapoints: { timestamp: string; value: number }[] | null
  error?: string
}

export interface MetricsDTO {
  instanceId: string
  start: string
  end: string
  resolution: string
  series: MetricSeriesDTO[]
  notice: string
}

/** 实例本月（UTC 自然月）的已用流量，取自 VCN 网卡指标 */
export interface InstanceTrafficDTO {
  instanceId: string
  /** YYYY-MM */
  month: string
  start: string
  end: string
  /** 参与统计的网卡数 */
  vnics: number
  outBytes: number
  inBytes: number
  last24hOutBytes: number
  last24hInBytes: number
  series: { date: string; outBytes: number; inBytes: number }[]
  /** 部分网卡查询失败时的错误；数字仍是其余网卡的 */
  error?: string
  notice: string
}

export interface AttentionItemDTO {
  kind: string
  accountId?: string
  target: string
  message: string
  severity: 'warning' | 'danger'
}

export interface OverviewDTO {
  accounts: { total: number; ok: number; error: number; disabled: number }
  instances: { total: number; byState: Record<string, number> }
  distribution: { accountId: string; region: string; count: number }[] | null
  regions: string[] | null
  sync: SyncStatusDTO
  attention: AttentionItemDTO[] | null
}

export interface SettingsDTO {
  allowTerminate: boolean
  allowBulkActions: boolean
  requireTotpForDanger: boolean
  syncIntervalMinutes: number
  /** 自动重跑凭据校验的间隔，0 为关闭 */
  checkIntervalHours: number
  /** 审计日志保留天数，0 为永久保留 */
  auditRetentionDays: number
}

export interface ChannelDTO {
  id: string
  kind: string
  name: string
  /** 机密字段已被后端打码，形如 1234••••••••WXYZ */
  config: Record<string, string>
  events: string[]
  enabled: boolean
  lastError: string
  lastSentAt: string | null
  createdAt: string
  updatedAt: string
}

export interface ChannelFieldDTO {
  key: string
  label: string
  required: boolean
  secret: boolean
  hint?: string
  /** 非空时渲染为下拉选择 */
  options?: { value: string; label: string }[]
}

export interface ChannelKindDTO {
  kind: string
  label: string
  fields: ChannelFieldDTO[]
}

export interface NotifyEventDTO {
  key: string
  label: string
  description: string
}

export interface AuditEntryDTO {
  id: number
  userId: string
  action: string
  accountId: string
  target: string
  detail: string
  ip: string
  result: string
  createdAt: string
}

export interface ShapeDTO {
  shape: string
  processorDescription: string
  ocpus: number
  memoryInGBs: number
  isFlexible: boolean
  /** 支持的突发基线（BASELINE_1_8 等），为空表示不支持突发 */
  baselineOcpuUtilizations: string[] | null
  ocpuOptions: { min: number; max: number } | null
  memoryOptions: {
    minInGBs: number
    maxInGBs: number
    minPerOcpuInGBs: number
    maxPerOcpuInGBs: number
  } | null
}

export interface ImageDTO {
  id: string
  displayName: string
  operatingSystem: string
  operatingSystemVersion: string
  timeCreated: string
}

export interface AvailabilityDomainDTO {
  id: string
  name: string
  compartmentId: string
}

export interface LaunchPresetDTO {
  key: string
  label: string
  shape: string
  ocpus: number
  memoryInGbs: number
  bootGb: number
  description: string
  freeTier: boolean
}

/** 后台任务状态。interrupted 是服务重启时还没跑完的任务，实际结果需要去对应页面确认 */
export type JobStatus = 'queued' | 'running' | 'succeeded' | 'failed' | 'canceled' | 'interrupted'

export interface JobStepDTO {
  at: string
  text: string
}

export interface JobDTO {
  id: string
  /** instance.launch / instance.change_ip / instance.reshape / … 见后端 httpapi/jobs.go */
  kind: string
  title: string
  accountId?: string
  instanceId?: string
  status: JobStatus
  /** 0–100，只增不减；100 只在成功时出现 */
  progress: number
  steps: JobStepDTO[]
  summary?: string
  error?: string
  /** 结构化结果，形状随 kind 而定（如换 IP 是 ChangeIpDTO） */
  result?: unknown
  createdAt: string
  startedAt?: string
  finishedAt?: string
}

/** 长耗时操作受理后的响应（HTTP 202） */
export interface JobAcceptedDTO {
  job: JobDTO
}

export interface VcnDTO {
  id: string
  displayName: string
  cidrBlock: string
  ipv6CidrBlocks: string[] | null
  lifecycleState: string
  defaultSecurityListId: string
  defaultRouteTableId: string
}

export interface SubnetDTO {
  id: string
  vcnId: string
  displayName: string
  cidrBlock: string
  ipv6CidrBlock: string
  availabilityDomain: string
  prohibitPublicIpOnVnic: boolean
  lifecycleState: string
}

export interface PortRange { min: number; max: number }

export interface IngressRuleDTO {
  protocol: string
  source: string
  sourceType?: string
  isStateless?: boolean
  tcpOptions?: { destinationPortRange?: PortRange; sourcePortRange?: PortRange }
  udpOptions?: { destinationPortRange?: PortRange; sourcePortRange?: PortRange }
  icmpOptions?: { type: number; code?: number }
  description?: string
}

export interface EgressRuleDTO {
  protocol: string
  destination: string
  destinationType?: string
  isStateless?: boolean
  tcpOptions?: { destinationPortRange?: PortRange; sourcePortRange?: PortRange }
  udpOptions?: { destinationPortRange?: PortRange; sourcePortRange?: PortRange }
  icmpOptions?: { type: number; code?: number }
  description?: string
}

export interface SecurityListDTO {
  id: string
  vcnId: string
  displayName: string
  ingressSecurityRules: IngressRuleDTO[] | null
  egressSecurityRules: EgressRuleDTO[] | null
  lifecycleState: string
  /** 后端标出的"等同全放行"规则下标，UI 需显著警示 */
  allowAllRules: number[] | null
  /** 一条出站规则都没有——该子网无法访问外网，UI 需主动提示 */
  noEgress?: boolean
}

export type RuleDirection = 'ingress' | 'egress'

export interface RuleTemplateDTO {
  key: string
  label: string
  /** 模板作用的方向。后端固定下发，前端据此决定追加到哪个数组 */
  direction: RuleDirection
  protocol: string
  port: number
  description: string
  dangerous: boolean
}

export interface PublicIpDTO {
  id: string
  displayName: string
  ipAddress: string
  lifetime: string
  scope: string
  privateIpId: string
  lifecycleState: string
}

export interface EnsureNetworkDTO {
  vcnId: string
  vcnName: string
  subnetId: string
  subnetName: string
  created: boolean
  steps: string[] | null
}

export interface ChangeIpDTO {
  oldIp: string
  newIp: string
}

export interface EnableIpv6DTO {
  address: string
  steps: string[] | null
}

export interface ConsoleConnectionDTO {
  id: string
  instanceId: string
  lifecycleState: string
  serialConsoleCommand: string
  vncConsoleCommand: string
  notice: string
  /** 两层 ssh 都要指定私钥，Oracle 给的命令串里没有 -i */
  keyHint?: string
}

/* ---------- 容量守候（抢机） ---------- */

export interface HuntTaskDTO {
  id: string
  accountId: string
  region: string
  name: string
  /** running / paused / succeeded / failed */
  state: string
  attempts: number
  intervalSeconds: number
  /** 每轮先查容量报告，说没货就跳过、不发创建请求 */
  precheckCapacity: boolean
  lastClass: string
  lastError: string
  lastAd: string
  lastTryAt: string
  nextAt: string
  instanceId: string
  maxAttempts: number
  expiresAt: string
  createdAt: string
  updatedAt: string

  shape: string
  ocpus: number
  memoryGb: number
  displayName: string
  ads: string[] | null
}

export interface HuntLimitsDTO {
  /** 允许配置的最小间隔，硬下限 */
  minIntervalSeconds: number
  defaultIntervalSeconds: number
  /** 低于它前端必须给出限流风险警告 */
  warnIntervalSeconds: number
}

export interface CreateHuntInput {
  accountId: string
  region?: string
  name?: string
  availabilityDomains?: string[]
  intervalSeconds: number
  precheckCapacity?: boolean
  maxAttempts?: number
  expiresInHours?: number

  displayName: string
  shape: string
  ocpus?: number
  memoryInGbs?: number
  baselineOcpuUtilization?: string
  imageId: string
  bootVolumeGb?: number
  subnetId?: string
  autoCreateNetwork?: boolean
  assignPublicIp?: boolean
  enableIpv6?: boolean
  sshPublicKey?: string
  cloudInit?: string
}

/* ---------- 容量监控 ---------- */

export interface CapacityWatchDTO {
  id: string
  accountId: string
  region: string
  availabilityDomain: string
  availabilityDomainShort: string
  shape: string
  ocpus: number
  memoryGb: number
  enabled: boolean
  /** AVAILABLE / OUT_OF_HOST_CAPACITY / HARDWARE_NOT_SUPPORTED */
  lastStatus: string
  statusText: string
  lastCount: number
  lastError: string
  lastCheckedAt: string
  lastChangedAt: string
  createdAt: string
  updatedAt: string
}

export interface CapacityProbeRow {
  availabilityDomain: string
  short: string
  status: string
  statusText: string
  availableCount: number
  error?: string
}

export interface CapacityProbeResult {
  region: string
  shape: string
  results: CapacityProbeRow[]
  notice: string
}

export interface CapacityProbeInput {
  accountId: string
  region?: string
  /** 留空表示查该区域全部可用域 */
  availabilityDomain?: string
  shape: string
  ocpus?: number
  memoryGb?: number
}

/* ---------- 账单 ---------- */

/**
 * 账单状态。
 *
 * free 与 no_permission 都不是错误，但含义完全相反：前者是"确实没花钱"，
 * 后者是"不知道花没花钱"。合成一个状态会让用户误以为账号免费。
 */
export type BillingStatusDTO = 'ok' | 'free' | 'no_permission' | 'disabled' | 'error'

export interface BillingSummaryDTO {
  accountId: string
  status: BillingStatusDTO
  /** Oracle 返回的币种。免费号常常为空 */
  currency: string
  thisMonth: number
  lastMonth: number
  region: string
  error?: string
  fetchedAt: string
}

/** 按币种分开的跨账号合计。不同币种绝不相加 */
export interface BillingCurrencyTotalDTO {
  currency: string
  thisMonth: number
  lastMonth: number
  accounts: number
}

export interface BillingListDTO {
  summaries: BillingSummaryDTO[]
  totals: BillingCurrencyTotalDTO[]
  countedAccounts: number
  noPermissionCount: number
  notice: string
}

/** 按维度（服务 / 区域）分组的一项 */
export interface BillingBucketDTO {
  key: string
  amount: number
  /** 用量视角的数字，免费号只有这一半有内容 */
  quantity: number
  unit: string
}

export interface BillingDayDTO {
  /** UTC 日期，YYYY-MM-DD */
  date: string
  amount: number
}

export interface BillingDetailDTO {
  accountId: string
  status: BillingStatusDTO
  currency: string
  region: string
  start: string
  end: string
  days: number
  total: number
  series: BillingDayDTO[] | null
  services: BillingBucketDTO[] | null
  regions: BillingBucketDTO[] | null
  usage: BillingBucketDTO[] | null
  /** 按计费项分组的用量与金额 */
  skus: BillingBucketDTO[] | null
  /** 出站流量合计（Outbound Data Transfer 各 Zone 相加）；没有这类计费项时缺省 */
  egress?: BillingBucketDTO
  error?: string
  fetchedAt: string
}

/** 账号本月（UTC 自然月）的计费出站流量，对比每月 10 TB 免费额度 */
export interface BillingEgressDTO {
  accountId: string
  status: BillingStatusDTO
  region: string
  /** YYYY-MM */
  month: string
  usedGb: number
  freeGb: number
  /** Oracle 原始返回的单位 */
  unit: string
  series: { date: string; gb: number }[]
  amount: number
  currency: string
  error?: string
  fetchedAt: string
}

/* ---------- 代理池 ---------- */

/** ok=通 / fail=不通 / unknown=还没检测过 */
export type ProxyStatusDTO = 'ok' | 'fail' | 'unknown'

export interface ProxyDTO {
  id: string
  label: string
  scheme: string
  host: string
  port: number
  username: string
  /** 只说明有没有配密码。密码本身永不回传 */
  hasPassword: boolean
  enabled: boolean
  lastStatus: ProxyStatusDTO
  lastLatencyMs: number
  lastError: string
  /** 上次检测打的区域。同一条代理连不同区域延迟差很多 */
  lastRegion: string
  lastCheckedAt: string
  /** 最近一次成功。用来区分「刚挂」和「从没通过」 */
  lastOkAt: string
  createdAt: string
  updatedAt: string
}

export interface ProxyListDTO {
  proxies: ProxyDTO[] | null
  /** accountId -> proxyId，只含已绑定的账号 */
  bindings: Record<string, string> | null
  checkTimeoutMs: number
  notice: string
}

/** 批量导入里的一行 */
export interface ProxyImportRowDTO {
  line: number
  masked: string
  label?: string
  error?: string
  /** 已存在，跳过，不算失败 */
  skipped?: boolean
}

export interface ProxyImportResultDTO {
  dryRun: boolean
  rows: ProxyImportRowDTO[] | null
  ok: number
  failed: number
  skipped: number
}

export interface ProxyCheckRowDTO {
  id: string
  status: ProxyStatusDTO
  latencyMs: number
  error?: string
  checkedAt: string
  region: string
}

/* ---------- 身份域密码策略 ---------- */

export interface PasswordPolicyDTO {
  accountId: string
  /** false 表示该租户没有身份域，压根没有「密码有效期」这回事 */
  supported: boolean
  domainName?: string
  domainUrl?: string
  policyId?: string
  policyName?: string
  /** null 表示该属性不存在。按 Oracle 语义应当就是不过期，但官方没有明文 */
  expiresAfterDays: number | null
  warnBeforeDays?: number | null
  minLength?: number | null
  error?: string
}

export interface PasswordPolicyResultDTO {
  policy: PasswordPolicyDTO
  notice: string
}

/* ---------- 计费风险 ---------- */

/** 一条计费风险。severity 只有两级：在扣钱 / 需要看一眼 */
export interface BillingRiskItemDTO {
  key: string
  label: string
  severity: 'billing' | 'attention'
  detail: string
  /** 估算的每日费用（人民币）。0 表示估不出或确实免费 */
  estDailyCny: number
  /** 估算依据，必须原样显示——「估算」不能只写在标题里 */
  basis: string
  /** 额度数字是否来自账号自己的 limits 接口。false 时不能说「你超额了」 */
  confirmed: boolean
  notice?: string
}

export interface BillingRiskDTO {
  accountId: string
  region: string
  generatedAt: string
  items: BillingRiskItemDTO[]
  totalDailyCny: number
  totalMonthlyCny: number
  notice: string
}

/* ---------- 备份与恢复 ---------- */

export interface BackupManifestDTO {
  createdAt: string
  appVersion: string
  backupVersion: number
  dbBytes: number
  dbSha256: string
  /** 包里是否带了主密钥。默认不带——它该和数据库分开保管 */
  includesMasterKey: boolean
  notice: string
}

export interface BackupStatusDTO {
  /** 是否有等待生效的恢复（需要重启） */
  pending: boolean
  /** 当前程序支持的备份格式版本 */
  backupVersion: number
  manifest?: BackupManifestDTO
  stagedAt?: string
  masterKeyPending?: boolean
}

export interface RestoreResultDTO {
  manifest: BackupManifestDTO
  /** 恒为 true：库文件被进程打开着，恢复只能在重启时生效 */
  requiresRestart: boolean
  /** 备份里的主密钥与当前不同 */
  masterKeyDiffers: boolean
  notice: string
}

/* ---------- 云侧策略模板 ---------- */

export interface PolicyLineDTO {
  /** 可直接粘贴的完整语句，占位符已替换 */
  text: string
  /** 这条为什么需要 */
  purpose: string
  /** 覆盖的能力探测族 */
  capabilities: string[]
}

export interface PolicyTemplateDTO {
  group: string
  /** 8 条拼好的全文 */
  text: string
  lines: PolicyLineDTO[]
  steps: string[]
  scope: string
  notice: string
  version: string
  readOnlyCount: number
}

/* ---------- Cloudflare 联动 ---------- */

export interface CFZoneDTO {
  id: string
  /** 可选归属账号，只用于筛选 */
  accountId: string
  zoneId: string
  zoneName: string
  /** 只说明有没有配 token，不含内容 */
  hasToken: boolean
  enabled: boolean
  /** 最后一次**成功**同步的时刻；失败不推进 */
  lastSyncAt: string
  lastError: string
  createdAt: string
  updatedAt: string
}

export interface CFRecordDTO {
  id: string
  zonePk: string
  recordId: string
  name: string
  type: string
  proxied: boolean
  ttl: number
  accountId: string
  instanceId: string
  lastIp: string
  lastSyncAt: string
  lastError: string
  createdAt: string
  updatedAt: string
  /** 列表查询顺带带出，方便直接显示 */
  zoneName: string
}

export interface CFListDTO {
  zones: CFZoneDTO[]
  records: CFRecordDTO[]
  /** 面板上常驻的说明：只收 API Token、需要什么权限 */
  notice: string
}

export interface CFRemoteRecordDTO {
  id: string
  type: string
  name: string
  content: string
  ttl: number
  proxied: boolean
  /** 这条是否已经绑定了实例 */
  bound: boolean
}

export interface CFSyncResultDTO {
  /** updated / partial / failed；空表示没有绑定 */
  status: string
  detail: string
  records: number
  ip: string
}

/* ---------- IP 情报与分布 ---------- */

export interface GeoStatusDTO {
  enabled: boolean
  /** 离线库目录。未启用时界面要把它显示出来，用户才知道该往哪放文件 */
  dir: string
  cityBuiltAt: string
  hasCity: boolean
  hasAsn: boolean
  /** 为什么不可用、该做什么 */
  notice: string
}

export interface GeoPointDTO {
  instanceId: string
  instanceName: string
  accountId: string
  accountCode: string
  /** OCI 区域名，离线库查不到时用它估算位置 */
  region: string
  ip: string
  found: boolean
  country: string
  countryCode: string
  city: string
  latitude: number
  longitude: number
  asn: string
  org: string
  /** mmdb（离线库）/ manual（手填）/ region（按区域估算）/ 空（查不到） */
  source: string
}

export interface GeoDistributionDTO {
  status: GeoStatusDTO
  points: GeoPointDTO[]
  notice: string
}

/* ---------- 资源页 ---------- */

export interface ResourceRefDTO {
  instanceId: string
  /** 实例缓存里查不到时为空（例如别的工具刚建的） */
  name: string
}

export interface ResourceVolumeDTO {
  id: string
  kind: 'boot' | 'block'
  displayName: string
  region: string
  availabilityDomain: string
  sizeInGbs: number
  vpusPerGb: number
  lifecycleState: string
  timeCreated: string
  /** 空数组表示未挂载 */
  attachedTo: ResourceRefDTO[]
}

export interface ResourcePublicIpDTO {
  id: string
  ipAddress: string
  region: string
  lifetime: 'EPHEMERAL' | 'RESERVED' | string
  lifecycleState: string
  displayName: string
  assigned: boolean
  instance?: ResourceRefDTO
}

export interface ResourceConsoleDTO {
  id: string
  region: string
  lifecycleState: string
  fingerprint: string
  instance: ResourceRefDTO
}

export interface ResourceVcnDTO {
  id: string
  displayName: string
  region: string
  cidrBlocks: string[]
  ipv6CidrBlocks: string[]
  lifecycleState: string
  subnets: number
  /** null 表示 VCN 里没有互联网网关 */
  internetGateway: { id: string; name: string; isEnabled: boolean } | null
}

export interface ResourceInstanceDTO {
  id: string
  name: string
  region: string
  shape: string
  lifecycleState: string
  ocpus: number
  memoryGb: number
  bootVolumeGb: number
}

export interface AccountResourcesDTO {
  accountId: string
  regions: string[]
  instances: ResourceInstanceDTO[]
  volumes: ResourceVolumeDTO[]
  publicIps: ResourcePublicIpDTO[]
  consoleConnections: ResourceConsoleDTO[]
  vcns: ResourceVcnDTO[]
  /** 按所选区域的资源清单算出的用量 */
  usage: { armOcpus: number; armMemoryGb: number; microInstances: number; storageGb: number }
  /** 永久免费参考额度与核对日期 */
  freeTier: {
    /** free 免费号 / paid 升级号（ARM 4/24）/ unknown 未识别（按免费号对照） */
    tier: 'free' | 'paid' | 'unknown'
    armOcpus: number; armMemoryGb: number; microInstances: number; storageGb: number; checkedAt: string
  }
  /** Oracle 给这个账号的真实限额（主区域） */
  quota: AccountQuotaDTO
  errors?: { region: string; part: string; error: string }[]
  fetchedAt: string
}

/* ---------- 运维任务（opssvc） ----------
 *
 * 与 JobDTO 是两套东西，刻意不合并：
 *   JobDTO   一次性的长操作，status 只有 queued/running/succeeded/failed/cancelled
 *   OpsTask  可重试、可暂停恢复、可能跑几十轮的任务，多出 paused、attempts、
 *            nextAt（下次重试时刻）、dnsStatus（换 IP 顺带更新域名的结果）
 * 把 OpsTask 硬塞进 JobDTO 会丢掉「第 3/30 轮」「下次重试还有 12 秒」这些
 * 恰恰是用户最需要的信息，也会让 paused 无处安放。
 */

export type OpsTaskState = 'pending' | 'running' | 'paused' | 'succeeded' | 'failed' | 'canceled'

export interface OpsTaskDTO {
  id: string
  /** change_ip_cidr / open_ports / enable_500m / rescue / bulk */
  kind: string
  accountId: string
  instanceId: string
  name: string
  state: OpsTaskState
  /** 分阶段任务（救援）当前停在哪一步 */
  stage: string
  /** 结构化进展，形状随 kind 而定 */
  progress: Record<string, unknown>
  attempts: number
  maxAttempts: number
  intervalSeconds: number
  lastError: string
  result: string
  /** 换 IP 顺带更新域名的结果；空串表示这次任务不涉及 DNS */
  dnsStatus: string
  dnsError: string
  expiresAt: string
  /** 下次被调度的时刻。未命中目标网段的换 IP 任务靠它排队下一轮 */
  nextAt: string
  startedAt: string
  finishedAt: string
  createdAt: string
  updatedAt: string
  accountAlias: string
  kindLabel: string
}

export interface OpsTaskListDTO {
  tasks: OpsTaskDTO[]
  /** 后端给的状态清单，前端不必自己维护一份 */
  states: string[]
}

/**
 * 创建换 IP 任务的请求。
 *
 * 只传 instanceId 就够了：region / vnicId / compartmentId 由后端从实例缓存
 * 补齐。前端实例模型里本来就没有 vnicId 与 compartmentId（它们是 OCI 的内部
 * 概念），硬要前端传会逼着把这两个字段一路透出来。
 */
export interface CreateChangeIPTaskInput {
  instanceId: string
  /** 目标网段，至少一个 */
  targetCidrs: string[]
  maxAttempts?: number
  maxDurationMinutes?: number
  /** 以下三项一般不用传，后端会从实例缓存补齐 */
  accountId?: string
  region?: string
  vnicId?: string
  compartmentId?: string
}

export interface OpsTaskAcceptedDTO {
  task: OpsTaskDTO
  notice: string
}
