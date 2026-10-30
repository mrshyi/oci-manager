<script setup lang="ts">
/**
 * 运维任务（opssvc）。
 *
 * 和「任务」页（jobsvc）是两套东西，刻意分成两页而不是合并：
 *
 *   jobsvc   一次性的长操作（创建实例、换一次 IP）。status 只有
 *            queued/running/succeeded/failed/cancelled，进度是 0–100 的数，
 *            取消即止，SSE 实时推。
 *
 *   opssvc   可重试、可暂停恢复、可能跑几十轮的作业（换 IP 到命中网段、
 *            救援编排、批量）。多出 paused、attempts/maxAttempts、
 *            nextAt（下一轮什么时候跑）、dnsStatus（换 IP 顺带更新域名）。
 *
 * 硬塞进 JobDTO 会丢掉「第 3/30 轮」「下次重试还有 12 秒」这些恰恰是用户
 * 最需要看的东西，paused 也无处安放。
 *
 * 数据靠轮询而不是 SSE：opssvc 没有事件推送，且任务本身就是 20 秒一轮的
 * 节奏，秒级实时没有意义。轮询间隔同时充当倒计时的刷新节拍。
 */
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useStore } from '@/store'
import { opsTasks, type OpsTaskAction } from '@/api/opsTasks'
import { errorText } from '@/api/http'
import type { OpsTaskDTO, OpsTaskState } from '@/api/types'
import { relativeTime } from '@/lib/adapt'
import SectionCard from '@/components/SectionCard.vue'
import EmptyState from '@/components/EmptyState.vue'
import SkeletonRows from '@/components/SkeletonRows.vue'
import AccountChip from '@/components/AccountChip.vue'

const { state, accountById, openDrawer, ask, toast, toastError } = useStore()

/** 轮询间隔。opssvc 一轮最短 20 秒，5 秒足够跟上进度又不至于打满后端 */
const POLL_MS = 5000

const tasks = ref<OpsTaskDTO[]>([])
const loaded = ref(false)
const loadErr = ref('')
const busyId = ref('')

type Filter = 'all' | 'active' | 'paused' | 'bad' | 'done'
const filter = ref<Filter>('all')

const STATE_LABEL: Record<OpsTaskState, string> = {
  pending: '排队中', running: '进行中', paused: '已暂停',
  succeeded: '已完成', failed: '失败', canceled: '已取消'
}
const STATE_TONE: Record<OpsTaskState, string> = {
  pending: 'var(--text-tertiary)', running: 'var(--accent)', paused: 'var(--warning)',
  succeeded: 'var(--success)', failed: 'var(--danger)', canceled: 'var(--text-tertiary)'
}

const KIND_ICON: Record<string, string> = {
  change_ip_cidr: '⇄', open_ports: '⇥', enable_500m: '⇈', rescue: '✚', bulk: '▤'
}

/**
 * 阶段名转中文。
 *
 * 阶段名是运维语义（"正在把引导卷挂到救援机"），原样显示 PRECHECK 之类的
 * 英文对排障没有帮助。救援与带宽共用 PRECHECK / DONE 两个名字，含义一致。
 */
const STAGE_LABEL: Record<string, string> = {
  PRECHECK: '前置检查',
  STOPPING: '关机中', DETACHING: '卸载引导卷', ATTACHING: '挂到救援机',
  READY_FOR_REPAIR: '等待修复', DETACHING_RESCUE: '从救援机卸下',
  REATTACHING: '挂回原机', STARTING: '启动原机',
  DONE: '完成', FAILED: '失败',
  NAT_CREATING: '创建 NAT 网关', ROUTE_SWITCHING: '切换默认路由',
  NLB_CREATING: '创建负载均衡', BACKEND_SET_CREATING: '创建后端集',
  LISTENER_CREATING: '创建监听器', DISCOVER: '盘点现有资源',
  LISTENER_DELETING: '删除监听器', BACKEND_SET_DELETING: '删除后端集',
  NLB_DELETING: '删除负载均衡', ROUTE_RESTORING: '还原路由',
  NAT_DELETING: '删除 NAT 网关', CLEANED: '已清理'
}

const DNS_LABEL: Record<string, string> = {
  updated: '域名已指向新 IP', failed: '域名未更新', partial: '部分域名未更新'
}
const DNS_TONE: Record<string, string> = {
  updated: 'var(--success)', failed: 'var(--danger)', partial: 'var(--warning)'
}

const ACTION_FAIL: Record<OpsTaskAction, string> = {
  pause: '暂停失败', resume: '恢复失败', cancel: '取消失败', retry: '重试失败'
}

const isActive = (t: OpsTaskDTO) => t.state === 'pending' || t.state === 'running'
const isBad = (t: OpsTaskDTO) => t.state === 'failed' || t.state === 'canceled'
/** 状态机允许删除的：终态，以及 paused / failed（后两者要先删再重建的场景很常见） */
const canRemove = (t: OpsTaskDTO) => t.state !== 'pending' && t.state !== 'running'

const counts = computed(() => ({
  all: tasks.value.length,
  active: tasks.value.filter(isActive).length,
  paused: tasks.value.filter(t => t.state === 'paused').length,
  bad: tasks.value.filter(isBad).length,
  done: tasks.value.filter(t => t.state === 'succeeded').length
}))

const list = computed(() => tasks.value.filter(t => {
  switch (filter.value) {
    case 'active': return isActive(t)
    case 'paused': return t.state === 'paused'
    case 'bad': return isBad(t)
    case 'done': return t.state === 'succeeded'
    default: return true
  }
}))

/* ---------- 轮询 ---------- */

let timer: number | undefined
let inFlight = false

async function load(): Promise<void> {
  if (inFlight) return
  inFlight = true
  try {
    const res = await opsTasks.list()
    tasks.value = res.tasks
    loadErr.value = ''
  } catch (err) {
    // 轮询失败不弹 toast：每 5 秒弹一次会把整页盖住。错误留在页面顶部，
    // 想立刻重试就点「刷新」。
    loadErr.value = errorText(err)
  } finally {
    inFlight = false
    loaded.value = true
  }
}

/** 切回前台立刻补一次：后台标签页里跳过的轮询不能让它显示半小时前的状态 */
function onVisible(): void {
  if (!document.hidden) void load()
}

onMounted(() => {
  void load()
  timer = window.setInterval(() => {
    if (!document.hidden) void load()
  }, POLL_MS)
  document.addEventListener('visibilitychange', onVisible)
})

onUnmounted(() => {
  if (timer !== undefined) window.clearInterval(timer)
  document.removeEventListener('visibilitychange', onVisible)
})

/* ---------- 展示 ---------- */

/** 展开的任务。进行中的默认展开——那正是用户点进来想看的。 */
const expanded = ref(new Set<string>())
const collapsed = ref(new Set<string>())
function isOpen(t: OpsTaskDTO): boolean {
  if (isActive(t)) return !collapsed.value.has(t.id)
  return expanded.value.has(t.id)
}
function toggle(t: OpsTaskDTO): void {
  const set = isActive(t) ? collapsed.value : expanded.value
  if (set.has(t.id)) set.delete(t.id)
  else set.add(t.id)
}

/** 已用时长。进行中的读当前时刻，结束的用结束时刻。 */
function duration(t: OpsTaskDTO): string {
  if (!t.startedAt || t.startedAt.startsWith('0001')) return ''
  const end = t.finishedAt && !t.finishedAt.startsWith('0001')
    ? Date.parse(t.finishedAt)
    : Date.now()
  const sec = Math.max(0, Math.round((end - Date.parse(t.startedAt)) / 1000))
  if (sec < 60) return `${sec} 秒`
  const min = Math.floor(sec / 60)
  if (min < 60) return `${min} 分 ${sec % 60} 秒`
  return `${Math.floor(min / 60)} 小时 ${min % 60} 分`
}

/**
 * 距下次调度还有几秒。
 *
 * 直接读 Date.now() 而不是全局时钟：全局时钟 30 秒一跳，倒计时会一次跳 30；
 * 这里的重算由 5 秒一次的轮询驱动，够用。
 */
function secondsUntil(iso: string): number {
  if (!iso || iso.startsWith('0001')) return 0
  return Math.max(0, Math.round((Date.parse(iso) - Date.now()) / 1000))
}

/** 是否正在等下一轮（而不是正在干活） */
function waitingNext(t: OpsTaskDTO): boolean {
  return isActive(t) && secondsUntil(t.nextAt) > 0
}

/** 待调度说明。一次都没跑过的任务说「开始」，「下一轮」只对已跑过轮的才成立 */
function waitText(t: OpsTaskDTO): string {
  const s = secondsUntil(t.nextAt)
  return t.attempts === 0 ? `${s} 秒后开始` : `${s} 秒后下一轮`
}

const str = (v: unknown): string => (typeof v === 'string' ? v : '')
const num = (v: unknown): number => (typeof v === 'number' ? v : 0)
const arr = (v: unknown): unknown[] => (Array.isArray(v) ? v : [])

/**
 * 每种任务的进展摘要。
 *
 * progress 是结构化 JSON，形状随 kind 而定（换 IP 记新旧地址，批量记台数，
 * 救援/带宽记阶段与说明）。这里按 kind 取最有用的那一两个字段，取不到就
 * 什么都不显示——比把原始 JSON 摊在界面上强。
 */
function progressLine(t: OpsTaskDTO): string {
  const p: Record<string, unknown> = t.progress
  if (t.kind === 'change_ip_cidr') {
    const newIP = str(p.newIp)
    if (!newIP) return ''
    const oldIP = str(p.oldIp) || '—'
    const n = num(p.attempt)
    return n ? `第 ${n} 轮：${oldIP} → ${newIP}` : `${oldIP} → ${newIP}`
  }
  if (t.kind === 'bulk') {
    const total = num(p.total)
    if (!total) return ''
    const ok = num(p.succeeded)
    const pending = arr(p.pending).length
    return `已成功 ${ok}/${total} 台${pending ? `，还有 ${pending} 台待处理` : ''}`
  }
  return str(p.note)
}

function stageLabel(t: OpsTaskDTO): string {
  if (!t.stage) return ''
  return STAGE_LABEL[t.stage] ?? t.stage
}

function instanceName(t: OpsTaskDTO): string {
  return state.instances.find(i => i.id === t.instanceId)?.name ?? ''
}

function canOpenInstance(t: OpsTaskDTO): boolean {
  return !!t.instanceId && state.instances.some(i => i.id === t.instanceId)
}

/* ---------- 动作 ---------- */

function replace(task: OpsTaskDTO): void {
  tasks.value = tasks.value.map(t => (t.id === task.id ? task : t))
}

async function run(t: OpsTaskDTO, action: OpsTaskAction): Promise<void> {
  if (busyId.value) return
  busyId.value = t.id
  try {
    const { task, notice } = await opsTasks.action(t.id, action)
    replace(task)
    toast({
      tone: action === 'cancel' ? 'warning' : 'accent',
      title: notice || `${STATE_LABEL[task.state]}`
    }, 9000)
  } catch (err) {
    // 409 是「状态刚被别人改过」。这时最有用的一句话是刷新，而不是让
    // 用户去怀疑自己点错了。
    toastError(ACTION_FAIL[action], err)
  } finally {
    busyId.value = ''
    void load()
  }
}

function askCancel(t: OpsTaskDTO): void {
  ask({
    level: 2,
    title: `取消任务：${t.name}`,
    body:
      '已经发往 Oracle 的请求不会被回滚。如果这一轮正在删建 IP 或改路由，' +
      '云端会停在中间状态，需要你到对应页面核对。',
    okLabel: '取消任务',
    onConfirm: () => { void run(t, 'cancel') }
  })
}

function askRetry(t: OpsTaskDTO): void {
  ask({
    level: 2,
    title: `重试任务：${t.name}`,
    body:
      `会从上次停下的阶段（${stageLabel(t) || '未知'}）继续，不会从头重做。\n\n` +
      '如果失败原因是云端条件没满足（配额、容量、权限），重试前请先确认那个条件已经变了。',
    okLabel: '重新排队',
    onConfirm: () => { void run(t, 'retry') }
  })
}

function askRemove(t: OpsTaskDTO): void {
  ask({
    level: 2,
    title: '删除任务记录',
    body: `将从列表中移除「${t.name}」。云端已经发生的变更不受影响，操作本身已记在审计日志里。`,
    okLabel: '删除',
    onConfirm: () => { void doRemove(t) }
  })
}

async function doRemove(t: OpsTaskDTO): Promise<void> {
  if (busyId.value) return
  busyId.value = t.id
  try {
    const { notice } = await opsTasks.remove(t.id)
    tasks.value = tasks.value.filter(x => x.id !== t.id)
    toast({ tone: 'accent', title: notice || '记录已删除' }, 8000)
  } catch (err) {
    toastError('删除失败', err)
  } finally {
    busyId.value = ''
    void load()
  }
}
</script>

<template>
  <div class="page">
    <header class="page-head">
      <div>
        <h1 class="page-title">运维任务</h1>
        <p class="page-sub">
          可重试、可暂停的长时间作业 ·
          {{ counts.active }} 个进行中，{{ counts.paused }} 个已暂停，共 {{ counts.all }} 条
        </p>
      </div>
      <div class="head-actions">
        <button class="btn" @click="load()">刷新</button>
      </div>
    </header>

    <p v-if="loadErr" class="page-err">
      读取任务列表失败：{{ loadErr }}
    </p>

    <div class="filters" role="tablist">
      <button v-for="f in ([
        ['all', '全部'], ['active', '进行中'], ['paused', '已暂停'],
        ['bad', '失败或取消'], ['done', '已完成']
      ] as [Filter, string][])" :key="f[0]" role="tab"
              class="filter" :class="{ 'is-on': filter === f[0] }" :aria-selected="filter === f[0]"
              @click="filter = f[0]">
        {{ f[1] }} <span class="mono filter__n">{{ counts[f[0]] }}</span>
      </button>
    </div>

    <SectionCard title="任务列表" note="每 5 秒自动刷新">
      <SkeletonRows v-if="!loaded" :rows="3" :height="52" />
      <EmptyState v-else-if="list.length === 0" icon="⚒"
                  :title="tasks.length === 0 ? '还没有运维任务' : '这个分类下没有任务'"
                  sub="在实例详情的「运维」页签里可以发起按网段换 IP。其他运维作业尚未接入界面。" />

      <div v-for="(t, n) in list" :key="t.id" class="task reveal" :class="{ 'is-active': isActive(t) }"
           :style="{ '--i': Math.min(n, 8) }">
        <button class="task__head" :aria-expanded="isOpen(t)" @click="toggle(t)">
          <span class="task__icon" :style="{ color: STATE_TONE[t.state] }">
            <span v-if="t.state === 'running'" class="task__spin" />
            <template v-else>{{ KIND_ICON[t.kind] ?? '⚒' }}</template>
          </span>

          <span class="task__main">
            <span class="task__line1">
              <span class="task__title">{{ t.name }}</span>
              <AccountChip v-if="t.accountId" :account="accountById(t.accountId)" />
              <span v-if="instanceName(t)" class="t-2xs dim-3">{{ instanceName(t) }}</span>
            </span>
            <span class="task__line2">
              <span class="t-2xs task__state" :style="{ color: STATE_TONE[t.state] }">
                ● {{ STATE_LABEL[t.state] }}
              </span>
              <span v-if="t.maxAttempts > 0" class="mono t-2xs dim-3">
                第 {{ t.attempts }}/{{ t.maxAttempts }} 轮
              </span>
              <span v-if="waitingNext(t)" class="mono t-2xs dim-3">
                {{ waitText(t) }}
              </span>
              <span v-else-if="t.startedAt && !t.startedAt.startsWith('0001')" class="mono t-2xs dim-3">
                用时 {{ duration(t) }}
              </span>
              <span class="mono t-2xs dim-3">{{ relativeTime(t.createdAt) }}创建</span>
              <span v-if="stageLabel(t) && t.state === 'paused'" class="t-2xs task__stage">
                停在「{{ stageLabel(t) }}」
              </span>
              <!-- 进行中时这里是「上一轮为什么没成」，不是失败；两种语气必须分开 -->
              <span v-if="t.lastError" class="t-2xs task__note"
                    :class="{ 'is-err': t.state === 'failed' }" :title="t.lastError">
                {{ t.lastError }}
              </span>
            </span>
          </span>

          <span class="task__right">
            <span v-if="busyId === t.id" class="task__spin" />
            <span class="task__chev" :class="{ 'is-open': isOpen(t) }">›</span>
          </span>
        </button>

        <div v-if="isOpen(t)" class="task__body">
          <p v-if="progressLine(t)" class="task__prog mono">{{ progressLine(t) }}</p>

          <dl class="meta">
            <dt>类型</dt>
            <dd>{{ t.kindLabel }}</dd>
            <dt>任务 ID</dt>
            <dd class="mono">{{ t.id }}</dd>
            <template v-if="stageLabel(t)">
              <dt>阶段</dt>
              <dd>{{ stageLabel(t) }}</dd>
            </template>
            <template v-if="t.intervalSeconds > 0">
              <dt>轮次间隔</dt>
              <dd class="mono">{{ t.intervalSeconds }} 秒</dd>
            </template>
            <template v-if="t.startedAt && !t.startedAt.startsWith('0001')">
              <dt>开始于</dt>
              <dd>{{ new Date(t.startedAt).toLocaleString('zh-CN', { hour12: false }) }}</dd>
            </template>
            <template v-if="t.finishedAt && !t.finishedAt.startsWith('0001')">
              <dt>结束于</dt>
              <dd>{{ new Date(t.finishedAt).toLocaleString('zh-CN', { hour12: false }) }}</dd>
            </template>
            <template v-if="t.expiresAt && !t.expiresAt.startsWith('0001')">
              <dt>超时时刻</dt>
              <dd>{{ new Date(t.expiresAt).toLocaleString('zh-CN', { hour12: false }) }}</dd>
            </template>
          </dl>

          <p v-if="t.result" class="task__result">{{ t.result }}</p>

          <!--
            lastError 的两种含义必须分开展示：failed 时它是失败原因；
            进行中时它是「上一轮为什么没成」（换 IP 没命中目标网段、被限流退避），
            那是常态。用同一个红框会把正常推进的任务画成出错。
          -->
          <p v-if="t.lastError && t.state === 'failed'" class="task__errbox">
            {{ t.lastError }}
          </p>
          <p v-else-if="t.lastError" class="task__notebox">
            <span class="task__notek">上一轮</span>{{ t.lastError }}
          </p>

          <p v-if="t.dnsStatus" class="task__dns" :style="{ color: DNS_TONE[t.dnsStatus] }">
            {{ DNS_LABEL[t.dnsStatus] ?? t.dnsStatus }}
            <span v-if="t.dnsError" class="dim-3">· {{ t.dnsError }}</span>
          </p>

          <div class="task__actions">
            <button v-if="canOpenInstance(t)" class="btn btn--sm"
                    @click="openDrawer({ kind: 'instance', id: t.instanceId })">查看实例</button>
            <button v-if="t.state === 'running'" class="btn btn--sm" :disabled="!!busyId"
                    @click="run(t, 'pause')">暂停</button>
            <button v-if="t.state === 'paused'" class="btn btn--sm" :disabled="!!busyId"
                    @click="run(t, 'resume')">继续</button>
            <button v-if="t.state === 'failed'" class="btn btn--sm" :disabled="!!busyId"
                    @click="askRetry(t)">重试</button>
            <button v-if="isActive(t) || t.state === 'paused'" class="btn btn--sm btn--ghost"
                    :disabled="!!busyId" @click="askCancel(t)">取消</button>
            <button v-if="canRemove(t)" class="btn btn--sm btn--ghost"
                    :disabled="!!busyId" @click="askRemove(t)">删除记录</button>
          </div>
        </div>
      </div>
    </SectionCard>
  </div>
</template>

<style scoped>
.page-err {
  margin: 0 0 14px; padding: 10px 12px; border-radius: var(--radius-md);
  border: 1px solid var(--danger); color: var(--danger); font-size: 12px;
  white-space: pre-line; word-break: break-word;
}

.filters { display: flex; gap: 6px; margin-bottom: 14px; flex-wrap: wrap; }
.filter {
  height: 30px; padding: 0 12px; border-radius: var(--radius-full);
  border: 1px solid var(--border-subtle); background: transparent;
  color: var(--text-secondary); font-size: 12px; cursor: pointer;
  display: inline-flex; align-items: center; gap: 6px;
}
.filter:hover { border-color: var(--border-strong); color: var(--text-primary); }
.filter.is-on { border-color: var(--accent); color: var(--text-primary); background: var(--bg-hover); }
.filter__n { font-size: 11px; color: var(--text-tertiary); }

.task { border-bottom: 1px solid var(--border-subtle); }
.task:last-child { border-bottom: none; }

.task__head {
  width: 100%; display: flex; align-items: flex-start; gap: 12px; padding: 12px 16px;
  border: 0; background: transparent; color: inherit; text-align: left; cursor: pointer;
}
.task__head:hover { background: var(--bg-hover); }
.task__head:focus-visible { outline: 2px solid var(--accent); outline-offset: -2px; }

.task__icon {
  width: 26px; height: 26px; flex: 0 0 auto; display: flex; align-items: center; justify-content: center;
  border-radius: var(--radius-md); border: 1px solid var(--border-subtle); font-size: 13px;
}
.task__spin {
  width: 12px; height: 12px; border-radius: var(--radius-full);
  border: 2px solid var(--accent); border-right-color: transparent;
  animation: spin 0.9s linear infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }

.task__main { flex: 1 1 auto; min-width: 0; display: flex; flex-direction: column; gap: 4px; }
.task__line1 { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.task__line2 { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; min-width: 0; }
.task__title { font-size: var(--t-sm, 13px); font-weight: 600; }
.task__state { font-weight: 600; white-space: nowrap; }
.task__stage { color: var(--warning); }
/* 进行中时 lastError 装的是「上一轮为什么没成」，是常态而非错误，用次级色 */
.task__note {
  color: var(--text-secondary);
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  max-width: 100%; min-width: 0; flex: 1 1 200px;
}
.task__note.is-err { color: var(--danger); }

.task__right { display: flex; align-items: center; gap: 10px; flex: 0 0 auto; padding-top: 4px; }
.task__chev { color: var(--text-tertiary); transition: transform var(--dur-fast); font-size: 14px; }
.task__chev.is-open { transform: rotate(90deg); }

.task__body { padding: 0 16px 14px 54px; display: flex; flex-direction: column; gap: 10px; }
.task__prog {
  margin: 0; padding: 8px 12px; border-radius: var(--radius-md); font-size: 12px;
  background: var(--bg-inset); border: 1px solid var(--border-subtle);
  word-break: break-all;
}

.meta {
  margin: 0; display: grid; grid-template-columns: 88px 1fr; gap: 0;
}
.meta dt, .meta dd {
  margin: 0; padding: 6px 0; border-bottom: 1px solid var(--border-subtle);
  font-size: 12px; align-self: baseline; word-break: break-all;
}
.meta dt { color: var(--text-secondary); }

.task__result { margin: 0; font-size: 12px; line-height: 1.7; color: var(--text-primary); }
.task__errbox {
  margin: 0; padding: 10px 12px; border-radius: var(--radius-md);
  border: 1px solid var(--danger); color: var(--danger); font-size: 12px;
  white-space: pre-line; word-break: break-word;
}
.task__notebox {
  margin: 0; padding: 10px 12px; border-radius: var(--radius-md);
  border: 1px solid var(--border-subtle); background: var(--bg-inset);
  color: var(--text-secondary); font-size: 12px;
  white-space: pre-line; word-break: break-word;
}
.task__notek { margin-right: 8px; color: var(--text-tertiary); }
.task__dns { margin: 0; font-size: 12px; }
.task__actions { display: flex; gap: 8px; flex-wrap: wrap; }

@media (max-width: 640px) {
  .task__body { padding-left: 16px; }
}
</style>
