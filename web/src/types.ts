import type { MaskKind } from '@/lib/mask'

export type LifecycleState =
  | 'RUNNING' | 'STOPPED' | 'PROVISIONING' | 'STARTING' | 'STOPPING' | 'TERMINATING' | 'TERMINATED'

export type AccountStatus = 'ok' | 'checking' | 'error' | 'disabled'

/**
 * 账号健康度色块。
 *
 * 与 AccountStatus 的区别：status 只有"上次校验成没成"，health 说明
 * "为什么不成、要不要人管"。被限流是黄（等一会儿就好），密钥失效是红
 * （必须去控制台），两者都叫 error 但处理方式完全不同。
 *
 * unknown 是"还没校验过"，不能当成绿——那等于向用户保证一件没人验证过的事。
 */
export type AccountHealth = 'unknown' | 'green' | 'yellow' | 'orange' | 'red'

/** 一族能力的探测结论。key 稳定，前端按它匹配图标 */
export interface Capability {
  key: string
  label: string
  /** 接口调通了、拿到了数据 */
  ok: boolean
  /** 不可用的原因是缺权限（要去加策略），而不是接口报错（重试可能就好） */
  denied: boolean
  class: string
  detail: string
  /** 缺权限时该补的 IAM 策略 */
  policy: string
  /** 探测到的资源数量 */
  count: number
  durationMs: number
}

/** 一次权限自检的结果 */
export interface CapabilityProbe {
  region: string
  probedAt: string
  items: Capability[]
  /** 一句话结论 */
  summary: string
}

/** 1–8，对应 --acct-N。接入时分配后不再变化（§5.2） */
export type AccountColorIndex = 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8

export interface Quota {
  ocpuUsed: number; ocpuLimit: number
  memUsed: number; memLimit: number
  blockUsed: number; blockLimit: number
  /** AMD 免费微型实例，通常是 2 台封顶 */
  microUsed: number; microLimit: number
  /** 哪些项没有实际上限。升级号的 ARM 就属于这种 */
  unlimited: { ocpu: boolean; mem: boolean; block: boolean; micro: boolean }
}

export type AccountTier = 'trial' | 'paid' | 'unknown'

/** 镜像系统归成几类：SSH 用户和扩容后的命令都按它分。 */
export type OsFamily = 'oracle' | 'ubuntu' | 'debian' | 'windows' | 'unknown'

export interface Account {
  /** 后端主键，16 位十六进制。前端只当作不透明标识使用 */
  id: string
  alias: string
  /** 三字母短代号，全局唯一，与身份色同时出现 */
  code: string
  colorIndex: AccountColorIndex
  tenancyTail: string
  regions: string[]
  status: AccountStatus
  /** 上次校验时刻（ISO）。文案在模板里现算，不预先格式化 */
  lastCheckedAt: string | null
  /** 校验失败时后端给的原因 */
  statusMessage: string
  /** 账号性质：试用 / 升级 / 未知 */
  tier: AccountTier
  /** 试用到期日（ISO）。仅试用号有 */
  trialEndsAt: string | null
  /** 甲骨文账号开户时刻（ISO）。取不到时为 null */
  openedAt: string | null
  email: string
  fingerprint: string
  quota: Quota
  /** 配额数据来自哪个区域（后端只查主区域），为空表示还没拉到 */
  quotaRegion: string
  createdAt: string

  /** 自由文本标签。没有标签时是空数组，模板里可以无脑 v-for */
  tags: string[]
  /** 分组名。空串表示未分组 */
  group: string
  note: string
  /** 到期日（ISO）。null 表示不设提醒 */
  expiresAt: string | null
  /** 距离到期的天数：负数已过期，0 今天，null 不设到期 */
  expiryDays: number | null
  /** 手动排序权重，大的在前 */
  sortWeight: number

  /** 健康度色块。level 由后端算好，前端只负责画 */
  health: AccountHealth
  /** 连续凭据类失败次数。0 表示没在连续失败 */
  healthFailures: number
  /** 最近一次失败的错误分类名，如 AuthFailed */
  healthErrorClass: string
  /** 最后一次失败的时刻（ISO）。成功不清空，是历史事实 */
  healthErrorAt: string | null
  /** 最近一次成功的时刻（ISO）。null 表示从来没成功过 */
  healthOkAt: string | null
  /** 给用户看的一句话说明 */
  healthAdvice: string

  /** 上次权限自检的结果。null 表示还没自检过 */
  capability: CapabilityProbe | null
}

export interface Instance {
  /** 实例 OCID。是完整的 ocid1.instance… 串，不是数字 */
  id: string
  accountId: string
  name: string
  ocidTail: string
  region: string
  /** 给人看的简写，形如 AD-1。调 OCI 接口一律用 adFull */
  ad: string
  /** 完整可用域名，形如 xxxx:US-SANJOSE-1-AD-1 */
  adFull: string
  shape: string
  ocpu: number
  memGb: number
  /** 突发基线 BASELINE_1_8 / BASELINE_1_2；不突发时为空串 */
  baseline: string
  publicIp: string
  privateIp: string
  /** 主网卡上的全部 IPv6，未启用时为空数组 */
  ipv6: string[]
  /** 附属网卡上的公网 IPv4 / IPv6。列表只在「+N」里计数，换 IP 等操作不涉及它们 */
  secondaryIpv4: string[]
  secondaryIpv6: string[]
  /** SSH 默认登录用户，按镜像系统推断 */
  sshUser: string
  /** 镜像系统的归类，决定扩容后给哪套命令 */
  osFamily: OsFamily
  /** 本次页面会话里更换公网 IP 前的地址，列表据此标出新旧对照 */
  previousIp?: string
  bootGb: number
  bootLimitGb: number
  vpu: number
  /** 面板观测到开机的时刻。null 表示未观测到，只能退回"创建至今" */
  runningSince: string | null
  /** 用户备注 */
  note: string
  /** 实例创建时刻 */
  createdAt: string
  state: LifecycleState
  /** 状态异常（如非用户操作导致的 STOPPED），列表中加 ⚠ */
  anomaly?: boolean
  /** 后端记录的最近一次操作失败原因，非空时该行浮出错误条 */
  lastError?: string
  /** 乐观更新期间按钮 spinner */
  busy?: boolean
  /** 刚落定：触发 instance-ready 高光，时间戳 */
  settledAt?: number
}

export interface ToastPayload {
  tone: 'success' | 'warning' | 'danger' | 'info' | 'accent'
  title: string
  body?: string
  command?: string
  /** 附带一个跳转链接，如「查看任务」 */
  link?: { label: string; to: string }
}

export interface ConfirmL2 {
  level: 2
  title: string
  body: string
  okLabel: string
  onConfirm: () => void
}

export interface ConfirmL3 {
  level: 3
  title: string
  body: string
  /** 必须手动输入的资源全名 */
  noun: string
  nounLabel: string
  losses: string[]
  okLabel: string
  onConfirm: () => void
}

export type ConfirmRequest = ConfirmL2 | ConfirmL3

export type DrawerRequest =
  | { kind: 'instance'; id: string; tab?: string }
  | { kind: 'account'; id: string; tab?: string }
  | { kind: 'add-account' }
  | { kind: 'create-instance' }

/** KeyValueList 的一行 */
export interface KvItem {
  k: string
  v: string
  mono?: boolean
  tone?: string
  copyable?: boolean
  /**
   * 非空表示这是敏感值，按该类型打码并带展开按钮。
   * 复制仍然复制原文。
   */
  secret?: MaskKind
  /** 非空时值渲染成外链，新标签页打开。 */
  href?: string
  /** 行尾的小按钮。目前只在 secret 行上渲染（IPv6 地址旁的「移除」） */
  action?: { label: string; onClick: () => void; disabled?: boolean; tone?: 'warning' }
}

/** CheckList 的一项（权限自检、连接校验、诊断） */
export interface CheckItem {
  tone: 'ok' | 'warn' | 'fail' | 'info'
  text: string
}
