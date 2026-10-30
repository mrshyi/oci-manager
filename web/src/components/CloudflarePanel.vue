<script setup lang="ts">
/**
 * Cloudflare 联动（P5-1）。
 *
 * 一条纪律写在最前面：**DNS 更新失败不是换 IP 失败**。
 *
 * 换 IP 是「删旧 + 建新」，旧地址一旦释放就回不来了。域名没跟着改，
 * 结果只是「域名打不开，机器本身好着」。如果把这个混成一句「任务失败」，
 * 用户会去重启机器、重装系统，而真正该做的只是点一下「立即同步」。
 * 所以这个面板里，域名状态与任务状态始终分开显示，文案也不说「失败」
 * 而说「域名没跟上」。
 */
import { computed, onMounted, ref } from 'vue'
import { useStore } from '@/store'
import {
  cf as cfApi,
  type CFZoneDTO, type CFRecordDTO, type CFRemoteRecordDTO
} from '@/api'
import SectionCard from '@/components/SectionCard.vue'
import SwitchRow from '@/components/SwitchRow.vue'
import KeyValueList from '@/components/KeyValueList.vue'
import CheckList from '@/components/CheckList.vue'
import EmptyState from '@/components/EmptyState.vue'
import SelectMenu from '@/components/SelectMenu.vue'

const { state, toast, toastError, ask } = useStore()

const zones = ref<CFZoneDTO[]>([])
const records = ref<CFRecordDTO[]>([])
const notice = ref('')
const loading = ref(false)

async function load() {
  loading.value = true
  try {
    const res = await cfApi.list()
    zones.value = res.zones
    records.value = res.records
    notice.value = res.notice
  } catch (err) {
    toastError('读取 Cloudflare 配置失败', err)
  } finally {
    loading.value = false
  }
}

onMounted(load)

/* ---------- 添加域名 ---------- */

const newZone = ref('')
const newToken = ref('')
const addingZone = ref(false)

async function addZone() {
  if (!newZone.value.trim() || !newToken.value.trim()) return
  addingZone.value = true
  try {
    const { zone } = await cfApi.addZone({
      zone: newZone.value.trim(),
      token: newToken.value.trim()
    })
    toast({
      tone: 'success',
      title: '域名已添加',
      body: `${zone.zoneName} 的 token 已验证可用。接下来在下面绑定要跟随的 A 记录。`
    }, 8000)
    newZone.value = ''
    newToken.value = ''
    await load()
  } catch (err) {
    toastError('添加失败', err)
  } finally {
    addingZone.value = false
  }
}

/* ---------- 域名操作 ---------- */

const busyZone = ref('')

async function verifyZone(z: CFZoneDTO) {
  busyZone.value = z.id
  try {
    await cfApi.verifyZone(z.id)
    toast({ tone: 'success', title: 'Token 可用', body: `${z.zoneName} 验证通过。` }, 5000)
    await load()
  } catch (err) {
    toastError('验证失败', err)
  } finally {
    busyZone.value = ''
  }
}

async function toggleZone(z: CFZoneDTO, enabled: boolean) {
  busyZone.value = z.id
  try {
    await cfApi.updateZone(z.id, { enabled })
    await load()
  } catch (err) {
    toastError('修改失败', err)
  } finally {
    busyZone.value = ''
  }
}

function removeZone(z: CFZoneDTO) {
  ask({
    level: 2,
    title: `删除 ${z.zoneName || z.zoneId}？`,
    body: '这个域名下已绑定的记录会一起删掉。Cloudflare 上的 DNS 记录本身不受影响，' +
      '只是面板不再跟着它更新。',
    okLabel: '删除',
    onConfirm: () => { void doRemoveZone(z) }
  })
}

async function doRemoveZone(z: CFZoneDTO) {
  try {
    await cfApi.removeZone(z.id)
    toast({ tone: 'success', title: '已删除', body: `${z.zoneName || z.zoneId} 已移除。` }, 5000)
    await load()
  } catch (err) {
    toastError('删除失败', err)
  }
}

/* ---------- 绑定新记录 ---------- */

const bindZonePk = ref('')
const remote = ref<CFRemoteRecordDTO[]>([])
const remoteLoading = ref(false)
const bindRecordId = ref('')
const bindInstanceId = ref('')
const binding = ref(false)

const zoneOptions = computed(() =>
  zones.value.map((z) => ({ value: z.id, label: z.zoneName || z.zoneId })))

const bindableRecords = computed(() => remote.value.filter((r) => !r.bound))

const instanceOptions = computed(() =>
  state.instances.map((i) => ({
    value: i.id,
    label: `${i.name || i.id}（${i.accountId.slice(0, 4)} · ${i.region}）`
  })))

async function loadRemote() {
  bindRecordId.value = ''
  remote.value = []
  if (!bindZonePk.value) return
  remoteLoading.value = true
  try {
    const res = await cfApi.remoteRecords(bindZonePk.value)
    remote.value = res.records
  } catch (err) {
    toastError('拉取 DNS 记录失败', err)
  } finally {
    remoteLoading.value = false
  }
}

async function bindRecord() {
  const rec = bindableRecords.value.find((r) => r.id === bindRecordId.value)
  const inst = state.instances.find((i) => i.id === bindInstanceId.value)
  if (!rec || !inst) return
  binding.value = true
  try {
    await cfApi.addRecord({
      zonePk: bindZonePk.value,
      recordId: rec.id,
      name: rec.name,
      ttl: rec.ttl,
      proxied: rec.proxied,
      accountId: inst.accountId,
      instanceId: inst.id
    })
    toast({
      tone: 'success',
      title: '已绑定',
      body: `${rec.name} 现在跟随 ${inst.name || inst.id}。这台机器换 IP 时会自动更新。`
    }, 8000)
    bindRecordId.value = ''
    await load()
    await loadRemote()
  } catch (err) {
    toastError('绑定失败', err)
  } finally {
    binding.value = false
  }
}

/* ---------- 记录操作 ---------- */

const busyRecord = ref('')

async function syncRecord(r: CFRecordDTO) {
  busyRecord.value = r.id
  try {
    const res = await cfApi.syncRecord(r.id)
    if (res.status === 'updated') {
      toast({ tone: 'success', title: '已同步', body: `${r.name} → ${res.ip}` }, 6000)
    } else {
      // 不同步不是「失败」而是「没跟上」：机器本身没事。
      toast({
        tone: 'warning',
        title: '域名没跟上',
        body: `${res.detail || '同步未完成'}。机器本身的公网 IP 是 ${res.ip}，不受影响。`
      }, 10000)
    }
    await load()
  } catch (err) {
    toastError('同步失败', err)
  } finally {
    busyRecord.value = ''
  }
}

function removeRecord(r: CFRecordDTO) {
  ask({
    level: 2,
    title: `解除 ${r.name} 的绑定？`,
    body: 'Cloudflare 上的 A 记录不会被删除，只是这台机器换 IP 时面板不再更新它。',
    okLabel: '解除',
    onConfirm: () => { void doRemoveRecord(r) }
  })
}

async function doRemoveRecord(r: CFRecordDTO) {
  try {
    await cfApi.removeRecord(r.id)
    await load()
  } catch (err) {
    toastError('解除失败', err)
  }
}

/* ---------- 展示 ---------- */

const instanceName = computed(() => {
  const m = new Map<string, string>()
  for (const i of state.instances) m.set(i.id, i.name || i.id)
  return m
})

function fmtTime(t: string): string {
  if (!t || t.startsWith('0001-')) return '从未'
  const d = new Date(t)
  return Number.isNaN(d.getTime()) ? t : d.toLocaleString('zh-CN', { hour12: false })
}

function zoneItems(z: CFZoneDTO) {
  return [
    { k: 'Zone ID', v: z.zoneId, mono: true },
    { k: 'Token', v: z.hasToken ? '已配置（加密存储，不回显）' : '未配置' },
    { k: '上次成功同步', v: fmtTime(z.lastSyncAt), mono: true }
  ]
}
</script>

<template>
  <div>
    <SectionCard title="Cloudflare 联动" note="换 IP 后自动更新 A 记录">
      <div class="pad">
        <p class="t-sm">{{ notice || '只接受 API Token，不接受账号密码。' }}</p>
        <CheckList class="mt" :items="[
          { tone: 'info', text: '换 IP 的每一轮都会同步一次：旧 IP 已经释放，域名指着它就是一个打不开的站。' },
          { tone: 'warn', text: 'DNS 更新失败不会让换 IP 判失败。IP 确实换了、机器是通的，只是域名还指着旧地址——任务详情里两个结果分开显示。' }
        ]" />
      </div>
    </SectionCard>

    <SectionCard title="添加域名" class="mt">
      <div class="pad">
        <div class="field">
          <label class="field__label" for="cf-zone">域名或 Zone ID</label>
          <input id="cf-zone" v-model="newZone" class="input" spellcheck="false"
                 placeholder="example.com" autocomplete="off" />
          <p class="hint">可以填域名、Zone ID，也可以直接粘 Cloudflare 控制台地址。</p>
        </div>
        <div class="field">
          <label class="field__label" for="cf-token">API Token</label>
          <input id="cf-token" v-model="newToken" class="input" type="password"
                 spellcheck="false" autocomplete="new-password" />
          <p class="hint">
            权限只需 <span class="mono">Zone → DNS → 编辑</span> 与
            <span class="mono">Zone → Zone → 读取</span>，并限定到具体域名。
            添加时会立刻验证一次，token 不对当场就报。
          </p>
        </div>
        <div class="actions">
          <button class="btn btn--primary"
                  :disabled="!newZone.trim() || !newToken.trim() || addingZone"
                  @click="addZone()">
            {{ addingZone ? '验证中…' : '添加并验证' }}
          </button>
        </div>
      </div>
    </SectionCard>

    <SectionCard title="已添加的域名" :note="`${zones.length} 个`" class="mt">
      <div class="pad">
        <EmptyState v-if="!zones.length && !loading" title="还没有添加域名"
                    hint="添加之后，换 IP 任务会顺带把绑定的 A 记录指向新地址。" />
        <div v-for="z in zones" :key="z.id" class="row">
          <div class="row__head">
            <span class="row__name">{{ z.zoneName || z.zoneId }}</span>
            <span v-if="!z.enabled" class="chip chip--muted">已停用</span>
            <span v-if="z.lastError" class="chip chip--warn">上次出错</span>
          </div>
          <KeyValueList :items="zoneItems(z)" />
          <p v-if="z.lastError" class="err">{{ z.lastError }}</p>
          <div class="row__acts">
            <button class="btn btn--sm" :disabled="busyZone === z.id" @click="verifyZone(z)">
              重新验证
            </button>
            <SwitchRow class="inline-switch" :model-value="z.enabled" title="启用"
                       sub="停用后这台域名不再参与换 IP 后的同步"
                       @update:model-value="(v: boolean) => toggleZone(z, v)" />
            <button class="btn btn--sm btn--danger" :disabled="busyZone === z.id"
                    @click="removeZone(z)">删除</button>
          </div>
        </div>
      </div>
    </SectionCard>

    <SectionCard title="绑定 A 记录" note="让一条记录跟随一台机器" class="mt">
      <div class="pad">
        <EmptyState v-if="!zones.length" title="先添加一个域名"
                    hint="绑定要挂在某个域名下。" />

        <template v-else>
          <div class="grid2">
            <div class="field">
              <label class="field__label">域名</label>
              <SelectMenu :groups="[{ label: '域名', options: zoneOptions }]"
                          :model-value="bindZonePk" placeholder="选择域名"
                          @update:model-value="(v: string) => { bindZonePk = v; loadRemote() }" />
            </div>
            <div class="field">
              <label class="field__label">A 记录</label>
              <SelectMenu :groups="[{ label: 'A 记录',
                            options: bindableRecords.map((r) => ({ value: r.id, label: r.name })) }]"
                          :model-value="bindRecordId" :disabled="!bindZonePk || remoteLoading"
                          :placeholder="remoteLoading ? '拉取中…'
                            : bindableRecords.length ? '选择记录' : '没有可绑定的 A 记录'"
                          @update:model-value="(v: string) => bindRecordId = v" />
            </div>
          </div>

          <div class="field">
            <label class="field__label">跟随哪台实例</label>
            <SelectMenu :groups="[{ label: '实例', options: instanceOptions }]"
                        :model-value="bindInstanceId" placeholder="选择实例"
                        @update:model-value="(v: string) => bindInstanceId = v" />
          </div>

          <div class="actions">
            <button class="btn btn--primary"
                    :disabled="!bindRecordId || !bindInstanceId || binding"
                    @click="bindRecord()">
              {{ binding ? '绑定中…' : '绑定' }}
            </button>
            <span v-if="bindZonePk && !remoteLoading && !bindableRecords.length" class="t-2xs dim-3">
              这个域名下的 A 记录都绑过了，或者还没有 A 记录。
            </span>
          </div>
        </template>
      </div>
    </SectionCard>

    <SectionCard title="已绑定的记录" :note="`${records.length} 条`" class="mt">
      <div class="pad">
        <EmptyState v-if="!records.length && !loading" title="还没有绑定记录"
                    hint="绑定之后，这台机器每次换到新 IP 都会自动更新对应的 A 记录。" />
        <div v-for="r in records" :key="r.id" class="row">
          <div class="row__head">
            <span class="row__name">{{ r.name }}</span>
            <span class="chip">{{ r.type }}</span>
            <span v-if="r.proxied" class="chip">Cloudflare 代理</span>
            <span v-if="r.lastError" class="chip chip--warn">域名没跟上</span>
          </div>
          <KeyValueList :items="[
            { k: '域名', v: r.zoneName || r.zonePk, mono: true },
            { k: '跟随实例', v: instanceName.get(r.instanceId) || r.instanceId, mono: true },
            { k: '上次写入', v: r.lastIp || '尚未写入', mono: true },
            { k: '上次成功', v: fmtTime(r.lastSyncAt), mono: true }
          ]" />
          <p v-if="r.lastError" class="err">{{ r.lastError }}</p>
          <div class="row__acts">
            <button class="btn btn--sm" :disabled="busyRecord === r.id" @click="syncRecord(r)">
              {{ busyRecord === r.id ? '同步中…' : '立即同步' }}
            </button>
            <button class="btn btn--sm btn--danger" :disabled="busyRecord === r.id"
                    @click="removeRecord(r)">解除绑定</button>
          </div>
        </div>
      </div>
    </SectionCard>
  </div>
</template>

<style scoped>
.field { margin-top: 12px; }
.field__label { display: block; font-size: 11px; color: var(--text-secondary); margin-bottom: 4px; }
.hint { margin: 4px 0 0; font-size: 11px; color: var(--text-tertiary); line-height: 1.6; }

.actions { display: flex; align-items: center; gap: 10px; margin-top: 14px; flex-wrap: wrap; }

.grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
@media (max-width: 720px) { .grid2 { grid-template-columns: 1fr; } }

.row { padding: 12px 0; border-top: 1px solid var(--border-subtle); }
.row:first-child { border-top: none; padding-top: 0; }
.row__head { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; margin-bottom: 8px; }
.row__name { font-weight: 600; font-size: 13px; }
.row__acts { display: flex; align-items: center; gap: 10px; margin-top: 10px; flex-wrap: wrap; }

.inline-switch { margin: 0; }

.err { margin: 6px 0 0; font-size: 11px; color: var(--warning); line-height: 1.6; }

.chip {
  font-size: 10px; padding: 1px 6px; border-radius: 4px;
  background: var(--surface-sunken); color: var(--text-secondary);
}
.chip--muted { opacity: 0.6; }
.chip--warn { background: color-mix(in srgb, var(--warning) 16%, transparent); color: var(--warning); }

.mt { margin-top: 14px; }
</style>
