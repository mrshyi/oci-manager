<script setup lang="ts">
/**
 * 实例的运维操作面板。
 *
 * 目前只有「按网段换 IP」。它和上游那个「更换公网 IP」不是一回事：
 *   - 上游的换一次就结束，新 IP 是什么看运气
 *   - 这个会一直换到落进目标网段、用尽次数或超时为止，适合「想要某个
 *     特定 IP 段」的场景（比如某段 IP 的落地解锁流媒体）
 *
 * 参数只传 instanceId，region / vnicId / compartmentId 由后端从实例缓存
 * 补齐——前端实例模型里没有这两个字段，它们是 OCI 的内部概念。
 */
import { computed, ref } from 'vue'
import SectionCard from '@/components/SectionCard.vue'
import { useStore } from '@/store'
import { opsTasks } from '@/api/opsTasks'
import type { Instance } from '@/types'

const props = defineProps<{ instance: Instance }>()
const { ask, toast, toastError } = useStore()

/** 目标网段，每行一个。用 textarea 而不是标签输入：多数人是从别处粘一段过来 */
const cidrs = ref('')
const attempts = ref(30)
const minutes = ref(30)
const busy = ref(false)

const hasPublicIp = computed(
  () => !!props.instance.publicIp && props.instance.publicIp !== '—'
)

/** 拆成行、去空、去重。用户常把同一段贴两遍，重复会让后端多换一轮 */
const parsedCidrs = computed(() => {
  const seen = new Set<string>()
  for (const raw of cidrs.value.split(/[\n,;]+/)) {
    const c = raw.trim()
    if (c) seen.add(c)
  }
  return [...seen]
})

/**
 * 只做「看起来像 CIDR」的检查，真正的合法性交给后端。
 *
 * 前端不重复实现 netip.ParsePrefix：IPv6 的合法写法太多，自己写正则一定会
 * 漏，而漏掉时用户拿到的是「格式不对」却又看不出哪里不对。
 */
const looksLikeCidr = computed(() =>
  parsedCidrs.value.length > 0 && parsedCidrs.value.every((c) => /^[0-9a-fA-F:.]+\/\d{1,3}$/.test(c))
)

const canSubmit = computed(
  () => hasPublicIp.value && looksLikeCidr.value && !busy.value
)

const tooMany = computed(() => parsedCidrs.value.length > 20)

function submit() {
  if (!canSubmit.value) return
  const list = parsedCidrs.value
  ask({
    level: 2,
    title: `按网段换 IP：${props.instance.name}`,
    body:
      `最多换 ${attempts.value} 次、最长 ${minutes.value} 分钟，直到落进 ${list.join('、')} 为止。\n\n` +
      '每换一次都会先删除旧 IP 再申请新 IP，旧地址不可找回，SSH 连接会中断。' +
      '任务在后台跑，可以在「运维任务」页看进度，也可以随时暂停或取消。',
    okLabel: '开始换 IP',
    onConfirm: () => { void doSubmit(list) }
  })
}

async function doSubmit(list: string[]) {
  busy.value = true
  try {
    const { task, notice } = await opsTasks.createChangeIP({
      instanceId: props.instance.id,
      targetCidrs: list,
      maxAttempts: attempts.value,
      maxDurationMinutes: minutes.value
    })
    toast(
      {
        tone: 'accent',
        title: '换 IP 任务已创建',
        body: notice || `任务 ${task.id.slice(0, 8)} 已进入队列，可在「运维任务」页查看进度。`,
        link: { label: '查看运维任务', to: '/ops-tasks' }
      },
      12000
    )
    cidrs.value = ''
  } catch (err) {
    toastError('创建换 IP 任务失败', err)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <SectionCard title="按网段换 IP">
    <p class="ops__intro">
      反复更换公网 IP，直到落进目标网段为止。与「更换公网 IP」不同：那个换一次就结束，
      这个会一直换到命中、用尽次数或超时。
    </p>

    <p v-if="!hasPublicIp" class="ops__warn">
      这台实例没有公网 IP，换 IP 无从谈起。先去网络页分配一个，或确认实例状态。
    </p>

    <label class="field">
      <span class="field__label">目标网段</span>
      <textarea
        v-model="cidrs"
        class="textarea"
        rows="3"
        spellcheck="false"
        placeholder="198.51.100.0/24&#10;203.0.113.0/24"
        :disabled="!hasPublicIp || busy"
      />
      <span class="field__hint">
        每行一个 CIDR，也可以逗号分隔。留空则无法提交。
      </span>
    </label>

    <div class="ops__row">
      <label class="field">
        <span class="field__label">最多换</span>
        <input v-model.number="attempts" class="input" type="number" min="1" max="100"
               :disabled="!hasPublicIp || busy" />
        <span class="field__hint">次，1–100</span>
      </label>
      <label class="field">
        <span class="field__label">最长运行</span>
        <input v-model.number="minutes" class="input" type="number" min="1" max="120"
               :disabled="!hasPublicIp || busy" />
        <span class="field__hint">分钟，1–120</span>
      </label>
    </div>

    <p v-if="tooMany" class="ops__warn">
      目标网段超过 20 个了。段越多命中越快，但每次换 IP 都要真的删建一次，
      建议先只填最想要的那几段。
    </p>

    <div class="ops__actions">
      <button class="btn btn--primary" :disabled="!canSubmit" @click="submit">
        {{ busy ? '提交中…' : '开始换 IP' }}
      </button>
      <span v-if="parsedCidrs.length" class="ops__count">
        已填 {{ parsedCidrs.length }} 个网段
      </span>
    </div>
  </SectionCard>
</template>

<style scoped>
.ops__intro {
  margin: 0 0 14px;
  font-size: 12px;
  line-height: 1.7;
  color: var(--text-secondary);
}

.ops__warn {
  margin: 0 0 12px;
  padding: 8px 10px;
  border-radius: 6px;
  border: 1px solid var(--border-strong);
  background: var(--bg-sunken);
  font-size: 11px;
  line-height: 1.7;
  color: var(--text-secondary);
}

.ops__row {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 12px;
  margin-top: 14px;
}

.ops__actions {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-top: 16px;
}

.ops__count {
  font-size: 11px;
  color: var(--text-tertiary, var(--text-secondary));
}
</style>
