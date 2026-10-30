<script setup lang="ts">
/**
 * 备份与恢复（P3-2）。
 *
 * 这个面板的措辞比别处更保守，因为两个方向都可能造成不可逆的损失：
 *  - 导出：文件里装着全部账号的私钥密文。丢了它无所谓，泄露它等于交钥匙。
 *  - 恢复：整体替换数据库。错了之后连「用面板修回来」的入口都没了。
 *
 * 三处刻意写死在文案里的事实：
 *  1. 恢复**必须重启**才生效。数据库文件被进程打开着，运行中替换会把库写坏。
 *     不能让用户点完看到「成功」就去干别的，几天后重启发现数据回去了。
 *  2. 默认不打包主密钥。恢复到新机器必须同时带上原 master.key，
 *     否则账号私钥解不开——这是最容易在迁移时踩的坑。
 *  3. 换主密钥是不可逆的另一个方向：它会让当前库里其他账号的私钥失效。
 */
import { computed, onMounted, ref } from 'vue'
import { useStore } from '@/store'
import { backup as backupApi, type BackupStatusDTO, type RestoreResultDTO } from '@/api'
import SectionCard from '@/components/SectionCard.vue'
import SwitchRow from '@/components/SwitchRow.vue'
import KeyValueList from '@/components/KeyValueList.vue'
import CheckList from '@/components/CheckList.vue'

const { toast, toastError } = useStore()

/* ---------- 导出 ---------- */

const exportPassword = ref('')
const exportPassword2 = ref('')
const includeMasterKey = ref(false)
const exporting = ref(false)

const exportReady = computed(() =>
  exportPassword.value.length >= 8 && exportPassword.value === exportPassword2.value)

async function doExport() {
  if (!exportReady.value) return
  exporting.value = true
  try {
    const blob = await backupApi.exportArchive(exportPassword.value, includeMasterKey.value)
    // 用 createObjectURL 而不是拼一个 GET 链接：口令在请求体里，
    // 不能出现在 URL（会被代理与访问日志记下来）。
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `oci-tools-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, '')}.ocibak`
    document.body.appendChild(a)
    a.click()
    a.remove()
    URL.revokeObjectURL(url)
    toast({
      tone: 'success',
      title: '备份已导出',
      body: includeMasterKey.value
        ? '这份文件里既有全部账号的私钥密文，也有主密钥——请当作最高敏感数据保管。'
        : '请同时保管好原 master.key：恢复到新机器时没有它，账号私钥解不开。'
    }, 10000)
    exportPassword.value = ''
    exportPassword2.value = ''
  } catch (err) {
    toastError('导出失败', err)
  } finally {
    exporting.value = false
  }
}

/* ---------- 恢复 ---------- */

const restoreFile = ref<File | null>(null)
const restorePassword = ref('')
const allowMasterKeyChange = ref(false)
const restoring = ref(false)
const restoreResult = ref<RestoreResultDTO | null>(null)

function onPickFile(e: Event) {
  const input = e.target as HTMLInputElement
  restoreFile.value = input.files?.[0] ?? null
  restoreResult.value = null
}

const canRestore = computed(() =>
  !!restoreFile.value && restorePassword.value.length > 0 && !restoring.value)

async function doRestore() {
  const file = restoreFile.value
  if (!file) return
  restoring.value = true
  restoreResult.value = null
  try {
    // 没有二次确认框：后端的 L4 复核（TOTP + 回传 RESTORE）才是防线，
    // 前端再弹一层只会让人习惯性点「确定」。
    restoreResult.value = await backupApi.restore(file, restorePassword.value, allowMasterKeyChange.value)
    toast({
      tone: 'warning',
      title: '备份已就绪，需要重启才生效',
      body: '数据库文件正被进程打开，运行中替换会把库写坏。重启后会自动完成替换，并先备份当前库。'
    }, 15000)
  } catch (err) {
    toastError('恢复失败', err)
  } finally {
    restoring.value = false
  }
}

/* ---------- 状态 ---------- */

const status = ref<BackupStatusDTO | null>(null)

async function loadStatus() {
  try {
    status.value = await backupApi.status()
  } catch (err) {
    toastError('读取备份状态失败', err)
  }
}

onMounted(loadStatus)

const pendingItems = computed(() => {
  const s = status.value
  if (!s?.pending) return []
  return [
    { k: '备份创建于', v: s.manifest ? fmtTime(s.manifest.createdAt) : '未知', mono: true },
    { k: '解包时间', v: s.stagedAt ? fmtTime(s.stagedAt) : '未知', mono: true },
    { k: '库大小', v: s.manifest ? `${(s.manifest.dbBytes / 1024 / 1024).toFixed(2)} MB` : '未知', mono: true },
    { k: '包含主密钥', v: s.masterKeyPending ? '是（重启后会一并替换）' : '否' }
  ]
})

function fmtTime(t: string): string {
  const d = new Date(t)
  return Number.isNaN(d.getTime()) ? t : d.toLocaleString('zh-CN', { hour12: false })
}
</script>

<template>
  <div>
    <SectionCard v-if="status?.pending" title="有一份恢复等待生效"
                 note="重启服务后自动完成，替换前会先备份当前库">
      <div class="pad">
        <KeyValueList :items="pendingItems" />
        <CheckList class="mt" :items="[
          { tone: 'warn', text: '重启之前一切照旧：当前数据库仍在正常服务，这份备份只是躺在磁盘上等重启。' },
          { tone: 'info', text: '重启时会把当前库另存为 *.before-restore-<时间>，恢复错了还能手工换回来。' }
        ]" />
      </div>
    </SectionCard>

    <SectionCard title="导出备份" :note="includeMasterKey ? '包含主密钥' : '不含主密钥'">
      <div class="pad">
        <p class="t-sm">
          整库加密导出：账号、实例、标签、审计与全部凭据密文。文件用 AES-256-GCM 加密，
          密钥由口令经 argon2id 派生——<b>口令丢了，这份备份就永远打不开</b>。
        </p>

        <div class="field">
          <label class="field__label" for="bk-pw">备份口令（至少 8 位）</label>
          <input id="bk-pw" v-model="exportPassword" class="input" type="password"
                 autocomplete="new-password" spellcheck="false" />
        </div>
        <div class="field">
          <label class="field__label" for="bk-pw2">再输一次</label>
          <input id="bk-pw2" v-model="exportPassword2" class="input" type="password"
                 autocomplete="new-password" spellcheck="false" />
        </div>
        <p v-if="exportPassword2 && exportPassword !== exportPassword2" class="field__err">
          两次输入不一致
        </p>

        <SwitchRow v-model="includeMasterKey" title="同时打包主密钥"
                   sub="默认不打包。勾上之后这份文件同时含主密钥与全部私钥密文，泄露它等于交出所有账号；好处是迁到新机器时不必再单独拷 master.key。" />

        <div class="actions">
          <button class="btn btn--primary" :disabled="!exportReady || exporting" @click="doExport()">
            {{ exporting ? '导出中…' : '导出加密备份' }}
          </button>
          <span v-if="!exportReady" class="t-2xs dim-3">
            口令至少 8 位且两次一致才能导出
          </span>
        </div>
      </div>
    </SectionCard>

    <SectionCard title="从备份恢复" note="需要两步验证 + 重启生效" class="mt">
      <div class="pad">
        <CheckList :items="[
          { tone: 'warn', text: '恢复会整体替换当前数据库：账号、实例、标签、审计全部回到备份那一刻。' },
          { tone: 'info', text: '数据库文件正被进程打开着，运行中替换会把库写坏，所以恢复只在重启时生效。' },
          { tone: 'info', text: '恢复到新机器时，若备份未打包主密钥，需要先把原 master.key 放到数据目录，否则账号私钥解不开。' }
        ]" />

        <div class="field">
          <label class="field__label" for="bk-file">备份文件（.ocibak）</label>
          <input id="bk-file" class="input" type="file" accept=".ocibak,application/octet-stream"
                 @change="onPickFile" />
        </div>
        <div class="field">
          <label class="field__label" for="bk-rpw">备份口令</label>
          <input id="bk-rpw" v-model="restorePassword" class="input" type="password"
                 autocomplete="off" spellcheck="false" />
        </div>

        <SwitchRow v-model="allowMasterKeyChange" title="允许替换主密钥"
                   sub="只有在备份里打包了主密钥、且与当前不同时才需要。开启会让当前库中其他账号的私钥无法解密——那是不可逆的。" />

        <div class="actions">
          <button class="btn btn--danger" :disabled="!canRestore" @click="doRestore()">
            {{ restoring ? '校验并暂存…' : '恢复这份备份' }}
          </button>
          <span class="t-2xs dim-3">会要求两步验证码，并回传确认词 RESTORE</span>
        </div>

        <div v-if="restoreResult" class="result">
          <CheckList :items="[
            { tone: 'warn', text: '已校验并暂存，**重启服务后生效**。' },
            { tone: 'info', text: `备份创建于 ${fmtTime(restoreResult.manifest.createdAt)}，库大小 ${(restoreResult.manifest.dbBytes / 1024 / 1024).toFixed(2)} MB。` },
            { tone: 'info', text: restoreResult.masterKeyDiffers
                ? '备份里的主密钥与当前不同，重启后会一并替换——届时当前库中其他账号的私钥将无法解密。'
                : '主密钥一致，恢复后账号私钥可以正常解密。' }
          ]" />
        </div>
      </div>
    </SectionCard>

    <p class="foot">
      备份格式版本 {{ status?.backupVersion ?? 1 }}。容器为
      <span class="mono">MAGIC | version | salt | nonce | AES-256-GCM</span>，
      头部同时作为 GCM 的附加认证数据——改任何一个字节都会导致导入失败。
    </p>
  </div>
</template>

<style scoped>
.field { margin-top: 12px; }
.field__label { display: block; font-size: 11px; color: var(--text-secondary); margin-bottom: 4px; }
.field__err { margin: 4px 0 0; font-size: 11px; color: var(--danger); }

.actions { display: flex; align-items: center; gap: 10px; margin-top: 14px; flex-wrap: wrap; }

.result { margin-top: 14px; padding-top: 12px; border-top: 1px solid var(--border-subtle); }

.foot { margin: 14px 0 0; font-size: 11px; color: var(--text-tertiary); }

.mt { margin-top: 14px; }
</style>
