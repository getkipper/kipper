<script setup lang="ts">
import { computed } from 'vue'
import type { Series } from '@/api/activity'
import { areaPaths, indexAtFraction, linePath, niceMax, stackedBars, stepPath, timeScale } from '@/utils/activityChart'
import type { MarkerStyle } from '@/utils/activity'

export interface ChartLine {
  values: Series
  color: string
  dashed?: boolean
  width?: number
  step?: boolean
}

export interface ChartBand {
  lower: Series
  upper: Series
  color: string
}

export interface ChartMarker {
  time: number
  style: MarkerStyle
  label?: string
}

interface Props {
  timestamps: number[]
  bars?: { values: Series; color: string }[]
  lines?: ChartLine[]
  bands?: ChartBand[]
  markers?: ChartMarker[]
  /** The smallest axis maximum, so a quiet chart is not drawn at full height. */
  floor?: number
  height?: number
  /** Formats the axis maximum. */
  format?: (v: number) => string
  /** Point index shared across charts to synchronize the readout. */
  hover?: number | null
  /** Marker index shared across charts to synchronize highlighting. */
  activeMarker?: number | null
  label: string
}

const props = withDefaults(defineProps<Props>(), {
  bars: () => [],
  lines: () => [],
  bands: () => [],
  markers: () => [],
  floor: 1,
  height: 96,
  format: (v: number) => String(v),
  hover: null,
  activeMarker: null,
})

const emit = defineEmits<{ 'update:hover': [index: number | null]; 'update:activeMarker': [index: number | null] }>()

// The plot is drawn in a fixed coordinate space stretched to the container;
// strokes keep their width through vector-effect.
const width = 1000
const pad = 4

const max = computed(() => {
  const stacked = props.timestamps.map((_, i) => props.bars.reduce((sum, b) => sum + (b.values[i] ?? 0), 0))
  const all = [...stacked, ...props.lines.flatMap(l => l.values), ...props.bands.flatMap(b => b.upper)]
  return niceMax(all, props.floor)
})

const sx = computed(() => timeScale(props.timestamps, width))
const x = (i: number) => sx.value(props.timestamps[i] ?? 0)
const y = (v: number) => pad + (1 - Math.min(v, max.value) / max.value) * (props.height - pad * 2)

const barWidth = computed(() => Math.max(1, (width / Math.max(1, props.timestamps.length - 1)) * 0.8))
const bars = computed(() => stackedBars(props.bars.map(b => b.values), x, y, barWidth.value))
const lines = computed(() => props.lines.map(l => ({ ...l, d: l.step ? stepPath(l.values, x, y) : linePath(l.values, x, y) })))
const bands = computed(() => props.bands.map(b => ({ color: b.color, shapes: areaPaths(b.lower, b.upper, x, y) })))
const placed = computed(() => props.markers.map(m => ({ ...m, x: sx.value(m.time) })))
const hoverX = computed(() => (props.hover === null ? null : x(props.hover)))

const dash: Record<MarkerStyle, string | undefined> = { solid: undefined, dashed: '4 3', dotted: '1 3' }

function move(event: MouseEvent) {
  const box = (event.currentTarget as HTMLElement).getBoundingClientRect()
  if (box.width <= 0) return
  const fraction = (event.clientX - box.left) / box.width
  emit('update:hover', indexAtFraction(fraction, props.timestamps.length))
  // A marker within 1.5% of the width counts as under the pointer.
  const px = fraction * width
  let near: number | null = null
  placed.value.forEach((m, k) => {
    const d = Math.abs(m.x - px)
    if (d <= width * 0.015 && (near === null || d < Math.abs(placed.value[near].x - px))) near = k
  })
  emit('update:activeMarker', near)
}

function leave() {
  emit('update:hover', null)
  emit('update:activeMarker', null)
}
</script>

<template>
  <div class="relative" :data-testid="`chart-${label}`">
    <span class="pointer-events-none absolute left-1 top-0 text-[10px] text-slate-400 dark:text-slate-500">{{ format(max) }}</span>
    <svg
      :viewBox="`0 0 ${width} ${height}`"
      preserveAspectRatio="none"
      class="block w-full"
      :style="{ height: `${height}px` }"
      role="img"
      :aria-label="label"
      @mousemove="move"
      @mouseleave="leave"
    >
      <line :x1="0" :x2="width" :y1="height - pad" :y2="height - pad" class="stroke-slate-200 dark:stroke-slate-700" vector-effect="non-scaling-stroke" />
      <g v-for="(band, b) in bands" :key="`band-${b}`">
        <path v-for="(d, k) in band.shapes" :key="k" :d="d" :fill="band.color" fill-opacity="0.12" data-testid="chart-band" />
      </g>
      <rect
        v-for="(bar, k) in bars"
        :key="`bar-${k}`"
        :x="bar.x"
        :y="bar.y"
        :width="bar.width"
        :height="bar.height"
        :fill="props.bars[bar.layer].color"
        data-testid="chart-bar"
      />
      <path
        v-for="(line, k) in lines"
        :key="`line-${k}`"
        :d="line.d"
        fill="none"
        :stroke="line.color"
        :stroke-width="line.width ?? 1.5"
        :stroke-dasharray="line.dashed ? '5 4' : undefined"
        stroke-linecap="round"
        stroke-linejoin="round"
        vector-effect="non-scaling-stroke"
        data-testid="chart-line"
      />
      <line
        v-for="(m, k) in placed"
        :key="`marker-${k}`"
        :x1="m.x"
        :x2="m.x"
        :y1="0"
        :y2="height"
        :stroke-width="activeMarker === k ? 2.5 : 1.25"
        :stroke-dasharray="dash[m.style]"
        class="stroke-violet-500 dark:stroke-violet-400"
        vector-effect="non-scaling-stroke"
        :data-testid="`chart-marker-${m.style}`"
      />
      <line
        v-if="hoverX !== null"
        :x1="hoverX"
        :x2="hoverX"
        :y1="0"
        :y2="height"
        class="stroke-slate-400 dark:stroke-slate-500"
        stroke-width="1"
        vector-effect="non-scaling-stroke"
        data-testid="chart-hover"
      />
    </svg>
    <span
      v-for="(m, k) in placed.filter(m => m.label)"
      :key="`label-${k}`"
      class="pointer-events-none absolute top-0 -translate-x-1/2 rounded bg-violet-100 px-1 text-[10px] text-violet-700 dark:bg-violet-950 dark:text-violet-300"
      :style="{ left: `${(m.x / width) * 100}%` }"
      data-testid="chart-marker-label"
    >{{ m.label }}</span>
  </div>
</template>
