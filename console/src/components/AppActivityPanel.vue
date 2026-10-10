<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import TimeChart, { type ChartBand, type ChartLine, type ChartMarker } from '@/components/activity/TimeChart.vue'
import { ACTIVITY_RANGES, fetchAppActivity, type ActivityRange, type AppActivity, type Series } from '@/api/activity'
import type { AutoscaleConfig } from '@/api/apps'
import { changeObservation, changeSummary, DEFAULT_TOLERANCE, markerLook, targetGuide, trafficNote, trafficPartial } from '@/utils/activity'
import { capacityBadges } from '@/utils/capacity'
import { formatCpu, formatMemory } from '@/utils/resources'

const props = defineProps<{ project: string; appName: string; config: AutoscaleConfig | null }>()

const RANGE_KEY = 'kipper_activity_range'
const REFRESH_MS = 60_000

function storedRange(): ActivityRange {
  try {
    const v = localStorage.getItem(RANGE_KEY)
    if (v && (ACTIVITY_RANGES as readonly string[]).includes(v)) return v as ActivityRange
  } catch {
    // Storage can be blocked; the default range applies.
  }
  return '1h'
}

const range = ref<ActivityRange>(storedRange())
const activity = ref<AppActivity | null>(null)
const loading = ref(false)
const failed = ref(false)
const unsupported = ref(false)
const hover = ref<number | null>(null)
const activeMarker = ref<number | null>(null)
const frozenReadout = ref<string | null>(null)
const frozenChange = ref<AppActivity['changes'][number] | null>(null)
const metric = ref<'cpu' | 'memory'>('cpu')

watch(
  () => props.config,
  c => {
    if (c && c.memory_target > 0 && c.cpu_target === 0) metric.value = 'memory'
  },
  { immediate: true },
)

// Only apply the latest request so overlapping loads keep the selected range current.
let loadSeq = 0

async function load() {
  const seq = ++loadSeq
  loading.value = true
  try {
    const data = await fetchAppActivity(props.project, props.appName, range.value)
    if (seq !== loadSeq) return
    const selectedIndex = activeMarker.value
    if (selectedIndex !== null && data.changes[selectedIndex]?.time !== activity.value?.changes[selectedIndex]?.time) activeMarker.value = null
    activity.value = data
    failed.value = false
    unsupported.value = false
  } catch (err) {
    if (seq !== loadSeq) return
    // Treat a text 404 as an older API without this endpoint. Handler errors,
    // including a missing app, use JSON.
    const response = (err as { response?: { status?: number; data?: unknown } }).response
    unsupported.value = response?.status === 404 && typeof response.data === 'string'
    failed.value = !unsupported.value
  } finally {
    if (seq === loadSeq) loading.value = false
  }
}

function selectRange(r: ActivityRange) {
  range.value = r
  try {
    localStorage.setItem(RANGE_KEY, r)
  } catch {
    // Remembering the range is a convenience.
  }
  hover.value = null
  activeMarker.value = null
  frozenReadout.value = null
  frozenChange.value = null
  load()
}

let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  load()
  timer = setInterval(() => {
    if (document.visibilityState === 'visible') load()
  }, REFRESH_MS)
})
onUnmounted(() => clearInterval(timer))

const ts = computed(() => activity.value?.timestamps ?? [])
const ready = computed(() => activity.value?.available === true && ts.value.length > 1)
const atMax = computed(() => (props.config ? capacityBadges(props.config).some(b => b.key === 'at-max') : false))
const guide = computed(() => (props.config && activity.value?.available ? targetGuide(props.config, activity.value, atMax.value) : []))
const note = computed(() => (activity.value ? trafficNote(activity.value) : null))
const partial = computed(() => (activity.value ? trafficPartial(activity.value) : false))

const classes = [
  { key: 'ok', label: '2xx', color: '#10b981' },
  { key: 'redirect', label: '3xx', color: '#38bdf8' },
  { key: 'client_error', label: '4xx', color: '#f59e0b' },
  { key: 'aborted', label: 'Abandoned', color: '#94a3b8' },
  { key: 'server_error', label: '5xx', color: '#ef4444' },
] as const

const requestBars = computed(() => {
  const r = activity.value?.requests_per_min
  return r ? classes.map(c => ({ values: r[c.key], color: c.color })) : []
})
const requestLines = computed<ChartLine[]>(() => {
  const peak = activity.value?.requests_peak_per_min
  return peak ? [{ values: peak, color: '#334155', width: 1 }] : []
})

function scaleSeries(s: Series | undefined, f: number): Series {
  return (s ?? []).map(v => (v === null ? null : v * f))
}

const usageLines = computed<ChartLine[]>(() => {
  const a = activity.value
  if (!a) return []
  if (metric.value === 'memory') {
    return [
      { values: a.memory_pct_of_request ?? [], color: '#8b5cf6' },
      { values: a.policy?.memory_target_pct ?? [], color: '#64748b', dashed: true, width: 1 },
    ]
  }
  return [
    { values: a.cpu_peak_pct_of_request ?? [], color: '#7dd3fc', width: 1 },
    { values: a.cpu_pct_of_request ?? [], color: '#0284c7' },
    { values: a.policy?.cpu_target_pct ?? [], color: '#64748b', dashed: true, width: 1 },
  ]
})
const usageBands = computed<ChartBand[]>(() => {
  const target = metric.value === 'memory' ? activity.value?.policy?.memory_target_pct : activity.value?.policy?.cpu_target_pct
  return target ? [{ lower: target, upper: scaleSeries(target, 1 + DEFAULT_TOLERANCE), color: '#64748b' }] : []
})

const podLines = computed<ChartLine[]>(() => [{ values: activity.value?.replicas ?? [], color: '#7c3aed', step: true }])
const podBands = computed<ChartBand[]>(() => {
  const p = activity.value?.policy
  return p ? [{ lower: p.min, upper: p.max, color: '#7c3aed' }] : []
})

const changes = computed(() => activity.value?.changes ?? [])
const markers = computed<ChartMarker[]>(() => changes.value.map(c => ({ time: c.time, ...markerLook(c.cause) })))
const qualifying = computed(() => changes.value.filter(c => c.cause === 'autoscaler' || c.cause === 'while_autoscaling').length)
const selected = computed(() => (activeMarker.value === null ? frozenChange.value : changes.value[activeMarker.value] ?? frozenChange.value))

function clock(t: number): string {
  const opts: Intl.DateTimeFormatOptions = { hour: '2-digit', minute: '2-digit', hour12: false }
  if (range.value === '3d' || range.value === '24h') opts.weekday = 'short'
  return new Date(t * 1000).toLocaleString('en-GB', opts)
}

const axis = computed(() => {
  const t = ts.value
  if (t.length < 2) return []
  return [t[0], t[Math.floor(t.length / 2)], t[t.length - 1]].map(clock)
})

function at(s: Series | undefined, i: number): number | null {
  return s?.[i] ?? null
}

function readoutAt(i: number): string | null {
  const a = activity.value
  if (!a || ts.value[i] === undefined) return null
  const parts = [clock(ts.value[i])]
  if (a.traffic === 'available' && a.requests_per_min) {
    const total = classes.reduce<number | null>((sum, c) => {
      const v = at(a.requests_per_min![c.key], i)
      return v === null ? sum : (sum ?? 0) + v
    }, null)
    if (total !== null) {
      const peak = at(a.requests_peak_per_min, i)
      parts.push(`${Math.round(total)} requests/min${peak !== null ? ` (peak ${Math.round(peak)})` : ''}`)
    }
  }
  const cpu = at(a.cpu_pct_of_request, i)
  if (cpu !== null) {
    const used = at(a.cpu_used_millis, i)
    const req = at(a.cpu_requested_millis, i)
    parts.push(`CPU ${Math.round(cpu)}%${used !== null && req !== null ? ` (${formatCpu(used)} of ${formatCpu(req)})` : ''}`)
  }
  const mem = at(a.memory_pct_of_request, i)
  if (mem !== null) {
    const used = at(a.memory_used_bytes, i)
    const req = at(a.memory_requested_bytes, i)
    parts.push(`memory ${Math.round(mem)}%${used !== null && req !== null ? ` (${formatMemory(used)} of ${formatMemory(req)})` : ''}`)
  }
  const pods = at(a.replicas, i)
  if (pods !== null) parts.push(pods === 1 ? '1 pod' : `${pods} pods`)
  return parts.join(' · ')
}

// Snapshot hovered values and changes so details stay stable across refreshes
// while users read or scroll them after leaving the chart.
watch([hover, activity], ([h]) => {
  if (h !== null) frozenReadout.value = readoutAt(h)
})
watch([activeMarker, activity], ([m]) => {
  if (m !== null) frozenChange.value = activity.value?.changes[m] ?? frozenChange.value
})
const readout = computed(() => (hover.value === null ? frozenReadout.value : readoutAt(hover.value)))

const pctFormat = (v: number) => `${Math.round(v)}%`
const countFormat = (v: number) => String(Math.round(v))
const buttonClass = (active: boolean) =>
  active
    ? 'rounded px-2 py-0.5 text-xs font-medium bg-kipper-600 text-white'
    : 'rounded px-2 py-0.5 text-xs font-medium text-slate-600 hover:bg-slate-200 dark:text-slate-300 dark:hover:bg-slate-700'
</script>

<template>
  <div class="mt-4 rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800" data-testid="activity-panel">
    <div class="mb-3 flex flex-wrap items-start justify-between gap-2">
      <div>
        <p class="text-sm font-medium text-slate-900 dark:text-slate-50">Traffic and scaling</p>
        <p class="text-xs text-slate-500 dark:text-slate-400">Compare traffic, resource use and desired pods. Change times are approximate.</p>
      </div>
      <div class="flex items-center gap-1">
        <button
          v-for="r in ACTIVITY_RANGES"
          :key="r"
          type="button"
          :data-testid="`activity-range-${r}`"
          :class="buttonClass(range === r)"
          @click="selectRange(r)"
        >{{ r }}</button>
      </div>
    </div>

    <p v-if="unsupported" data-testid="activity-unsupported" class="text-sm text-slate-600 dark:text-slate-300">
      Upgrade Kipper on this cluster to use this view.
    </p>
    <p v-else-if="failed && !activity" data-testid="activity-error" class="text-sm text-red-600 dark:text-rose-300">
      Traffic and scaling figures could not be loaded.
    </p>
    <p v-else-if="!activity" class="text-sm text-slate-500 dark:text-slate-400">Loading…</p>
    <p v-else-if="!activity.available" data-testid="activity-unavailable" class="text-sm text-slate-600 dark:text-slate-300">
      {{ activity.reason || 'Monitoring is not available.' }}
    </p>

    <div v-else class="space-y-5">
      <p v-if="failed" data-testid="activity-refresh-failed" class="text-xs text-amber-700 dark:text-orange-300">
        Refresh failed. The charts show the last available figures.
      </p>
      <div v-if="guide.length" data-testid="activity-guide" class="space-y-0.5 text-xs text-slate-600 dark:text-slate-300">
        <p v-for="(line, k) in guide" :key="k">{{ line }}</p>
      </div>
      <p v-if="activity.degraded.length" data-testid="activity-degraded" class="text-xs text-amber-700 dark:text-orange-300">
        Some chart values or change details could not be loaded.
      </p>

      <template v-if="ready">
        <div>
          <div class="mb-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-slate-600 dark:text-slate-300">
            <span class="font-medium">Requests per minute</span>
            <span v-for="c in classes" :key="c.key" class="inline-flex items-center gap-1">
              <span class="inline-block h-2 w-2 rounded-sm" :style="{ background: c.color }" />{{ c.label }}
            </span>
          </div>
          <p v-if="note" data-testid="activity-traffic-note" class="py-6 text-center text-xs text-slate-500 dark:text-slate-400">{{ note }}</p>
          <template v-else>
            <TimeChart
              v-model:hover="hover"
              v-model:active-marker="activeMarker"
              label="requests"
              :timestamps="ts"
              :bars="requestBars"
              :lines="requestLines"
              :markers="markers"
              :floor="5"
              :format="countFormat"
            />
            <p v-if="partial" data-testid="activity-traffic-partial" class="mt-1 text-xs text-slate-500 dark:text-slate-400">
              Gaps mean request figures are unavailable, not zero.
            </p>
          </template>
        </div>

        <div>
          <div class="mb-2 flex items-center gap-2 text-xs text-slate-600 dark:text-slate-300">
            <span class="font-medium">{{ metric === 'cpu' ? 'CPU' : 'Memory' }} use (% of resource request)</span>
            <button type="button" data-testid="activity-metric-cpu" :class="buttonClass(metric === 'cpu')" @click="metric = 'cpu'">CPU</button>
            <button type="button" data-testid="activity-metric-memory" :class="buttonClass(metric === 'memory')" @click="metric = 'memory'">Memory</button>
          </div>
          <TimeChart
            v-model:hover="hover"
            v-model:active-marker="activeMarker"
            label="usage"
            :timestamps="ts"
            :lines="usageLines"
            :bands="usageBands"
            :markers="markers"
            :floor="100"
            :format="pctFormat"
          />
        </div>

        <div>
          <p class="mb-2 text-xs font-medium text-slate-600 dark:text-slate-300">Desired pods</p>
          <TimeChart
            v-model:hover="hover"
            v-model:active-marker="activeMarker"
            label="pods"
            :timestamps="ts"
            :lines="podLines"
            :bands="podBands"
            :markers="markers"
            :floor="2"
            :height="64"
            :format="countFormat"
          />
          <div class="mt-1 flex justify-between pl-10 text-[10px] text-slate-400 dark:text-slate-500">
            <span v-for="(label, k) in axis" :key="k">{{ label }}</span>
          </div>
        </div>

        <!-- Keep the layout stable as hover details change. -->
        <div
          data-testid="activity-details"
          tabindex="0"
          aria-label="Chart details"
          class="h-36 overflow-y-auto rounded border border-slate-200 p-2 text-xs text-slate-600 dark:border-slate-700 dark:text-slate-300"
        >
          <p v-if="readout" data-testid="activity-readout">{{ readout }}</p>
          <p v-else class="text-slate-500 dark:text-slate-400">Hover over a chart to see values.</p>
          <div v-if="selected" data-testid="activity-change" class="mt-2 rounded bg-violet-50 p-2 text-slate-700 dark:bg-violet-950 dark:text-slate-200">
            <p class="font-medium">About {{ clock(selected.time) }}</p>
            <p>{{ changeSummary(selected) }}</p>
            <p v-if="changeObservation(selected)" class="mt-1">{{ changeObservation(selected) }}</p>
          </div>
          <p v-else-if="changes.length" data-testid="activity-changes-hint" class="mt-2 text-slate-500 dark:text-slate-400">
            {{ changes.length === 1 ? '1 change' : `${changes.length} changes` }} in the desired pod count. Hover over a marker for details<template v-if="qualifying > activity.changes_detailed">; measurements cover up to the latest {{ activity.changes_detailed }}</template>.
          </p>
        </div>
      </template>
      <p v-else class="text-sm text-slate-500 dark:text-slate-400">No figures are available for this range.</p>
    </div>
  </div>
</template>
