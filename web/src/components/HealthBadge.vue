<script setup lang="ts">
/**
 * 账号健康度色块。
 *
 * 只画后端算好的 level，不在前端做任何判定——「连续几次失败算坏」
 * 「哪种错误算红」这类规则散到前端就会两边不一致。
 *
 * 五档的分工是刻意的：
 * - green   凭据可用，不用管
 * - yellow  暂时性的（限流、单次抖动），等一会儿会自己好
 * - orange  配额满了，账号没坏但再操作也做不成
 * - red     已确认凭据或权限有问题，必须去控制台
 * - unknown 还没校验过。不能画成绿——那是向用户保证一件没人验证过的事
 */
import { computed } from 'vue'
import type { AccountHealth } from '@/types'

const props = withDefaults(defineProps<{
  level: AccountHealth
  /** 后端给的一句话说明，作为 tooltip */
  advice?: string
  /** 连续失败次数，>1 时在 tooltip 里点出来 */
  failures?: number
  /** 最近一次失败的错误分类名 */
  errorClass?: string
  /** 只显示色块，不带文字。列表行内用 */
  compact?: boolean
}>(), { advice: '', failures: 0, errorClass: '', compact: false })

const LABEL: Record<AccountHealth, string> = {
  unknown: '尚未校验',
  green: '正常',
  yellow: '暂时异常',
  orange: '配额已满',
  red: '凭据或权限异常'
}

const label = computed(() => LABEL[props.level])

/**
 * tooltip 把"为什么是这个颜色"说清楚。
 *
 * 只说"异常"没有用——用户看到红块的第一反应是"哪个账号坏了"，
 * 第二反应是"我该做什么"。advice 就是回答第二个问题的。
 */
const tip = computed(() => {
  const parts = [label.value]
  if (props.advice) parts.push(props.advice)
  if (props.failures > 1) parts.push(`连续失败 ${props.failures} 次`)
  if (props.errorClass) parts.push(`错误类型：${props.errorClass}`)
  return parts.join(' · ')
})
</script>

<template>
  <span class="health" :class="[`is-${level}`, { 'is-compact': compact }]" :title="tip">
    <span class="health__dot" />
    <span v-if="!compact" class="health__label">{{ label }}</span>
  </span>
</template>

<style scoped>
.health {
  display: inline-flex; align-items: center; gap: 6px;
  /* 让浏览器把 title 当成这个元素的提示 */
  cursor: default;
}
.health__dot {
  width: 8px; height: 8px; flex: 0 0 auto;
  border-radius: var(--radius-full);
  border: 1.5px solid currentColor;
  background: currentColor;
}
.health__label { font-size: 11px; font-weight: 500; color: currentColor; white-space: nowrap; }

/* 尚未校验：空心圈。它不是"好"，也不是"坏" */
.is-unknown { color: var(--text-tertiary); }
.is-unknown .health__dot { background: transparent; }

.is-green { color: var(--success); }
.is-yellow { color: var(--warning); }
/* 橙色没有现成的 token：它是"账号没坏但做不成事"，介于警告与危险之间 */
.is-orange { color: var(--health-orange); }
.is-red { color: var(--danger); }

/* 红块轻微呼吸，让它在列表里能被一眼扫到。用 opacity 而不是 box-shadow，
   避免在长列表里触发重绘 */
.is-red .health__dot { animation: health-breathe 2.4s ease-in-out infinite; }

@keyframes health-breathe {
  0%, 100% { opacity: 1; }
  50% { opacity: .45; }
}

/* 尊重系统的减少动效偏好 */
@media (prefers-reduced-motion: reduce) {
  .is-red .health__dot { animation: none; }
}
</style>
