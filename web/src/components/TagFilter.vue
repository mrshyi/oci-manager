<script setup lang="ts">
/**
 * 标签多选筛选器。
 *
 * 语义是「或」：选中「东京」和「主力」时，显示带其中任一标签的账号。
 * 选「与」看起来更精确，但实际用起来几乎总是错的——用户想的是「把东京的
 * 和主力的都给我看看」，而不是「既是东京又是主力的」（后者通常是空集，
 * 于是界面一片空白，用户以为筛选器坏了）。
 *
 * 标签是自由文本，所以这里不做任何枚举校验：清单来自服务端的实际使用
 * 情况，用户看到的就是真的能筛出东西的那些。
 */
import { computed } from 'vue'

const props = defineProps<{
  /** 全部标签及其使用次数，按次数降序 */
  tags: { tag: string; count: number }[]
  /** 已选中的标签 */
  modelValue: string[]
  /** 当前筛选结果命中多少个账号，用于回显 */
  matched: number
  total: number
}>()

const emit = defineEmits<{ 'update:modelValue': [string[]] }>()

const picked = computed(() => new Set(props.modelValue))

function toggle(tag: string) {
  const next = new Set(props.modelValue)
  if (next.has(tag)) next.delete(tag)
  else next.add(tag)
  emit('update:modelValue', [...next])
}

function clear() {
  emit('update:modelValue', [])
}
</script>

<template>
  <div v-if="tags.length" class="tagfilter" role="group" aria-label="按标签筛选">
    <span class="tagfilter__label">标签</span>
    <button v-for="t in tags" :key="t.tag" type="button" class="chip"
            :class="{ 'is-on': picked.has(t.tag) }" :aria-pressed="picked.has(t.tag)"
            @click="toggle(t.tag)">
      {{ t.tag }}<span class="chip__n mono">{{ t.count }}</span>
    </button>
    <button v-if="modelValue.length" type="button" class="tagfilter__clear" @click="clear">
      清除筛选
    </button>
    <span v-if="modelValue.length" class="tagfilter__hint">
      命中 {{ matched }} / {{ total }} 个账号（含任一选中标签）
    </span>
  </div>
</template>

<style scoped>
.tagfilter {
  display: flex; align-items: center; gap: 6px; flex-wrap: wrap;
  padding: 10px 12px; margin-bottom: 14px;
  border: 1px solid var(--border-subtle); border-radius: var(--radius-md);
  background: var(--bg-inset);
}
.tagfilter__label {
  font-size: 11px; font-weight: 600; color: var(--text-tertiary);
  letter-spacing: .04em; margin-right: 2px;
}

.chip {
  display: inline-flex; align-items: center; gap: 5px;
  padding: 3px 9px; border-radius: var(--radius-full); cursor: pointer;
  border: 1px solid var(--border-default); background: var(--bg-surface);
  color: var(--text-secondary); font-size: 11px; line-height: 16px;
  transition: border-color var(--dur-fast), color var(--dur-fast);
}
.chip:hover { border-color: var(--border-strong); color: var(--text-primary); }
.chip.is-on {
  border-color: var(--accent); color: var(--accent);
  background: color-mix(in srgb, var(--accent) 12%, transparent);
}
.chip__n {
  font-size: 10px; opacity: .62;
}

.tagfilter__clear {
  border: 0; background: none; cursor: pointer; padding: 2px 4px;
  color: var(--text-tertiary); font-size: 11px; text-decoration: underline;
}
.tagfilter__clear:hover { color: var(--text-primary); }

.tagfilter__hint { font-size: 11px; color: var(--text-tertiary); margin-left: auto; }
</style>
