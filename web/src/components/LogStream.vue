<script setup lang="ts">
/**
 * 运行日志（P3-1）。
 *
 * 数据来自后端 5000 条的环形缓冲，脱敏已经在写入端做完——这里拿到的
 * 内容里不会有私钥、口令、token。**所以这个组件绝不做"补一次脱敏"**：
 * 那只会让人误以为展示端是安全边界。
 *
 * 两个体验上的取舍：
 *  - 用户往上翻看时不能自动滚到底。日志每秒都在来，跟着滚等于让人
 *    读不了任何一行。所以"跟随最新"是个显式状态，用户一滚动就关掉。
 *  - 只保留最近 500 条在内存里。后端有 5000 条可回看，但浏览器里
 *    渲染几千行 DOM 会让这一页卡住，而排障时最不需要的就是卡住的界面。
 */
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { connectLogs, type LogEntry } from '@/api/logs'
import SectionCard from '@/components/SectionCard.vue'
import EmptyState from '@/components/EmptyState.vue'

/** 浏览器里最多留多少条。后端有 5000 条，翻更早的请用 docker logs。 */
const KEEP = 500

const entries = ref<LogEntry[]>([])
const connected = ref(false)
const paused = ref(false)
const follow = ref(true)
const keyword = ref('')
const levelFilter = ref<'all' | 'warn' | 'error'>('all')

const scroller = ref<HTMLElement | null>(null)
let disconnect: (() => void) | null = null

const LEVELS = [
  { key: 'all', label: '全部' },
  { key: 'warn', label: '警告以上' },
  { key: 'error', label: '仅错误' }
] as const

function push(e: LogEntry) {
  if (paused.value) return
  entries.value.push(e)
  if (entries.value.length > KEEP) {
    entries.value.splice(0, entries.value.length - KEEP)
  }
}

onMounted(() => {
  disconnect = connectLogs({
    onEntry: push,
    onOpen: () => { connected.value = true },
    onError: () => { connected.value = false }
  })
})

onBeforeUnmount(() => {
  disconnect?.()
  disconnect = null
})

const shown = computed(() => {
  const kw = keyword.value.trim().toLowerCase()
  return entries.value.filter(e => {
    if (levelFilter.value === 'warn' && !isWarnOrWorse(e.level)) return false
    if (levelFilter.value === 'error' && !isError(e.level)) return false
    if (!kw) return true
    const hay = (e.msg + ' ' + Object.entries(e.attrs ?? {}).map(([k, v]) => `${k}=${v}`).join(' ')).toLowerCase()
    return hay.includes(kw)
  })
})

function isError(level: string): boolean {
  return level === 'ERROR'
}
function isWarnOrWorse(level: string): boolean {
  return level === 'WARN' || level === 'ERROR'
}

function levelClass(level: string): string {
  switch (level) {
    case 'ERROR': return 'is-error'
    case 'WARN': return 'is-warn'
    case 'DEBUG': return 'is-debug'
    default: return 'is-info'
  }
}

/** 时间只留时分秒：日志看的是"刚才"，日期在这一页没有信息量。 */
function clock(t: string): string {
  const d = new Date(t)
  if (Number.isNaN(d.getTime())) return t
  return d.toLocaleTimeString('zh-CN', { hour12: false })
}

function attrText(attrs?: Record<string, string>): string {
  if (!attrs) return ''
  return Object.entries(attrs).map(([k, v]) => `${k}=${v}`).join('  ')
}

// 跟随最新：只在用户没有往上翻时滚到底。
watch(shown, async () => {
  if (!follow.value) return
  await nextTick()
  const el = scroller.value
  if (el) el.scrollTop = el.scrollHeight
})

function onScroll() {
  const el = scroller.value
  if (!el) return
  // 距底部 24px 以内算"还在跟随"，避免因像素取整来回抖。
  const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 24
  follow.value = atBottom
}

function clearLocal() {
  entries.value = []
}

/** 断线时手动重连：EventSource 自己会重试，但用户等不了那个退避。 */
function reconnect() {
  disconnect?.()
  entries.value = []
  connected.value = false
  disconnect = connectLogs({
    onEntry: push,
    onOpen: () => { connected.value = true },
    onError: () => { connected.value = false }
  })
}
</script>

<template>
  <SectionCard title="运行日志" :note="connected ? `最近 ${entries.length} 条 · 实时` : '已断开'">
    <template #action>
      <button class="btn btn--sm" :disabled="paused" @click="clearLocal">清屏</button>
      <button class="btn btn--sm" @click="paused = !paused">
        {{ paused ? '继续' : '暂停' }}
      </button>
      <button v-if="!connected" class="btn btn--sm" @click="reconnect">重连</button>
    </template>

    <div class="logs__bar">
      <div class="logs__levels">
        <button v-for="l in LEVELS" :key="l.key" class="logs__level"
                :class="{ 'is-on': levelFilter === l.key }" @click="levelFilter = l.key">
          {{ l.label }}
        </button>
      </div>
      <input v-model="keyword" class="input logs__search" placeholder="搜索消息或字段" spellcheck="false" />
      <span class="logs__hint">
        {{ follow ? '跟随最新' : '已暂停跟随（滚回底部恢复）' }}
      </span>
    </div>

    <div ref="scroller" class="logs__pane" @scroll="onScroll">
      <EmptyState v-if="shown.length === 0" title="还没有日志"
                  desc="服务启动后的日志会实时出现在这里。最多保留最近 500 条，更早的请用 docker logs 查看。" />
      <div v-for="e in shown" :key="e.seq" class="logs__row">
        <span class="logs__time mono">{{ clock(e.time) }}</span>
        <span class="logs__level-tag" :class="levelClass(e.level)">{{ e.level }}</span>
        <span class="logs__msg">{{ e.msg }}</span>
        <span v-if="e.attrs" class="logs__attrs mono">{{ attrText(e.attrs) }}</span>
      </div>
    </div>

    <p class="logs__foot">
      日志在写入时就已脱敏：私钥、口令、token、API 密钥与长十六进制串不会出现在这里。
      本页不落库，刷新即重新从缓冲读取。
    </p>
  </SectionCard>
</template>

<style scoped>
.logs__bar {
  display: flex; align-items: center; gap: 10px;
  padding: 10px 16px; border-bottom: 1px solid var(--border-subtle);
}
.logs__levels { display: inline-flex; gap: 2px; }
.logs__level {
  padding: 3px 8px; border-radius: var(--radius-sm);
  font-size: 11px; color: var(--text-tertiary);
  background: transparent; border: 1px solid transparent; cursor: pointer;
}
.logs__level:hover { color: var(--text-primary); }
.logs__level.is-on {
  color: var(--text-primary);
  background: var(--bg-inset);
  border-color: var(--border-default);
}
.logs__search { flex: 1 1 auto; min-width: 0; height: 26px; font-size: 11px; }
.logs__hint { flex: 0 0 auto; font-size: 11px; color: var(--text-tertiary); }

.logs__pane {
  height: 460px; overflow-y: auto;
  background: var(--bg-inset);
  font-size: 11px; line-height: 18px;
}

.logs__row {
  display: grid; grid-template-columns: 64px 48px 1fr;
  gap: 8px; padding: 1px 16px;
  align-items: baseline;
}
.logs__row:hover { background: var(--bg-surface); }
.logs__time { color: var(--text-tertiary); }
.logs__level-tag { font-size: 10px; font-weight: 600; letter-spacing: .02em; }
.logs__level-tag.is-info { color: var(--text-tertiary); }
.logs__level-tag.is-debug { color: var(--text-tertiary); opacity: .7; }
.logs__level-tag.is-warn { color: var(--warning); }
.logs__level-tag.is-error { color: var(--danger); }
.logs__msg { color: var(--text-primary); word-break: break-word; }
.logs__row:has(.is-error) .logs__msg { color: var(--danger); }
.logs__attrs {
  grid-column: 3; color: var(--text-tertiary);
  font-size: 10px; word-break: break-all;
}

.logs__foot {
  margin: 0; padding: 10px 16px;
  font-size: 11px; color: var(--text-tertiary);
  border-top: 1px solid var(--border-subtle);
}
</style>
