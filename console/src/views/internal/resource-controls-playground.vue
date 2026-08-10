<script setup lang="ts">
import { ref } from 'vue'
import ResourceControl from '@/components/ResourceControl.vue'
import { CPU_STOPS, MEMORY_STOPS } from '@/utils/resources'

const Gi = 1024 ** 3
const Mi = 1024 ** 2

interface Demo {
  title: string
  kind: 'memory' | 'cpu'
  usage: number
  limit: number
  size?: 'sm' | 'md' | 'lg'
  throttlingPct?: number
  readonly?: boolean
  stops?: number[]
  min?: number
  max?: number
}

const demos: Demo[] = [
  { title: 'Healthy memory', kind: 'memory', usage: 0.4 * Gi, limit: 2 * Gi },
  { title: 'Warning memory', kind: 'memory', usage: 1.4 * Gi, limit: 2 * Gi },
  { title: 'Critical memory', kind: 'memory', usage: 1.85 * Gi, limit: 2 * Gi },
  { title: 'Over the limit', kind: 'memory', usage: 2.3 * Gi, limit: 2 * Gi },
  { title: 'Compact (sm)', kind: 'memory', usage: 0.6 * Gi, limit: 1 * Gi, size: 'sm' },
  { title: 'Large (lg)', kind: 'memory', usage: 5 * Gi, limit: 8 * Gi, size: 'lg' },
  { title: 'CPU with throttling', kind: 'cpu', usage: 850, limit: 1000, throttlingPct: 18.4 },
  { title: 'CPU healthy', kind: 'cpu', usage: 80, limit: 500 },
  { title: 'Read-only (dashboard)', kind: 'memory', usage: 6.2 * Gi, limit: 16 * Gi, readonly: true, size: 'sm' },
  {
    title: 'Bounded slider (1–4 Gi)',
    kind: 'memory',
    usage: 1.6 * Gi,
    limit: 2 * Gi,
    min: 1 * Gi,
    max: 4 * Gi,
  },
  {
    title: 'Custom stops (128–512 Mi)',
    kind: 'memory',
    usage: 200 * Mi,
    limit: 256 * Mi,
    stops: [128 * Mi, 192 * Mi, 256 * Mi, 384 * Mi, 512 * Mi],
  },
]

const applied = ref<Record<number, number>>({})

function onApply(i: number, newLimit: number) {
  applied.value = { ...applied.value, [i]: newLimit }
}
</script>

<template>
  <div class="mx-auto max-w-7xl p-6">
    <header class="mb-6">
      <h1 class="text-xl font-semibold text-slate-900 dark:text-slate-50">
        Resource Controls playground
      </h1>
      <p class="mt-1 text-sm text-slate-600 dark:text-slate-400">
        Internal sandbox for the <code class="rounded bg-slate-100 px-1 dark:bg-slate-800">&lt;ResourceControl&gt;</code>
        primitive. Drag a slider to preview a new limit; Apply records the value on this page (no
        backend call). Designers can iterate here without touching the Platform page.
      </p>
      <p class="mt-1 text-xs text-slate-500 dark:text-slate-500">
        Memory stops: {{ MEMORY_STOPS.length }} from 128 Mi to 16 Gi. CPU stops: {{ CPU_STOPS.length }} from 50m to 4.
      </p>
    </header>

    <div class="grid grid-cols-1 gap-6 md:grid-cols-2 xl:grid-cols-3">
      <div
        v-for="(d, i) in demos"
        :key="i"
        class="rounded-lg border border-slate-200 bg-white p-4 dark:border-slate-800 dark:bg-slate-900"
      >
        <h2 class="mb-3 text-sm font-medium text-slate-700 dark:text-slate-200">
          {{ d.title }}
        </h2>
        <ResourceControl
          :kind="d.kind"
          :usage="d.usage"
          :limit="applied[i] ?? d.limit"
          :size="d.size"
          :throttling-pct="d.throttlingPct ?? null"
          :readonly="d.readonly"
          :stops="d.stops"
          :min="d.min"
          :max="d.max"
          @apply="(v) => onApply(i, v)"
        />
        <p
          v-if="applied[i]"
          class="mt-2 text-xs text-emerald-700 dark:text-emerald-400"
        >
          Applied: new limit recorded ({{ applied[i] }} units).
        </p>
      </div>
    </div>
  </div>
</template>
