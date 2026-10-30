<script setup lang="ts">
/**
 * 计费风险面板（P2-4）。
 *
 * 回答一个问题：「这个账号上有没有东西正在扣钱」。
 *
 * 三条纪律写在这个文件里：
 *  1. 金额一律标「估算」。单价是公开标价的粗算，不是账单；不标清楚，
 *     用户会拿它跟账单对，然后不再相信这个面板。
 *  2. `confirmed === false` 时不能说「你超额了」。那种情况下的额度来自
 *     公开常量，Oracle 改过免费额度而且没公告，所以只能写成「按公开额度估算」。
 *  3. 「没查到」和「查了没问题」分开显示。前者是 attention 条目，
 *     后者是空列表——后端已经保证了这一点，这里只负责别把它抹平。
 */
import { computed, onMounted, ref, watch } from 'vue'
import { accounts as accountsApi, type BillingRiskDTO } from '@/api'
import SectionCard from '@/components/SectionCard.vue'
import CheckList from '@/components/CheckList.vue'
import type { CheckItem } from '@/types'

const props = defineProps<{ id: string }>()

const risk = ref<BillingRiskDTO | null>(null)
const loading = ref(false)
const errorText = ref('')

async function load() {
  if (loading.value) return
  loading.value = true
  errorText.value = ''
  try {
    risk.value = await accountsApi.billingRisk(props.id)
  } catch (err) {
    errorText.value = err instanceof Error ? err.message : String(err)
  } finally {
    loading.value = false
  }
}

onMounted(() => load())
// 同一个抽屉换账号时组件会被复用，不重取就会显示上一个账号的数字。
watch(() => props.id, () => { risk.value = null; load() })

/** 已经确定在扣钱的条目。 */
const billingItems = computed(() => (risk.value?.items ?? []).filter(i => i.severity === 'billing'))
/** 没能确认的条目。它们不是风险，是「不知道」。 */
const unknownItems = computed(() => (risk.value?.items ?? []).filter(i => i.severity === 'attention'))

const note = computed(() => {
  if (loading.value && !risk.value) return '检查中…'
  if (!risk.value) return ''
  return risk.value.region || ''
})

/**
 * 顶部一句话结论。
 *
 * 有确定计费项时报金额；只有「没查到」时报条数——把两者混成一句
 * 「发现 N 项问题」，用户会以为那 N 项都在扣钱。
 */
const headline = computed(() => {
  const r = risk.value
  if (!r) return null
  if (billingItems.value.length > 0) {
    return {
      tone: 'danger' as const,
      text: `预计 ¥${money(r.totalDailyCny)} / 日（约 ¥${money(r.totalMonthlyCny)} / 月）`,
      sub: `${billingItems.value.length} 项已确定计费`
    }
  }
  if (unknownItems.value.length > 0) {
    return {
      tone: 'warn' as const,
      text: `${unknownItems.value.length} 项没能查到`,
      sub: '没查到不等于没有费用'
    }
  }
  return { tone: 'ok' as const, text: '未发现计费项', sub: '六条规则都没命中' }
})

const emptyChecks = computed<CheckItem[]>(() => [
  { tone: 'ok', text: 'ARM / Micro / 块存储都在免费额度内' },
  { tone: 'ok', text: '没有网络负载均衡，也没有保留公网 IP' },
  { tone: 'ok', text: '本月出站流量未超出免费额度' },
  { tone: 'info', text: '这里只覆盖六类常见计费项，不等于账单为零——对象存储、跨区备份、带宽升级等不在检查范围内。' }
])

function money(v: number): string {
  return v.toFixed(2)
}
</script>

<template>
  <SectionCard title="计费风险" :note="note">
    <template #action>
      <button class="btn btn--sm" :disabled="loading" @click="load()">
        {{ loading ? '检查中…' : '重新检查' }}
      </button>
    </template>

    <!-- 首次加载：给一句话，别放骨架条——这个面板本来就不高 -->
    <div v-if="loading && !risk" class="risk__hint">正在检查限额、公网 IP、负载均衡与出站流量…</div>

    <div v-else-if="errorText" class="risk__hint risk__hint--bad">
      检查失败：{{ errorText }}
    </div>

    <template v-else-if="risk">
      <div v-if="headline" class="risk__head" :class="`is-${headline.tone}`">
        <span class="risk__head-main mono">{{ headline.text }}</span>
        <span class="risk__head-sub">{{ headline.sub }}</span>
      </div>

      <CheckList v-if="risk.items.length === 0" :items="emptyChecks" />

      <div v-else class="risk__list">
        <div v-for="it in risk.items" :key="it.key" class="risk__row" :class="`is-${it.severity}`">
          <span class="risk__bar" />
          <div class="risk__body">
            <div class="risk__line">
              <span class="risk__label">{{ it.label }}</span>
              <!-- 金额只在算得出来时出现。0 不是「免费」，是「没算」 -->
              <span v-if="it.estDailyCny > 0" class="risk__amount mono">≈ ¥{{ money(it.estDailyCny) }} / 日</span>
              <span v-else-if="it.severity === 'billing'" class="risk__amount dim-3">金额未估</span>
            </div>
            <p class="risk__detail">{{ it.detail }}</p>
            <p class="risk__basis">
              <span v-if="!it.confirmed" class="risk__tag">未确认</span>
              {{ it.basis }}
            </p>
          </div>
        </div>
      </div>

      <p v-if="risk.items.length > 0" class="risk__notice">{{ risk.notice }}</p>
    </template>
  </SectionCard>
</template>

<style scoped>
.risk__hint { padding: 14px 16px; font-size: 12px; color: var(--text-tertiary); }
.risk__hint--bad { color: var(--danger); }

.risk__head {
  display: flex; align-items: baseline; gap: 10px;
  padding: 12px 16px; border-bottom: 1px solid var(--border-subtle);
}
.risk__head-main { font-size: 14px; font-weight: 600; }
.risk__head-sub { font-size: 11px; color: var(--text-tertiary); }
.risk__head.is-danger .risk__head-main { color: var(--danger); }
.risk__head.is-warn .risk__head-main { color: var(--warning); }
.risk__head.is-ok .risk__head-main { color: var(--success); }

.risk__list { display: flex; flex-direction: column; }
.risk__row {
  display: grid; grid-template-columns: 3px 1fr; gap: 12px;
  padding: 12px 16px;
  border-bottom: 1px solid var(--border-subtle);
}
.risk__row:last-child { border-bottom: none; }
.risk__bar { border-radius: var(--radius-full); background: var(--border-strong); }
.risk__row.is-billing .risk__bar { background: var(--danger); }
.risk__row.is-attention .risk__bar { background: var(--warning); }

.risk__body { min-width: 0; }
.risk__line { display: flex; align-items: baseline; gap: 10px; }
.risk__label { font-size: 12px; font-weight: 600; flex: 1 1 auto; min-width: 0; }
.risk__amount { flex: 0 0 auto; font-size: 12px; }
.risk__row.is-billing .risk__amount { color: var(--danger); }

.risk__detail { margin: 4px 0 0; font-size: 11px; color: var(--text-secondary); }
.risk__basis { margin: 3px 0 0; font-size: 11px; color: var(--text-tertiary); }

.risk__tag {
  display: inline-block; margin-right: 4px; padding: 0 4px;
  border-radius: var(--radius-sm); font-size: 10px;
  border: 1px solid var(--warning); color: var(--warning);
}

.risk__notice {
  margin: 0; padding: 10px 16px 14px;
  font-size: 11px; color: var(--text-tertiary);
  border-top: 1px solid var(--border-subtle);
}
</style>
