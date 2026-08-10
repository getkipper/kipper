<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter, RouterLink } from 'vue-router'
import { ArrowLeft, ServerCog, Rocket, Database, AlertTriangle } from 'lucide-vue-next'
import NoticeCallout from '@/components/NoticeCallout.vue'
import ResourceControl from '@/components/ResourceControl.vue'
import { useResourceUsage } from '@/composables/useResourceUsage'
import { platformConfig } from '@/utils/platformComponents'
import type { UsageScope } from '@/api/resources'

type Scope = 'platform' | 'apps' | 'services'

const route = useRoute()
const router = useRouter()

const scope = computed<Scope>(() => {
  const raw = String(route.params.scope ?? route.meta.scope ?? '')
  if (raw === 'platform' || raw === 'apps' || raw === 'services') return raw
  // Fall back to the path segment so the route can be wired three ways
  // (one per scope) without exposing the scope as a parameter.
  const segment = route.path.split('/')[1]
  return (segment === 'platform' || segment === 'apps' || segment === 'services') ? segment : 'apps'
})

const name = computed(() => String(route.params.name ?? ''))

// Namespace resolution:
//   - platform: static lookup in PLATFORM_COMPONENTS — every supported
//     system component lives in a known namespace.
//   - apps / services: the caller passes ?ns=<namespace> because the
//     list views show entries across projects. Without ns we can't
//     scope the metrics fetch.
const namespace = computed(() => {
  if (scope.value === 'platform') {
    return platformConfig(name.value)?.namespace ?? ''
  }
  const ns = route.query.ns
  return typeof ns === 'string' ? ns : ''
})

const selector = computed(() => {
  if (scope.value === 'platform') {
    return platformConfig(name.value)?.selector ?? ''
  }
  return `app=${name.value}`
})

const usageScope = computed<UsageScope | null>(() => {
  if (!namespace.value || !selector.value) return null
  return { namespace: namespace.value, selector: selector.value }
})

const usage = useResourceUsage(usageScope)

// One row per pod with every container summed. Strictly speaking the
// OOM-killer fires per-container so the worst container is the real
// hard limit, but Kipper workloads are mostly single-container and
// users read this view as "how full is this pod". Matching kubectl
// top pod's behaviour keeps the mental model simple.
const perPod = computed(() => {
  const rows = usage.data.value?.containers ?? []
  const byPod = new Map<string, {
    pod: string
    namespace: string
    memory: number
    memoryLimit: number
    cpu: number
    cpuLimit: number
    metricsPresent: boolean
  }>()

  for (const c of rows) {
    const existing = byPod.get(c.pod) ?? {
      pod: c.pod,
      namespace: c.namespace,
      memory: 0,
      memoryLimit: 0,
      cpu: 0,
      cpuLimit: 0,
      metricsPresent: false,
    }
    existing.memory += c.metrics_present ? c.memory_bytes : 0
    existing.cpu += c.metrics_present ? c.cpu_millis : 0
    existing.memoryLimit += c.memory_limit_bytes
    existing.cpuLimit += c.cpu_limit_millis
    existing.metricsPresent = existing.metricsPresent || c.metrics_present
    byPod.set(c.pod, existing)
  }
  return Array.from(byPod.values()).sort((a, b) => a.pod.localeCompare(b.pod))
})

const podCount = computed(() => usage.data.value?.totals.pod_count ?? 0)
const metricsAvailable = computed(() => usage.data.value?.metrics_available ?? false)

const scopeIcon = computed(() => ({ platform: ServerCog, apps: Rocket, services: Database })[scope.value])

const scopeLabel = computed(() => ({ platform: 'Platform component', apps: 'App', services: 'Service' })[scope.value])

const backTarget = computed(() => ({ platform: '/platform', apps: '/apps', services: '/services' })[scope.value])

function close() {
  router.push(backTarget.value)
}
</script>

<template>
  <div class="mx-auto max-w-6xl p-6">
    <header class="mb-6 flex items-start justify-between gap-4">
      <div class="flex items-center gap-3">
        <button
          type="button"
          class="rounded-md border border-slate-200 p-2 text-slate-500 hover:bg-slate-100 dark:border-slate-700 dark:text-slate-400 dark:hover:bg-slate-800"
          @click="close"
        >
          <ArrowLeft class="h-4 w-4" />
        </button>
        <div>
          <p class="flex items-center gap-1.5 text-xs font-medium uppercase tracking-wide text-slate-500 dark:text-slate-400">
            <component :is="scopeIcon" class="h-3.5 w-3.5" />
            {{ scopeLabel }}
          </p>
          <h1 class="text-2xl font-semibold tracking-tight text-slate-900 dark:text-slate-50">
            {{ name }}
          </h1>
          <p v-if="namespace" class="mt-0.5 text-xs text-slate-500 dark:text-slate-400">
            namespace: <span class="font-mono">{{ namespace }}</span>
            <span v-if="podCount > 0" class="ml-2">· {{ podCount }} pod{{ podCount === 1 ? '' : 's' }}</span>
          </p>
        </div>
      </div>
      <RouterLink
        :to="backTarget"
        class="text-xs font-medium text-slate-500 hover:text-slate-900 dark:text-slate-400 dark:hover:text-slate-100"
      >
        Back to {{ scope }}
      </RouterLink>
    </header>

    <NoticeCallout v-if="!namespace" tone="warning" class="p-4 text-sm text-amber-800">
      <div class="flex items-start gap-2">
        <AlertTriangle class="mt-0.5 h-4 w-4 shrink-0 dark:text-orange-300" />
        <div>
          <p class="font-medium dark:text-orange-300">Namespace unknown.</p>
          <p class="mt-0.5 text-xs dark:text-slate-400">
            <template v-if="scope === 'platform'">
              No platform config entry for <span class="font-mono dark:text-slate-300">{{ name }}</span>. The slider table doesn't know where this component's pods live.
            </template>
            <template v-else>
              Open this page from the {{ scope }} list so the namespace is set on the URL (<span class="font-mono dark:text-slate-300">?ns=...</span>).
            </template>
          </p>
        </div>
      </div>
    </NoticeCallout>

    <NoticeCallout v-else-if="usage.error.value" tone="danger" class="p-4 text-sm text-rose-700 dark:text-slate-300">
      Failed to load pod usage: {{ usage.error.value }}
    </NoticeCallout>

    <div v-else-if="usage.loading.value && !usage.data.value" class="text-sm text-slate-500 dark:text-slate-400">
      Loading…
    </div>

    <div v-else-if="perPod.length === 0" class="rounded-lg border border-slate-200 bg-white p-6 text-center text-sm text-slate-500 dark:border-slate-800 dark:bg-slate-900 dark:text-slate-400">
      No pods match this workload.
    </div>

    <div v-else>
      <p v-if="!metricsAvailable" class="mb-3 text-xs text-amber-700 dark:text-amber-400">
        metrics-server hasn't returned data yet: gauges may show 0% until the next scrape.
      </p>
      <div class="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
        <div
          v-for="row in perPod"
          :key="row.pod"
          class="rounded-lg border border-slate-200 bg-white p-4 dark:border-slate-800 dark:bg-slate-900"
        >
          <p class="mb-3 truncate text-xs font-medium text-slate-700 dark:text-slate-300" :title="row.pod">{{ row.pod }}</p>
          <p v-if="!row.metricsPresent" class="mb-2 text-[10px] uppercase tracking-wide text-amber-700 dark:text-amber-400">
            waiting for metrics
          </p>
          <div class="grid grid-cols-2 gap-3">
            <div>
              <p class="mb-1 text-[10px] font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">Memory</p>
              <ResourceControl
                kind="memory"
                :usage="row.memory"
                :limit="row.memoryLimit"
                size="sm"
                readonly
              />
            </div>
            <div>
              <p class="mb-1 text-[10px] font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">CPU</p>
              <ResourceControl
                kind="cpu"
                :usage="row.cpu"
                :limit="row.cpuLimit"
                size="sm"
                readonly
              />
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
