/**
 * 演示站「本地增强版」接口的冒烟脚本。
 *
 * 演示站是纯浏览器端的，CI 里没有浏览器，所以这里用 esbuild 把
 * `src/demo/local.ts` 单独打包（它只 import 类型，运行时零依赖），
 * 再用一份最小的假数据把每条路由都调一遍，验证：
 *   - 路由都能匹配上（没有手误写错的路径）
 *   - 返回值形状对（前端读的字段都在）
 *   - 状态机、校验分支按预期拒绝
 *
 * 不是替代浏览器测试，是替代「点开页面什么都没有」这一类事故。
 *
 * 跑法：`npm run test:demo`（在 web/ 下）
 */
import { build } from 'esbuild'

const result = await build({
  entryPoints: ['src/demo/local.ts'],
  bundle: true,
  format: 'esm',
  platform: 'neutral',
  write: false,
  logLevel: 'warning'
})
const code = result.outputFiles[0].text
// 走 data URL 而不是落盘：不产生构建产物，也就没有「忘了删」和「忘了 ignore」。
const { registerLocalRoutes, demoLogStream } = await import(
  'data:text/javascript;base64,' + Buffer.from(code).toString('base64')
)

const NOW = Date.now()
const iso = (ms) => new Date(ms).toISOString()
const ago = (ms) => iso(NOW - ms)

/**
 * 本地时区的 `YYYY-MM-DD HH:MM:SS`，与 local.ts 的 csvTime 同口径。
 *
 * 这条断言只在 TZ 不是 UTC 时才有意义（本机 +08:00）：要是哪天有人把
 * csvTime 改成 toISOString，导出时间就会比页面上的早 8 小时。
 */
const csvTimeLocal = (v) => {
  const d = new Date(v)
  const p = (n) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} `
    + `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}
const DAY = 86400000

let seq = 0
const uid = (p) => `${p}${++seq}`
const fail = (status, code, message) => {
  const e = new Error(message)
  e.status = status
  e.code = code
  throw e
}
const log = () => {}

/* 审计流水（新的在前）。导出 CSV 用，形状与 GET /api/audit 返回的一致。
   第一条的 target 故意写成公式、第二条故意带逗号与引号、最后一条的 detail
   以两个空格开头——这三件事 Go 的 csv.Writer 都会特殊处理，得逐一对上。 */
const audit = [
  { id: 4, userId: 'u-demo', action: 'login', accountId: '', target: "=cmd|'/c calc'!A0",
    detail: '', ip: '127.0.0.1', result: 'ok', createdAt: ago(60000) },
  { id: 3, userId: 'u-demo', action: 'account_create', accountId: 'acc-tok', target: '东京主号, 备用',
    detail: '备注里有"引号"', ip: '127.0.0.1', result: 'ok', createdAt: ago(120000) },
  { id: 2, userId: 'u-demo', action: 'account_create', accountId: 'acc-pho', target: '凤凰城试用',
    detail: '', ip: '127.0.0.1', result: 'fail', createdAt: ago(180000) },
  { id: 1, userId: 'u-demo', action: 'note_update', accountId: 'acc-tok', target: '缩进备注',
    detail: '  两个空格开头', ip: '127.0.0.1', result: 'ok', createdAt: ago(240000) }
]

const accounts = [
  mkAccount('acc-tok', '东京主号', 'TOK', 'PAY_AS_YOU_GO', 'ap-tokyo-1'),
  mkAccount('acc-pho', '凤凰城试用', 'PHO', 'FREE_TRIAL', 'us-phoenix-1')
]
function mkAccount(id, alias, code, paymentModel, region) {
  return {
    id, alias, code, colorIndex: 1, tenancyOcid: `t-${id}`, userOcid: `u-${id}`,
    fingerprint: 'de:mo', defaultRegion: region, compartmentOcid: '', enabled: true,
    status: 'ok', statusMessage: '', lastCheckedAt: ago(3600000),
    paymentModel, subscribedRegions: [region], homeRegion: region,
    email: `${code}@example.com`, tenancyName: `demo-${code}`,
    createdAt: ago(200 * DAY), updatedAt: ago(3600000),
    tags: [], capability: null, expiryDays: null,
    meta: { accountId: id, groupName: '', note: '', expiresAt: null, sortWeight: 0, huntAckAt: null, updatedAt: ago(DAY) },
    health: { accountId: id, lastOkAt: ago(3600000), lastErrorClass: '', lastErrorAt: null, lastErrorMessage: '', consecutiveFailures: 0, fatal: false, updatedAt: ago(3600000), level: 'green', label: '正常', advice: '' }
  }
}

const instances = [
  mkInstance('i-tok-web', 'acc-tok', 'ap-tokyo-1', 'tok-web', 'VM.Standard.A1.Flex', 4, 24, 'RUNNING', 11),
  mkInstance('i-tok-micro', 'acc-tok', 'ap-tokyo-1', 'tok-relay', 'VM.Standard.E2.1.Micro', 1, 1, 'RUNNING', 12),
  mkInstance('i-pho-lab', 'acc-pho', 'us-phoenix-1', 'pho-lab', 'VM.Standard.E4.Flex', 1, 8, 'RUNNING', 13)
]
function mkInstance(id, accountId, region, name, shape, ocpus, memoryGb, state, ipTail) {
  return {
    dto: {
      id, accountId, region, compartmentId: `c-${accountId}`, displayName: name,
      availabilityDomain: 'AD-1', faultDomain: 'FD-1', shape, ocpus, memoryGb,
      baselineOcpuUtilization: '', lifecycleState: state, imageId: 'img', imageOs: 'Ubuntu',
      publicIp: '203.0.113.' + ipTail, privateIp: '10.0.0.5',
      ipv6: '', secondaryIpv4: '', secondaryIpv6: '', vnicId: `v-${id}`, subnetId: 's-1',
      bootVolumeId: `bv-${id}`, bootVolumeGb: 50, bootVolumeVpus: 10,
      timeCreated: ago(30 * DAY), runningSince: ago(DAY), note: '', syncedAt: ago(60000),
      lastError: '', accountAlias: '', accountCode: '', accountColorIndex: 1
    },
    vnics: [], blockVolumes: []
  }
}

const nets = [{ accountId: 'acc-tok', region: 'ap-tokyo-1', vcn: { id: 'vcn-1' }, subnets: [] }]
const reservedIps = [{ accountId: 'acc-tok', ip: { ipAddress: '203.0.113.99' } }]

/* ---------- 路由表 ---------- */

const routes = []
const on = (method, pattern, h) => {
  const keys = []
  const re = new RegExp('^' + pattern.replace(/\{(\w+)\}/g, (_, k) => { keys.push(k); return '([^/]+)' }) + '$')
  routes.push([method, re, keys, h])
  // 与 server.ts 一致：静态段优先。
  routes.sort((a, b) => a[2].length - b[2].length)
}

registerLocalRoutes({
  on, accounts, instances, nets, reservedIps, log, fail, uid, iso, ago,
  auditEntries: () => audit
})

function call(method, path, query = {}, body = {}) {
  for (const [m, re, keys, h] of routes) {
    if (m !== method) continue
    const hit = re.exec(path)
    if (!hit) continue
    const params = {}
    keys.forEach((k, n) => { params[k] = decodeURIComponent(hit[n + 1]) })
    return structuredClone(h(params, query, body))
  }
  throw new Error(`没有匹配的路由：${method} ${path}`)
}

function expectError(method, path, query, body, status) {
  try {
    call(method, path, query, body)
  } catch (err) {
    if (err.status !== status) throw new Error(`${method} ${path} 期望 ${status}，实际 ${err.status}：${err.message}`)
    return err
  }
  throw new Error(`${method} ${path} 期望报错 ${status}，却成功了`)
}

/* ---------- 断言 ---------- */

let checked = 0
function ok(name, cond, extra) {
  checked++
  if (!cond) throw new Error(`✗ ${name}${extra ? ' — ' + JSON.stringify(extra) : ''}`)
  console.log(`  ✓ ${name}`)
}

console.log('账号组织')
const tags = call('GET', '/api/accounts/tags')
ok('标签用量是数组', Array.isArray(tags.tags))
const put = call('PUT', '/api/accounts/acc-tok/tags', {}, { tags: [' 生产 ', '生产', '华东'] })
ok('标签去重去空白后排序', JSON.stringify(put.tags) === JSON.stringify(['华东', '生产']), put.tags)
ok('账号上的标签同步更新', JSON.stringify(accounts[0].tags) === JSON.stringify(['华东', '生产']))
ok('标签用量反映新标签', call('GET', '/api/accounts/tags').tags.length === 2)
expectError('PUT', '/api/accounts/acc-tok/tags', {}, { tags: ['x'.repeat(40)] }, 400)

const meta = call('PUT', '/api/accounts/acc-tok/meta', {}, {
  groupName: '主力', note: '备注', expiresAt: '2026-12-31', sortWeight: 10, huntAckAt: null
})
ok('附加信息落库', meta.meta.groupName === '主力' && meta.meta.sortWeight === 10)
ok('到期天数算出来了', typeof accounts[0].expiryDays === 'number' && accounts[0].expiryDays > 0, accounts[0].expiryDays)
expectError('PUT', '/api/accounts/acc-tok/meta', {}, { expiresAt: '不是时间' }, 400)

const health = call('GET', '/api/accounts/acc-tok/health')
ok('健康度带等级与文案', health.level === 'green' && health.label === '正常')
const cleared = call('DELETE', '/api/accounts/acc-tok/health')
ok('清除后回到未校验', cleared.level === 'unknown' && cleared.label === '尚未校验')

console.log('权限自检')
const noProbe = call('GET', '/api/accounts/acc-tok/capability-probe')
ok('未自检时 probe 为 null 且下发五族清单', noProbe.probe === null && noProbe.specs.length === 5)
const probe = call('POST', '/api/accounts/acc-tok/capability-probe').probe
ok('自检产出五条', probe.items.length === 5)
ok('实例族数量取自实例缓存', probe.items.find(i => i.key === 'compute').count === 2)
const denied = call('POST', '/api/accounts/acc-pho/capability-probe').probe
ok('缺权限的族标 denied 并给出策略', denied.items.find(i => i.key === 'usage').denied === true &&
  !!denied.items.find(i => i.key === 'usage').policy)
ok('摘要写明几族可用', denied.summary.includes('/5'))
ok('自检结果可再读回', call('GET', '/api/accounts/acc-pho/capability-probe').probe.items.length === 5)

console.log('计费风险')
const risk = call('GET', '/api/accounts/acc-tok/billing-risk')
ok('保留 IP 被识别为在计费', risk.items.some(i => i.key === 'reserved_public_ip' && i.severity === 'billing'))
ok('金额大于 0 且有估算依据', risk.totalDailyCny > 0 && risk.items.every(i => i.basis))
ok('月额是日额的 30 倍', Math.abs(risk.totalMonthlyCny - risk.totalDailyCny * 30) < 0.02)
ok('说明里标了「不是账单」', risk.notice.includes('不是账单'))

console.log('策略模板')
const tpl = call('GET', '/api/iam/policy-template', { group: 'oci-readers' })
ok('8 条策略', tpl.lines.length === 8 && tpl.readOnlyCount === 5)
ok('组名替换进语句', tpl.lines[0].text === 'Allow group oci-readers to read instances in tenancy', tpl.lines[0].text)
const tplBad = call('GET', '/api/iam/policy-template', { group: 'a"b;c' })
ok('非法组名退回占位符', tplBad.group === '<你的组>', tplBad.group)

console.log('备份')
ok('初始没有待生效的恢复', call('GET', '/api/backup').pending === false)
const blob = call('POST', '/api/backup/export', {}, { password: 'longenough', includeMasterKey: false })
ok('导出返回 Blob', blob instanceof Blob && blob.size > 100, blob?.constructor?.name)
expectError('POST', '/api/backup/export', {}, { password: 'short' }, 400)
const form = new FormData()
form.append('password', 'longenough')
form.append('file', new Blob(['x']), 'a.ocibak')
const restored = call('POST', '/api/backup/restore', {}, form)
ok('恢复要求重启生效', restored.requiresRestart === true)
ok('状态接口能看到待生效的恢复', call('GET', '/api/backup').pending === true)
expectError('POST', '/api/backup/restore', {}, new FormData(), 400)

console.log('Cloudflare')
const cfList = call('GET', '/api/cf/zones')
ok('列表带 zones / records / notice', cfList.zones.length === 1 && cfList.records.length === 1 && !!cfList.notice)
const zone = call('POST', '/api/cf/zones', {}, { zone: 'https://new.example.net/path', token: 'tok' }).zone
ok('从控制台地址里解析出域名', zone.zoneName === 'new.example.net', zone.zoneName)
expectError('POST', '/api/cf/zones', {}, { zone: 'new.example.net', token: 'tok' }, 409)
const remote = call('GET', `/api/cf/zones/${zone.id}/records`).records
ok('远程记录带 bound 标记', remote.length === 3 && remote.every(r => typeof r.bound === 'boolean'))
const rec = call('POST', '/api/cf/records', {}, {
  zonePk: zone.id, recordId: 'rec-new', name: 'a.new.example.net', instanceId: 'i-tok-web'
}).record
ok('新建绑定带上实例当前 IP', rec.lastIp === instances[0].dto.publicIp, rec.lastIp)
const sync = call('POST', `/api/cf/records/${rec.id}/sync`)
ok('同步返回 updated', sync.status === 'updated' && !!sync.ip)
expectError('POST', '/api/cf/records', {}, { zonePk: zone.id, recordId: 'rec-new', name: 'x', instanceId: 'i-tok-web' }, 409)
ok('删除绑定', call('DELETE', `/api/cf/records/${rec.id}`).ok === true)

console.log('IP 分布')
const geoStatus = call('GET', '/api/geo/status')
ok('状态说明为什么没有离线库', geoStatus.status.enabled === false && !!geoStatus.status.notice)
const dist = call('GET', '/api/geo/distribution')
ok('每台有公网 IP 的实例都落点', dist.points.length === instances.length)
ok('没有离线库时按区域估算并标 region', dist.points.every(p => p.found && p.source === 'region'))
ok('同一区域的多台机器坐标相同', (() => {
  const tok = dist.points.filter(p => p.region === 'ap-tokyo-1')
  return tok.length === 2 && tok[0].latitude === tok[1].latitude
})())
const manual = call('PUT', '/api/geo/manual', {}, {
  ip: instances[0].dto.publicIp, country: '中国', city: '上海', latitude: 31.23, longitude: 121.47
})
ok('手填位置写入成功', manual.info.source === 'manual')
const afterManual = call('GET', '/api/geo/distribution').points.find(p => p.ip === instances[0].dto.publicIp)
ok('手填覆盖区域估算', afterManual.source === 'manual' && afterManual.city === '上海')
ok('撤销手填', call('DELETE', `/api/geo/manual/${encodeURIComponent(instances[0].dto.publicIp)}`).ok === true)

console.log('运维任务')
const list = call('GET', '/api/tasks')
ok('种子任务覆盖四种状态', new Set(list.tasks.map(t => t.state)).size >= 4, list.tasks.map(t => t.state))
ok('状态清单随列表下发', list.states.length === 6)
ok('任务带账号别名与类型中文名', list.tasks.every(t => t.accountAlias && t.kindLabel))
ok('按账号筛选生效', call('GET', '/api/tasks', { accountId: 'acc-pho' }).tasks.length === 0)
const running = list.tasks.find(t => t.state === 'running')
const paused = call('POST', `/api/tasks/${running.id}/pause`)
ok('暂停返回说明', paused.task.state === 'paused' && paused.notice.includes('暂停'))
// 重复暂停是合法（同状态同状态），与 Go 侧 CanTransitionOps 的口径一致。
ok('重复暂停不报错', call('POST', `/api/tasks/${running.id}/pause`).task.state === 'paused')
const resumed = call('POST', `/api/tasks/${running.id}/resume`)
ok('恢复回到 running 并给出下次调度', resumed.task.state === 'running' && !!resumed.task.nextAt)
const failed = list.tasks.find(t => t.state === 'failed')
ok('失败任务可重试', call('POST', `/api/tasks/${failed.id}/retry`).task.state === 'pending')
expectError('POST', '/api/tasks/nope/pause', {}, {}, 404)
const done = list.tasks.find(t => t.state === 'succeeded')
expectError('POST', `/api/tasks/${done.id}/pause`, {}, {}, 409)
ok('终态任务可删除', call('DELETE', `/api/tasks/${done.id}`).ok === true)
expectError('DELETE', `/api/tasks/${running.id}`, {}, {}, 409)

console.log('运维作业创建')
const changeIp = call('POST', '/api/ops/change-ip', {}, {
  instanceId: 'i-tok-web', targetCidrs: ['203.0.113.0/24'], maxAttempts: 5, maxDurationMinutes: 10
})
ok('换 IP 任务创建成功', changeIp.task.kind === 'change_ip_cidr' && changeIp.task.maxAttempts === 5)
ok('创建说明提到 SSH 会中断', changeIp.notice.includes('SSH'))
expectError('POST', '/api/ops/change-ip', {}, { instanceId: 'i-tok-web', targetCidrs: [] }, 400)
const openPorts = call('POST', '/api/ops/open-ports', {}, {
  accountId: 'acc-tok', region: 'ap-tokyo-1', securityListId: 'sl-1',
  source: '0.0.0.0/0', rules: [{ protocol: '6', port: 8080 }]
})
ok('放行端口任务名带端口', openPorts.task.name.includes('tcp/8080'), openPorts.task.name)
const bw = call('POST', '/api/ops/bandwidth', {}, { accountId: 'acc-tok', instanceId: 'i-tok-web' })
ok('带宽任务带阶段清单', bw.stages.length === 7 && bw.stages[0] === 'PRECHECK')
const bwOff = call('POST', '/api/ops/bandwidth', {}, { accountId: 'acc-tok', instanceId: 'i-tok-web', action: 'disable' })
ok('关闭流程按逆序', bwOff.stages[0] === 'DISCOVER' && bwOff.stages.at(-1) === 'CLEANED')
const rescue = call('POST', '/api/ops/rescue', {}, {
  accountId: 'acc-tok', targetInstanceId: 'i-tok-web', rescueInstanceId: 'i-tok-micro'
})
ok('救援任务带九阶段', rescue.stages.length === 9)
expectError('POST', '/api/ops/rescue', {}, {
  accountId: 'acc-tok', targetInstanceId: 'i-tok-micro', rescueInstanceId: 'i-tok-micro'
}, 400)

console.log('调用预算')
const budget = call('GET', '/api/risk-budget')
ok('预算已启用并带窗口', budget.enabled === true && !!budget.window)
ok('每个账号一条', budget.accounts.length === accounts.length)
ok('有降频的账号倍数为 4', budget.accounts.some(a => a.multiplier === 4))

console.log('审计导出')
const csvBlob = call('GET', '/api/audit/export')
ok('导出返回 CSV Blob', csvBlob instanceof Blob && csvBlob.type.startsWith('text/csv'), csvBlob?.type)
// 查原始字节而不是 blob.text()：TextDecoder 默认会把 BOM 吃掉，
// 但 Excel 看的是文件里那几个字节，必须在。
const head = new Uint8Array(await csvBlob.arrayBuffer()).subarray(0, 3)
ok('开头带 BOM（Excel 不认无 BOM 的 UTF-8）', head[0] === 0xef && head[1] === 0xbb && head[2] === 0xbf, [...head])
const csvRows = (await csvBlob.text()).trimEnd().split('\n')
ok('表头与后端逐字一致', csvRows[0] === '时间,动作,账号,目标资源,详情,来源 IP,结果', csvRows[0])
ok('每条审计一行', csvRows.length === audit.length + 1, csvRows.length)
ok('时间格式是 YYYY-MM-DD HH:MM:SS', /^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2},/.test(csvRows[1]), csvRows[1])
// 公式注入：target 来自请求体里的用户名，未认证即可写入，必须中和。
ok('公式开头的单元格加了前导单引号', csvRows[1].split(',')[3] === "'=cmd|'/c calc'!A0", csvRows[1])
ok('含逗号的字段被引号包住', csvRows[2].includes('"东京主号, 备用"'), csvRows[2])
ok('字段内的引号翻倍', csvRows[2].includes('"备注里有""引号"""'), csvRows[2])
// Go 的 fieldNeedsQuotes 结尾是 unicode.IsSpace(r1)：以空白开头也要加引号。
ok('以空格开头的字段加引号', csvRows[4].includes('"  两个空格开头"'), csvRows[4])
ok('时间按本地时区渲染，不是 UTC', csvRows[1].startsWith(csvTimeLocal(audit[0].createdAt)), csvRows[1])
ok('按账号筛选生效', (await call('GET', '/api/audit/export', { accountId: 'acc-pho' }).text())
  .trimEnd().split('\n').length === 2)
ok('按动作筛选生效', (await call('GET', '/api/audit/export', { action: 'login' }).text())
  .trimEnd().split('\n').length === 2)

console.log(`\n全部通过：${checked} 项断言，${routes.length} 条路由`)

console.log('运行日志流')
// demoLogStream 用 window.setInterval 驱动，Node 里补一个最小替身。
globalThis.window = { setInterval, clearInterval }
const got = []
let opened = false
const stop = demoLogStream({ onEntry: e => got.push(e), onOpen: () => { opened = true } })
ok('连接建立时回调 onOpen', opened)
ok('先补发历史日志', got.length >= 10, got.length)
ok('日志带序号/时间/级别/正文', got.every(e => e.seq && e.time && e.level && e.msg))
ok('级别只有 INFO/WARN/ERROR', got.every(e => ['INFO', 'WARN', 'ERROR'].includes(e.level)))
ok('历史按时间递增', got.every((e, i) => i === 0 || Date.parse(e.time) >= Date.parse(got[i - 1].time)))
stop()
const before = got.length
await new Promise(r => setTimeout(r, 60))
ok('断开后不再推送', got.length === before)
