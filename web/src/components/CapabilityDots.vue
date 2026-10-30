<script setup lang="ts">
/**
 * 权限自检的五个小点。
 *
 * 每族一个点：绿=可用，黄=缺权限，红=接口报错，灰=没自检过。
 *
 * 「缺权限」和「接口报错」必须分色：前者要去控制台加一条策略，后者过一会儿
 * 重试可能就好了。合成一个红点，用户会拿着"接口报错"去翻 IAM 文档。
 *
 * 颜色只由后端给的 ok / denied 决定，前端不自己判——探测的判定规则
 * （什么算成功、什么算缺权限）只该有一份。
 */
import { computed } from 'vue'
import type { CapabilityProbe } from '@/types'

const props = defineProps<{
  /** null 表示还没自检过 */
  probe: CapabilityProbe | null
  /** 是否显示文字结论。卡片上不显示，列表里显示 */
  showSummary?: boolean
  /** 自检进行中 */
  busy?: boolean
}>()

type Tone = 'ok' | 'denied' | 'error' | 'unknown'

/** 后端没给 label 时的兜底文案。正常情况下用的是后端下发的 label */
const LABEL: Record<string, string> = {
  compute: '实例',
  network: '网络',
  storage: '存储',
  usage: '账单',
  monitoring: '监控'
}

/**
 * 五族的展示顺序。
 *
 * 与后端 capabilitySpecs 的顺序一致：主干功能（实例/网络/存储）在前，
 * 锦上添花（账单/监控）在后。前端写死一份顺序是可以的——顺序错了只是
 * 排列不同，不会漏显示；漏显示才需要靠后端下发清单来防。
 */
const ORDER = ['compute', 'network', 'storage', 'usage', 'monitoring']

const items = computed(() => {
  const probe = props.probe
  if (!probe) {
    return ORDER.map(key => ({ key, label: LABEL[key] ?? key, tone: 'unknown' as Tone, tip: '尚未自检' }))
  }
  const byKey = new Map(probe.items.map(c => [c.key, c]))
  return ORDER.map(key => {
    const c = byKey.get(key)
    if (!c) {
      return { key, label: LABEL[key] ?? key, tone: 'unknown' as Tone, tip: '本次未探测' }
    }
    let tone: Tone = 'error'
    if (c.ok) tone = 'ok'
    else if (c.denied) tone = 'denied'
    return { key, label: c.label || LABEL[key] || key, tone, tip: tipOf(c) }
  })
})

/**
 * tooltip 要说清楚"为什么是黄的"以及"该怎么办"。
 *
 * 缺权限时把该补的策略原样贴出来：让用户去翻文档找策略名，等于把这件事
 * 推给了一个已经因为 403 而烦躁的人。
 */
function tipOf(c: CapabilityProbe['items'][number]): string {
  const head = `${c.label}：`
  if (c.ok) {
    return c.count > 0 ? `${head}可用（${c.count} 项）` : `${head}可用`
  }
  if (c.denied) {
    const policy = c.policy ? ` · 需补策略：${c.policy}` : ''
    return `${head}缺少读取权限${policy}`
  }
  return `${head}接口调用失败${c.detail ? ' · ' + c.detail : ''}`
}

const summary = computed(() => {
  if (!props.probe) return '尚未自检'
  return props.probe.summary
})
</script>

<template>
  <span class="caps" :class="{ 'is-busy': busy }">
    <span class="caps__dots">
      <span v-for="it in items" :key="it.key" class="caps__dot" :class="`is-${it.tone}`" :title="it.tip" />
    </span>
    <span v-if="showSummary" class="caps__summary">{{ summary }}</span>
  </span>
</template>

<style scoped>
.caps { display: inline-flex; align-items: center; gap: 8px; min-width: 0; }
.caps__dots { display: inline-flex; align-items: center; gap: 3px; flex: 0 0 auto; }

.caps__dot {
  width: 6px; height: 6px; border-radius: var(--radius-full);
  /* 默认灰：没自检过。它既不是好也不是坏 */
  background: var(--border-strong);
}
.caps__dot.is-ok { background: var(--success); }
.caps__dot.is-denied { background: var(--warning); }
.caps__dot.is-error { background: var(--danger); }

/* 自检进行中：点变暗并轻微脉动，让用户知道"还在查"而不是"查完了全是灰的" */
.is-busy .caps__dot { animation: caps-pulse 1.2s ease-in-out infinite; }

@keyframes caps-pulse {
  0%, 100% { opacity: 1; }
  50% { opacity: .3; }
}

@media (prefers-reduced-motion: reduce) {
  .is-busy .caps__dot { animation: none; opacity: .5; }
}

.caps__summary {
  font-size: 11px; color: var(--text-tertiary);
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap; min-width: 0;
}
</style>
