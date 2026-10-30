<script setup lang="ts">
/**
 * 云侧策略模板（P4）。
 *
 * 这一页没有后端逻辑，只有文案。**不要在这上面加功能**——方案 ADR-2 的
 * 结论是「不需要策略生成」：让面板持有 `manage policies` 去生成策略，
 * 等于为了限制面板的权限先给它一个更大的权限。
 *
 * 所以这里做三件事：给出 8 条可直接粘贴的语句、写清四步怎么建组、
 * 让复制足够省事。剩下的交给用户在控制台完成，回来点一次权限自检就知道
 * 配没配对。
 */
import { computed, onMounted, ref, watch } from 'vue'
import { iam, type PolicyTemplateDTO } from '@/api'
import { copy } from '@/lib/format'
import { useStore } from '@/store'
import SectionCard from '@/components/SectionCard.vue'
import CheckList from '@/components/CheckList.vue'

const { toast } = useStore()

const group = ref('')
const tpl = ref<PolicyTemplateDTO | null>(null)
const loading = ref(false)
const errorText = ref('')

async function load() {
  loading.value = true
  errorText.value = ''
  try {
    tpl.value = await iam.policyTemplate(group.value.trim())
  } catch (err) {
    errorText.value = err instanceof Error ? err.message : String(err)
  } finally {
    loading.value = false
  }
}

onMounted(load)
// 组名改动后重新取一次：模板文本是后端拼的，前端不自己拼第二份。
// 加个防抖，别每敲一个字打一次接口。
let timer: ReturnType<typeof setTimeout> | undefined
watch(group, () => {
  if (timer) clearTimeout(timer)
  timer = setTimeout(load, 300)
})

const usingPlaceholder = computed(() => !group.value.trim())

async function copyAll() {
  if (!tpl.value) return
  await copy(tpl.value.text)
  toast({
    tone: 'accent',
    title: '已复制 8 条策略',
    body: usingPlaceholder.value
      ? '粘贴前记得把 <你的组> 换成你实际创建的组名。'
      : ''
  })
}

async function copyLine(text: string) {
  await copy(text)
  toast({ tone: 'accent', title: '已复制这一条' })
}
</script>

<template>
  <SectionCard title="云侧权限模板" :note="`共 8 条 · 版本 ${tpl?.version ?? '—'}`">
    <div class="pad">
      <p class="t-sm">
        面板用 API 密钥接入时，这个用户所属的组需要下面 8 条策略。
        <b>这里没有 manage policies、manage users、manage groups</b>——
        面板不建组、不写策略、不改 IAM。少了这三条，凭据泄露时能造成的破坏
        被限制在「动资源」而不是「改权限」。
      </p>

      <div class="row-inline">
        <label class="field__label" for="grp">你的组名</label>
        <input id="grp" v-model="group" class="input grp" spellcheck="false"
               placeholder="oci-tools-admins" />
        <button class="btn btn--sm" :disabled="loading || !tpl" @click="copyAll()">
          复制全部 8 条
        </button>
      </div>
      <p class="hint">
        {{ usingPlaceholder
          ? '留空则保留占位符 <你的组>，复制后自己替换。填了组名会自动套进去。'
          : `已替换为「${group.trim()}」。改回留空可以恢复占位符。` }}
      </p>

      <p v-if="errorText" class="hint hint--bad">加载失败：{{ errorText }}</p>
    </div>

    <div v-if="tpl" class="lines">
      <div v-for="(l, i) in tpl.lines" :key="i" class="line" @click="copyLine(l.text)">
        <span class="line__no mono">{{ String(i + 1).padStart(2, '0') }}</span>
        <span class="line__text mono">{{ l.text }}</span>
        <span class="line__purpose">{{ l.purpose }}</span>
      </div>
    </div>
  </SectionCard>

  <SectionCard v-if="tpl" title="怎么创建组和用户" note="四步" class="mt">
    <div class="pad">
      <ol class="steps">
        <li v-for="(s, i) in tpl.steps" :key="i">{{ s }}</li>
      </ol>
      <CheckList :items="[
        { tone: 'info', text: tpl.notice },
        { tone: 'ok', text: '配完后回到「账号」页，打开该账号的抽屉 →「权限自检」点一次探测。五个点全绿就说明策略生效了。' }
      ]" />
    </div>
  </SectionCard>
</template>

<style scoped>
.pad { padding: 14px 16px; }

.row-inline { display: flex; align-items: center; gap: 10px; margin-top: 14px; flex-wrap: wrap; }
.field__label { font-size: 11px; color: var(--text-secondary); flex: 0 0 auto; }
.grp { flex: 0 1 240px; min-width: 140px; }
.hint { margin: 6px 0 0; font-size: 11px; color: var(--text-tertiary); }
.hint--bad { color: var(--danger); }

.lines { display: flex; flex-direction: column; }
.line {
  display: grid; grid-template-columns: 24px 1fr; gap: 4px 10px;
  padding: 9px 16px; cursor: copy;
  border-bottom: 1px solid var(--border-subtle);
}
.line:last-child { border-bottom: none; }
.line:hover { background: var(--bg-hover); }
.line__no { color: var(--text-tertiary); font-size: 11px; }
.line__text { font-size: 11px; color: var(--text-primary); word-break: break-all; }
.line__purpose { grid-column: 2; font-size: 11px; color: var(--text-tertiary); }

.steps { margin: 0 0 12px; padding-left: 18px; }
.steps li { font-size: 12px; line-height: 20px; color: var(--text-secondary); margin-bottom: 6px; }

.mt { margin-top: 14px; }
</style>
