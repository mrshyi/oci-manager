<script setup lang="ts">
/**
 * 账号整理对话框：标签、分组、备注、到期日、排序权重。
 *
 * 没有做成 Drawer 的一种 tab：抽屉是「看这个账号」的地方，而这里改的是
 * 「这个账号在列表里怎么被找到」。两者的心智模型不同，混在一起会让用户
 * 在抽屉里翻了半天才想起来标签在哪。
 *
 * 标签输入用 datalist 而不是自制的下拉：自由文本 + 浏览器原生补全，
 * 既能让用户从已有标签里挑，也不阻止他打一个全新的。
 */
import { computed, nextTick, ref, watch } from 'vue'
import type { Account } from '@/types'

const props = defineProps<{
  account: Account
  /** 已有标签，用于补全 */
  knownTags: { tag: string; count: number }[]
  /** 已有分组名，用于补全 */
  knownGroups: string[]
}>()

const emit = defineEmits<{
  close: []
  save: [payload: {
    tags: string[]
    groupName: string
    note: string
    expiresAt: string | null
    sortWeight: number
  }]
}>()

/** 单个标签最长 24 字，与服务端保持一致。超了会被拒，不如在输入框里就拦住。 */
const MAX_TAG = 24
const MAX_NOTE = 200
const MAX_GROUP = 32

const tags = ref<string[]>([...props.account.tags])
const draft = ref('')
const group = ref(props.account.group)
const note = ref(props.account.note)
/** <input type="date"> 只认 YYYY-MM-DD，所以这里截到日 */
const expires = ref(props.account.expiresAt ? props.account.expiresAt.slice(0, 10) : '')
const weight = ref(props.account.sortWeight)

const inputEl = ref<HTMLInputElement>()
const saving = ref(false)

watch(() => props.account.id, () => {
  tags.value = [...props.account.tags]
  group.value = props.account.group
  note.value = props.account.note
  expires.value = props.account.expiresAt ? props.account.expiresAt.slice(0, 10) : ''
  weight.value = props.account.sortWeight
  draft.value = ''
})

const suggestions = computed(() => {
  // 已经加过的不用再提示。
  const has = new Set(tags.value.map(t => t.toLowerCase()))
  return props.knownTags.filter(t => !has.has(t.tag.toLowerCase()))
})

function addTag(raw?: string) {
  const v = (raw ?? draft.value).trim()
  if (!v || v.length > MAX_TAG) return
  if (tags.value.some(t => t.toLowerCase() === v.toLowerCase())) {
    draft.value = ''
    return
  }
  tags.value = [...tags.value, v]
  draft.value = ''
  nextTick(() => inputEl.value?.focus())
}

function removeTag(tag: string) {
  tags.value = tags.value.filter(t => t !== tag)
}

/** 逗号 / 回车都能确认输入。粘贴一段「东京,主力」也应当变成两个标签。 */
function onKeydown(e: KeyboardEvent) {
  if (e.key === 'Enter' || e.key === ',' || e.key === '，') {
    e.preventDefault()
    // 一次粘贴多个时按分隔符拆开，逐个加。
    for (const part of draft.value.split(/[,，]/)) addTag(part)
  } else if (e.key === 'Backspace' && !draft.value && tags.value.length) {
    // 输入框空了再按退格，删掉最后一个标签。这是标签输入的通行手感。
    tags.value = tags.value.slice(0, -1)
  }
}

function onPaste(e: ClipboardEvent) {
  const text = e.clipboardData?.getData('text') ?? ''
  if (!/[,，]/.test(text)) return
  e.preventDefault()
  for (const part of text.split(/[,，]/)) addTag(part)
}

function submit() {
  if (saving.value) return
  saving.value = true
  emit('save', {
    tags: tags.value,
    groupName: group.value.trim(),
    note: note.value.trim(),
    // 空串传 null：服务端把 null 当成「不设到期」，空串会被判成格式错误。
    expiresAt: expires.value || null,
    sortWeight: Number.isFinite(weight.value) ? weight.value : 0
  })
}

/** 保存成功后由父组件关闭；失败时父组件不会关，这里要把按钮恢复可点。 */
function done() {
  saving.value = false
}
defineExpose({ done })
</script>

<template>
  <div class="overlay" @click.self="emit('close')">
    <div class="dialog card" role="dialog" aria-modal="true" aria-label="整理账号">
      <header class="dialog__head">
        <h2 class="dialog__title">整理「{{ account.alias }}」</h2>
        <button class="dialog__x" aria-label="关闭" @click="emit('close')">✕</button>
      </header>

      <div class="dialog__body">
        <label class="field">
          <span class="field__label">标签</span>
          <div class="tageditor">
            <span v-for="t in tags" :key="t" class="tageditor__chip">
              {{ t }}
              <button type="button" class="tageditor__x" :aria-label="`移除 ${t}`"
                      @click="removeTag(t)">✕</button>
            </span>
            <input ref="inputEl" v-model="draft" class="tageditor__input"
                   :maxlength="MAX_TAG" list="tag-suggest"
                   placeholder="输入后回车，逗号可分隔"
                   @keydown="onKeydown" @paste="onPaste" @blur="addTag()">
            <datalist id="tag-suggest">
              <option v-for="t in suggestions" :key="t.tag" :value="t.tag">{{ t.count }} 个账号</option>
            </datalist>
          </div>
          <span class="field__hint">自由文本，最多 20 个、每个 {{ MAX_TAG }} 字以内。大小写不同的同名标签会合并。</span>
        </label>

        <label class="field">
          <span class="field__label">分组</span>
          <input v-model="group" class="input" :maxlength="MAX_GROUP" list="group-suggest"
                 placeholder="留空表示不分组">
          <datalist id="group-suggest">
            <option v-for="g in knownGroups" :key="g" :value="g" />
          </datalist>
        </label>

        <label class="field">
          <span class="field__label">备注</span>
          <textarea v-model="note" class="input input--area" :maxlength="MAX_NOTE" rows="2"
                    placeholder="例如：主力号，别动里面的实例" />
        </label>

        <div class="field-row">
          <label class="field">
            <span class="field__label">到期日</span>
            <input v-model="expires" type="date" class="input">
            <span class="field__hint">留空表示不提醒</span>
          </label>
          <label class="field">
            <span class="field__label">排序权重</span>
            <input v-model.number="weight" type="number" class="input" min="-1000" max="1000">
            <span class="field__hint">大的排前面，0 为默认</span>
          </label>
        </div>
      </div>

      <footer class="dialog__foot">
        <button class="btn" :disabled="saving" @click="emit('close')">取消</button>
        <button class="btn btn--primary" :disabled="saving" @click="submit">
          {{ saving ? '保存中…' : '保存' }}
        </button>
      </footer>
    </div>
  </div>
</template>

<style scoped>
.overlay {
  position: fixed; inset: 0; z-index: 60; display: flex;
  align-items: center; justify-content: center; padding: 24px;
  background: color-mix(in srgb, var(--bg-base) 72%, transparent);
  backdrop-filter: blur(2px);
}
.dialog {
  width: min(520px, 100%); max-height: calc(100vh - 48px);
  display: flex; flex-direction: column; overflow: hidden;
}
.dialog__head {
  display: flex; align-items: center; gap: 12px;
  padding: 16px 18px; border-bottom: 1px solid var(--border-subtle);
}
.dialog__title { margin: 0; flex: 1 1 auto; font-size: 14px; font-weight: 600; }
.dialog__x { border: 0; background: none; color: var(--text-secondary); cursor: pointer; font-size: 13px; }
.dialog__body { padding: 16px 18px; overflow-y: auto; display: flex; flex-direction: column; gap: 14px; }
.dialog__foot {
  display: flex; justify-content: flex-end; gap: 8px;
  padding: 12px 18px; border-top: 1px solid var(--border-subtle);
}

.field { display: flex; flex-direction: column; gap: 6px; }
.field__label { font-size: 11px; font-weight: 600; color: var(--text-secondary); }
.field__hint { font-size: 10px; color: var(--text-tertiary); }
.field-row { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }

.input {
  width: 100%; padding: 7px 10px; font-size: 12px;
  border: 1px solid var(--border-default); border-radius: var(--radius-md);
  background: var(--bg-surface); color: var(--text-primary);
  font-family: inherit;
}
.input:focus { outline: 2px solid var(--accent); outline-offset: -1px; }
.input--area { resize: vertical; min-height: 52px; }

/* 标签编辑器做成"输入框里长标签"的样子：边框画在外层，输入框本身无边框，
   这样标签换行时视觉上仍然是一个整体。 */
.tageditor {
  display: flex; flex-wrap: wrap; align-items: center; gap: 6px;
  padding: 6px 8px; min-height: 38px;
  border: 1px solid var(--border-default); border-radius: var(--radius-md);
  background: var(--bg-surface);
}
.tageditor:focus-within { outline: 2px solid var(--accent); outline-offset: -1px; }
.tageditor__chip {
  display: inline-flex; align-items: center; gap: 4px;
  padding: 2px 4px 2px 8px; border-radius: var(--radius-full);
  background: var(--bg-inset); color: var(--text-secondary); font-size: 11px;
}
.tageditor__x {
  border: 0; background: none; cursor: pointer; color: var(--text-tertiary);
  font-size: 9px; line-height: 1; padding: 2px;
}
.tageditor__x:hover { color: var(--danger); }
.tageditor__input {
  flex: 1 1 120px; min-width: 120px; border: 0; outline: 0; background: transparent;
  color: var(--text-primary); font-size: 12px; font-family: inherit; padding: 2px 0;
}
</style>
