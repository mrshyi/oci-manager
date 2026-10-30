/**
 * 演示站的「本地增强版」接口。
 *
 * 上游的模拟后端（`demo/server.ts`）只覆盖 upstream 那一套接口；本地分支新增的
 * 账号组织、权限自检、计费风险、运维任务、运行日志、Cloudflare 联动、加密备份、
 * 调用预算在那边一律落到 `demo_unsupported`，页面会提示「下载部署后可用」。
 *
 * 这里把它们补齐。规则照抄真实后端，不是随手编的：
 *   - 运维任务的状态迁移用 `store/opstask.go` 的白名单，非法迁移回 409
 *   - 健康度色块由「最近一次校验结论」算出来，不是一个随机颜色
 *   - 计费风险守住「没查到 ≠ 没风险」：取不到的项走 attention，不进空列表
 *   - 权限自检把「缺权限」与「接口报错」分开，前者给可复制的 IAM 策略
 *
 * 单独一个文件而不是塞进 `server.ts`：后者是上游文件，每次合并上游都会改它，
 * 把本地的东西堆进去只会让冲突越滚越大（与 `api/opsTasks.ts` 同样的理由）。
 */
import type * as D from '@/api/types'

/* ---------- 与 demo/server.ts 的契约 ---------- */

/** 演示站的账号。`proxyId` 由 server.ts 附加，这里用不到但保持结构一致。 */
export interface DemoAccount extends D.AccountDTO {
  proxyId?: string
}

export interface DemoInstance {
  dto: D.InstanceDTO
  vnics: D.VnicInfoDTO[]
  blockVolumes: D.VolumeInfoDTO[]
}

export interface DemoNet {
  accountId: string
  region: string
  vcn: D.VcnDTO
  subnets: D.SubnetDTO[]
}

/** 保留型公网 IP。计费风险要靠它算出「有东西正在扣钱」。 */
export interface DemoReservedIp {
  accountId: string
  ip: { ipAddress: string }
}

type Params = Record<string, string>
type Query = Record<string, string>
type Handler = (p: Params, q: Query, body: any) => unknown

export interface DemoDeps {
  /** 往演示站的路由表里注册一条。与 server.ts 里的 on() 是同一个函数。 */
  on: (method: string, pattern: string, h: Handler) => void
  accounts: DemoAccount[]
  instances: DemoInstance[]
  nets: DemoNet[]
  reservedIps: DemoReservedIp[]
  /** 写一条审计。与真实后端一样，写操作都要留痕。 */
  log: (action: string, target?: string, accountId?: string, detail?: string, result?: string) => void
  fail: (status: number, code: string, message: string) => never
  uid: (prefix: string) => string
  iso: (ms: number) => string
  ago: (ms: number) => string
}

/* ---------- 常量：与后端同源的文案 ---------- */

/**
 * 五族能力的静态描述，对应 `accountsvc/capability.go` 的 capabilitySpecs。
 *
 * 顺序即展示顺序。策略文本必须与下面的策略模板对得上——两处说的是同一件事，
 * 写法不一致用户就不知道该信哪个。
 */
const CAP_SPECS: D.CapabilitySpecDTO[] = [
  { key: 'compute', label: '实例', policy: 'read instances in tenancy' },
  { key: 'network', label: '网络', policy: 'read virtual-network-family in tenancy' },
  { key: 'storage', label: '存储', policy: 'read volume-family in tenancy' },
  { key: 'usage', label: '用量账单', policy: 'read usage-report in tenancy' },
  { key: 'monitoring', label: '监控指标', policy: 'read metrics in tenancy' }
]

/**
 * 演示「缺权限」这条路径的账号。
 *
 * 只让一个账号缺一族权限，其余全绿：全部绿看不出「去加策略」这条引导，
 * 全都缺又像是坏了。挑用量账单是因为它最贴近真实——试用账号经常没开。
 */
const DENIED_CAPS: Record<string, string[]> = { 'acc-pho': ['usage'] }

/** 8 条策略，对应 `accountsvc/policytpl.go` 的 policyLines。 */
const POLICY_LINES: { verb: string; family: string; purpose: string; caps: string[] }[] = [
  { verb: 'read', family: 'instances', purpose: '列出实例、查看详情与状态。没有它面板里一台机器都看不到', caps: ['compute'] },
  { verb: 'manage', family: 'instances', purpose: '开机、关机、重启、改规格、终止、连控制台', caps: ['compute'] },
  { verb: 'read', family: 'virtual-network-family', purpose: '列出 VCN、子网、安全列表与公网 IP', caps: ['network'] },
  { verb: 'manage', family: 'virtual-network-family', purpose: '放行端口、换公网 IP、建 NAT 网关与负载均衡', caps: ['network'] },
  { verb: 'read', family: 'volume-family', purpose: '列出引导卷与块存储，查看容量与 VPU', caps: ['storage'] },
  { verb: 'manage', family: 'volume-family', purpose: '扩容引导卷、调整 VPU、分离与挂载', caps: ['storage'] },
  { verb: 'read', family: 'usage-report', purpose: '读取本月用量与出站流量（账单页与计费风险都要它）', caps: ['usage'] },
  { verb: 'read', family: 'metrics', purpose: '读取 CPU 与网络指标，画实例详情里的图表', caps: ['monitoring'] }
]

/** 组名占位符。用中文尖括号：用户是把文本直接粘进 OCI 控制台的，得一眼看出要改。 */
const GROUP_PLACEHOLDER = '<你的组>'

/** 区域城市中心坐标，对应 `geoip/regions.go` 里本演示用到的几个区域。 */
const REGION_COORDS: Record<string, [number, number]> = {
  'ap-tokyo-1': [35.68, 139.69],
  'ap-osaka-1': [34.69, 135.5],
  'ap-singapore-1': [1.35, 103.82],
  'us-phoenix-1': [33.45, -112.07],
  'eu-frankfurt-1': [50.11, 8.68]
}

/** 永久免费额度，对应 `ociclient/freetier.go`。核对日期 2026-08-18。 */
const FREE_ARM_OCPUS = 2
const FREE_MICRO = 2
const FREE_BLOCK_GB = 200

/** 估算单价（美元），与 `accountsvc/billing_risk.go` 同源。**只用于估算，不是账单。** */
const PRICE_RESERVED_IP_HOUR = 0.0025
const USD_TO_CNY = 7.2

const HOUR = 60 * 60 * 1000
const DAY = 24 * HOUR

/** 健康度的文案，对应 `accountsvc/health.go`。 */
const HEALTH_TEXT: Record<string, { label: string; advice: string }> = {
  green: { label: '正常', advice: '' },
  yellow: { label: '暂时异常', advice: '请求被限流或凭据校验偶发失败，稍后会自动复查。' },
  orange: { label: '配额已满', advice: '该账号的配额或服务限额已用尽，需在控制台调整限额或释放资源。' },
  red: { label: '凭据或权限异常', advice: '凭据或权限异常，请到控制台确认账号状态。' },
  unknown: { label: '尚未校验', advice: '还没有校验过这个账号，点「复检」跑一次。' }
}

/** 运维任务类型的中文名，对应 `httpapi/tasks.go` 的 opsKindLabels。 */
const KIND_LABELS: Record<string, string> = {
  change_ip_cidr: '更换公网 IP',
  open_ports: '放行端口',
  enable_500m: '提升带宽至 500Mbps',
  rescue: '救援编排',
  bulk: '批量操作'
}

/** 状态迁移白名单，对应 `store/opstask.go` 的 opsTransitions。 */
const TRANSITIONS: Record<D.OpsTaskState, D.OpsTaskState[]> = {
  pending: ['running', 'canceled'],
  running: ['succeeded', 'failed', 'paused', 'canceled'],
  paused: ['running', 'canceled'],
  failed: ['pending', 'canceled'],
  succeeded: [],
  canceled: []
}

const ACTION_NOTICES: Record<string, string> = {
  pause: '已暂停。恢复后会从停下的位置继续。',
  resume: '已恢复，下一轮会立刻开始。',
  cancel: '已取消。已经发往 Oracle 的请求不会被回滚，云端已产生的变更请到对应页面核对。',
  retry: '已重新排队。会从上次停下的阶段继续，不会从头重做。'
}

/** 救援与带宽的阶段清单，对应 `opssvc` 里的 RescueStages / BandwidthStages。 */
const RESCUE_STAGES = [
  'PRECHECK', 'STOPPING', 'DETACHING', 'ATTACHING', 'READY_FOR_REPAIR',
  'DETACHING_RESCUE', 'REATTACHING', 'STARTING', 'DONE'
]
const BW_ENABLE_STAGES = [
  'PRECHECK', 'NAT_CREATING', 'ROUTE_SWITCHING', 'NLB_CREATING',
  'BACKEND_SET_CREATING', 'LISTENER_CREATING', 'DONE'
]
const BW_DISABLE_STAGES = [
  'DISCOVER', 'LISTENER_DELETING', 'BACKEND_SET_DELETING',
  'NLB_DELETING', 'ROUTE_RESTORING', 'NAT_DELETING', 'CLEANED'
]

/* ---------- 供 server.ts 复用的两个派生视图 ---------- */

/** 标签用量，按次数降序、同次数按字母序。筛选器与输入联想都吃这份数据。 */
export function tagUsageOf(accounts: DemoAccount[]): D.TagUsageDTO[] {
  const count = new Map<string, number>()
  for (const a of accounts) for (const t of a.tags) count.set(t, (count.get(t) ?? 0) + 1)
  return [...count.entries()]
    .map(([tag, n]) => ({ tag, count: n }))
    .sort((x, y) => (y.count - x.count) || x.tag.localeCompare(y.tag))
}

/** 分组顺序：未分组固定最后，其余按组内最大排序权重降序。 */
export function groupsOf(accounts: DemoAccount[]): string[] {
  const best = new Map<string, number>()
  for (const a of accounts) {
    const name = a.meta.groupName
    best.set(name, Math.max(best.get(name) ?? -Infinity, a.meta.sortWeight))
  }
  return [...best.keys()].sort((x, y) => {
    if ((x === '') !== (y === '')) return y === '' ? -1 : 1
    const wx = best.get(x) ?? 0
    const wy = best.get(y) ?? 0
    if (wx !== wy) return wy - wx
    return x.localeCompare(y)
  })
}

/* ---------- 注册 ---------- */

export function registerLocalRoutes(d: DemoDeps): void {
  const { on, accounts, instances, nets, reservedIps, log, uid, iso, ago } = d

  /**
   * 报错即终止，与真实后端一致。
   *
   * 单独包一层是因为 TS 只对「函数声明」和「带类型标注的 const」做 never
   * 收窄，从参数里解构出来的函数不会被当作控制流终止点——那样
   * `if (!x) fail(...)` 之后 x 仍然被当成可能为空。
   */
  function fail(status: number, code: string, message: string): never {
    return d.fail(status, code, message)
  }

  const acctById = (id: string): DemoAccount =>
    accounts.find(a => a.id === id) ?? fail(404, 'not_found', '账号不存在')
  const instById = (id: string): DemoInstance =>
    instances.find(i => i.dto.id === id) ?? fail(404, 'not_found', '实例不存在或尚未同步')
  const live = () => instances.filter(i => i.dto.lifecycleState !== 'TERMINATED')
  const isArm = (shape: string) => shape.includes('.A1.')
  const isMicro = (shape: string) => shape === 'VM.Standard.E2.1.Micro'

  /* ================= 账号组织：标签 / 附加信息 / 健康度 ================= */

  /** 标签用量。`GET /api/accounts` 也会带上同一份，见 server.ts。 */
  const tagUsage = () => tagUsageOf(accounts)

  /**
   * 把健康度记录补上等级、标签与建议。
   *
   * 真实后端里这一步是 `accountsvc.HealthView`：level 由「连续失败次数 +
   * 是否凭据类错误」算，前端不自己判颜色。演示站沿用同一条纪律。
   */
  const healthView = (h: D.AccountHealthDTO): D.AccountHealthDTO => {
    const text = HEALTH_TEXT[h.level] ?? HEALTH_TEXT.unknown
    return { ...h, label: text.label, advice: text.advice }
  }

  /** 距到期天数。负数已过期，0 是今天，null 表示不设到期。 */
  const expiryDays = (expiresAt: string | null): number | null => {
    if (!expiresAt) return null
    const end = Date.parse(expiresAt)
    if (Number.isNaN(end)) return null
    return Math.ceil((end - Date.now()) / DAY)
  }

  on('GET', '/api/accounts/tags', () => ({ tags: tagUsage() }))

  on('GET', '/api/accounts/{id}/tags', p => ({ tags: [...acctById(p.id).tags] }))

  on('PUT', '/api/accounts/{id}/tags', (p, _, b) => {
    const a = acctById(p.id)
    // 与 store 层同样的清洗：去空白、去空项、去重、排序。
    const raw: unknown[] = Array.isArray(b?.tags) ? b.tags : []
    const tags = [...new Set(raw.map(t => String(t).trim()).filter(Boolean))].sort()
    if (tags.some(t => t.length > 32)) {
      fail(400, 'bad_tag', '单个标签最多 32 个字符')
    }
    a.tags = tags
    a.updatedAt = iso(Date.now())
    log('account_tags', tags.join(','), a.id)
    return { tags }
  })

  on('GET', '/api/accounts/{id}/meta', p => ({ meta: { ...acctById(p.id).meta } }))

  on('PUT', '/api/accounts/{id}/meta', (p, _, b) => {
    const a = acctById(p.id)
    // PUT 是整体替换：没传的字段等于清空，这与后端的语义一致。
    const expiresAt = b?.expiresAt ? String(b.expiresAt) : null
    const huntAckAt = b?.huntAckAt ? String(b.huntAckAt) : null
    if (expiresAt && Number.isNaN(Date.parse(expiresAt))) fail(400, 'bad_time', 'expiresAt 不是合法时间')
    if (huntAckAt && Number.isNaN(Date.parse(huntAckAt))) fail(400, 'bad_time', 'huntAckAt 不是合法时间')
    a.meta = {
      accountId: a.id,
      groupName: String(b?.groupName ?? '').trim(),
      note: String(b?.note ?? ''),
      expiresAt,
      sortWeight: Number(b?.sortWeight ?? 0) || 0,
      huntAckAt,
      updatedAt: iso(Date.now())
    }
    a.expiryDays = expiryDays(expiresAt)
    a.updatedAt = iso(Date.now())
    log('account_meta', a.meta.groupName, a.id)
    return { meta: { ...a.meta } }
  })

  on('GET', '/api/accounts/{id}/health', p => healthView(acctById(p.id).health))

  // 清除健康度只清「据此推断出的告警等级」，不动 accounts.status 这个原始结论。
  // 下次校验若仍然失败，红块会重新出现——不能变成永久静音。
  on('DELETE', '/api/accounts/{id}/health', p => {
    const a = acctById(p.id)
    a.health = {
      accountId: a.id, lastOkAt: null, lastErrorClass: '', lastErrorAt: null,
      lastErrorMessage: '', consecutiveFailures: 0, fatal: false,
      updatedAt: iso(Date.now()), level: 'unknown', label: '', advice: ''
    }
    log('account_health_clear', a.alias, a.id)
    return healthView(a.health)
  })

  /* ================= 权限自检 ================= */

  /** 跑一次五族探测。结果落回账号上，列表页的色块与详情页读的是同一份。 */
  const probeFor = (a: DemoAccount): D.CapabilityProbeDTO => {
    const mine = live().filter(i => i.dto.accountId === a.id)
    const denied = new Set(DENIED_CAPS[a.id] ?? [])
    const countOf: Record<string, number> = {
      compute: mine.length,
      network: nets.filter(n => n.accountId === a.id).length,
      storage: mine.reduce((n, i) => n + 1 + i.blockVolumes.length, 0)
    }
    const items: D.CapabilityDTO[] = CAP_SPECS.map(spec => {
      const isDenied = denied.has(spec.key)
      const item: D.CapabilityDTO = {
        key: spec.key,
        label: spec.label,
        ok: !isDenied,
        denied: isDenied,
        count: isDenied ? 0 : (countOf[spec.key] ?? 0),
        durationMs: isDenied ? 210 : Math.round(120 + Math.random() * 480)
      }
      if (isDenied) {
        item.class = 'NotAuthorizedOrNotFound'
        item.detail = '没有权限调用这族接口'
        item.policy = spec.policy
      }
      return item
    })
    const ok = items.filter(i => i.ok).length
    return {
      accountId: a.id,
      region: a.defaultRegion,
      probedAt: iso(Date.now()),
      items,
      summary: ok === items.length
        ? `五族全部可用（${items.length}/${items.length}）`
        : `${ok}/${items.length} 族可用，缺：${items.filter(i => !i.ok).map(i => i.label).join('、')}`
    }
  }

  on('GET', '/api/accounts/{id}/capability-probe', p => {
    const a = acctById(p.id)
    if (!a.capability) {
      // 没自检过不是 404，是「还没查过」。五族清单一起下发，前端不必自己维护一份。
      return { probe: null, notice: '这个账号还没有做过权限自检。', specs: CAP_SPECS }
    }
    return { probe: a.capability }
  })

  on('POST', '/api/accounts/{id}/capability-probe', p => {
    const a = acctById(p.id)
    if (!a.enabled) fail(400, 'account_disabled', '该账号已停用，启用后才能做权限自检。')
    // 连客户端都建不出来时直接报错，而不是返回一份全 false 的结果——
    // 后者看起来像「缺权限」，会把人引向错误的方向。
    if (a.status === 'error') {
      fail(502, 'credentials', '读取凭据失败：指纹与上传到 Oracle 的公钥不匹配（演示数据）')
    }
    const probe = probeFor(a)
    a.capability = probe
    a.updatedAt = iso(Date.now())
    log('account_capability_probe', a.alias, a.id, probe.summary)
    return { probe }
  })

  /* ================= 计费风险 ================= */

  /**
   * 回答一个问题：「这个账号上有没有东西正在扣钱」。
   *
   * 六条规则与 `accountsvc/billing_risk.go` 一致：arm_over / micro_over /
   * block_over / nlb / reserved_public_ip / cross_region_egress。取不到的项
   * 走 attention 而不是消失——「没查到」和「查了没问题」必须能分开，
   * 否则空列表会被读成「一切正常」。
   *
   * 演示数据里没有 NLB，出站流量也远低于每月 10 TB 的免费额度，
   * 所以这两条规则不会触发；它们留在后端，这里不编造假风险。
   */
  const billingRisk = (a: DemoAccount): D.BillingRiskDTO => {
    const items: D.BillingRiskItemDTO[] = []
    const paid = a.paymentModel === 'PAY_AS_YOU_GO'

    if (a.status === 'error') {
      // 凭据坏了：本地能算的项照样算，其余五项明确写「没查到」。
      for (const key of ['arm_over', 'micro_over', 'block_over', 'nlb', 'reserved_public_ip', 'cross_region_egress']) {
        items.push({
          key, label: '未查到', severity: 'attention', estDailyCny: 0,
          detail: '读取凭据失败，这一项无法确认', basis: '凭据不可用', confirmed: false
        })
      }
      return { accountId: a.id, region: a.homeRegion, generatedAt: iso(Date.now()), items,
        totalDailyCny: 0, totalMonthlyCny: 0,
        notice: '凭据不可用，六项全部没能查到。这不等于没有风险。' }
    }

    const mine = live().filter(i => i.dto.accountId === a.id)
    const armCpus = mine.filter(i => isArm(i.dto.shape)).reduce((n, i) => n + i.dto.ocpus, 0)
    const micros = mine.filter(i => isMicro(i.dto.shape)).length
    const blockGb = mine.reduce((n, i) => n + i.dto.bootVolumeGb +
      i.blockVolumes.reduce((m, v) => m + v.sizeInGbs, 0), 0)
    const reserved = reservedIps.filter(r => r.accountId === a.id).length

    // 付费账号的额度来自账号自己的 limits 接口，不套免费额度——
    // 对它们说「超出免费额度」是错的。
    const freeBasis = '按公开免费额度估算（未从 limits 接口确认）'
    if (!paid && armCpus > FREE_ARM_OCPUS) {
      items.push({
        key: 'arm_over', label: 'ARM 超出免费额度', severity: 'billing',
        detail: `ARM 已用 ${armCpus} OCPU，免费额度 ${FREE_ARM_OCPUS} OCPU，超出 ${armCpus - FREE_ARM_OCPUS} 个`,
        estDailyCny: 0, basis: freeBasis, confirmed: false
      })
    }
    if (micros > FREE_MICRO) {
      items.push({
        key: 'micro_over', label: 'Micro 实例超出免费额度', severity: 'billing',
        detail: `Micro 实例 ${micros} 台，免费额度 ${FREE_MICRO} 台`,
        estDailyCny: 0, basis: freeBasis, confirmed: false
      })
    }
    if (!paid && blockGb > FREE_BLOCK_GB) {
      items.push({
        key: 'block_over', label: '块存储超出免费额度', severity: 'billing',
        detail: `块存储已用 ${blockGb} GB，免费额度 ${FREE_BLOCK_GB} GB，超出 ${blockGb - FREE_BLOCK_GB} GB`,
        estDailyCny: 0, basis: freeBasis, confirmed: false
      })
    }
    if (reserved > 0) {
      const daily = reserved * PRICE_RESERVED_IP_HOUR * 24 * USD_TO_CNY
      items.push({
        key: 'reserved_public_ip', label: '存在保留公网 IP', severity: 'billing',
        detail: `${reserved} 个保留型公网 IP 未绑定实例。保留状态按小时计费，临时 IP 不额外收费`,
        estDailyCny: Math.round(daily * 100) / 100,
        basis: `按 $${PRICE_RESERVED_IP_HOUR}/小时估算，汇率 ${USD_TO_CNY}`,
        confirmed: true
      })
    }

    const daily = items.reduce((n, i) => n + i.estDailyCny, 0)
    return {
      accountId: a.id, region: a.homeRegion, generatedAt: iso(Date.now()), items,
      totalDailyCny: Math.round(daily * 100) / 100,
      totalMonthlyCny: Math.round(daily * 30 * 100) / 100,
      notice: '金额按 Oracle 公开标价粗算，不是账单。演示站没有真正调用 limits 与账单接口。'
    }
  }

  on('GET', '/api/accounts/{id}/billing-risk', p => billingRisk(acctById(p.id)))

  /* ================= 云侧策略模板 ================= */

  on('GET', '/api/iam/policy-template', (_, q) => {
    const raw = (q.group ?? '').trim()
    // 组名会进策略语句，挡掉能改变语句结构的东西。这不是安全边界（最终由 OCI 校验），
    // 而是别让人复制出一份语法错误的文本然后以为是面板写错了。
    const group = /^[\w .@+-]{1,64}$/.test(raw) ? raw : GROUP_PLACEHOLDER
    const lines: D.PolicyLineDTO[] = POLICY_LINES.map(pl => ({
      text: `Allow group ${group} to ${pl.verb} ${pl.family} in tenancy`,
      purpose: pl.purpose,
      capabilities: pl.caps
    }))
    return {
      group,
      text: lines.map(l => l.text).join('\n'),
      lines,
      steps: [
        `在 OCI 控制台左上角菜单进入 Identity & Security → Domains → Default（或你的身份域）→ Groups，新建一个组，比如 ${group}。`,
        '进入该组 → Add user to group，把面板使用的那个用户加进去。（用 API 密钥接入的用户就是密钥的属主，别加成别的用户。）',
        '进入 Identity → Policies，选中你的 tenancy（根 compartment），点 Create Policy，名字随意，比如 oci-tools-minimal。',
        '把下面 8 条粘进策略编辑器（Policy Builder 切到 JSON 或直接粘贴文本均可），保存。策略通常立即生效，偶尔需要等几十秒。'
      ],
      scope: 'in tenancy',
      notice: '这 8 条里没有 manage policies、manage users、manage groups——' +
        '面板不建组、不写策略、不改 IAM。凭据泄露时能造成的破坏被限制在' +
        '「动资源」而不是「改权限」。粘完之后回到账号页点一次「权限自检」，' +
        '五个点全绿就说明配对了。',
      version: '1',
      readOnlyCount: POLICY_LINES.filter(l => l.verb === 'read').length
    } satisfies D.PolicyTemplateDTO
  })

  /* ================= 加密备份 ================= */

  let pendingRestore: { manifest: D.BackupManifestDTO; stagedAt: string } | null = null

  const manifestOf = (): D.BackupManifestDTO => ({
    createdAt: ago(3 * HOUR),
    appVersion: '0.5.5-demo',
    backupVersion: 1,
    dbBytes: 1_486_848,
    dbSha256: '9f2c4a1e7b3d58c6a0e14f92bd7c3a5e8d61042bf9c7ae35d8b1046c2fe97a3d',
    includesMasterKey: false,
    notice: '包里默认不含主密钥：它该和数据库分开保管。少了它，恢复出来的私钥全部解不开。'
  })

  on('GET', '/api/backup', () => {
    const out: D.BackupStatusDTO = { pending: pendingRestore !== null, backupVersion: 1 }
    if (pendingRestore) {
      out.manifest = pendingRestore.manifest
      out.stagedAt = pendingRestore.stagedAt
      out.masterKeyPending = false
    }
    return out
  })

  on('POST', '/api/backup/export', (_, __, b) => {
    const password = String(b?.password ?? '')
    if (!password.trim()) fail(400, 'password_required', '备份必须设置口令')
    if (password.length < 8) {
      fail(400, 'password_too_short', '备份口令至少 8 位。这份文件会被拷到别处，口令是它唯一的防线。')
    }
    log('backup_export', 'database', '', `含主密钥=${!!b?.includeMasterKey}`)
    // 真实后端回的是 AES-256-GCM 密文。演示站给一段等长的占位字节，
    // 足够让下载流程（文件名、大小、保存对话框）完整走一遍。
    const body = `OCICORE-BACKUP-DEMO\ncreatedAt=${iso(Date.now())}\n` + 'x'.repeat(2048)
    return new Blob([body], { type: 'application/octet-stream' })
  })

  on('POST', '/api/backup/restore', (_, __, b) => {
    // 恢复上传的是 multipart，body 是 FormData 而不是普通对象。
    const form = b instanceof FormData ? b : null
    if (!form?.get('file')) fail(400, 'file_required', '请选择备份文件')
    if (!form.get('password')) fail(400, 'password_required', '请输入备份口令')
    const manifest = manifestOf()
    pendingRestore = { manifest, stagedAt: iso(Date.now()) }
    log('backup_restore', 'database', '', `备份创建于 ${manifest.createdAt}`)
    return {
      manifest,
      requiresRestart: true,
      masterKeyDiffers: false,
      notice: '备份已校验并暂存。**重启服务后生效**——数据库文件正被进程打开，' +
        '在运行中替换它会把库写坏。替换前会自动备份当前库。'
    } satisfies D.RestoreResultDTO
  })

  /* ================= Cloudflare 联动 ================= */

  const CF_NOTICE = '只接受 API Token，不接受账号密码。Token 请在 Cloudflare 后台' +
    '「我的个人资料 → API 令牌」里创建，权限只需 Zone.DNS 编辑 + Zone 读取，' +
    '并限定到具体域名。面板里存的是加密后的密文，界面上不会回显。'

  const webInstance = live().find(i => i.dto.accountId === 'acc-tok' && i.dto.displayName === 'tok-web')

  const cfZones: D.CFZoneDTO[] = [{
    id: 'cfz-1', accountId: 'acc-tok', zoneId: 'demo-zone-0001', zoneName: 'example.com',
    hasToken: true, enabled: true, lastSyncAt: ago(2 * DAY), lastError: '',
    createdAt: ago(20 * DAY), updatedAt: ago(2 * DAY)
  }]

  const cfRecords: D.CFRecordDTO[] = webInstance ? [{
    id: 'cfr-1', zonePk: 'cfz-1', recordId: 'demo-rec-0001', name: 'home.example.com', type: 'A',
    proxied: false, ttl: 300, accountId: 'acc-tok', instanceId: webInstance.dto.id,
    lastIp: webInstance.dto.publicIp, lastSyncAt: ago(2 * DAY), lastError: '',
    createdAt: ago(20 * DAY), updatedAt: ago(2 * DAY), zoneName: 'example.com'
  }] : []

  const cfZoneById = (id: string) => cfZones.find(z => z.id === id) ?? fail(404, 'not_found', '域名不存在')

  on('GET', '/api/cf/zones', () => ({
    zones: structuredClone(cfZones),
    records: cfRecords.map(r => ({ ...r, zoneName: cfZones.find(z => z.id === r.zonePk)?.zoneName ?? '' })),
    notice: CF_NOTICE
  }))

  on('POST', '/api/cf/zones', (_, __, b) => {
    const zone = String(b?.zone ?? '').trim()
    const token = String(b?.token ?? '').trim()
    if (!zone) fail(400, 'invalid_zone', '请填域名或 zone id')
    if (!token) fail(400, 'invalid_token', '请填 API Token')
    // 真实后端会立刻拿 token 去 Cloudflare 解析一次 zone，把「填错了」提前到
    // 添加这一步，而不是等第一次换 IP 之后才发现。
    const name = zone.replace(/^https?:\/\//, '').split('/')[0]
    if (!/^[a-z0-9.-]+\.[a-z]{2,}$/i.test(name)) {
      fail(400, 'cf_zone_unresolved', 'Cloudflare 返回 400：无法识别这个域名或 token 权限不足（演示数据）')
    }
    if (cfZones.some(z => z.zoneName === name)) fail(409, 'cf_zone_exists', `域名 ${name} 已经添加过了`)
    const z: D.CFZoneDTO = {
      id: uid('cfz-'), accountId: String(b?.accountId ?? ''), zoneId: `demo-zone-${uid('')}`,
      zoneName: name, hasToken: true, enabled: true, lastSyncAt: '', lastError: '',
      createdAt: iso(Date.now()), updatedAt: iso(Date.now())
    }
    cfZones.push(z)
    log('cf_zone_create', z.zoneName, z.accountId)
    return { zone: structuredClone(z) }
  })

  on('PATCH', '/api/cf/zones/{id}', (p, _, b) => {
    const z = cfZoneById(p.id)
    if (b?.accountId !== undefined) z.accountId = String(b.accountId)
    if (b?.enabled !== undefined) z.enabled = !!b.enabled
    if (b?.token !== undefined) z.hasToken = String(b.token).trim() !== ''
    z.updatedAt = iso(Date.now())
    return { zone: structuredClone(z) }
  })

  on('DELETE', '/api/cf/zones/{id}', p => {
    const z = cfZoneById(p.id)
    cfZones.splice(cfZones.indexOf(z), 1)
    for (let n = cfRecords.length - 1; n >= 0; n--) if (cfRecords[n].zonePk === z.id) cfRecords.splice(n, 1)
    log('cf_zone_delete', z.zoneName)
    return { ok: true }
  })

  on('POST', '/api/cf/zones/{id}/verify', p => {
    const z = cfZoneById(p.id)
    if (!z.hasToken) fail(400, 'cf_verify_failed', '这个域名没有配置 API Token（演示数据）')
    z.lastError = ''
    z.updatedAt = iso(Date.now())
    return { zone: structuredClone(z) }
  })

  on('GET', '/api/cf/zones/{id}/records', p => {
    const z = cfZoneById(p.id)
    if (!z.hasToken) fail(400, 'cf_list_failed', '这个域名没有配置 API Token（演示数据）')
    // bound 让界面能标出「已经绑过实例」的行，避免重复绑定。
    const bound = new Set(cfRecords.map(r => r.recordId))
    const remote: D.CFRemoteRecordDTO[] = [
      { id: 'demo-rec-0001', type: 'A', name: `home.${z.zoneName}`, content: webInstance?.dto.publicIp ?? '203.0.113.10', ttl: 300, proxied: false, bound: bound.has('demo-rec-0001') },
      { id: 'demo-rec-0002', type: 'A', name: `api.${z.zoneName}`, content: '203.0.113.24', ttl: 300, proxied: false, bound: bound.has('demo-rec-0002') },
      { id: 'demo-rec-0003', type: 'A', name: `www.${z.zoneName}`, content: '198.51.100.7', ttl: 1, proxied: true, bound: bound.has('demo-rec-0003') }
    ]
    return { records: remote }
  })

  on('POST', '/api/cf/records', (_, __, b) => {
    const zonePk = String(b?.zonePk ?? '')
    const zone = cfZoneById(zonePk)
    const instanceId = String(b?.instanceId ?? '')
    if (!instanceId) fail(400, 'invalid_instance', '请选择要跟随的实例')
    const inst = instById(instanceId)
    if (cfRecords.some(r => r.recordId === String(b?.recordId ?? ''))) {
      fail(409, 'cf_record_exists', '这条记录已经绑定过实例了')
    }
    const rec: D.CFRecordDTO = {
      id: uid('cfr-'), zonePk, recordId: String(b?.recordId ?? uid('rec-')),
      name: String(b?.name ?? ''), type: 'A', proxied: !!b?.proxied,
      ttl: Number(b?.ttl ?? 300) || 300,
      accountId: String(b?.accountId ?? inst.dto.accountId), instanceId,
      lastIp: inst.dto.publicIp, lastSyncAt: '', lastError: '',
      createdAt: iso(Date.now()), updatedAt: iso(Date.now()), zoneName: zone.zoneName
    }
    cfRecords.push(rec)
    log('cf_record_create', rec.name, rec.accountId)
    return { record: structuredClone(rec) }
  })

  on('DELETE', '/api/cf/records/{id}', p => {
    const n = cfRecords.findIndex(r => r.id === p.id)
    if (n < 0) fail(404, 'not_found', '绑定不存在')
    cfRecords.splice(n, 1)
    return { ok: true }
  })

  on('POST', '/api/cf/records/{id}/sync', p => {
    const rec = cfRecords.find(r => r.id === p.id) ?? fail(404, 'not_found', '绑定不存在')
    const inst = instances.find(i => i.dto.id === rec.instanceId)
    if (!inst) fail(400, 'instance_missing', '这条绑定指向的实例已经不在面板里了，请重新选择实例')
    if (!inst.dto.publicIp) fail(400, 'no_public_ip', '这台实例当前没有公网 IP，无法同步；请先分配公网 IP')
    rec.lastIp = inst.dto.publicIp
    rec.lastSyncAt = iso(Date.now())
    rec.lastError = ''
    rec.updatedAt = rec.lastSyncAt
    return { status: 'updated', detail: `已把 ${rec.name} 指向 ${rec.lastIp}`, records: 1, ip: rec.lastIp }
  })

  /* ================= IP 情报与分布 ================= */

  const GEO_STATUS: D.GeoStatusDTO = {
    enabled: false,
    dir: 'data/geoip',
    cityBuiltAt: '',
    hasCity: false,
    hasAsn: false,
    notice: '没有检测到离线库（GeoLite2-City.mmdb / GeoLite2-ASN.mmdb）。' +
      '把文件放进 data/geoip/ 后重启即可按城市定位；现在按 OCI 区域的城市中心估算。'
  }

  /** 手填的位置。不会被离线库覆盖——刚分配的地址段常常还没进 GeoLite2。 */
  const manual = new Map<string, { country: string; city: string; latitude: number; longitude: number }>()

  on('GET', '/api/geo/status', () => ({
    status: GEO_STATUS,
    cachedIps: manual.size,
    knownRegions: Object.keys(REGION_COORDS).length
  }))

  on('GET', '/api/geo/distribution', () => {
    const points: D.GeoPointDTO[] = []
    for (const inst of live()) {
      const ip = (inst.dto.publicIp ?? '').trim()
      if (!ip) continue
      const acct = accounts.find(a => a.id === inst.dto.accountId)
      const point: D.GeoPointDTO = {
        instanceId: inst.dto.id, instanceName: inst.dto.displayName,
        accountId: inst.dto.accountId, accountCode: acct?.code ?? '',
        region: inst.dto.region, ip,
        found: false, country: '', countryCode: '', city: '',
        latitude: 0, longitude: 0, asn: '', org: '', source: ''
      }
      const m = manual.get(ip)
      if (m) {
        point.found = true
        point.country = m.country
        point.city = m.city
        point.latitude = m.latitude
        point.longitude = m.longitude
        point.source = 'manual'
      } else {
        // 没有离线库就退回区域坐标。不够精确，但比空白有用，
        // 而且 source 明确写着 region，不会被当成精确位置。
        const coords = REGION_COORDS[inst.dto.region]
        if (coords) {
          point.found = true
          point.latitude = coords[0]
          point.longitude = coords[1]
          point.source = 'region'
          point.country = inst.dto.region.split('-')[0].toUpperCase()
        }
      }
      points.push(point)
    }
    return {
      status: GEO_STATUS,
      points,
      notice: '坐标来自本地离线库；没有离线库时按 OCI 区域估算，同一区域的多台机器会落在同一个点上。' +
        '图里不画国界，只按经纬度定位。'
    }
  })

  on('PUT', '/api/geo/manual', (_, __, b) => {
    const ip = String(b?.ip ?? '').trim()
    if (!ip) fail(400, 'invalid_ip', '缺少 IP')
    const latitude = Number(b?.latitude ?? 0)
    const longitude = Number(b?.longitude ?? 0)
    if (latitude < -90 || latitude > 90 || longitude < -180 || longitude > 180) {
      fail(400, 'invalid_coords', '经纬度超出范围')
    }
    const info = { country: String(b?.country ?? ''), city: String(b?.city ?? ''), latitude, longitude }
    manual.set(ip, info)
    log('geo_manual_set', ip, '', `${info.country} ${info.city}`)
    return { info: { ip, ...info, found: true, source: 'manual' } }
  })

  on('DELETE', '/api/geo/manual/{ip}', p => {
    manual.delete(p.ip)
    log('geo_manual_clear', p.ip)
    return { ok: true }
  })

  /* ================= 运维任务 ================= */

  /**
   * 任务表。与真实后端一样只存当前状态，不存历史——历史在审计日志里。
   * 下面先种几条演示任务，任务页一打开就能看到「进行中 / 已暂停 / 已完成 / 失败」四种形态。
   */
  const opsTasks: D.OpsTaskDTO[] = []

  const taskById = (id: string): D.OpsTaskDTO =>
    opsTasks.find(t => t.id === id) ?? fail(404, 'not_found', '任务不存在')

  const aliasOf = (accountId: string) => accounts.find(a => a.id === accountId)?.alias ?? ''

  const makeTask = (patch: Partial<D.OpsTaskDTO> & { kind: string; accountId: string; name: string }): D.OpsTaskDTO => {
    const t: D.OpsTaskDTO = {
      id: uid('ops-'),
      instanceId: '',
      state: 'pending',
      stage: '',
      progress: {},
      attempts: 0,
      maxAttempts: 0,
      intervalSeconds: 20,
      lastError: '',
      result: '',
      dnsStatus: '',
      dnsError: '',
      expiresAt: '',
      nextAt: iso(Date.now() + 3000),
      startedAt: '',
      finishedAt: '',
      createdAt: iso(Date.now()),
      updatedAt: iso(Date.now()),
      accountAlias: aliasOf(patch.accountId),
      kindLabel: KIND_LABELS[patch.kind] ?? patch.kind,
      ...patch
    }
    opsTasks.unshift(t)
    return t
  }

  /**
   * 推进一步「进行中」的任务。
   *
   * 真实后端是 20 秒一轮的调度器；演示站没有常驻定时器，就在每次查询时
   * 按 nextAt 补算——页面本来就在 5 秒轮询，看起来是一样的。
   */
  const tick = (): void => {
    const now = Date.now()
    for (const t of opsTasks) {
      if (!t.nextAt) continue
      const due = Date.parse(t.nextAt)
      if (Number.isNaN(due) || due > now) continue

      // 排队中的任务到点被调度器捞走。没有这一步，刚创建的任务会一直停在
      // 「排队中」，而真实后端几秒内就会开始跑第一轮。
      if (t.state === 'pending') {
        t.state = 'running'
        t.startedAt = iso(now)
        t.nextAt = iso(now + t.intervalSeconds * 1000)
        t.updatedAt = iso(now)
        continue
      }
      if (t.state !== 'running') continue

      t.attempts += 1
      t.nextAt = iso(now + t.intervalSeconds * 1000)
      t.updatedAt = iso(now)
      if (t.kind === 'change_ip_cidr') {
        t.progress = { ...t.progress, attempt: t.attempts }
      }
    }
  }

  /**
   * 演示用的种子任务：每种状态各一条，任务页一打开就能看到四种形态。
   *
   * 目标实例按「账号 + 区域 + 运行状态」推导，不按名字写死——写死的话上游
   * 给演示数据改个机器名，这几条任务就会静默消失，而页面上只剩一句「还没有
   * 运维任务」，看不出是数据没种上还是功能没做。
   */
  const tokInstances = live().filter(i => i.dto.accountId === 'acc-tok')
  const seededWeb = tokInstances.find(i => i.dto.lifecycleState === 'RUNNING') ?? tokInstances[0]
  const seededOsaka = tokInstances.find(i => i.dto.region === 'ap-osaka-1' && i !== seededWeb) ?? tokInstances[1]
  const tokAlias = aliasOf('acc-tok')

  if (seededOsaka) {
    makeTask({
      kind: 'change_ip_cidr', accountId: 'acc-tok', instanceId: seededOsaka.dto.id,
      name: `换 IP：${tokAlias} ${seededOsaka.dto.displayName} → 203.0.113.0/24`, state: 'running',
      attempts: 7, maxAttempts: 30, intervalSeconds: 20,
      startedAt: ago(4 * 60_000), nextAt: iso(Date.now() + 12_000),
      progress: { attempt: 7, oldIp: '198.51.100.31', newIp: '198.51.100.88' },
      lastError: '这一轮的地址不在目标网段，20 秒后重试',
      dnsStatus: 'updated', createdAt: ago(4 * 60_000)
    })
  }

  if (seededWeb) {
    makeTask({
      kind: 'rescue', accountId: 'acc-tok', instanceId: seededWeb.dto.id,
      name: `救援：${tokAlias} ${seededWeb.dto.displayName}`, state: 'paused', stage: 'READY_FOR_REPAIR',
      attempts: 4, maxAttempts: 27, intervalSeconds: 20,
      startedAt: ago(52 * 60_000), nextAt: '',
      progress: { note: '引导卷已挂到救援机，等待人工修复' },
      createdAt: ago(55 * 60_000), updatedAt: ago(30 * 60_000)
    })
    makeTask({
      kind: 'enable_500m', accountId: 'acc-tok', instanceId: seededWeb.dto.id,
      name: `开启 500Mbps：${tokAlias} ${seededWeb.dto.displayName}`, state: 'succeeded', stage: 'DONE',
      attempts: 6, maxAttempts: 21, intervalSeconds: 20,
      startedAt: ago(2 * DAY), finishedAt: ago(2 * DAY - 90_000),
      result: 'NAT 网关、路由与 NLB 已就绪，实例当前可用带宽上限 500Mbps。',
      createdAt: ago(2 * DAY), updatedAt: ago(2 * DAY - 90_000)
    })
    makeTask({
      kind: 'open_ports', accountId: 'acc-tok', name: `放行端口：${tokAlias} tcp/8080,tcp/8443`,
      state: 'failed', attempts: 3, maxAttempts: 30, intervalSeconds: 20,
      startedAt: ago(6 * HOUR), finishedAt: ago(6 * HOUR - 40_000),
      lastError: '读取安全列表失败：NotAuthorizedOrNotFound（演示数据）',
      createdAt: ago(6 * HOUR), updatedAt: ago(6 * HOUR - 40_000)
    })
  }

  const taskView = (t: D.OpsTaskDTO): D.OpsTaskDTO => ({ ...t, accountAlias: aliasOf(t.accountId) })

  on('GET', '/api/tasks', (_, q) => {
    tick()
    const list = opsTasks.filter(t =>
      (!q.accountId || t.accountId === q.accountId) &&
      (!q.kind || t.kind === q.kind) &&
      (!q.state || t.state === q.state))
    return {
      tasks: list.map(taskView),
      // 状态清单随列表一起给：前端不必自己维护一份，将来加状态也不会漏改。
      states: ['pending', 'running', 'succeeded', 'failed', 'paused', 'canceled']
    } satisfies D.OpsTaskListDTO
  })

  on('GET', '/api/tasks/{id}', p => {
    tick()
    return { task: taskView(taskById(p.id)) }
  })

  on('POST', '/api/tasks/{id}/{action}', (p, _, b) => {
    const target: Record<string, D.OpsTaskState> = {
      pause: 'paused', resume: 'running', cancel: 'canceled', retry: 'pending'
    }
    const to = target[p.action]
    if (!to) fail(400, 'bad_action', '只支持 pause / resume / cancel / retry')
    void b

    const t = taskById(p.id)
    // 白名单之外一律 409 而不是 400：请求本身没写错，是任务当前状态不允许。
    if (t.state !== to && !TRANSITIONS[t.state].includes(to)) {
      fail(409, 'illegal_transition', `任务当前状态（${t.state}）不允许 ${p.action}。请刷新后重试`)
    }
    t.state = to
    t.updatedAt = iso(Date.now())
    if (to === 'paused') t.nextAt = ''
    if (to === 'running') {
      t.nextAt = iso(Date.now() + t.intervalSeconds * 1000)
      if (!t.startedAt) t.startedAt = iso(Date.now())
    }
    if (to === 'canceled') t.finishedAt = iso(Date.now())
    if (to === 'pending') {
      // 重试是「从上次停下的阶段继续」，不是从头重做，所以清掉的是失败信息而不是阶段。
      t.lastError = ''
      t.finishedAt = ''
      t.nextAt = iso(Date.now() + 3000)
    }
    log('task_' + p.action, t.name, t.accountId)
    return { task: taskView(t), notice: ACTION_NOTICES[p.action] }
  })

  on('DELETE', '/api/tasks/{id}', p => {
    const t = taskById(p.id)
    // 与后端一致：只允许删终态与 paused / failed，进行中的任务不能被悄悄丢掉。
    if (t.state === 'pending' || t.state === 'running') {
      fail(409, 'illegal_transition', '进行中的任务不能删除，请先取消')
    }
    opsTasks.splice(opsTasks.indexOf(t), 1)
    log('task_delete', t.name, t.accountId)
    return { ok: true, notice: '记录已删除。云端已经发生的变更不受影响。' }
  })

  /* ---------- 运维作业入口 ---------- */

  const clamp = (v: number, lo: number, hi: number, def: number): number => {
    if (!v) return def
    return Math.min(hi, Math.max(lo, v))
  }

  on('POST', '/api/ops/change-ip', (_, __, b) => {
    const instanceId = String(b?.instanceId ?? '').trim()
    const inst = instanceId ? instById(instanceId) : null
    const accountId = String(b?.accountId ?? inst?.dto.accountId ?? '').trim()
    if (!accountId) fail(400, 'bad_request', '缺少 accountId')
    const targets: string[] = Array.isArray(b?.targetCidrs) ? b.targetCidrs.map((s: unknown) => String(s)) : []
    if (!targets.length) fail(400, 'bad_request', '至少指定一个目标网段')
    const attempts = clamp(Number(b?.maxAttempts ?? 0), 1, 100, 30)
    const minutes = clamp(Number(b?.maxDurationMinutes ?? 0), 1, 180, 30)
    const acc = acctById(accountId)
    const t = makeTask({
      kind: 'change_ip_cidr', accountId, instanceId: inst?.dto.id ?? '',
      name: `换 IP：${acc.alias} → ${targets.join(',')}`,
      state: 'pending', maxAttempts: attempts, intervalSeconds: 20,
      expiresAt: iso(Date.now() + minutes * 60_000),
      progress: { oldIp: inst?.dto.publicIp ?? '' }
    })
    log('ops_change_ip', t.name, accountId)
    return {
      task: taskView(t),
      notice: `任务已创建：最多换 ${attempts} 次、最长 ${minutes} 分钟，命中目标网段即停止。` +
        '每次换 IP 会先删除旧 IP 再申请新 IP，SSH 连接会中断。'
    } satisfies D.OpsTaskAcceptedDTO
  })

  on('POST', '/api/ops/open-ports', (_, __, b) => {
    const accountId = String(b?.accountId ?? '').trim()
    if (!accountId) fail(400, 'bad_request', '缺少 accountId')
    if (!String(b?.region ?? '').trim()) fail(400, 'bad_request', '缺少 region')
    if (!String(b?.securityListId ?? '').trim()) fail(400, 'bad_request', '缺少 securityListId')
    const rules: { protocol: string; port: number }[] = Array.isArray(b?.rules) ? b.rules : []
    if (!rules.length) fail(400, 'bad_request', '至少指定一个端口')
    const acc = acctById(accountId)
    const label = rules.map(r => `${r.protocol === '17' ? 'udp' : r.protocol === '1' ? 'icmp' : 'tcp'}/${r.port || '*'}`).join(',')
    const t = makeTask({
      kind: 'open_ports', accountId, name: `放行端口：${acc.alias} ${label}`,
      progress: { note: `来源 ${String(b?.source ?? '0.0.0.0/0')}，共 ${rules.length} 条` }
    })
    log('ops_open_ports', t.name, accountId)
    return {
      task: taskView(t),
      notice: '任务已创建，后台会读取安全列表、追加规则后整体写回。' +
        '原有规则会原样保留；已经放行的端口不会重复添加。'
    }
  })

  on('POST', '/api/ops/bandwidth', (_, __, b) => {
    const accountId = String(b?.accountId ?? '').trim()
    if (!accountId) fail(400, 'bad_request', '缺少 accountId')
    const acc = acctById(accountId)
    const action = String(b?.action ?? 'enable').toLowerCase() === 'disable' ? 'disable' : 'enable'
    const stages = action === 'disable' ? BW_DISABLE_STAGES : BW_ENABLE_STAGES
    const minutes = clamp(Number(b?.maxDurationMinutes ?? 0), 5, 240, 60)
    const t = makeTask({
      kind: 'enable_500m', accountId, instanceId: String(b?.instanceId ?? ''),
      name: `${action === 'disable' ? '关闭' : '开启'} 500Mbps：${acc.alias} ${String(b?.instanceId ?? '').split('.').pop()}`,
      maxAttempts: stages.length * 3,
      expiresAt: iso(Date.now() + minutes * 60_000)
    })
    log('ops_bandwidth_' + action, t.name, accountId)
    return {
      task: taskView(t),
      stages,
      notice: action === 'disable'
        ? '关闭流程按逆序执行：删监听 → 删后端集 → 删 NLB → 还原默认路由 → 删 NAT 网关。' +
          '任一步失败会停在原地并保留提示，不会继续删后面的资源。'
        : '任务已创建。流程会依次创建 NAT 网关、切换默认路由、创建 NLB 与后端，' +
          '任一步失败都会停在原地，可以在任务列表里点「重试」从该步继续。'
    }
  })

  on('POST', '/api/ops/rescue', (_, __, b) => {
    const accountId = String(b?.accountId ?? '').trim()
    if (!accountId) fail(400, 'bad_request', '缺少 accountId')
    const targetId = String(b?.targetInstanceId ?? '').trim()
    if (!targetId) fail(400, 'bad_request', '缺少 targetInstanceId')
    if (!String(b?.rescueInstanceId ?? '').trim()) fail(400, 'bad_request', '缺少 rescueInstanceId')
    if (targetId === String(b.rescueInstanceId)) fail(400, 'bad_request', '目标实例与救援机不能是同一台')
    const acc = acctById(accountId)
    const minutes = clamp(Number(b?.maxDurationMinutes ?? 0), 10, 720, 120)
    const t = makeTask({
      kind: 'rescue', accountId, instanceId: targetId,
      name: `救援：${acc.alias} ${targetId.split('.').pop()}`,
      maxAttempts: RESCUE_STAGES.length * 3,
      expiresAt: iso(Date.now() + minutes * 60_000)
    })
    log('ops_rescue', t.name, accountId)
    return {
      task: taskView(t),
      stages: RESCUE_STAGES,
      notice: '任务已创建。流程会把目标实例关机、卸下引导卷、挂到救援机，' +
        '然后停下来等你修完——在任务列表里点「继续」才会挂回并启动。' +
        '这期间目标实例不可用，引导卷不属于任何一台运行中的机器。'
    }
  })

  /* ================= 调用预算 ================= */

  on('GET', '/api/risk-budget', () => ({
    enabled: true,
    window: '1h0m0s',
    windowStart: iso(Math.floor(Date.now() / HOUR) * HOUR),
    limit: 2000,
    accounts: accounts.map((a, n) => ({
      accountId: a.id,
      alias: a.alias,
      used: [412, 88, 0, 37][n % 4],
      limit: 2000,
      multiplier: n === 0 ? 4 : 1
    })),
    notice: '调用预算按小时计数。用量接近上限时，抢机与运维任务的间隔会自动放大，' +
      '界面上的「降频倍数」就是当前放大的比例。'
  }))

  /* ================= 运行日志 ================= */

  // 日志流不走路由表：真实实现是 SSE，而演示站没有 HTTP 层。
  // 见文件末尾的 demoLogStream——api/logs.ts 在演示构建里直接调它。
}

/**
 * 演示站的运行日志流。
 *
 * 单独导出而不是走路由表：真实实现是 SSE，而演示站没有 HTTP 层。
 * `api/logs.ts` 在 VITE_DEMO 构建里直接调这个函数，拿到与 EventSource 一致的
 * 三个回调。
 */
export interface DemoLogHandlers {
  onEntry?: (entry: { seq: number; time: string; level: string; msg: string; attrs?: Record<string, string> }) => void
  onOpen?: (reconnected: boolean) => void
  onError?: () => void
}

export function demoLogStream(handlers: DemoLogHandlers): () => void {
  const seed: [string, string, Record<string, string>?][] = [
    ['INFO', '服务启动完成', { version: '0.5.5-demo', addr: '127.0.0.1:8080' }],
    ['INFO', '已加载账号', { count: '4' }],
    ['INFO', '实例同步开始', { accounts: '4', regions: '5' }],
    ['INFO', '实例同步完成', { instances: '9', durationMs: '1284' }],
    ['INFO', '运维调度器已启动', { interval: '20s' }],
    ['WARN', '账号校验失败，稍后重试', { account: '法兰克福', class: 'NotAuthenticated', attempt: '3' }],
    ['WARN', '请求被限流，退避 8 秒', { service: 'compute', account: '新加坡', opcRequestId: 'demo-7f3a' }],
    ['ERROR', '软重启超时', { instance: 'sg-micro', waited: '10m0s' }],
    ['INFO', '审计已写入', { action: 'account_check_auto', result: 'fail' }],
    ['INFO', '通知已发送', { channel: '我的 Telegram', event: 'account.auth' }]
  ]

  let seq = 5000
  const started = Date.now()
  // 补发的历史按时间往前排，最后一条是"刚刚"。
  for (let n = 0; n < seed.length; n++) {
    const [level, msg, attrs] = seed[n]
    handlers.onEntry?.({
      seq: ++seq,
      time: new Date(started - (seed.length - n) * 4000).toISOString(),
      level, msg, attrs
    })
  }
  handlers.onOpen?.(false)

  // 之后每 3 秒来一条，模拟后台在干活。内容在几条之间轮转。
  const live: [string, string, Record<string, string>?][] = [
    ['INFO', '实例同步完成', { instances: '9', durationMs: '1102' }],
    ['INFO', '抢机任务下一轮', { task: '新加坡 ARM 2C12G', attempt: '1874', class: 'OutOfCapacity' }],
    ['INFO', '运维任务推进', { task: '换 IP：东京主号 → 203.0.113.0/24', attempt: '8' }],
    ['WARN', 'Cloudflare 同步失败，换 IP 不受影响', { record: 'home.example.com', status: 'partial' }],
    ['INFO', '凭据解密成功', { account: '东京主号', aad: 'account/acc-tok' }],
    ['INFO', '通知已发送', { channel: '我的 Telegram', event: 'instance.updated' }]
  ]
  let n = 0
  const timer = window.setInterval(() => {
    const [level, msg, attrs] = live[n++ % live.length]
    handlers.onEntry?.({ seq: ++seq, time: new Date().toISOString(), level, msg, attrs })
  }, 3000)

  return () => window.clearInterval(timer)
}
