<script setup lang="ts">
import { ref, onMounted, onUnmounted, computed } from 'vue'
import { RouterLink } from 'vue-router'
import { Activity, Server, AlertTriangle, RefreshCw, TrendingUp, TrendingDown, Minus } from 'lucide-vue-next'
import type { DashboardResponse } from '@/api/types'
import { fetchDashboard, fetchUsageHistory } from '@/api/dashboard'
import { formatDateTime } from '@/utils/datetime'
import type { UsageHistoryResponse, WorkloadTrend } from '@/api/dashboard'
import { fetchClusterResourceSummary, type ClusterResourceSummary } from '@/api/resources'
import { useAuthStore } from '@/stores/auth'
import MetricSparkline from '@/components/MetricSparkline.vue'
import NoticeCallout from '@/components/NoticeCallout.vue'
import ResourceControl from '@/components/ResourceControl.vue'

const auth = useAuthStore()

function oomLinkTarget(namespace: string): string | null {
  // Only admins can mutate the Platform CR, so non-admins get the visual
  // alert but no deep-link target.
  if (namespace === 'monitoring' && auth.isAdmin) {
    return '/platform'
  }
  return null
}

const hasMonitoringOOM = computed(() =>
  Boolean(data.value?.oom_kills?.some(k => k.namespace === 'monitoring')) && auth.isAdmin,
)

const data = ref<DashboardResponse | null>(null)
const usageHistory = ref<UsageHistoryResponse | null>(null)
const clusterSummary = ref<ClusterResourceSummary | null>(null)
const loading = ref(false)
const error = ref<string | null>(null)
let refreshInterval: ReturnType<typeof setInterval> | null = null

const summarySystemMemory = computed(() => clusterSummary.value?.system.memory_bytes ?? 0)
const summarySystemCpu = computed(() => clusterSummary.value?.system.cpu_millis ?? 0)
const summaryAppsMemory = computed(() => clusterSummary.value?.apps.memory_bytes ?? 0)
const summaryAppsCpu = computed(() => clusterSummary.value?.apps.cpu_millis ?? 0)
const summaryAllocatableMemory = computed(() => clusterSummary.value?.allocatable.memory_bytes ?? 0)
const summaryAllocatableCpu = computed(() => clusterSummary.value?.allocatable.cpu_millis ?? 0)

const healthyCount = computed(() =>
  data.value?.components.filter(c => c.healthy).length ?? 0,
)
const totalCount = computed(() => data.value?.components.length ?? 0)

const overallHealth = computed(() => {
  if (!data.value) return 'unknown'
  if (healthyCount.value === totalCount.value) return 'healthy'
  return 'degraded'
})

const nodeMemoryPct = computed(() => {
  const h = usageHistory.value?.node
  if (!h?.allocatable_memory_bytes || !h.history.length) return 0
  const latest = h.history[h.history.length - 1]
  return Math.round(latest.memory_bytes / h.allocatable_memory_bytes * 100)
})

const nodeCPUPct = computed(() => {
  const h = usageHistory.value?.node
  if (!h?.allocatable_cpu_millis || !h.history.length) return 0
  const latest = h.history[h.history.length - 1]
  return Math.round(latest.cpu_millis / h.allocatable_cpu_millis * 100)
})

const nodeMemoryHistory = computed(() =>
  usageHistory.value?.node.history.map(s => s.memory_bytes) ?? [],
)

const nodeCPUHistory = computed(() =>
  usageHistory.value?.node.history.map(s => s.cpu_millis) ?? [],
)

function pressureColor(pct: number): string {
  if (pct >= 90) return '#ef4444'  // red
  if (pct >= 70) return '#f59e0b'  // amber
  return '#10b981'                  // green
}

function pressureBarClass(pct: number): string {
  if (pct >= 90) return 'bg-red-500'
  if (pct >= 70) return 'bg-amber-500'
  return 'bg-emerald-500'
}

function formatBytes(bytes: number): string {
  if (bytes >= 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024 * 1024)).toFixed(1)} Gi`
  return `${Math.round(bytes / (1024 * 1024))} Mi`
}

function workloadMemHistory(wl: WorkloadTrend): number[] {
  return wl.history.map(s => s.memory_bytes)
}

function sparklineColor(wl: WorkloadTrend): string {
  if (wl.anomaly) return '#ef4444'
  if (wl.growth_pct > 10) return '#f59e0b'
  return '#0ea5e9'
}

// Sort workloads: anomalies first, then by current memory descending
const sortedWorkloads = computed(() => {
  if (!usageHistory.value?.workloads) return []
  return [...usageHistory.value.workloads].sort((a, b) => {
    if (a.anomaly !== b.anomaly) return a.anomaly ? -1 : 1
    const aMem = a.history.length ? a.history[a.history.length - 1].memory_bytes : 0
    const bMem = b.history.length ? b.history[b.history.length - 1].memory_bytes : 0
    return bMem - aMem
  })
})

async function load() {
  loading.value = true
  error.value = null
  try {
    const [dashboard, history, summary] = await Promise.all([
      fetchDashboard(),
      fetchUsageHistory().catch(() => null),
      fetchClusterResourceSummary().catch(() => null),
    ])
    data.value = dashboard
    if (history) usageHistory.value = history
    if (summary) clusterSummary.value = summary
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'unknown error'
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  load()
  // Pause polling while the tab is hidden so idle windows don't keep hitting
  // the dashboard endpoint.
  refreshInterval = setInterval(() => {
    if (typeof document !== 'undefined' && document.hidden) return
    load()
  }, 30_000)
})

onUnmounted(() => {
  if (refreshInterval) {
    clearInterval(refreshInterval)
  }
})
</script>

<template>
  <div class="animate-fade-in">
    <!-- Header -->
    <div class="mb-8 flex items-center justify-between">
      <div>
        <h1 class="text-2xl font-semibold tracking-tight text-slate-900 dark:text-slate-50">Dashboard</h1>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Cluster health and component status</p>
      </div>
      <button
        class="flex items-center gap-2 rounded-lg border border-slate-200 px-3 py-1.5 text-sm text-slate-600 transition-colors hover:bg-slate-50 dark:border-slate-700 dark:text-slate-400 dark:hover:bg-slate-800"
        :disabled="loading"
        @click="load"
      >
        <RefreshCw class="h-4 w-4" :class="{ 'animate-spin': loading }" :stroke-width="1.75" />
        Refresh
      </button>
    </div>

    <!-- Error banner -->
    <NoticeCallout
      v-if="error"
      tone="danger"
      class="mb-6 p-4 text-sm text-red-700 dark:text-slate-300"
    >
      {{ error }}
    </NoticeCallout>

    <!-- OOM kills warning -->
    <NoticeCallout
      v-if="data?.oom_kills?.length"
      tone="warning"
      class="mb-6 p-4"
    >
      <div class="flex items-center gap-2">
        <AlertTriangle class="h-5 w-5 text-amber-600 dark:text-orange-300" :stroke-width="1.75" />
        <span class="text-sm font-medium text-amber-800 dark:text-orange-300">
          {{ data.oom_kills.length }} OOM-killed pod{{ data.oom_kills.length > 1 ? 's' : '' }} in the last 24 hours
        </span>
      </div>
      <ul class="mt-2 space-y-1 pl-7">
        <li
          v-for="kill in data.oom_kills"
          :key="`${kill.namespace}/${kill.pod}`"
          class="text-sm text-amber-700 dark:text-slate-400"
        >
          <RouterLink
            v-if="oomLinkTarget(kill.namespace)"
            :to="oomLinkTarget(kill.namespace) as string"
            class="font-mono underline-offset-2 hover:underline dark:text-orange-300"
          >{{ kill.namespace }}/{{ kill.pod }}</RouterLink>
          <span v-else class="font-mono dark:text-slate-300">{{ kill.namespace }}/{{ kill.pod }}</span>
          <span class="ml-2 text-amber-500 dark:text-slate-400">{{ formatDateTime(kill.time) }}</span>
        </li>
      </ul>
      <p v-if="hasMonitoringOOM" class="mt-3 pl-7 text-xs text-amber-700 dark:text-slate-400">
        System component OOMs are auto-bumped in the background. Click the row to review the Platform page and see how much memory each component has.
      </p>
    </NoticeCallout>

    <!-- Cluster resource summary: four read-only gauges. Each is a link
         to the page that controls the workloads it covers. -->
    <div v-if="clusterSummary" class="mb-8 grid grid-cols-2 gap-4 lg:grid-cols-4">
      <RouterLink
        v-if="auth.isAdmin"
        to="/platform"
        class="rounded-xl border border-slate-200 bg-white p-4 transition-colors hover:border-slate-300 dark:border-slate-800 dark:bg-slate-900 dark:hover:border-slate-700"
      >
        <p class="mb-1 text-xs font-medium uppercase tracking-wide text-slate-500 dark:text-slate-400">System memory</p>
        <ResourceControl
          kind="memory"
          :usage="summarySystemMemory"
          :limit="summaryAllocatableMemory"
          size="sm"
          readonly
        />
      </RouterLink>
      <div
        v-else
        class="rounded-xl border border-slate-200 bg-white p-4 dark:border-slate-800 dark:bg-slate-900"
      >
        <p class="mb-1 text-xs font-medium uppercase tracking-wide text-slate-500 dark:text-slate-400">System memory</p>
        <ResourceControl
          kind="memory"
          :usage="summarySystemMemory"
          :limit="summaryAllocatableMemory"
          size="sm"
          readonly
        />
      </div>

      <RouterLink
        v-if="auth.isAdmin"
        to="/platform"
        class="rounded-xl border border-slate-200 bg-white p-4 transition-colors hover:border-slate-300 dark:border-slate-800 dark:bg-slate-900 dark:hover:border-slate-700"
      >
        <p class="mb-1 text-xs font-medium uppercase tracking-wide text-slate-500 dark:text-slate-400">System CPU</p>
        <ResourceControl
          kind="cpu"
          :usage="summarySystemCpu"
          :limit="summaryAllocatableCpu"
          size="sm"
          readonly
        />
      </RouterLink>
      <div
        v-else
        class="rounded-xl border border-slate-200 bg-white p-4 dark:border-slate-800 dark:bg-slate-900"
      >
        <p class="mb-1 text-xs font-medium uppercase tracking-wide text-slate-500 dark:text-slate-400">System CPU</p>
        <ResourceControl
          kind="cpu"
          :usage="summarySystemCpu"
          :limit="summaryAllocatableCpu"
          size="sm"
          readonly
        />
      </div>

      <RouterLink
        to="/apps"
        class="rounded-xl border border-slate-200 bg-white p-4 transition-colors hover:border-slate-300 dark:border-slate-800 dark:bg-slate-900 dark:hover:border-slate-700"
      >
        <p class="mb-1 text-xs font-medium uppercase tracking-wide text-slate-500 dark:text-slate-400">App memory</p>
        <ResourceControl
          kind="memory"
          :usage="summaryAppsMemory"
          :limit="summaryAllocatableMemory"
          size="sm"
          readonly
        />
      </RouterLink>

      <RouterLink
        to="/apps"
        class="rounded-xl border border-slate-200 bg-white p-4 transition-colors hover:border-slate-300 dark:border-slate-800 dark:bg-slate-900 dark:hover:border-slate-700"
      >
        <p class="mb-1 text-xs font-medium uppercase tracking-wide text-slate-500 dark:text-slate-400">App CPU</p>
        <ResourceControl
          kind="cpu"
          :usage="summaryAppsCpu"
          :limit="summaryAllocatableCpu"
          size="sm"
          readonly
        />
      </RouterLink>
    </div>

    <!-- Summary cards -->
    <div class="mb-8 grid grid-cols-1 gap-4 sm:grid-cols-3">
      <!-- Overall health -->
      <div class="rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
        <div class="flex items-center justify-between">
          <span class="text-sm font-medium text-slate-500 dark:text-slate-400">Cluster Health</span>
          <Activity class="h-5 w-5 text-slate-400 dark:text-slate-500" :stroke-width="1.75" />
        </div>
        <div class="mt-3 flex items-center gap-2.5">
          <span
            class="inline-block h-3 w-3 rounded-full"
            :class="{
              'bg-emerald-500': overallHealth === 'healthy',
              'bg-amber-500': overallHealth === 'degraded',
              'bg-slate-400': overallHealth === 'unknown',
            }"
          />
          <span class="font-mono text-2xl font-bold text-slate-900 dark:text-slate-50">
            {{ overallHealth }}
          </span>
        </div>
      </div>

      <!-- Components -->
      <div class="rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
        <div class="flex items-center justify-between">
          <span class="text-sm font-medium text-slate-500 dark:text-slate-400">Components</span>
          <Server class="h-5 w-5 text-slate-400 dark:text-slate-500" :stroke-width="1.75" />
        </div>
        <div class="mt-3">
          <span class="font-mono text-2xl font-bold text-slate-900 dark:text-slate-50">
            {{ healthyCount }}/{{ totalCount }}
          </span>
          <span class="ml-2 text-sm text-slate-500 dark:text-slate-400">healthy</span>
        </div>
      </div>

      <!-- Nodes -->
      <div class="rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
        <div class="flex items-center justify-between">
          <span class="text-sm font-medium text-slate-500 dark:text-slate-400">Nodes</span>
          <Server class="h-5 w-5 text-slate-400 dark:text-slate-500" :stroke-width="1.75" />
        </div>
        <div class="mt-3">
          <span class="font-mono text-2xl font-bold text-slate-900 dark:text-slate-50">
            {{ data?.nodes?.length ?? 0 }}
          </span>
          <span class="ml-2 text-sm text-slate-500 dark:text-slate-400">
            {{ data?.nodes?.filter(n => n.status === 'Ready').length ?? 0 }} ready
          </span>
        </div>
      </div>
    </div>

    <!-- Components grid -->
    <div class="mb-8">
      <div class="mb-4">
        <h2 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Components</h2>
      </div>

      <div v-if="loading && !data" class="text-sm text-slate-500 dark:text-slate-400">Loading...</div>

      <div v-else-if="data" class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
        <div
          v-for="component in data.components"
          :key="component.name"
          class="flex items-center gap-3 rounded-xl border border-slate-200 bg-white px-4 py-3 dark:border-slate-800 dark:bg-slate-900"
        >
          <span
            class="inline-block h-2.5 w-2.5 shrink-0 rounded-full"
            :class="component.healthy ? 'bg-emerald-500' : 'bg-red-500'"
          />
          <div class="min-w-0">
            <p class="text-sm font-medium text-slate-900 dark:text-slate-50">{{ component.name }}</p>
            <p class="truncate text-xs text-slate-500 dark:text-slate-400">{{ component.message }}</p>
          </div>
        </div>
      </div>
    </div>

    <!-- Nodes table -->
    <div class="rounded-xl border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900">
      <div class="border-b border-slate-200 px-5 py-4 dark:border-slate-800">
        <h2 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Nodes</h2>
      </div>

      <div v-if="loading && !data" class="px-5 py-8 text-center text-sm text-slate-500 dark:text-slate-400">
        Loading...
      </div>

      <div v-else-if="data?.nodes?.length" class="overflow-x-auto">
        <table class="w-full">
          <thead>
            <tr class="border-b border-slate-100 text-left text-xs font-medium uppercase tracking-wider text-slate-500 dark:border-slate-800 dark:text-slate-400">
              <th class="px-5 py-3">Name</th>
              <th class="px-5 py-3">Status</th>
              <th class="px-5 py-3">CPU</th>
              <th class="px-5 py-3">Memory</th>
              <th class="px-5 py-3">Disk</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="node in data.nodes"
              :key="node.name"
              class="border-b border-slate-50 last:border-0 dark:border-slate-800/50"
            >
              <td class="px-5 py-3.5 text-sm font-medium text-slate-900 dark:text-slate-50">
                {{ node.name }}
              </td>
              <td class="px-5 py-3.5">
                <span class="inline-flex items-center gap-1.5 text-sm">
                  <span
                    class="inline-block h-2 w-2 rounded-full"
                    :class="node.status === 'Ready' ? 'bg-emerald-500' : 'bg-red-500'"
                  />
                  <span :class="node.status === 'Ready' ? 'text-emerald-700 dark:text-emerald-400' : 'text-red-700 dark:text-red-400'">
                    {{ node.status }}
                  </span>
                </span>
              </td>
              <td class="px-5 py-3.5 font-mono text-sm text-slate-600 dark:text-slate-400">
                {{ node.cpu_usage }}
              </td>
              <td class="px-5 py-3.5 font-mono text-sm text-slate-600 dark:text-slate-400">
                {{ node.memory_usage }}
              </td>
              <td class="px-5 py-3.5 font-mono text-sm text-slate-600 dark:text-slate-400">
                {{ node.disk_usage }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <div v-else class="px-5 py-8 text-center text-sm text-slate-500 dark:text-slate-400">
        No nodes found. Run <code class="rounded bg-slate-100 px-1.5 py-0.5 font-mono text-xs dark:bg-slate-800">kip install</code> to set up your cluster.
      </div>
    </div>

    <!-- Metrics unavailable -->
    <NoticeCallout
      v-if="usageHistory?.degraded"
      tone="warning"
      class="mt-8 flex items-start gap-3 p-4"
    >
      <AlertTriangle class="mt-0.5 h-5 w-5 shrink-0 text-amber-600 dark:text-orange-300" :stroke-width="1.75" />
      <p class="text-sm text-amber-800 dark:text-slate-300">
        Metrics are unavailable right now. Prometheus didn't respond, so the usage charts may be stale or empty until it recovers.
      </p>
    </NoticeCallout>

    <!-- Node resource pressure -->
    <div v-if="usageHistory?.node.history.length" class="mt-8 grid grid-cols-1 gap-4 sm:grid-cols-2">
      <!-- Memory pressure -->
      <div class="rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
        <div class="mb-3 flex items-center justify-between">
          <span class="text-sm font-medium text-slate-500 dark:text-slate-400">Node Memory</span>
          <span
            class="font-mono text-sm font-bold"
            :style="{ color: pressureColor(nodeMemoryPct) }"
          >
            {{ nodeMemoryPct }}%
          </span>
        </div>
        <div class="mb-3 h-2 w-full overflow-hidden rounded-full bg-slate-100 dark:bg-slate-800">
          <div
            class="h-full rounded-full transition-all duration-500"
            :class="pressureBarClass(nodeMemoryPct)"
            :style="{ width: `${Math.min(nodeMemoryPct, 100)}%` }"
          />
        </div>
        <MetricSparkline
          :data="nodeMemoryHistory"
          :width="280"
          :height="40"
          :color="pressureColor(nodeMemoryPct)"
          :fill-opacity="0.1"
        />
        <p class="mt-2 text-xs text-slate-400 dark:text-slate-500">
          {{ formatBytes(usageHistory.node.history[usageHistory.node.history.length - 1].memory_bytes) }}
          of {{ formatBytes(usageHistory.node.allocatable_memory_bytes) }} — last hour
        </p>
      </div>

      <!-- CPU pressure -->
      <div class="rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
        <div class="mb-3 flex items-center justify-between">
          <span class="text-sm font-medium text-slate-500 dark:text-slate-400">Node CPU</span>
          <span
            class="font-mono text-sm font-bold"
            :style="{ color: pressureColor(nodeCPUPct) }"
          >
            {{ nodeCPUPct }}%
          </span>
        </div>
        <div class="mb-3 h-2 w-full overflow-hidden rounded-full bg-slate-100 dark:bg-slate-800">
          <div
            class="h-full rounded-full transition-all duration-500"
            :class="pressureBarClass(nodeCPUPct)"
            :style="{ width: `${Math.min(nodeCPUPct, 100)}%` }"
          />
        </div>
        <MetricSparkline
          :data="nodeCPUHistory"
          :width="280"
          :height="40"
          :color="pressureColor(nodeCPUPct)"
          :fill-opacity="0.1"
        />
        <p class="mt-2 text-xs text-slate-400 dark:text-slate-500">
          {{ usageHistory.node.history[usageHistory.node.history.length - 1].cpu_millis }}m
          of {{ usageHistory.node.allocatable_cpu_millis }}m, last hour
        </p>
      </div>
    </div>

    <!-- Workload trends -->
    <div v-if="sortedWorkloads.length" class="mt-8 rounded-xl border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900">
      <div class="border-b border-slate-200 px-5 py-4 dark:border-slate-800">
        <h2 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Workload Memory Trends</h2>
        <p class="mt-0.5 text-xs text-slate-400 dark:text-slate-500">Last hour, sorted by memory usage</p>
      </div>

      <div class="overflow-x-auto">
        <table class="w-full">
          <thead>
            <tr class="border-b border-slate-100 text-left text-xs font-medium uppercase tracking-wider text-slate-500 dark:border-slate-800 dark:text-slate-400">
              <th class="px-5 py-3">Workload</th>
              <th class="px-5 py-3">Memory</th>
              <th class="px-5 py-3">Trend</th>
              <th class="px-5 py-3 text-right">Change</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="wl in sortedWorkloads"
              :key="`${wl.namespace}/${wl.name}`"
              class="border-b border-slate-50 last:border-0 dark:border-slate-800/50"
              :class="wl.anomaly ? 'bg-red-50/50 dark:bg-red-950/20' : ''"
            >
              <td class="px-5 py-3">
                <div class="text-sm font-medium text-slate-900 dark:text-slate-50">{{ wl.name }}</div>
                <div class="text-xs text-slate-400 dark:text-slate-500">{{ wl.namespace }}</div>
              </td>
              <td class="px-5 py-3 font-mono text-sm text-slate-600 dark:text-slate-400">
                {{ wl.history.length ? formatBytes(wl.history[wl.history.length - 1].memory_bytes) : 'n/a' }}
              </td>
              <td class="px-5 py-3">
                <MetricSparkline
                  :data="workloadMemHistory(wl)"
                  :width="100"
                  :height="24"
                  :color="sparklineColor(wl)"
                  :fill-opacity="0.1"
                />
              </td>
              <td class="px-5 py-3 text-right">
                <span
                  v-if="wl.growth_pct > 1"
                  class="inline-flex items-center gap-1 text-sm font-medium"
                  :class="wl.anomaly ? 'text-red-600 dark:text-red-400' : wl.growth_pct > 10 ? 'text-amber-600 dark:text-amber-400' : 'text-slate-500 dark:text-slate-400'"
                >
                  <TrendingUp class="h-3.5 w-3.5" />
                  +{{ wl.growth_pct.toFixed(0) }}%
                </span>
                <span
                  v-else-if="wl.growth_pct < -1"
                  class="inline-flex items-center gap-1 text-sm font-medium text-emerald-600 dark:text-emerald-400"
                >
                  <TrendingDown class="h-3.5 w-3.5" />
                  {{ wl.growth_pct.toFixed(0) }}%
                </span>
                <span v-else class="inline-flex items-center gap-1 text-sm text-slate-400 dark:text-slate-500">
                  <Minus class="h-3.5 w-3.5" />
                  stable
                </span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>
