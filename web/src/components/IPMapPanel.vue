<script setup lang="ts">
/**
 * IP 分布图（P5-2）。
 *
 * # 为什么是一张「示意图」而不是地图
 *
 * 三条约束叠在一起，结论只能是「不画海岸线与国界」：
 *
 *  1. CSP 是 `default-src 'self'`，加载不了任何外部瓦片服务；
 *  2. 程序要能离线运行，不能依赖在线底图；
 *  3. 自己手绘国界必然在领土要素上出错——台湾、南海诸岛这些地方的画法
 *     有国家标准，随手描一条线就是错的。
 *
 * 所以这里只做等距圆柱投影下的经纬网格 + 点。没有边界，就没有画错边界的
 * 可能；点落在哪，完全由经纬度算出来。界面上也直说了这一点。
 *
 * # 为什么没有离线库也能画
 *
 * GeoLite2 要用户自己去 MaxMind 注册下载，绝大多数部署没有。此时退回
 * OCI 区域的城市中心，source 标成 region。图上不区分这两者会让人以为
 * 面板知道机器在哪栋楼里，所以样式与图例都分开。
 */
import { computed, onMounted, ref } from 'vue'
import { useStore } from '@/store'
import { geo, type GeoPointDTO, type GeoStatusDTO } from '@/api'
import SectionCard from '@/components/SectionCard.vue'
import EmptyState from '@/components/EmptyState.vue'

const { toast, toastError } = useStore()

const points = ref<GeoPointDTO[]>([])
const status = ref<GeoStatusDTO | null>(null)
const notice = ref('')
const loading = ref(false)

async function load() {
  loading.value = true
  try {
    const res = await geo.distribution()
    points.value = res.points
    status.value = res.status
    notice.value = res.notice
  } catch (err) {
    toastError('读取 IP 分布失败', err)
  } finally {
    loading.value = false
  }
}

onMounted(load)

/* ---------- 投影 ---------- */

// 等距圆柱投影：经度线性映射到 x，纬度线性映射到 y。viewBox 2:1 与
// 360°×180° 的比例一致，所以这里不做任何缩放修正。
const W = 720
const H = 360

function px(lon: number): number { return ((lon + 180) / 360) * W }
function py(lat: number): number { return ((90 - lat) / 180) * H }

const meridians = computed(() => {
  const out: number[] = []
  for (let lon = -150; lon <= 150; lon += 30) out.push(px(lon))
  return out
})

const parallels = computed(() => {
  const out: number[] = []
  for (let lat = -60; lat <= 60; lat += 30) out.push(py(lat))
  return out
})

/**
 * 把落在同一格的点合成一个。
 *
 * 按区域估算时，同一区域的多台机器坐标完全相同——不去重的话图上是一个
 * 点，列表里是五台机器，用户会以为图漏了四台。
 */
interface Cluster {
  x: number
  y: number
  lat: number
  lon: number
  items: GeoPointDTO[]
  /** 组里最高的可信度：mmdb/manual > region > 无 */
  best: string
}

const clusters = computed<Cluster[]>(() => {
  const map = new Map<string, Cluster>()
  for (const p of points.value) {
    if (!p.found) continue
    // 半度一格：比这更近的点在 720px 宽的图上本来就画不出区别。
    const key = `${p.latitude.toFixed(1)},${p.longitude.toFixed(1)}`
    const hit = map.get(key)
    if (hit) {
      hit.items.push(p)
      if (rank(p.source) > rank(hit.best)) hit.best = p.source
      continue
    }
    map.set(key, {
      x: px(p.longitude), y: py(p.latitude),
      lat: p.latitude, lon: p.longitude,
      items: [p], best: p.source
    })
  }
  return [...map.values()]
})

function rank(source: string): number {
  switch (source) {
    case 'mmdb': return 3
    case 'manual': return 2
    case 'region': return 1
    default: return 0
  }
}

function radius(c: Cluster): number {
  return Math.min(14, 4 + (c.items.length - 1) * 1.6)
}

function tip(c: Cluster): string {
  const names = c.items.map((p) => `${p.instanceName || p.instanceId} · ${p.ip}`)
  const head = describe(c.items[0])
  return `${head}\n${names.join('\n')}`
}

function describe(p: GeoPointDTO): string {
  if (!p.found) return '位置未知'
  const place = [p.country, p.city].filter(Boolean).join(' ')
  if (p.source === 'region') return `${place || p.region}（按区域估算）`
  return place || '未知地点'
}

const located = computed(() => points.value.filter((p) => p.found))
const unknown = computed(() => points.value.filter((p) => !p.found))

/* ---------- 手填位置 ---------- */

const editing = ref('')
const editLat = ref('')
const editLon = ref('')
const editCity = ref('')

function startEdit(p: GeoPointDTO) {
  editing.value = p.ip
  editLat.value = p.found ? String(p.latitude) : ''
  editLon.value = p.found ? String(p.longitude) : ''
  editCity.value = p.city
}

async function saveEdit(ip: string) {
  const lat = Number(editLat.value)
  const lon = Number(editLon.value)
  if (!Number.isFinite(lat) || !Number.isFinite(lon)) {
    toast({ tone: 'warning', title: '经纬度要填数字' }, 5000)
    return
  }
  try {
    await geo.setManual({ ip, city: editCity.value, latitude: lat, longitude: lon })
    toast({ tone: 'success', title: '已保存', body: `${ip} 的位置已改为手填值。` }, 5000)
    editing.value = ''
    await load()
  } catch (err) {
    toastError('保存失败', err)
  }
}

async function clearManual(ip: string) {
  try {
    await geo.clearManual(ip)
    await load()
  } catch (err) {
    toastError('撤销失败', err)
  }
}

function fmtDate(t: string): string {
  if (!t || t.startsWith('0001-')) return '未知'
  const d = new Date(t)
  return Number.isNaN(d.getTime()) ? t : d.toLocaleDateString('zh-CN')
}
</script>

<template>
  <SectionCard title="IP 分布" note="按经纬度定位，示意图不画国界" class="stack">
    <div class="pad">
      <EmptyState v-if="!points.length && !loading" title="还没有带公网 IP 的实例"
                  hint="有公网 IP 的机器才会出现在这里。" />

      <template v-else>
        <div class="map-wrap">
          <svg class="map" :viewBox="`0 0 ${W} ${H}`" role="img"
               aria-label="实例公网 IP 的经纬度分布示意">
            <!-- 经纬网格。没有海岸线与国界：手绘边界必然在领土要素上出错，
                 而这里不需要边界也能看懂点在哪。 -->
            <g class="grid">
              <line v-for="(x, i) in meridians" :key="`m${i}`" :x1="x" y1="0" :x2="x" :y2="H" />
              <line v-for="(y, i) in parallels" :key="`p${i}`" x1="0" :y1="y" :x2="W" :y2="y" />
            </g>
            <line class="axis" :x1="0" :y1="H / 2" :x2="W" :y2="H / 2" />
            <line class="axis" :x1="W / 2" y1="0" :x2="W / 2" :y2="H" />

            <g v-for="(c, i) in clusters" :key="i">
              <circle :cx="c.x" :cy="c.y" :r="radius(c)"
                      :class="['dot', `dot--${c.best || 'unknown'}`]" />
              <text v-if="c.items.length > 1" :x="c.x" :y="c.y + 3.5"
                    class="dot__count">{{ c.items.length }}</text>
              <title>{{ tip(c) }}</title>
            </g>
          </svg>

          <div class="legend">
            <span class="legend__item"><span class="legend__dot legend__dot--mmdb" />离线库（城市级）</span>
            <span class="legend__item"><span class="legend__dot legend__dot--manual" />手填</span>
            <span class="legend__item"><span class="legend__dot legend__dot--region" />按区域估算</span>
            <span class="legend__item"><span class="legend__dot legend__dot--unknown" />位置未知</span>
          </div>
        </div>

        <p class="caption">
          {{ notice }}
          <template v-if="status && !status.enabled">
            当前未配置离线库：<span class="mono">{{ status.dir }}</span>
          </template>
          <template v-else-if="status?.enabled">
            库构建日期 <span class="mono">{{ fmtDate(status.cityBuiltAt) }}</span>，
            {{ status.hasAsn ? '含运营商信息' : '无运营商库' }}。
          </template>
        </p>

        <!-- 已定位 -->
        <div v-if="located.length" class="list">
          <div v-for="p in located" :key="p.instanceId" class="row">
            <span class="row__name">{{ p.instanceName || p.instanceId }}</span>
            <span class="row__ip mono">{{ p.ip }}</span>
            <span class="row__where">{{ describe(p) }}</span>
            <span v-if="p.asn" class="row__asn mono">{{ p.asn }}</span>
            <span class="row__src" :class="`row__src--${p.source}`">
              {{ p.source === 'region' ? '估算' : p.source === 'manual' ? '手填' : '精确' }}
            </span>
            <button class="btn btn--sm btn--ghost" @click="startEdit(p)">改位置</button>
          </div>
        </div>

        <!-- 未定位：不隐藏。让它们消失比显示「不知道在哪」更糟。 -->
        <div v-if="unknown.length" class="list">
          <p class="list__head">位置未知</p>
          <div v-for="p in unknown" :key="p.instanceId" class="row">
            <span class="row__name">{{ p.instanceName || p.instanceId }}</span>
            <span class="row__ip mono">{{ p.ip }}</span>
            <span class="row__where dim">区域 {{ p.region }} 不在坐标表里，也没配离线库</span>
            <button class="btn btn--sm btn--ghost" @click="startEdit(p)">指定位置</button>
          </div>
        </div>

        <!-- 手填表单 -->
        <div v-if="editing" class="edit">
          <p class="edit__head">
            为 <span class="mono">{{ editing }}</span> 指定位置
          </p>
          <div class="edit__grid">
            <label class="field">
              <span class="field__label">纬度（-90 ~ 90）</span>
              <input v-model="editLat" class="input" inputmode="decimal" placeholder="35.68" />
            </label>
            <label class="field">
              <span class="field__label">经度（-180 ~ 180）</span>
              <input v-model="editLon" class="input" inputmode="decimal" placeholder="139.69" />
            </label>
            <label class="field">
              <span class="field__label">城市（可选）</span>
              <input v-model="editCity" class="input" placeholder="东京" />
            </label>
          </div>
          <div class="edit__acts">
            <button class="btn btn--sm btn--primary" @click="saveEdit(editing)">保存</button>
            <button class="btn btn--sm" @click="editing = ''">取消</button>
            <button class="btn btn--sm btn--ghost" @click="clearManual(editing); editing = ''">
              撤销手填
            </button>
            <span class="t-2xs dim-3">手填的值不会被离线库覆盖</span>
          </div>
        </div>
      </template>
    </div>
  </SectionCard>
</template>

<style scoped>
.map-wrap { position: relative; }

.map {
  width: 100%;
  height: auto;
  display: block;
  border: 1px solid var(--border-subtle);
  border-radius: 8px;
  background: var(--surface-sunken);
}

.grid line { stroke: var(--border-subtle); stroke-width: 1; opacity: 0.7; }
.axis { stroke: var(--border-strong, var(--border-subtle)); stroke-width: 1; opacity: 0.9; }

.dot { stroke: var(--surface, #fff); stroke-width: 1.5; cursor: default; }
.dot--mmdb { fill: var(--accent); }
.dot--manual { fill: var(--success); }
.dot--region { fill: none; stroke: var(--accent); stroke-width: 2; }
.dot--unknown { fill: var(--text-tertiary); opacity: 0.5; }

.dot__count {
  fill: var(--surface, #fff);
  font-size: 9px;
  font-weight: 600;
  text-anchor: middle;
  pointer-events: none;
}

.legend {
  display: flex; flex-wrap: wrap; gap: 14px;
  margin-top: 10px; font-size: 11px; color: var(--text-secondary);
}
.legend__item { display: inline-flex; align-items: center; gap: 5px; }
.legend__dot { width: 9px; height: 9px; border-radius: 50%; display: inline-block; }
.legend__dot--mmdb { background: var(--accent); }
.legend__dot--manual { background: var(--success); }
.legend__dot--region { border: 2px solid var(--accent); }
.legend__dot--unknown { background: var(--text-tertiary); opacity: 0.5; }

.caption {
  margin: 10px 0 0; font-size: 11px; line-height: 1.7; color: var(--text-tertiary);
}

.list { margin-top: 14px; }
.list__head {
  margin: 0 0 6px; font-size: 11px; font-weight: 600; color: var(--text-secondary);
}

.row {
  display: grid;
  grid-template-columns: minmax(0, 1.4fr) minmax(0, 1fr) minmax(0, 1.6fr) auto auto auto;
  align-items: center; gap: 10px;
  padding: 7px 0; border-top: 1px solid var(--border-subtle); font-size: 12px;
}
.row:first-of-type { border-top: none; }
.row__name { font-weight: 500; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.row__ip { color: var(--text-secondary); }
.row__where { color: var(--text-secondary); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.row__asn { color: var(--text-tertiary); font-size: 11px; }
.row__src {
  font-size: 10px; padding: 1px 6px; border-radius: 4px;
  background: var(--surface-sunken); color: var(--text-secondary);
}
.row__src--region { color: var(--warning); }

.edit {
  margin-top: 14px; padding-top: 12px; border-top: 1px solid var(--border-subtle);
}
.edit__head { margin: 0 0 8px; font-size: 12px; }
.edit__grid { display: grid; grid-template-columns: repeat(3, 1fr); gap: 10px; }
@media (max-width: 720px) { .edit__grid { grid-template-columns: 1fr; } }
.field__label { display: block; font-size: 11px; color: var(--text-secondary); margin-bottom: 4px; }
.edit__acts { display: flex; align-items: center; gap: 10px; margin-top: 10px; flex-wrap: wrap; }

.stack { margin-top: 24px; }
</style>
