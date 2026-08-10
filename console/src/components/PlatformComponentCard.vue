<script setup lang="ts">
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'
import { AlertTriangle, Check, Loader2, EyeOff, Eye, ChevronRight } from 'lucide-vue-next'
import ResourceControl from '@/components/ResourceControl.vue'
import MetricSparkline from '@/components/MetricSparkline.vue'
import NoticeCallout from '@/components/NoticeCallout.vue'
import { useResourceUsage } from '@/composables/useResourceUsage'
import { usePlatformStore } from '@/stores/platform'
import { parseMemoryQuantity, toKubernetesMemoryQuantity } from '@/utils/resources'
import {
  PLATFORM_DEFAULT_BOUNDS,
  platformConfig,
} from '@/utils/platformComponents'
import type { PlatformComponent } from '@/api/platform'

interface Props {
  component: PlatformComponent
}

const props = defineProps<Props>()
const store = usePlatformStore()

const config = computed(() => platformConfig(props.component.name))

const message = ref<string>('')
const messageKind = ref<'ok' | 'error' | ''>('')

const updating = computed(() => Boolean(store.updating[props.component.name]))

const phaseLabel = computed(() => props.component.phase || (props.component.enabled ? 'Running' : 'Disabled'))

const phaseClass = computed(() => {
  switch (phaseLabel.value) {
    case 'Running':
      return 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400'
    case 'OOMKilled':
    case 'CrashLoopBackOff':
      return 'bg-rose-50 text-rose-700 dark:bg-rose-900/30 dark:text-rose-400'
    case 'Disabled':
      return 'bg-slate-100 text-slate-600 dark:bg-slate-800 dark:text-slate-400'
    default:
      return 'bg-amber-50 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400'
  }
})

// Slider denominator priority:
//   1. override_memory_limit — the user's most recent intent. After a
//      successful PATCH this is what comes back, so the ResourceControl's
//      "userMoved" flag clears and Apply disables itself again. Without
//      this prop changing, the slider would stay dirty until the helm
//      rollout completes and the status block updates current_*.
//   2. current_memory_limit — what the cluster is actually running.
//   3. profile_memory_limit — fallback while the status block is empty.
const memoryLimitBytes = computed(() => {
  const raw =
    props.component.override_memory_limit ||
    props.component.current_memory_limit ||
    props.component.profile_memory_limit
  if (!raw) return 0
  try {
    return parseMemoryQuantity(raw)
  } catch {
    return 0
  }
})

// Bounds come from the API when the backend is recent enough to populate
// them; the static config is a fallback for older builds. Parsing happens
// at use time so a malformed string falls through to the fallback rather
// than blowing up the whole card.
function parseBound(raw: string | undefined): number | null {
  if (!raw) return null
  try {
    return parseMemoryQuantity(raw)
  } catch {
    return null
  }
}

const memoryMin = computed(
  () =>
    parseBound(props.component.memory_min) ??
    config.value?.memoryMin ??
    PLATFORM_DEFAULT_BOUNDS.memoryMin,
)
const memoryMax = computed(
  () =>
    parseBound(props.component.memory_max) ??
    config.value?.memoryMax ??
    PLATFORM_DEFAULT_BOUNDS.memoryMax,
)

// Only poll when the component is enabled. Disabled components have no
// pods, so the usage gauge would just churn 404s. The composable handles
// scope === null by pausing the poller. includePrometheus pulls the 1h
// memory sparkline rendered below the gauge — opt-in so the per-card
// poll doesn't burden nano clusters that don't run Prometheus.
const usageScope = computed(() => {
  if (!props.component.enabled || !config.value) return null
  return {
    namespace: config.value.namespace,
    selector: config.value.selector,
    includePrometheus: true,
  }
})

const usage = useResourceUsage(usageScope)

const memorySparkline = computed(() => usage.data.value?.memory_sparkline ?? [])

// Match the gauge numerator to the slider's denominator: only containers
// whose name matches the platform component get summed. Without this
// filter, sidecars (config-reloader, thanos, etc.) inflate the numerator
// while the limit covers just the managed container, painting a
// misleadingly hot gauge that the slider cannot fix. Falls back to the
// totals when no container matches by name, so an unknown component still
// renders a rough gauge.
const usageMatches = computed(() => {
  const rows = usage.data.value?.containers ?? []
  return rows.filter((c) => c.name === props.component.name)
})

const memoryUsageBytes = computed(() => {
  const matches = usageMatches.value
  if (matches.length === 0) return usage.data.value?.totals.memory_bytes ?? 0
  // Average per-pod usage: each pod of a multi-replica workload has the
  // same per-container limit, so the per-pod average is what tells the
  // user "how tight am I on memory per replica".
  const sum = matches.reduce((acc, c) => acc + (c.metrics_present ? c.memory_bytes : 0), 0)
  const denom = matches.filter((c) => c.metrics_present).length || 1
  return Math.round(sum / denom)
})

// Treat metrics as known only when every selector-matched container has
// been sampled. Partial coverage (rollouts mid-flight, pending pods)
// renders as "waiting" so the gauge can't lie green while metrics are
// still arriving.
const metricsKnown = computed(() => {
  const d = usage.data.value
  if (!d || !d.metrics_available) return false
  const matches = usageMatches.value
  if (matches.length > 0) return matches.every((c) => c.metrics_present)
  return d.totals.container_count > 0 && d.totals.containers_with_metrics === d.totals.container_count
})

async function applyLimit(newLimit: number) {
  message.value = ''
  messageKind.value = ''
  const quantity = toKubernetesMemoryQuantity(newLimit)
  try {
    await store.updateComponent(props.component.name, { memory_limit: quantity })
    message.value = `Memory limit set to ${quantity}`
    messageKind.value = 'ok'
    usage.refresh()
  } catch (e) {
    message.value = e instanceof Error ? e.message : 'failed to update'
    messageKind.value = 'error'
  }
}

async function toggleEnabled() {
  message.value = ''
  messageKind.value = ''
  try {
    await store.updateComponent(props.component.name, { enabled: !props.component.enabled })
    message.value = props.component.enabled ? `${props.component.name} disabled` : `${props.component.name} enabled`
    messageKind.value = 'ok'
  } catch (e) {
    message.value = e instanceof Error ? e.message : 'failed to update'
    messageKind.value = 'error'
  }
}
</script>

<template>
  <div class="rounded-lg border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
    <div class="flex items-start justify-between gap-3">
      <div>
        <h3 class="text-base font-semibold capitalize text-slate-900 dark:text-slate-50">{{ component.name }}</h3>
        <p class="mt-0.5 text-xs text-slate-500 dark:text-slate-400">
          Profile default: {{ component.profile_memory_limit || 'n/a' }}
          <span v-if="component.override_memory_limit" class="ml-2 text-slate-400">
            override: {{ component.override_memory_limit }}
          </span>
        </p>
      </div>
      <span class="rounded-full px-2.5 py-0.5 text-xs font-medium" :class="phaseClass">
        {{ phaseLabel }}
      </span>
    </div>

    <NoticeCallout v-if="component.at_ceiling" tone="warning" class="mt-3 flex items-start gap-2 p-2.5 text-xs text-amber-800 dark:text-slate-400">
      <AlertTriangle class="mt-0.5 h-4 w-4 shrink-0 dark:text-orange-300" />
      <div>
        <div class="font-medium dark:text-orange-300">At the ceiling.</div>
        <div>Auto-bump has run out of room. Raise the limit yourself or move to a bigger node.</div>
      </div>
    </NoticeCallout>

    <div class="mt-4 grid grid-cols-2 gap-3 text-xs">
      <div class="rounded-md bg-slate-50 p-2.5 dark:bg-slate-800/60">
        <div class="text-slate-500 dark:text-slate-400">Current limit</div>
        <div class="mt-0.5 font-semibold text-slate-900 dark:text-slate-50">{{ component.current_memory_limit || '—' }}</div>
      </div>
      <div class="rounded-md bg-slate-50 p-2.5 dark:bg-slate-800/60">
        <div class="text-slate-500 dark:text-slate-400">Restarts (7d)</div>
        <div class="mt-0.5 font-semibold text-slate-900 dark:text-slate-50">{{ component.restart_count_7d ?? 0 }}</div>
      </div>
    </div>

    <div v-if="component.enabled" class="mt-5">
      <NoticeCallout v-if="!config" tone="warning" class="mb-2 flex items-start gap-2 p-2 text-xs text-amber-800 dark:text-slate-300">
        <AlertTriangle class="mt-0.5 h-3.5 w-3.5 shrink-0 dark:text-orange-300" />
        <span>Selector unknown for this component — gauge will show no data. Slider still writes through.</span>
      </NoticeCallout>
      <div v-else-if="usage.data.value && !metricsKnown" class="mb-2 text-xs text-slate-500 dark:text-slate-400">
        Waiting for metrics-server… gauge shows the limit; usage will populate shortly.
      </div>

      <ResourceControl
        kind="memory"
        :usage="memoryUsageBytes"
        :limit="memoryLimitBytes"
        :min="memoryMin"
        :max="memoryMax"
        :applying="updating"
        size="md"
        @apply="applyLimit"
      />

      <div v-if="memorySparkline.length > 1" class="mt-2 flex items-center justify-center gap-2 text-[10px] text-slate-400 dark:text-slate-500">
        <span class="uppercase tracking-wide">Last hour</span>
        <MetricSparkline :data="memorySparkline" :width="180" :height="28" color="#0ea5e9" />
      </div>

      <div v-if="config" class="mt-2 flex justify-end">
        <RouterLink
          :to="{ name: 'platform-pods', params: { name: component.name } }"
          class="inline-flex items-center gap-0.5 text-xs font-medium text-slate-500 hover:text-slate-900 dark:text-slate-400 dark:hover:text-slate-100"
        >
          View per pod
          <ChevronRight class="h-3 w-3" />
        </RouterLink>
      </div>
    </div>

    <div v-else class="mt-4 rounded-md border border-slate-200 bg-slate-50 p-3 text-center text-xs text-slate-500 dark:border-slate-800 dark:bg-slate-800/40 dark:text-slate-400">
      Disabled. No pods running.
    </div>

    <div v-if="component.last_bump_at" class="mt-4 rounded-md border border-slate-200 bg-slate-50 p-2.5 text-xs text-slate-600 dark:border-slate-800 dark:bg-slate-800/40 dark:text-slate-400">
      <div class="font-medium text-slate-700 dark:text-slate-300">Last auto-bump</div>
      <div class="mt-0.5">
        {{ component.last_bump_from }} → {{ component.last_bump_to }} at {{ component.last_bump_at }}
      </div>
      <div v-if="component.last_bump_reason" class="mt-0.5 italic">{{ component.last_bump_reason }}</div>
    </div>

    <button
      type="button"
      :disabled="updating"
      class="mt-3 inline-flex items-center gap-1 text-xs font-medium text-slate-600 hover:text-slate-900 dark:text-slate-400 dark:hover:text-slate-100"
      @click="toggleEnabled"
    >
      <Loader2 v-if="updating" class="h-3.5 w-3.5 animate-spin" />
      <EyeOff v-else-if="component.enabled" class="h-3.5 w-3.5" />
      <Eye v-else class="h-3.5 w-3.5" />
      {{ component.enabled ? 'Disable this component' : 'Enable this component' }}
    </button>

    <div v-if="message" class="mt-3 flex items-center gap-1.5 text-xs" :class="messageKind === 'ok' ? 'text-emerald-700 dark:text-emerald-400' : 'text-rose-700 dark:text-rose-400'">
      <Check v-if="messageKind === 'ok'" class="h-3.5 w-3.5" />
      <AlertTriangle v-else class="h-3.5 w-3.5" />
      <span>{{ message }}</span>
    </div>
  </div>
</template>
