<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { AlertTriangle, Check, Loader2, RotateCcw } from 'lucide-vue-next'
import {
  CPU_STOPS,
  DEFAULT_BANDS,
  MEMORY_STOPS,
  type BandThresholds,
  type ResourceBand,
  type ResourceKind,
  formatQuantity,
  nearestStop,
  ratioBand,
} from '@/utils/resources'

interface Props {
  usage: number
  limit: number
  kind: ResourceKind
  stops?: number[]
  min?: number
  max?: number
  throttlingPct?: number | null
  size?: 'sm' | 'md' | 'lg'
  label?: string
  readonly?: boolean
  applying?: boolean
  bands?: BandThresholds
}

const props = withDefaults(defineProps<Props>(), {
  size: 'md',
  readonly: false,
  applying: false,
  throttlingPct: null,
  label: '',
  bands: () => DEFAULT_BANDS,
  stops: undefined,
  min: undefined,
  max: undefined,
})

const emit = defineEmits<{
  apply: [newLimit: number]
}>()

// Effective stops, clamped to [min, max] when provided. If no preset stop
// falls inside the bounds, fall back to the bounds themselves so the slider
// stays inside the caller's contract instead of silently widening.
const effectiveStops = computed<number[]>(() => {
  const base = props.stops ?? (props.kind === 'memory' ? MEMORY_STOPS : CPU_STOPS)
  const lo = props.min ?? -Infinity
  const hi = props.max ?? Infinity
  const filtered = base.filter((v) => v >= lo && v <= hi)
  if (filtered.length > 0) return filtered
  const synth: number[] = []
  if (Number.isFinite(lo)) synth.push(lo as number)
  if (Number.isFinite(hi) && hi !== lo) synth.push(hi as number)
  return synth.length > 0 ? synth : base
})

// The slider is bound to an integer index into effectiveStops. The slider
// position snaps to the nearest stop, but pendingLimit stays equal to
// props.limit until the user actually moves the slider — otherwise a limit
// that doesn't sit on a stop (e.g. Loki's 384Mi default) would silently
// "drift" to the nearest stop and Apply would downgrade it.
const sliderIndex = ref(0)
const userMoved = ref(false)

function syncSliderToLimit(limit: number) {
  const stops = effectiveStops.value
  if (stops.length === 0) return
  sliderIndex.value = nearestStop(limit, stops)
  userMoved.value = false
}

syncSliderToLimit(props.limit)

watch(() => props.limit, (next) => {
  syncSliderToLimit(next)
})

watch(effectiveStops, () => {
  syncSliderToLimit(props.limit)
})

function onSliderInput(e: Event) {
  const idx = Number((e.target as HTMLInputElement).value)
  if (Number.isFinite(idx)) {
    sliderIndex.value = idx
    userMoved.value = true
  }
}

const pendingLimit = computed<number>(() => {
  if (!userMoved.value) return props.limit
  const stops = effectiveStops.value
  if (stops.length === 0) return props.limit
  return stops[Math.min(Math.max(sliderIndex.value, 0), stops.length - 1)]
})

const hasChange = computed(() => userMoved.value && pendingLimit.value !== props.limit)

const ratio = computed(() => {
  if (pendingLimit.value <= 0) return 0
  return props.usage / pendingLimit.value
})

const clampedRatio = computed(() => Math.max(0, Math.min(1, ratio.value)))

const band = computed<ResourceBand>(() => ratioBand(ratio.value, props.bands))

const overLimit = computed(() => ratio.value > 1)

const showThrottling = computed(
  () => props.kind === 'cpu' && props.throttlingPct != null && props.throttlingPct > 5,
)

// Per-size geometry. The arc is a semicircle inscribed in a rectangle of
// width W and height roughly W/2; we add padding below for the hub and value
// label, plus a little above for stroke thickness.
interface GaugeGeometry {
  viewBoxW: number
  viewBoxH: number
  cx: number
  cy: number
  r: number
  stroke: number
  needleLength: number
  hubR: number
  valueFont: number
  unitFont: number
}

const geometry = computed<GaugeGeometry>(() => {
  switch (props.size) {
    case 'sm':
      return { viewBoxW: 140, viewBoxH: 92, cx: 70, cy: 76, r: 56, stroke: 10, needleLength: 48, hubR: 5, valueFont: 14, unitFont: 10 }
    case 'lg':
      return { viewBoxW: 280, viewBoxH: 176, cx: 140, cy: 148, r: 116, stroke: 18, needleLength: 100, hubR: 9, valueFont: 24, unitFont: 13 }
    case 'md':
    default:
      return { viewBoxW: 200, viewBoxH: 128, cx: 100, cy: 108, r: 82, stroke: 14, needleLength: 70, hubR: 7, valueFont: 18, unitFont: 11 }
  }
})

const arcPath = computed(() => {
  const { cx, cy, r } = geometry.value
  return `M ${cx - r} ${cy} A ${r} ${r} 0 0 1 ${cx + r} ${cy}`
})

// Needle rotation matches the universal fuel/speedometer mental model:
// 0% usage → -90° (points left toward green),
// 100% usage → +90° (points right toward red).
const needleRotation = computed(() => -90 + clampedRatio.value * 180)

// Stable per-instance gradient id so multiple ResourceControls on the same
// page do not share a gradient definition.
const uid = Math.random().toString(36).slice(2, 9)
const gradientId = `rc-grad-${uid}`

const bandColors: Record<ResourceBand, { needle: string; text: string }> = {
  healthy: { needle: '#16a34a', text: 'text-emerald-700 dark:text-emerald-400' },
  warning: { needle: '#d97706', text: 'text-amber-700 dark:text-amber-400' },
  critical: { needle: '#dc2626', text: 'text-rose-700 dark:text-rose-400' },
}

const tickPositions = computed(() => {
  if (props.size === 'sm') return [0, 0.5, 1]
  return [0, 0.25, 0.5, 0.75, 1]
})

function tickCoords(t: number) {
  const { cx, cy, r, stroke } = geometry.value
  const angle = Math.PI * (1 - t)
  const outer = r + stroke / 2 + 2
  const inner = r - stroke / 2 - 2
  return {
    x1: cx + Math.cos(angle) * inner,
    y1: cy - Math.sin(angle) * inner,
    x2: cx + Math.cos(angle) * outer,
    y2: cy - Math.sin(angle) * outer,
  }
}

const usageLabel = computed(() => formatQuantity(props.usage, props.kind))
const limitLabel = computed(() => formatQuantity(pendingLimit.value, props.kind))
const currentLimitLabel = computed(() => formatQuantity(props.limit, props.kind))
const ratioPct = computed(() => Math.round(clampedRatio.value * 100))

function onApply() {
  if (!hasChange.value || props.applying) return
  emit('apply', pendingLimit.value)
}

function onReset() {
  syncSliderToLimit(props.limit)
  userMoved.value = false
}

const stopLabels = computed(() =>
  effectiveStops.value.map((v) => formatQuantity(v, props.kind)),
)

const kindLabel = computed(() => (props.kind === 'memory' ? 'Memory' : 'CPU'))
</script>

<template>
  <div class="rc-root" :class="`rc-size-${size}`" data-testid="resource-control">
    <div v-if="label" class="mb-2 flex items-center justify-between gap-2">
      <h4 class="text-sm font-medium text-slate-700 dark:text-slate-200">{{ label }}</h4>
      <span
        v-if="showThrottling"
        class="rounded-full bg-amber-100 px-2 py-0.5 text-xs font-medium text-amber-800 dark:bg-amber-900/40 dark:text-amber-300"
        data-testid="throttling-badge"
      >
        throttled {{ Math.round(throttlingPct ?? 0) }}%
      </span>
    </div>

    <div class="flex flex-col items-center">
      <svg
        :viewBox="`0 0 ${geometry.viewBoxW} ${geometry.viewBoxH}`"
        :width="geometry.viewBoxW"
        :height="geometry.viewBoxH"
        class="max-w-full"
        role="img"
        :aria-label="`${kindLabel} usage ${ratioPct} percent of ${limitLabel}`"
      >
        <defs>
          <linearGradient
            :id="gradientId"
            :x1="geometry.cx - geometry.r"
            :y1="geometry.cy"
            :x2="geometry.cx + geometry.r"
            :y2="geometry.cy"
            gradientUnits="userSpaceOnUse"
          >
            <stop offset="0%" stop-color="#22c55e" />
            <stop offset="25%" stop-color="#84cc16" />
            <stop offset="45%" stop-color="#eab308" />
            <stop offset="65%" stop-color="#f59e0b" />
            <stop offset="100%" stop-color="#ef4444" />
          </linearGradient>
        </defs>

        <path
          :d="arcPath"
          fill="none"
          :stroke="`url(#${gradientId})`"
          :stroke-width="geometry.stroke"
          stroke-linecap="round"
        />

        <g class="text-slate-300 dark:text-slate-700" stroke="currentColor" stroke-width="1.5">
          <line
            v-for="t in tickPositions"
            :key="t"
            v-bind="tickCoords(t)"
          />
        </g>

        <g :transform="`rotate(${needleRotation} ${geometry.cx} ${geometry.cy})`">
          <line
            :x1="geometry.cx"
            :y1="geometry.cy"
            :x2="geometry.cx"
            :y2="geometry.cy - geometry.needleLength"
            :stroke="bandColors[band].needle"
            :stroke-width="Math.max(2, geometry.stroke / 4)"
            stroke-linecap="round"
            data-testid="gauge-needle"
          />
        </g>
        <circle
          :cx="geometry.cx"
          :cy="geometry.cy"
          :r="geometry.hubR"
          :fill="bandColors[band].needle"
        />

        <text
          :x="geometry.cx"
          :y="geometry.cy - geometry.r * 0.4"
          text-anchor="middle"
          class="font-semibold fill-slate-900 dark:fill-slate-50"
          :style="{ fontSize: `${geometry.valueFont}px` }"
        >
          {{ ratioPct }}%
        </text>
      </svg>

      <div
        class="mt-1 flex items-baseline gap-1 text-center"
        :style="{ fontSize: `${geometry.unitFont + 1}px` }"
      >
        <span class="font-medium" :class="bandColors[band].text" data-testid="usage-label">{{ usageLabel }}</span>
        <span class="text-slate-400 dark:text-slate-500">/</span>
        <span class="text-slate-600 dark:text-slate-300" data-testid="limit-label">{{ limitLabel }}</span>
        <span
          v-if="overLimit"
          class="ml-1 inline-flex items-center gap-0.5 rounded-full bg-rose-100 px-1.5 text-[10px] font-medium text-rose-700 dark:bg-rose-900/40 dark:text-rose-300"
        >
          <AlertTriangle class="h-3 w-3" /> over
        </span>
      </div>
    </div>

    <div v-if="!readonly" class="mt-3" data-testid="slider-region">
      <input
        :value="sliderIndex"
        type="range"
        min="0"
        :max="effectiveStops.length - 1"
        step="1"
        class="w-full accent-kipper-600"
        :aria-label="`${kindLabel} limit`"
        :aria-valuenow="pendingLimit"
        :aria-valuemin="effectiveStops[0]"
        :aria-valuemax="effectiveStops[effectiveStops.length - 1]"
        data-testid="slider"
        @input="onSliderInput"
      />
      <div
        class="mt-1 flex justify-between text-[10px] text-slate-400 dark:text-slate-500 select-none"
      >
        <span
          v-for="(s, i) in stopLabels"
          :key="i"
          :class="i === sliderIndex ? 'font-semibold text-slate-700 dark:text-slate-200' : ''"
        >{{ s }}</span>
      </div>

      <div class="mt-3 flex items-center justify-between gap-2">
        <div class="text-xs text-slate-500 dark:text-slate-400">
          <template v-if="hasChange">
            New limit:
            <span class="font-medium text-slate-900 dark:text-slate-50">{{ limitLabel }}</span>
            <span class="text-slate-400 dark:text-slate-500"> (was {{ currentLimitLabel }})</span>
          </template>
          <template v-else>
            Current limit: <span class="font-medium text-slate-700 dark:text-slate-200">{{ currentLimitLabel }}</span>
          </template>
        </div>
        <div class="flex items-center gap-1.5">
          <button
            type="button"
            :disabled="!hasChange || applying"
            class="inline-flex items-center gap-1 rounded-md px-2 py-1 text-xs font-medium text-slate-600 hover:bg-slate-100 disabled:opacity-40 dark:text-slate-300 dark:hover:bg-slate-800"
            data-testid="reset-button"
            @click="onReset"
          >
            <RotateCcw class="h-3 w-3" />
            Reset
          </button>
          <button
            type="button"
            :disabled="!hasChange || applying"
            class="inline-flex items-center gap-1 rounded-md bg-slate-900 px-3 py-1 text-xs font-medium text-white hover:bg-slate-800 disabled:opacity-40 dark:bg-slate-700 dark:hover:bg-slate-600"
            data-testid="apply-button"
            @click="onApply"
          >
            <Loader2 v-if="applying" class="h-3 w-3 animate-spin" />
            <Check v-else class="h-3 w-3" />
            Apply
          </button>
        </div>
      </div>
    </div>
  </div>
</template>
