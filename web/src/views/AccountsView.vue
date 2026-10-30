<script setup lang="ts">
/** §4.3 账号管理：卡片网格 + 四种连通性状态，P2-1 加标签筛选与分组折叠 */
import { computed, ref, watch } from 'vue'
import { useStore } from '@/store'
import { acctColor, pct, usageTone } from '@/lib/format'
import { accountAgeText, accountStatusText, relativeTime, trialDaysLeft } from '@/lib/adapt'
import type { Account } from '@/types'
import MaskedText from '@/components/MaskedText.vue'
import EmptyState from '@/components/EmptyState.vue'
import TagFilter from '@/components/TagFilter.vue'
import HealthBadge from '@/components/HealthBadge.vue'
import CapabilityDots from '@/components/CapabilityDots.vue'
import AccountOrganizeDialog from '@/components/AccountOrganizeDialog.vue'
import SkeletonRows from '@/components/SkeletonRows.vue'
import LoadingBar from '@/components/LoadingBar.vue'

const {
  state, openDrawer, saveAccountOrganize, clearAccountHealth,
  runCapabilityProbe, probingAccount
} = useStore()

const tierLabel: Record<Account['tier'], string> = {
  trial: '试用号',
  paid: '升级号',
  // 读不到订阅信息时不猜。写"升级号"会让用户以为资源是稳的,
  // 写"试用号"又会平白吓人一跳。
  unknown: '类型未知'
}

/**
 * 试用到期提示。
 *
 * 试用期内的配额远高于永久免费额度,到期那天会被打回永久免费额度,
 * 超出的实例会被 Oracle 回收。这行字就是提前把那一天摆在用户眼前。
 */
function trialText(a: Account): string {
  const d = trialDaysLeft(a)
  if (d === null) return ''
  if (d < 0) return '试用已到期'
  if (d === 0) return '试用今日到期'
  return `试用 ${d} 天后到期`
}

function trialTone(a: Account): string {
  const d = trialDaysLeft(a)
  if (d === null) return 'var(--text-secondary)'
  if (d <= 0) return 'var(--danger)'
  if (d <= 7) return 'var(--warning)'
  return 'var(--text-secondary)'
}

/**
 * 手动设置的到期提醒。
 *
 * 与上面的试用到期不是一回事：试用期由订阅决定，改不了；这个是人自己
 * 填的「这个号我打算用到什么时候」，可能更早也可能更晚。
 */
function expiryText(a: Account): string {
  const d = a.expiryDays
  if (d === null || d === undefined) return ''
  if (d < 0) return `已过期 ${-d} 天`
  if (d === 0) return '今日到期'
  return `${d} 天后到期`
}

function expiryTone(a: Account): string {
  const d = a.expiryDays
  if (d === null || d === undefined) return 'var(--text-secondary)'
  if (d <= 0) return 'var(--danger)'
  if (d <= 7) return 'var(--warning)'
  return 'var(--text-secondary)'
}

const lampTone: Record<Account['status'], string> = {
  ok: 'var(--success)', checking: 'var(--info)', error: 'var(--danger)', disabled: 'var(--neutral)'
}
const checkIcon: Record<Account['status'], string> = { ok: '✔', checking: '◍', error: '✕', disabled: '○' }

/* ---------- 标签筛选 ---------- */

/**
 * 选中的标签。语义是「或」：命中任一即显示。
 *
 * 不做持久化：筛选是一次性的观察动作，刷新后还停留在上次的筛选结果上
 * 更容易让人以为账号丢了。分组折叠则相反，那是长期偏好，要记住。
 */
const tagFilter = ref<string[]>([])

const visible = computed(() => {
  if (!tagFilter.value.length) return state.accounts
  const want = new Set(tagFilter.value)
  return state.accounts.filter(a => a.tags.some(t => want.has(t)))
})

/* ---------- 分组 ---------- */

/**
 * 折叠起来的组名。存 localStorage：折叠是长期偏好，
 * 和卡片/列表视图一样属于「这个人习惯怎么看」。
 */
const collapsed = ref<Set<string>>(
  new Set(JSON.parse(localStorage.getItem('oci.collapsedGroups') ?? '[]') as string[])
)
watch(collapsed, v => localStorage.setItem('oci.collapsedGroups', JSON.stringify([...v])), { deep: true })

function toggleGroup(name: string) {
  const next = new Set(collapsed.value)
  if (next.has(name)) next.delete(name)
  else next.add(name)
  collapsed.value = next
}

/**
 * 按分组切开，组的顺序用后端给的（组内最高权重降序，未分组固定最后）。
 *
 * 只保留有账号的组：筛选之后某个组可能一个都不剩，那时不该留一个
 * 空标题在那儿占位。
 */
const grouped = computed(() => {
  const order = state.groups.length ? state.groups : ['']
  const buckets = new Map<string, Account[]>()
  for (const name of order) buckets.set(name, [])
  for (const a of visible.value) {
    if (!buckets.has(a.group)) buckets.set(a.group, [])
    buckets.get(a.group)!.push(a)
  }
  return [...buckets.entries()]
    .filter(([, list]) => list.length > 0)
    .map(([name, list]) => ({ name, list }))
})

/**
 * 只有真的分了组才显示组标题。
 *
 * 所有账号都没分组时，显示一个孤零零的「未分组」标题只是噪声——
 * 这个功能还没被用起来，界面不该先摆出它的骨架。
 */
const showGroups = computed(() =>
  grouped.value.length > 1 || (grouped.value[0]?.name ?? '') !== ''
)

/**
 * 已确认坏掉的账号。顶部单独一块红区把它们挑出来。
 *
 * 只收 health === 'red'：黄色（限流、单次抖动）会自己好，橙色（配额满）
 * 是另一个问题，都不该占用这块最显眼的位置。红区一旦混进不需要处理的
 * 东西，用户就会开始无视它。
 */
const fatalAccounts = computed(() => state.accounts.filter(a => a.health === 'red'))

/**
 * 翻红所需的连续失败次数，与后端 accountsvc.FatalThreshold 保持一致。
 *
 * 只用于文案——真正的判定在后端，前端不参与。写成常量而不是直接写 2，
 * 是为了让「改阈值要改两处」这件事在代码里显式可见。
 */
const fatalThreshold = 2

const summary = computed(() => {
  const off = state.accounts.filter(a => a.status === 'disabled').length
  const red = fatalAccounts.value.length
  const base = `${state.accounts.length} 个 Oracle 租户 · ${red} 个凭据异常 · ${off} 个已禁用`
  if (!tagFilter.value.length) return base
  return `${base} · 筛选后 ${visible.value.length} 个`
})

const instancesOf = (id: string) => state.instances.filter(i => i.accountId === id && i.state !== 'TERMINATED')

/**
 * 卡片 / 列表两种视图。
 *
 * 卡片适合三五个账号，一眼看清每个的状态；账号多起来之后，
 * 一屏放不下几张卡，横向比较（谁的配额快满了、谁的试用先到期）
 * 反而更难。列表按行对齐，扫起来快得多。
 *
 * 存进 localStorage：视图偏好是"这个人习惯怎么看"，
 * 不该每次刷新都退回默认。
 */
type AccountView = 'card' | 'list'
const view = ref<AccountView>(
  (localStorage.getItem('oci.accountView') as AccountView) ?? 'card'
)
watch(view, v => localStorage.setItem('oci.accountView', v))

/* ---------- 整理对话框 ---------- */

const organizeId = ref<string | null>(null)
const organizeAccount = computed(
  () => state.accounts.find(a => a.id === organizeId.value) ?? null
)
const dialogRef = ref<InstanceType<typeof AccountOrganizeDialog>>()

function openOrganize(id: string) {
  organizeId.value = id
}

async function onOrganizeSave(payload: {
  tags: string[]
  groupName: string
  note: string
  expiresAt: string | null
  sortWeight: number
}) {
  if (!organizeAccount.value) return
  const ok = await saveAccountOrganize(organizeAccount.value.id, payload)
  if (ok) organizeId.value = null
  // 失败时对话框不关：用户刚填的东西还在，可以改完再存。
  // 但按钮的「保存中…」要恢复，否则看起来像卡死了。
  else dialogRef.value?.done()
}

/** 已有分组名，用于对话框里的补全。 */
const knownGroups = computed(() =>
  [...new Set(state.accounts.map(a => a.group).filter(Boolean))]
)
</script>

<template>
  <div class="page">
    <header class="page-head">
      <div>
        <h1 class="page-title">账号</h1>
        <p class="page-sub">{{ summary }}</p>
      </div>
      <div class="head-actions">
        <div class="segmented" role="group" aria-label="视图切换">
          <button class="segmented__btn" :class="{ 'is-on': view === 'card' }"
                  :aria-pressed="view === 'card'" @click="view = 'card'">▦ 卡片</button>
          <button class="segmented__btn" :class="{ 'is-on': view === 'list' }"
                  :aria-pressed="view === 'list'" @click="view = 'list'">☰ 列表</button>
        </div>
        <button class="btn btn--primary" @click="openDrawer({ kind: 'add-account' })">添加账号</button>
      </div>
    </header>
    <LoadingBar :active="state.loading && state.accounts.length > 0" />

    <TagFilter v-if="state.accounts.length" v-model="tagFilter" :tags="state.tagUsage"
               :matched="visible.length" :total="state.accounts.length" />

    <!--
      凭据异常的红区块。文案是刻意的：「凭据或权限异常，请到控制台确认账号状态」。
      不能写「已被封号」——OCI 对「无权限」和「资源不存在」返回同一个错误码，
      从 API 侧区分不出来。账号可能只是策略被改、用户被停用，或 OCID 填错了。
    -->
    <section v-if="fatalAccounts.length" class="redzone" role="alert">
      <header class="redzone__head">
        <span class="redzone__title">
          {{ fatalAccounts.length }} 个账号的凭据或权限异常，请到控制台确认账号状态
        </span>
        <span class="redzone__sub">
          连续 {{ fatalThreshold }} 次校验失败才会列入此处，单次抖动不算。已确认过的账号可以点「已知悉」，
          下次校验若仍失败会重新出现。
        </span>
      </header>
      <ul class="redzone__list">
        <li v-for="a in fatalAccounts" :key="a.id" class="redzone__item">
          <span class="redzone__dot" :style="{ background: acctColor(a.colorIndex) }" />
          <button type="button" class="redzone__name" @click="openDrawer({ kind: 'account', id: a.id, tab: '密钥' })">
            {{ a.alias }}<span class="mono redzone__code">{{ a.code }}</span>
          </button>
          <span v-if="a.healthErrorClass" class="redzone__class mono">{{ a.healthErrorClass }}</span>
          <span v-if="a.healthErrorAt" class="redzone__when">{{ relativeTime(a.healthErrorAt) }}失败</span>
          <button type="button" class="redzone__ack" @click.stop="clearAccountHealth(a.id)">已知悉</button>
        </li>
      </ul>
    </section>

    <!-- 首次加载：按当前视图画骨架，而不是先闪一下「还没有接入任何账号」 -->
    <template v-if="state.loading && !state.accounts.length">
      <div v-if="view === 'card'" class="grid" aria-busy="true">
        <div v-for="n in 3" :key="n" class="card sk-acct">
          <span class="sk-acct__bar" />
          <div class="sk-acct__body">
            <span class="skeleton" style="width: 46%; height: 16px" />
            <span class="skeleton" style="width: 70%; height: 10px" />
            <span class="skeleton" style="width: 58%; height: 10px" />
            <span class="skeleton" style="width: 100%; height: 6px; margin-top: 8px" />
          </div>
        </div>
      </div>
      <div v-else class="card"><SkeletonRows :rows="3" /></div>
    </template>

    <EmptyState v-else-if="!state.accounts.length" icon="⬡" title="还没有接入任何 Oracle 账号"
                sub="接入后即可统一管理所有实例" action-label="添加第一个账号"
                @action="openDrawer({ kind: 'add-account' })" />

    <EmptyState v-else-if="!visible.length" icon="🏷" title="没有账号带这些标签"
                sub="换一个标签，或清除筛选看全部" action-label="清除筛选"
                @action="tagFilter = []" />

    <!-- 卡片视图 -->
    <div v-else-if="view === 'card'" class="groups reveal">
      <section v-for="g in grouped" :key="g.name" class="group">
        <button v-if="showGroups" type="button" class="grouphead" :aria-expanded="!collapsed.has(g.name)"
                @click="toggleGroup(g.name)">
          <span class="grouphead__caret">{{ collapsed.has(g.name) ? '▸' : '▾' }}</span>
          <span class="grouphead__name">{{ g.name || '未分组' }}</span>
          <span class="grouphead__n mono">{{ g.list.length }}</span>
        </button>

        <div v-show="!collapsed.has(g.name)" class="grid">
          <article v-for="a in g.list" :key="a.id" class="card acct"
                   :class="{ 'is-error': a.status === 'error', 'is-disabled': a.status === 'disabled' }"
                   :style="{ '--c': acctColor(a.colorIndex) }"
                   tabindex="0" @click="openDrawer({ kind: 'account', id: a.id, tab: '概览' })"
                   @keydown.enter="openDrawer({ kind: 'account', id: a.id, tab: '概览' })">
            <span class="acct__bar" />
            <div class="acct__body">
              <p v-if="a.status === 'error'" class="acct__errbar">凭据失效 · 该账号下的资源暂不可操作</p>

              <header class="acct__head">
                <span class="acct__lamp" :style="{ '--l': lampTone[a.status] }"
                      :class="{ 'is-solid': a.status !== 'disabled', 'is-pulsing': a.status === 'checking' || a.status === 'error' }" />
                <h2 class="acct__alias">{{ a.alias }}</h2>
                <span class="acct__code mono">{{ a.code }}</span>
                <button class="acct__menu" aria-label="整理标签与分组" title="整理标签与分组"
                        @click.stop="openOrganize(a.id)">🏷</button>
                <button class="acct__menu" aria-label="账号菜单"
                        @click.stop="openDrawer({ kind: 'account', id: a.id, tab: '密钥' })">⋯</button>
              </header>

              <p class="acct__mail"><MaskedText :value="a.email" kind="email" /></p>
              <p class="acct__ocid mono">ocid1.tenancy…<MaskedText :value="a.tenancyTail" /></p>

              <div class="acct__tier">
                <span class="acct__tag" :class="`is-${a.tier}`">{{ tierLabel[a.tier] }}</span>
                <span v-if="accountAgeText(a)" class="acct__age">{{ accountAgeText(a) }}</span>
                <span v-if="trialText(a)" class="acct__trial" :style="{ color: trialTone(a) }">
                  {{ trialText(a) }}
                </span>
                <span v-if="expiryText(a)" class="acct__trial" :style="{ color: expiryTone(a) }">
                  {{ expiryText(a) }}
                </span>
              </div>

              <!-- 标签。没有标签时整行不出现，不留空白。 -->
              <div v-if="a.tags.length" class="acct__tags">
                <span v-for="t in a.tags" :key="t" class="acct__tagchip">{{ t }}</span>
              </div>
              <p v-if="a.note" class="acct__note">{{ a.note }}</p>

              <!--
                权限自检的五个点。没自检过时全是灰的，旁边给一个按钮——
                灰点本身不说明任何事，不给按钮用户就永远不知道能点。
              -->
              <div class="acct__caps">
                <CapabilityDots :probe="a.capability" :busy="probingAccount === a.id" />
                <button type="button" class="acct__probe"
                        :disabled="probingAccount !== null"
                        :title="a.capability ? '重新做一次权限自检（5 次只读请求）' : '检查这个账号能调用哪些接口'"
                        @click.stop="runCapabilityProbe(a.id)">
                  {{ probingAccount === a.id ? '自检中…' : a.capability ? '重测' : '自检' }}
                </button>
              </div>

              <div class="acct__regions">
                <span v-for="r in a.regions.slice(0, 2)" :key="r" class="acct__region mono">{{ r }}</span>
                <span v-if="a.regions.length > 2" class="acct__region mono">+{{ a.regions.length - 2 }}</span>
              </div>

              <!-- 正在校验时状态区骨架屏 -->
              <div v-if="a.status === 'checking'" class="acct__stats">
                <span class="skeleton" style="width: 56px; height: 32px" />
                <span class="skeleton" style="width: 56px; height: 32px" />
                <span class="skeleton" style="width: 56px; height: 32px" />
              </div>
              <div v-else class="acct__stats">
                <div><p class="acct__stat-label">实例</p><p class="acct__stat mono">{{ instancesOf(a.id).length }}</p></div>
                <div><p class="acct__stat-label">运行</p>
                  <p class="acct__stat mono" style="color: var(--success)">{{ instancesOf(a.id).filter(i => i.state === 'RUNNING').length }}</p></div>
                <div><p class="acct__stat-label">停止</p>
                  <p class="acct__stat mono dim">{{ instancesOf(a.id).filter(i => i.state === 'STOPPED').length }}</p></div>
              </div>

              <div class="acct__quota">
                <span class="t-2xs dim">ARM 配额</span>
                <span class="acct__track">
                  <span v-if="!a.quota.unlimited.ocpu" class="acct__fill" :style="{
                    width: pct(a.quota.ocpuUsed, a.quota.ocpuLimit) + '%',
                    background: usageTone(a.quota.ocpuUsed, a.quota.ocpuLimit)
                  }" />
                </span>
                <span class="mono t-2xs acct__quota-val"
                      :style="{ color: a.quota.unlimited.ocpu ? 'var(--text-secondary)' : usageTone(a.quota.ocpuUsed, a.quota.ocpuLimit) }">
                  <template v-if="a.quota.unlimited.ocpu">{{ a.quota.ocpuUsed }} OCPU · 不限</template>
                  <template v-else>{{ a.quota.ocpuUsed }} / {{ a.quota.ocpuLimit }} OCPU</template>
                </span>
              </div>

              <footer class="acct__foot">
                <HealthBadge :level="a.health" :advice="a.healthAdvice"
                             :failures="a.healthFailures" :error-class="a.healthErrorClass" />
                <span class="acct__foot-status" :style="{ color: lampTone[a.status] }">
                  <span>{{ checkIcon[a.status] }}</span><span>{{ accountStatusText(a) }}</span>
                </span>
              </footer>
            </div>
          </article>
        </div>
      </section>
    </div>

    <!-- 列表视图。表头只画一次，分组标题作为穿插的行。 -->
    <div v-else class="card table reveal">
      <div class="table-head acols">
        <span>账号</span><span>类型 · 账龄</span><span class="col-region">区域</span>
        <span class="col-count">实例</span><span class="col-quota">ARM 配额</span>
        <span>健康度 · 最后校验</span><span></span>
      </div>

      <template v-for="g in grouped" :key="g.name">
        <button v-if="showGroups" type="button" class="grouprow" :aria-expanded="!collapsed.has(g.name)"
                @click="toggleGroup(g.name)">
          <span class="grouphead__caret">{{ collapsed.has(g.name) ? '▸' : '▾' }}</span>
          <span class="grouphead__name">{{ g.name || '未分组' }}</span>
          <span class="grouphead__n mono">{{ g.list.length }}</span>
        </button>

        <template v-if="!collapsed.has(g.name)">
          <div v-for="a in g.list" :key="a.id" class="table-row acols"
               :class="{ 'is-error': a.status === 'error', 'is-disabled': a.status === 'disabled' }"
               :style="{ '--c': acctColor(a.colorIndex) }"
               tabindex="0" @click="openDrawer({ kind: 'account', id: a.id, tab: '概览' })"
               @keydown.enter="openDrawer({ kind: 'account', id: a.id, tab: '概览' })">
            <span class="acct-bar" :style="{ background: acctColor(a.colorIndex) }" />

            <span class="cell lcell">
              <span class="lcell__top">
                <span class="acct__lamp" :style="{ '--l': lampTone[a.status] }"
                      :class="{ 'is-solid': a.status !== 'disabled', 'is-pulsing': a.status === 'checking' || a.status === 'error' }" />
                <span class="lcell__alias">{{ a.alias }}</span>
                <span class="acct__code mono">{{ a.code }}</span>
              </span>
              <span class="lcell__sub"><MaskedText :value="a.email" kind="email" /></span>
              <span v-if="a.tags.length || a.note" class="lcell__tags">
                <span v-for="t in a.tags" :key="t" class="acct__tagchip">{{ t }}</span>
                <span v-if="a.note" class="lcell__note">{{ a.note }}</span>
              </span>
            </span>

            <span class="cell lcell">
              <span class="lcell__top">
                <span class="acct__tag" :class="`is-${a.tier}`">{{ tierLabel[a.tier] }}</span>
                <span v-if="trialText(a)" class="acct__trial" :style="{ color: trialTone(a) }">{{ trialText(a) }}</span>
              </span>
              <span class="lcell__sub">
                {{ accountAgeText(a) || '开户时间未知' }}
                <span v-if="expiryText(a)" :style="{ color: expiryTone(a) }"> · {{ expiryText(a) }}</span>
              </span>
            </span>

            <span class="cell mono t-xs dim col-region">
              {{ a.regions.slice(0, 2).join(' · ') }}<template v-if="a.regions.length > 2"> +{{ a.regions.length - 2 }}</template>
            </span>

            <span class="cell mono t-xs col-count">
              {{ instancesOf(a.id).length }}
              <span class="dim-3">/ 运行 {{ instancesOf(a.id).filter(i => i.state === 'RUNNING').length }}</span>
            </span>

            <span class="cell lquota col-quota">
              <span class="acct__track">
                <span v-if="!a.quota.unlimited.ocpu" class="acct__fill" :style="{
                  width: pct(a.quota.ocpuUsed, a.quota.ocpuLimit) + '%',
                  background: usageTone(a.quota.ocpuUsed, a.quota.ocpuLimit)
                }" />
              </span>
              <span class="mono t-2xs acct__quota-val"
                    :style="{ color: a.quota.unlimited.ocpu ? 'var(--text-secondary)' : usageTone(a.quota.ocpuUsed, a.quota.ocpuLimit) }">
                <template v-if="a.quota.unlimited.ocpu">{{ a.quota.ocpuUsed }} · 不限</template>
                <template v-else>{{ a.quota.ocpuUsed }} / {{ a.quota.ocpuLimit }}</template>
              </span>
            </span>

            <span class="cell lcell">
              <span class="lcell__top">
                <HealthBadge :level="a.health" :advice="a.healthAdvice"
                             :failures="a.healthFailures" :error-class="a.healthErrorClass" />
              </span>
              <span class="lcell__sub">
                <CapabilityDots :probe="a.capability" :busy="probingAccount === a.id" />
                <span :style="{ color: lampTone[a.status] }">{{ accountStatusText(a) }}</span>
              </span>
            </span>

            <span class="cell">
              <button class="acct__menu" aria-label="整理标签与分组" title="整理标签与分组"
                      @click.stop="openOrganize(a.id)">🏷</button>
              <button class="acct__menu" aria-label="账号菜单"
                      @click.stop="openDrawer({ kind: 'account', id: a.id, tab: '密钥' })">⋯</button>
            </span>
          </div>
        </template>
      </template>
    </div>

    <AccountOrganizeDialog v-if="organizeAccount" ref="dialogRef" :account="organizeAccount"
                           :known-tags="state.tagUsage" :known-groups="knownGroups"
                           @close="organizeId = null" @save="onOrganizeSave" />
  </div>
</template>

<style scoped>
.head-actions { display: flex; align-items: center; gap: 10px; }

.segmented { display: inline-flex; border: 1px solid var(--border-default); border-radius: var(--radius-md); overflow: hidden; }
.segmented__btn {
  padding: 6px 12px; border: 0; background: transparent;
  color: var(--text-secondary); font-size: 12px; cursor: pointer;
}
.segmented__btn + .segmented__btn { border-left: 1px solid var(--border-default); }
.segmented__btn:hover { background: var(--bg-hover); }
.segmented__btn.is-on { background: var(--bg-inset); color: var(--accent); }

/* 分组 */
.groups { display: flex; flex-direction: column; gap: 18px; }
.group { display: flex; flex-direction: column; gap: 12px; }
.grouphead {
  display: flex; align-items: center; gap: 8px; align-self: flex-start;
  padding: 4px 2px; border: 0; background: none; cursor: pointer;
  color: var(--text-secondary);
}
.grouphead:hover { color: var(--text-primary); }
.grouphead__caret { font-size: 10px; width: 10px; }
.grouphead__name { font-size: 12px; font-weight: 600; letter-spacing: .02em; }
.grouphead__n {
  font-size: 10px; color: var(--text-tertiary);
  padding: 1px 6px; border-radius: var(--radius-full); background: var(--bg-inset);
}
/* 列表视图里的分组行：跨整行，与数据行区分开 */
.grouprow {
  display: flex; align-items: center; gap: 8px; width: 100%;
  padding: 8px 14px; border: 0; cursor: pointer; text-align: left;
  background: var(--bg-inset); color: var(--text-secondary);
  border-top: 1px solid var(--border-subtle);
}
.grouprow:hover { color: var(--text-primary); }

/* 列表视图。列宽与实例表同源，保持两张表的视觉节奏一致。 */
.acols {
  display: grid; align-items: center;
  grid-template-columns: minmax(220px, 1.5fr) minmax(150px, 1fr) 130px 110px 150px minmax(140px, 1fr) 76px;
  gap: 12px; padding-left: 14px;
}
/* .cell 定义在 InstancesView 的 scoped 块里，这边拿不到。不补这一条，
   状态灯会退化成 inline 元素——width / height 全部失效，8px 的圆点被压成
   一根 2×18 的竖条，正好贴在左边的身份色条上，看起来像"红条中间有点绿"。 */
.cell { min-width: 0; padding-right: 12px; display: flex; align-items: center; gap: 8px; }
.table-row.acols { position: relative; height: 56px; cursor: pointer; }
.table-row.acols:hover { background: var(--bg-hover); }
.table-row.acols.is-error { background: color-mix(in srgb, var(--danger) 5%, transparent); }
.table-row.acols.is-disabled { opacity: 0.55; }
.acols .acct-bar { position: absolute; left: 0; top: 0; bottom: 0; width: 3px; }

/* align-items 必须显式写 flex-start：.cell 给的是 center，
   在纵向 flex 里那是"水平居中"，整块内容会被推到列中间。 */
.lcell { display: flex; flex-direction: column; align-items: flex-start; gap: 3px; min-width: 0; }
.lcell__top { display: flex; align-items: center; gap: 8px; min-width: 0; }
.lcell__alias { font-size: 13px; font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.lcell__sub { font-size: 11px; color: var(--text-tertiary); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.lcell__tags { display: flex; align-items: center; gap: 4px; min-width: 0; flex-wrap: nowrap; overflow: hidden; }
.lcell__note {
  font-size: 10px; color: var(--text-tertiary); font-style: italic;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.lquota { display: flex; align-items: center; gap: 8px; }
.lquota .acct__track { flex: 1 1 auto; }

.acct__age { font-size: 10px; color: var(--text-tertiary); }
/* 不换行。升级号的上限曾经是一亿，"2 / 100000000 OCPU" 把这一行挤成三行，
   把卡片底部整个撑开。现在已经不显示那串数字了，但这条约束要留着：
   任何一个意外的长值都不该把卡片布局带歪。 */
.acct__quota-val { white-space: nowrap; flex: 0 0 auto; }

/* 窄屏收列。
 *
 * 卡片 .card 带 overflow: hidden（裁圆角用的），列放不下时不会出滚动条，
 * 而是被静默切掉——实测 1024px 下最后一列（⋯ 菜单）被切掉 307px，
 * 界面上看不出任何痕迹。所以必须主动收列，不能指望它自己挤。
 * 收的顺序按信息密度：先去区域，再去实例计数与配额条。 */
@media (max-width: 1279px) {
  .col-region { display: none; }
  .acols { grid-template-columns: minmax(210px, 1.5fr) minmax(150px, 1fr) 110px 150px minmax(140px, 1fr) 76px; }
}
@media (max-width: 1023px) {
  .col-quota, .col-count { display: none; }
  .acols { grid-template-columns: minmax(190px, 1.6fr) minmax(140px, 1fr) minmax(130px, 1fr) 76px; }
}

.grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(340px, 1fr)); gap: 16px; }
/* 账号卡骨架：左侧身份色条位置留一条灰，其余按卡片的几行文字排 */
.sk-acct { display: flex; overflow: hidden; min-height: 150px; }
.sk-acct__bar { width: 4px; flex: 0 0 auto; background: var(--bg-inset); }
.sk-acct__body { flex: 1 1 auto; padding: 16px; display: flex; flex-direction: column; gap: 12px; }
.acct { display: flex; overflow: hidden; cursor: pointer; transition: border-color var(--dur-fast); }
.acct:hover { border-color: var(--border-strong); }
.acct:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
.acct.is-error { border-color: var(--danger); filter: saturate(0.72); }
.acct.is-disabled { opacity: 0.55; }
.acct__bar { width: 4px; flex: 0 0 auto; background: var(--c); }
.acct__body { flex: 1 1 auto; min-width: 0; padding: 20px; }
.acct__errbar {
  margin: -20px -20px 14px; padding: 7px 20px; background: var(--danger-soft);
  border-bottom: 1px solid var(--danger); color: var(--danger); font-size: 11px; font-weight: 600;
}
.acct__head { display: flex; align-items: center; gap: 9px; }
.acct__lamp {
  position: relative; width: 8px; height: 8px; flex: 0 0 auto;
  border-radius: var(--radius-full); border: 1.5px solid var(--l);
}
.acct__lamp.is-solid { background: var(--l); }
.acct__lamp.is-pulsing::after {
  content: ''; position: absolute; inset: -1.5px; border-radius: var(--radius-full);
  background: var(--l); animation: ring 2s ease-out infinite;
}
.acct__alias { margin: 0; flex: 1 1 auto; min-width: 0; font-size: 16px; line-height: 24px; font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.acct__code { font-size: 11px; font-weight: 600; color: var(--c); padding: 2px 7px; border-radius: var(--radius-full); background: var(--bg-inset); }
.acct__menu { border: 0; background: none; color: var(--text-secondary); cursor: pointer; font-size: 13px; padding: 0 2px; }
.acct__menu:hover { color: var(--text-primary); }
.acct__mail { font-size: 11px; color: var(--text-secondary); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.acct__tier { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.acct__tag {
  padding: 2px 8px; border-radius: var(--radius-full);
  font-size: 10px; font-weight: 600; letter-spacing: .02em;
  border: 1px solid var(--border-default); color: var(--text-secondary);
}
.acct__tag.is-trial { border-color: color-mix(in srgb, var(--warning) 45%, transparent); color: var(--warning); }
.acct__tag.is-paid { border-color: color-mix(in srgb, var(--success) 45%, transparent); color: var(--success); }
.acct__trial { font-size: 10px; }

/* 用户打的标签。与 .acct__tag（账号类型）刻意用不同的外观：
   一个是系统判定的，一个是人写的，混在一起会分不清哪个能改。 */
.acct__tags { margin-top: 8px; display: flex; gap: 5px; flex-wrap: wrap; }
.acct__tagchip {
  padding: 1px 7px; border-radius: var(--radius-full);
  border: 1px dashed var(--border-default); background: transparent;
  font-size: 10px; color: var(--text-secondary);
}
.acct__note {
  margin: 8px 0 0; font-size: 11px; color: var(--text-tertiary);
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}

.acct__ocid { margin: 6px 0 0; font-size: 11px; color: var(--text-tertiary); }
.acct__regions { margin-top: 14px; display: flex; gap: 6px; flex-wrap: wrap; }
.acct__region {
  padding: 2px 8px; border-radius: var(--radius-full); border: 1px solid var(--border-subtle);
  background: var(--bg-inset); font-size: 11px; color: var(--text-secondary);
}
.acct__stats { margin-top: 16px; display: flex; gap: 20px; }
.acct__stat-label { margin: 0; font-size: 11px; color: var(--text-tertiary); }
.acct__stat { margin: 0; font-size: 16px; font-weight: 600; }
.acct__quota { margin-top: 14px; display: grid; grid-template-columns: 66px 1fr 82px; gap: 10px; align-items: center; }
.acct__track { height: 6px; border-radius: var(--radius-full); background: var(--bg-inset); overflow: hidden; }
.acct__fill { display: block; height: 6px; border-radius: var(--radius-full); }
.acct__quota .mono { text-align: right; }
.acct__foot {
  margin-top: 14px; padding-top: 12px; border-top: 1px solid var(--border-subtle);
  display: flex; align-items: center; gap: 10px; font-size: 12px;
}
/* 健康度与状态文字并排时，状态文字靠右，健康度靠左——
   一眼扫过去先看到颜色，再看到具体文案。 */
.acct__foot-status {
  margin-left: auto; display: inline-flex; align-items: center; gap: 5px;
  min-width: 0; overflow: hidden; white-space: nowrap; text-overflow: ellipsis;
}

/* 能力点与自检按钮。自检按钮做得比正文小一号：它是辅助动作，
   不该和「添加账号」抢视线。 */
.acct__caps { display: flex; align-items: center; gap: 8px; margin-top: 8px; }
.acct__probe {
  border: 1px solid var(--border-default); background: transparent;
  color: var(--text-tertiary); font-size: 10px; line-height: 14px;
  padding: 1px 7px; border-radius: var(--radius-full); cursor: pointer;
  flex: 0 0 auto;
}
.acct__probe:hover:not(:disabled) { border-color: var(--accent); color: var(--accent); }
.acct__probe:disabled { opacity: .45; cursor: default; }

/* ---------- 凭据异常红区块 ---------- */

.redzone {
  border: 1px solid color-mix(in srgb, var(--danger) 42%, var(--border-default));
  border-radius: var(--radius-md);
  background: color-mix(in srgb, var(--danger) 7%, var(--bg-surface));
  padding: 12px 14px; margin-bottom: 14px;
}
.redzone__head { display: flex; flex-direction: column; gap: 3px; margin-bottom: 10px; }
.redzone__title { font-size: 13px; font-weight: 600; color: var(--danger); }
.redzone__sub { font-size: 11px; color: var(--text-tertiary); line-height: 1.55; }

.redzone__list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 2px; }
.redzone__item {
  display: flex; align-items: center; gap: 8px;
  padding: 5px 6px; border-radius: var(--radius-sm);
}
.redzone__item:hover { background: color-mix(in srgb, var(--danger) 8%, transparent); }
.redzone__dot { width: 6px; height: 6px; border-radius: var(--radius-full); flex: 0 0 auto; }
.redzone__name {
  border: 0; background: none; padding: 0; cursor: pointer;
  color: var(--text-primary); font-size: 12px; font-weight: 500;
  display: inline-flex; align-items: center; gap: 6px;
}
.redzone__name:hover { color: var(--danger); text-decoration: underline; }
.redzone__code { font-size: 10px; color: var(--text-tertiary); }
.redzone__class {
  font-size: 10px; padding: 1px 6px; border-radius: var(--radius-full);
  background: color-mix(in srgb, var(--danger) 14%, transparent); color: var(--danger);
}
.redzone__when { font-size: 11px; color: var(--text-tertiary); margin-left: auto; }
.redzone__ack {
  border: 1px solid var(--border-default); background: var(--bg-surface);
  color: var(--text-secondary); font-size: 11px; padding: 2px 9px;
  border-radius: var(--radius-full); cursor: pointer; flex: 0 0 auto;
}
.redzone__ack:hover { border-color: var(--border-strong); color: var(--text-primary); }
</style>
