<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { useRouter } from 'vue-router'
import { Terminal } from 'lucide-vue-next'
import { Database, Server, Copy, RefreshCw, Sparkles, Plus, Trash2, FileCode2, ExternalLink } from 'lucide-vue-next'

const router = useRouter()
import SidePanel from '@/components/SidePanel.vue'
import ResourceControl from '@/components/ResourceControl.vue'
import MetricSparkline from '@/components/MetricSparkline.vue'
import TabBar from '@/components/TabBar.vue'
import ServiceDiagnoseModal from '@/components/ServiceDiagnoseModal.vue'
import ServiceMigrateDataPanel from '@/components/ServiceMigrateDataPanel.vue'
import ServiceSharesPanel from '@/components/ServiceSharesPanel.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import NoticeCallout from '@/components/NoticeCallout.vue'
import { useModal } from '@/composables/useModal'
import { useToast } from '@/composables/useToast'
import { useResourceUsage } from '@/composables/useResourceUsage'
import {
  parseCpuQuantity,
  parseMemoryQuantity,
  toKubernetesCpuQuantity,
  toKubernetesMemoryQuantity,
} from '@/utils/resources'
import { useProjectsStore } from '@/stores/projects'
import { useAuthStore } from '@/stores/auth'
import { fetchServices, fetchServiceInfo, fetchServiceResources, updateServiceResources, fetchRolloutStatus, fetchServiceLogs, createService, deleteService, type ServiceStatus, type ServiceInfo, type ServiceResources, type ServiceLogEntry } from '@/api/services'
import { fetchProjects, type Project } from '@/api/projects'
import LogAnalysis from '@/components/LogAnalysis.vue'

const toast = useToast()
const modal = useModal()
const projectsStore = useProjectsStore()
const authStore = useAuthStore()

function openServiceDiagnose(name: string, namespace: string) {
  modal.open(ServiceDiagnoseModal, { serviceName: name, namespace })
}

const services = ref<ServiceStatus[]>([])
const loading = ref(false)
const error = ref<string | null>(null)

// Create form
const showCreate = ref(false)
const creating = ref(false)
const allProjects = ref<Project[]>([])
const newName = ref('')
const newType = ref('postgres')
const newNamespace = ref('default')
const newStorage = ref('')
const newMemory = ref('')
const newCPU = ref('')
const newVersion = ref('')

const serviceTypes = [
  { value: 'postgres', label: 'PostgreSQL', defaultStorage: '5Gi' },
  { value: 'mysql', label: 'MySQL', defaultStorage: '5Gi' },
  { value: 'redis', label: 'Redis', defaultStorage: '1Gi' },
  { value: 'mongodb', label: 'MongoDB', defaultStorage: '5Gi' },
  { value: 'rabbitmq', label: 'RabbitMQ', defaultStorage: '1Gi' },
  { value: 'opensearch', label: 'OpenSearch', defaultStorage: '5Gi' },
  { value: 'minio', label: 'MinIO', defaultStorage: '10Gi' },
  { value: 'mailhog', label: 'MailHog', defaultStorage: '1Gi' },
]

const namespaceOptions = computed(() => {
  const options: { label: string; value: string }[] = [
    { label: 'default', value: 'default' },
  ]
  for (const p of allProjects.value) {
    for (const env of p.environments) {
      options.push({ label: `${p.display_name || p.name}, ${env.name}`, value: env.namespace })
    }
  }
  return options
})
const selectedService = ref<ServiceInfo | null>(null)
const selectedNamespace = ref<string>('')
const refreshing = ref(false)
const svcDetailTab = ref<'connection' | 'logs' | 'resources' | 'migrate' | 'shares'>('connection')
// The Share tab appears only for a service that ships a browseable UI, and
// only to admins — minting a link hands out non-Dex access to that UI.
const canShare = computed(() => authStore.isAdmin && !!selectedService.value?.ui_url)

function uiHostOf(rawURL: string): string {
  try {
    return new URL(rawURL).host
  } catch {
    return ''
  }
}

function withSSOCode(rawURL: string, code: string): string {
  try {
    const u = new URL(rawURL)
    u.searchParams.set('kipper_sso', code)
    return u.toString()
  } catch {
    return rawURL
  }
}

// openServiceUI pre-mints a single-use SSO code and opens the service UI with
// it appended, so the new tab lands already signed in. The tab is opened
// synchronously (before the async mint) to stay out of the popup blocker; a
// mint failure falls back to the plain URL, where the gate's redirect dance
// signs the user in with one extra hop. If the synchronous open was itself
// blocked, we navigate the current tab instead — a second window.open after
// the await has lost the user-activation and would be blocked too.
async function openServiceUI() {
  const uiURL = selectedService.value?.ui_url
  if (!uiURL) return
  const tab = window.open('about:blank', '_blank')
  if (tab) tab.opener = null
  const host = uiHostOf(uiURL)
  const code = host ? await authStore.mintUICode(host) : null
  const target = code ? withSSOCode(uiURL, code) : uiURL
  if (tab) {
    tab.location.href = target
  } else {
    window.location.href = target
  }
}
const svcTabs = computed(() => {
  const tabs = [
    { key: 'connection', label: 'Connection' },
    { key: 'logs', label: 'Logs' },
    { key: 'resources', label: 'Resources' },
    { key: 'migrate', label: 'Migrate data' },
  ]
  if (canShare.value) tabs.push({ key: 'shares', label: 'Share' })
  return tabs
})
// Selecting a UI-less service while the Share tab is open would leave a blank
// panel; fall back to Connection when the tab disappears.
watch(canShare, available => {
  if (!available && svcDetailTab.value === 'shares') svcDetailTab.value = 'connection'
})

// Service logs
const svcLogs = ref<ServiceLogEntry[]>([])
const svcLogsLoading = ref(false)
const svcLogSearch = ref('')
const svcLogSince = ref('1h')

// Service resources
const svcResources = ref<ServiceResources>({ memory_limit: '', memory_request: '', cpu_limit: '', cpu_request: '' })
const svcResourcesLoading = ref(false)
const svcResourcesSaving = ref(false)
const svcMemoryLimit = ref('')
const svcCPULimit = ref('')

onMounted(async () => {
  await loadServices()
})

function handleDelete(name: string, namespace: string) {
  modal.open(ConfirmDialog, {
    title: `Delete service ${name}?`,
    message: 'This deletes the service and all its data. This cannot be undone.',
    confirmLabel: 'Delete service',
    confirmPhrase: name,
    onConfirm: async () => {
      modal.close()
      try {
        await deleteService(name, namespace)
        toast.success(`Service ${name} deleted`)
        if (selectedService.value?.name === name && selectedNamespace.value === namespace) {
          selectedService.value = null
          selectedNamespace.value = ''
        }
        await loadServices()
      } catch {
        toast.error('Failed to delete service')
      }
    },
  })
}

async function loadServices() {
  loading.value = true
  error.value = null
  try {
    const ns = projectsStore.globalNamespace || undefined
    const [svcs, prj] = await Promise.all([fetchServices(ns), fetchProjects()])
    services.value = svcs
    allProjects.value = prj
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'unknown error'
  } finally {
    loading.value = false
  }
}

watch(() => projectsStore.globalNamespace, () => {
  loadServices()
})

async function handleCreate() {
  if (!newName.value || !newType.value) return
  creating.value = true
  try {
    await createService({
      name: newName.value,
      type: newType.value,
      namespace: newNamespace.value,
      storage: newStorage.value || undefined,
      memory: newMemory.value || undefined,
      cpu: newCPU.value || undefined,
      version: newVersion.value || undefined,
    })
    toast.success(`Service ${newName.value} created`)
    const createdName = newName.value
    newName.value = ''
    newType.value = 'postgres'
    newStorage.value = ''
    newMemory.value = ''
    newCPU.value = ''
    newVersion.value = ''
    showCreate.value = false
    for (let i = 0; i < 10; i++) {
      await loadServices()
      if (services.value.some(s => s.name === createdName)) break
      await new Promise(r => setTimeout(r, 500))
    }
  } catch {
    toast.error('Failed to create service')
  } finally {
    creating.value = false
  }
}

async function refreshList() {
  refreshing.value = true
  try {
    const ns = projectsStore.globalNamespace || undefined
    services.value = await fetchServices(ns)
    toast.success('Services refreshed')
  } catch {
    toast.error('Failed to refresh services')
  } finally {
    refreshing.value = false
  }
}

async function showInfo(name: string, namespace: string) {
  try {
    selectedService.value = await fetchServiceInfo(name, namespace)
    selectedNamespace.value = namespace
    svcDetailTab.value = 'connection'
  } catch {
    toast.error(`Failed to load details for ${name}`)
  }
}

function closeInfo() {
  selectedService.value = null
  selectedNamespace.value = ''
}

async function switchSvcTab(tab: typeof svcDetailTab.value) {
  svcDetailTab.value = tab
  if (tab === 'logs') await loadSvcLogs()
  if (tab === 'resources') await loadSvcResources()
}

async function loadSvcLogs() {
  if (!selectedService.value) return
  svcLogsLoading.value = true
  try {
    svcLogs.value = await fetchServiceLogs(selectedService.value.name, selectedNamespace.value, {
      search: svcLogSearch.value || undefined,
      since: svcLogSince.value,
    })
  } catch {
    svcLogs.value = []
  } finally {
    svcLogsLoading.value = false
  }
}

const svcLogsText = computed(() =>
  svcLogs.value.map(e => `${e.pod} ${e.line}`).join('\n')
)

function formatLogTime(nsTimestamp: string): string {
  const ms = Number(nsTimestamp) / 1_000_000
  if (isNaN(ms)) return ''
  return new Date(ms).toLocaleTimeString('en-GB', { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

async function loadSvcResources() {
  if (!selectedService.value) return
  svcResourcesLoading.value = true
  try {
    svcResources.value = await fetchServiceResources(selectedService.value.name, selectedNamespace.value)
    svcMemoryLimit.value = svcResources.value.memory_limit || ''
    svcCPULimit.value = svcResources.value.cpu_limit || ''
  } catch {
    svcResources.value = { memory_limit: '', memory_request: '', cpu_limit: '', cpu_request: '' }
  } finally {
    svcResourcesLoading.value = false
  }
}

const showSvcConfirm = ref(false)

const svcRolloutPhase = ref<'idle' | 'updating' | 'restarting' | 'done'>('idle')

// Live usage scoped to the selected service's pods. Both service create
// paths label pods with app=<service-name>, matching what apps use.
// includePrometheus pulls the 1h sparkline + CPU throttling badge for
// the gauges; pauses automatically when the resources tab isn't open.
const svcUsageScope = computed(() => {
  if (!selectedService.value || svcDetailTab.value !== 'resources') return null
  return {
    namespace: selectedNamespace.value,
    selector: `app=${selectedService.value.name}`,
    includePrometheus: true,
  }
})
const svcUsage = useResourceUsage(svcUsageScope)

const svcMemorySparkline = computed(() => svcUsage.data.value?.memory_sparkline ?? [])
const svcCpuSparkline = computed(() => svcUsage.data.value?.cpu_sparkline ?? [])
const svcCpuThrottlingPct = computed(() => svcUsage.data.value?.cpu_throttling_pct ?? null)

const svcMemoryLimitBytes = computed(() => {
  if (!svcMemoryLimit.value) return 0
  try {
    return parseMemoryQuantity(svcMemoryLimit.value)
  } catch {
    return 0
  }
})
const svcCpuLimitMillis = computed(() => {
  if (!svcCPULimit.value) return 0
  try {
    return parseCpuQuantity(svcCPULimit.value)
  } catch {
    return 0
  }
})

const svcUsagePodCount = computed(() => svcUsage.data.value?.totals.pod_count ?? 0)
const svcPerPodMemoryBytes = computed(() => {
  const pods = svcUsagePodCount.value
  if (pods === 0) return 0
  return Math.round((svcUsage.data.value?.totals.memory_bytes ?? 0) / pods)
})
const svcPerPodCpuMillis = computed(() => {
  const pods = svcUsagePodCount.value
  if (pods === 0) return 0
  return Math.round((svcUsage.data.value?.totals.cpu_millis ?? 0) / pods)
})

// Per-pod breakdown sums every container in each pod, matching the
// drill-down route's reading. Same trade-off as AppDetail: the OOM
// killer fires per-container, but for Kipper's mostly-single-container
// workloads "how full is this pod" is the reading users want.
const svcPerPodUsage = computed(() => {
  const rows = svcUsage.data.value?.containers ?? []
  if (!selectedService.value) return []
  const byPod = new Map<string, { memory: number; cpu: number; present: boolean }>()
  for (const c of rows) {
    const existing = byPod.get(c.pod) ?? { memory: 0, cpu: 0, present: false }
    existing.memory += c.metrics_present ? c.memory_bytes : 0
    existing.cpu += c.metrics_present ? c.cpu_millis : 0
    existing.present = existing.present || c.metrics_present
    byPod.set(c.pod, existing)
  }
  return Array.from(byPod.entries()).map(([pod, v]) => ({ pod, ...v }))
})

// pendingSvcResize captures the values that a confirmed Apply will push.
// The confirm modal exists because every service resize causes downtime
// (StatefulSet rolling restart with a single replica). We don't want the
// slider's Apply to start a restart without the user understanding that.
const pendingSvcResize = ref<{ memoryLimit: string; cpuLimit: string; kindLabel: string } | null>(null)

function requestSvcMemoryApply(bytes: number) {
  if (!selectedService.value) return
  const quantity = toKubernetesMemoryQuantity(bytes)
  pendingSvcResize.value = {
    memoryLimit: quantity,
    cpuLimit: svcCPULimit.value,
    kindLabel: `Memory limit → ${quantity}`,
  }
  showSvcConfirm.value = true
}

function requestSvcCpuApply(millis: number) {
  if (!selectedService.value) return
  const quantity = toKubernetesCpuQuantity(millis)
  pendingSvcResize.value = {
    memoryLimit: svcMemoryLimit.value,
    cpuLimit: quantity,
    kindLabel: `CPU limit → ${quantity}`,
  }
  showSvcConfirm.value = true
}

async function saveSvcResources() {
  if (!selectedService.value) return
  const serviceName = selectedService.value.name
  const namespace = selectedNamespace.value
  showSvcConfirm.value = false
  svcResourcesSaving.value = true
  svcRolloutPhase.value = 'updating'

  // pendingSvcResize is set by the slider-driven path; the Save-button
  // path leaves it nil and falls through to the text-input values.
  const payload = pendingSvcResize.value ?? {
    memoryLimit: svcMemoryLimit.value,
    cpuLimit: svcCPULimit.value,
  }
  pendingSvcResize.value = null

  try {
    await updateServiceResources(serviceName, namespace, {
      memory_limit: payload.memoryLimit,
      cpu_limit: payload.cpuLimit,
    })
    svcMemoryLimit.value = payload.memoryLimit
    svcCPULimit.value = payload.cpuLimit

    svcRolloutPhase.value = 'restarting'

    // Poll for rollout completion (max 2 minutes)
    const maxAttempts = 24
    for (let i = 0; i < maxAttempts; i++) {
      await new Promise(resolve => setTimeout(resolve, 5000))
      try {
        const status = await fetchRolloutStatus(serviceName, namespace)
        if (status.ready) {
          svcRolloutPhase.value = 'done'
          toast.success('Service restarted with new resource limits')
          setTimeout(() => { svcRolloutPhase.value = 'idle' }, 3000)
          return
        }
      } catch {
        // keep polling
      }
    }

    // Timeout — still not ready after 2 minutes
    toast.error('Service is still restarting. Check the dashboard')
    svcRolloutPhase.value = 'idle'
  } catch {
    toast.error('Failed to update resources')
    svcRolloutPhase.value = 'idle'
  } finally {
    svcResourcesSaving.value = false
  }
}

async function copyHost() {
  if (!selectedService.value) return
  try {
    await navigator.clipboard.writeText(selectedService.value.host)
    toast.success('Host copied to clipboard')
  } catch {
    toast.error('Failed to copy')
  }
}

function statusColor(status: string): string {
  return status === 'running' ? 'bg-emerald-500' : 'bg-amber-500'
}

function statusTextColor(status: string): string {
  return status === 'running'
    ? 'text-emerald-700 dark:text-emerald-400'
    : 'text-amber-700 dark:text-amber-400'
}

function typeIcon(type: string): string {
  switch (type) {
    case 'postgres': return 'PostgreSQL'
    case 'redis': return 'Redis'
    case 'minio': return 'MinIO'
    case 'mailhog': return 'MailHog'
    case 'elasticsearch': return 'Elasticsearch'
    default: return type
  }
}
</script>

<template>
  <div class="animate-fade-in">
    <!-- Header -->
    <div class="mb-8 flex items-center justify-between">
      <div>
        <h1 class="text-2xl font-semibold tracking-tight text-slate-900 dark:text-slate-50">Services</h1>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Databases, caches, and stateful infrastructure</p>
      </div>
      <div class="flex items-center gap-2">
        <button
          v-if="authStore.isDeployer"
          @click="showCreate = !showCreate"
          class="inline-flex items-center gap-1.5 rounded-lg bg-kipper-600 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-kipper-700"
        >
          <Plus class="h-4 w-4" />
          Add service
        </button>
        <button
          @click="refreshList"
          :disabled="refreshing"
          class="rounded-lg border border-slate-300 p-2.5 text-slate-600 transition-colors hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
          title="Refresh services"
        >
          <RefreshCw class="h-4 w-4" :class="refreshing ? 'animate-spin' : ''" :stroke-width="1.75" />
        </button>
      </div>
    </div>

    <!-- Create form -->
    <div v-if="showCreate" class="mb-6 rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
      <h3 class="mb-4 text-sm font-semibold text-slate-900 dark:text-slate-50">Add service</h3>
      <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div>
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Name</label>
          <input v-model="newName" type="text" placeholder="mydb" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Type</label>
          <select v-model="newType" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50">
            <option v-for="t in serviceTypes" :key="t.value" :value="t.value">{{ t.label }} (default {{ t.defaultStorage }})</option>
          </select>
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Namespace</label>
          <select v-model="newNamespace" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50">
            <option v-for="opt in namespaceOptions" :key="opt.value" :value="opt.value">{{ opt.label }}</option>
          </select>
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Storage (optional)</label>
          <input v-model="newStorage" type="text" :placeholder="serviceTypes.find(t => t.value === newType)?.defaultStorage || '5Gi'" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Memory limit (optional)</label>
          <input v-model="newMemory" type="text" placeholder="e.g. 512Mi, 1Gi" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">CPU limit (optional)</label>
          <input v-model="newCPU" type="text" placeholder="e.g. 250m, 500m" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Version (optional)</label>
          <input v-model="newVersion" type="text" placeholder="e.g. 16-alpine, 8-oracle, 7-alpine" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
        </div>
      </div>
      <div class="mt-4 flex justify-end gap-2">
        <button @click="showCreate = false" class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800">Cancel</button>
        <button @click="handleCreate" :disabled="!newName || creating" class="rounded-lg bg-kipper-600 px-4 py-2 text-sm font-medium text-white hover:bg-kipper-700 disabled:opacity-50">
          {{ creating ? 'Creating...' : 'Create service' }}
        </button>
      </div>
    </div>


    <!-- Loading -->
    <div v-if="loading" class="py-12 text-center text-sm text-slate-500 dark:text-slate-400">Loading...</div>

    <!-- Error -->
    <NoticeCallout v-else-if="error" tone="danger" class="px-4 py-3 text-sm text-red-700 dark:text-slate-300">
      {{ error }}
    </NoticeCallout>

    <!-- Service list -->
    <div v-else-if="services.length" class="space-y-3">
      <div
        v-for="svc in services"
        :key="svc.name"
        class="group flex flex-wrap items-center justify-between gap-y-3 rounded-xl border border-slate-200 bg-white p-5 transition-colors hover:border-kipper-300 dark:border-slate-800 dark:bg-slate-900 dark:hover:border-kipper-700 cursor-pointer"
        @click="showInfo(svc.name, svc.namespace)"
      >
        <div class="flex items-center gap-4">
          <span class="inline-flex h-10 w-10 items-center justify-center rounded-lg bg-slate-100 dark:bg-slate-800">
            <Database class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
          </span>
          <div>
            <div class="text-sm font-semibold text-slate-900 dark:text-slate-50">{{ svc.name }}</div>
            <div class="mt-0.5 text-xs text-slate-500 dark:text-slate-400">{{ typeIcon(svc.type) }}</div>
          </div>
        </div>

        <!--
          The remedy the reconciler wrote onto a refused service. Full width so
          the message keeps its sentence, and only present when there is one.
        -->
        <p
          v-if="svc.blockedReason"
          class="order-last w-full rounded-lg border border-amber-200 bg-amber-50 px-3 py-2 text-xs leading-relaxed text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-200"
        >
          <span class="font-semibold">{{ svc.blockedReason }}</span>
          {{ svc.blockedMessage }}
        </p>

        <div class="flex items-center gap-4">
          <!-- Status -->
          <span class="inline-flex items-center gap-1.5 text-sm">
            <span class="inline-block h-2 w-2 rounded-full" :class="statusColor(svc.status)" />
            <span :class="statusTextColor(svc.status)">{{ svc.status }}</span>
          </span>

          <!-- Ready -->
          <span class="font-mono text-xs text-slate-500 dark:text-slate-400">
            {{ svc.ready }}
          </span>

          <!-- Storage -->
          <span class="inline-flex items-center gap-1 rounded-md bg-slate-100 px-2 py-0.5 text-xs font-medium text-slate-700 dark:bg-slate-800 dark:text-slate-300">
            <Server class="h-3 w-3" :stroke-width="1.75" />
            {{ svc.storage }}
          </span>

          <!-- Open data console (postgres/mysql only for now) -->
          <button
            v-if="['postgres', 'mysql'].includes(svc.type)"
            @click.stop="router.push({ name: 'service-data', params: { name: svc.name }, query: { namespace: svc.namespace } })"
            class="rounded-lg p-1.5 text-slate-400 transition-colors hover:bg-kipper-50 hover:text-kipper-600 dark:hover:bg-kipper-950 dark:hover:text-kipper-400"
            title="Open SQL editor"
          >
            <FileCode2 class="h-4 w-4" :stroke-width="1.75" />
          </button>
          <!-- Delete -->
          <button
            v-if="authStore.isDeployer"
            @click.stop="handleDelete(svc.name, svc.namespace)"
            class="rounded-lg p-1.5 text-slate-400 md:opacity-0 transition-opacity hover:bg-red-50 hover:text-red-600 group-hover:opacity-100 dark:hover:bg-red-950 dark:hover:text-red-400"
            title="Delete service"
          >
            <Trash2 class="h-4 w-4" :stroke-width="1.75" />
          </button>
        </div>
      </div>
    </div>

    <!-- Empty state -->
    <div v-else class="rounded-xl border border-dashed border-slate-300 py-16 text-center dark:border-slate-700">
      <Database class="mx-auto mb-3 h-10 w-10 text-slate-400 dark:text-slate-500" :stroke-width="1.5" />
      <p class="text-sm font-medium text-slate-900 dark:text-slate-50">No services deployed</p>
      <p class="mt-1 max-w-sm mx-auto text-sm text-slate-500 dark:text-slate-400">
        Add a database or cache with <code class="rounded bg-slate-100 px-1.5 py-0.5 font-mono text-xs dark:bg-slate-800">kip service add postgres --name mydb</code>
      </p>
    </div>

    <!-- Service detail panel -->
    <SidePanel :open="!!selectedService" :label="selectedService ? `Service details for ${selectedService.name}` : undefined" :default-width="512" @close="closeInfo">
      <template #header>
        <h2 class="text-sm font-semibold text-slate-900 dark:text-slate-50">{{ selectedService?.name }}</h2>
        <span class="text-xs text-slate-500 dark:text-slate-400">{{ selectedService ? typeIcon(selectedService.type) : '' }}</span>
      </template>
      <template #actions>
        <!--
          Opens the service's browseable web UI (e.g. MailHog
          inbox). Pre-mints a single-use SSO code so the new tab
          lands already signed in through the forwardAuth gate,
          which seats a per-host session cookie. Only shown when
          the server-side ServiceInfo response carries a ui_url,
          the single source of truth for whether a service ships a
          UI and what hostname it lives at.
        -->
        <button
          v-if="selectedService?.ui_url"
          @click="openServiceUI"
          class="rounded-lg p-1.5 text-slate-400 hover:bg-kipper-50 hover:text-kipper-600 dark:hover:bg-kipper-950 dark:hover:text-kipper-400"
          :title="`Open ${selectedService.type} UI`"
        >
          <ExternalLink class="h-4 w-4" :stroke-width="1.75" />
        </button>
        <button
          v-if="selectedService && ['postgres', 'mysql'].includes(selectedService.type)"
          @click="router.push({ name: 'service-data', params: { name: selectedService.name }, query: { namespace: selectedNamespace } })"
          class="rounded-lg p-1.5 text-slate-400 hover:bg-kipper-50 hover:text-kipper-600 dark:hover:bg-kipper-950 dark:hover:text-kipper-400"
          title="Open SQL editor"
        >
          <FileCode2 class="h-4 w-4" :stroke-width="1.75" />
        </button>
        <button
          v-if="selectedService"
          @click="openServiceDiagnose(selectedService.name, selectedNamespace)"
          class="rounded-lg p-1.5 text-slate-400 hover:bg-slate-100 hover:text-slate-700 dark:hover:bg-slate-800 dark:hover:text-slate-200"
          title="AI Diagnose"
        >
          <Sparkles class="h-4 w-4" :stroke-width="1.75" />
        </button>
      </template>

      <!-- Tabs -->
      <TabBar
        :tabs="svcTabs"
        :model-value="svcDetailTab"
        aria-label="Service detail sections"
        @update:model-value="(k) => switchSvcTab(k as typeof svcDetailTab.value)"
      />

      <!-- Connection tab -->
      <div v-if="svcDetailTab === 'connection' && selectedService" class="flex-1 overflow-y-auto p-5">
        <h3 class="mb-4 text-sm font-semibold text-slate-900 dark:text-slate-50">Connection Details</h3>

        <div class="space-y-3">
          <div class="rounded-lg border border-slate-200 bg-slate-50 px-3 py-2 dark:border-slate-700 dark:bg-slate-800">
            <span class="text-xs font-medium text-slate-500 dark:text-slate-400">Host</span>
            <div class="font-mono text-sm text-slate-900 dark:text-slate-50">{{ selectedService.host }}</div>
          </div>

          <div class="rounded-lg border border-slate-200 bg-slate-50 px-3 py-2 dark:border-slate-700 dark:bg-slate-800">
            <span class="text-xs font-medium text-slate-500 dark:text-slate-400">Port</span>
            <div class="font-mono text-sm text-slate-900 dark:text-slate-50">{{ selectedService.port }}</div>
          </div>

          <div v-if="selectedService.username" class="rounded-lg border border-slate-200 bg-slate-50 px-3 py-2 dark:border-slate-700 dark:bg-slate-800">
            <span class="text-xs font-medium text-slate-500 dark:text-slate-400">Username</span>
            <div class="font-mono text-sm text-slate-900 dark:text-slate-50">{{ selectedService.username }}</div>
          </div>

          <div v-if="selectedService.database" class="rounded-lg border border-slate-200 bg-slate-50 px-3 py-2 dark:border-slate-700 dark:bg-slate-800">
            <span class="text-xs font-medium text-slate-500 dark:text-slate-400">Database</span>
            <div class="font-mono text-sm text-slate-900 dark:text-slate-50">{{ selectedService.database }}</div>
          </div>

          <!-- Host with copy button -->
          <div class="rounded-lg border border-slate-200 bg-slate-50 px-3 py-2 dark:border-slate-700 dark:bg-slate-800">
            <div class="flex items-center justify-between">
              <span class="text-xs font-medium text-slate-500 dark:text-slate-400">Host</span>
              <button @click="copyHost" class="text-slate-400 hover:text-slate-600 dark:hover:text-slate-300" title="Copy host to clipboard">
                <Copy class="h-3.5 w-3.5" />
              </button>
            </div>
            <div class="mt-1 font-mono text-xs break-all text-slate-900 dark:text-slate-50">
              {{ selectedService.host }}
            </div>
          </div>
        </div>

        <!-- Usage hint -->
        <div class="mt-6 rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800">
          <p class="text-xs font-medium text-slate-700 dark:text-slate-300">Bind to an app:</p>
          <code class="mt-1 block font-mono text-xs text-slate-600 dark:text-slate-400">
            kip service bind {{ selectedService.name }} &lt;app&gt;
          </code>
        </div>
      </div>

      <!-- Logs tab -->
      <template v-if="svcDetailTab === 'logs'">
        <div class="flex items-center gap-2 border-b border-slate-100 px-5 py-2 dark:border-slate-800">
          <input
            v-model="svcLogSearch"
            type="text"
            placeholder="Search logs..."
            class="flex-1 rounded-md border border-slate-300 bg-white px-2.5 py-1 text-xs text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
            @keyup.enter="loadSvcLogs"
          />
          <select
            v-model="svcLogSince"
            @change="loadSvcLogs"
            class="rounded-md border border-slate-300 bg-white px-2 py-1 text-xs text-slate-700 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-300"
          >
            <option value="15m">15 min</option>
            <option value="1h">1 hour</option>
            <option value="3h">3 hours</option>
            <option value="12h">12 hours</option>
            <option value="24h">24 hours</option>
          </select>
          <button @click="loadSvcLogs" class="rounded-md bg-kipper-600 px-2.5 py-1 text-xs font-medium text-white hover:bg-kipper-700">Search</button>
          <LogAnalysis
            :logs="svcLogsText"
            :app-name="selectedService?.name || ''"
            :namespace="''"
          />
        </div>

        <div class="flex-1 overflow-y-auto bg-slate-950 p-4 font-mono text-xs leading-relaxed text-slate-300">
          <div v-if="svcLogsLoading" class="text-slate-600">Loading logs...</div>
          <div v-else-if="svcLogs.length">
            <div v-for="(entry, i) in svcLogs" :key="i" class="flex gap-2">
              <span class="flex-shrink-0 text-slate-600">{{ formatLogTime(entry.timestamp) }}</span>
              <span class="flex-shrink-0 text-kipper-500">{{ entry.pod }}</span>
              <span>{{ entry.line }}</span>
            </div>
          </div>
          <div v-else class="text-slate-600">
            <Terminal class="mb-2 h-5 w-5" />
            No logs found for this service.
          </div>
        </div>
      </template>

      <!-- Resources tab -->
      <div v-if="svcDetailTab === 'resources'" class="flex-1 overflow-y-auto p-5">
        <div v-if="svcResourcesLoading" class="text-sm text-slate-500 dark:text-slate-400">Loading...</div>
        <div v-else class="space-y-4">
          <p class="text-xs text-slate-500 dark:text-slate-400">
            Drag a slider to preview a new limit; Apply restarts the service with the new value. Database services pre-fill from the <code class="rounded bg-slate-100 px-1 dark:bg-slate-800">database</code> profile defaults at creation.
          </p>

          <div class="grid grid-cols-1 gap-6 md:grid-cols-2">
            <div>
              <h4 class="mb-2 text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">Memory</h4>
              <ResourceControl
                kind="memory"
                :usage="svcPerPodMemoryBytes"
                :limit="svcMemoryLimitBytes"
                :applying="svcResourcesSaving || svcRolloutPhase !== 'idle'"
                size="md"
                @apply="requestSvcMemoryApply"
              />
              <div v-if="svcMemorySparkline.length > 1" class="mt-2 flex items-center justify-center gap-2 text-[10px] text-slate-400 dark:text-slate-500">
                <span class="uppercase tracking-wide">Last hour</span>
                <MetricSparkline :data="svcMemorySparkline" :width="180" :height="28" color="#0ea5e9" />
              </div>
              <p v-if="svcResources.memory_request" class="mt-1 text-xs text-slate-500 dark:text-slate-400">
                Current request: {{ svcResources.memory_request }}
              </p>
            </div>
            <div>
              <h4 class="mb-2 text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">CPU</h4>
              <ResourceControl
                kind="cpu"
                :usage="svcPerPodCpuMillis"
                :limit="svcCpuLimitMillis"
                :throttling-pct="svcCpuThrottlingPct"
                :applying="svcResourcesSaving || svcRolloutPhase !== 'idle'"
                size="md"
                @apply="requestSvcCpuApply"
              />
              <div v-if="svcCpuSparkline.length > 1" class="mt-2 flex items-center justify-center gap-2 text-[10px] text-slate-400 dark:text-slate-500">
                <span class="uppercase tracking-wide">Last hour</span>
                <MetricSparkline :data="svcCpuSparkline" :width="180" :height="28" color="#a855f7" />
              </div>
              <p v-if="svcResources.cpu_request" class="mt-1 text-xs text-slate-500 dark:text-slate-400">
                Current request: {{ svcResources.cpu_request }}
              </p>
            </div>
          </div>

          <p v-if="svcUsagePodCount > 1" class="text-xs text-slate-500 dark:text-slate-400">
            Gauges show the average usage across {{ svcUsagePodCount }} pods. See per-pod breakdown below.
          </p>

          <!-- Per-replica breakdown (rare for services; useful for stateful sets with HPA). -->
          <div v-if="svcUsagePodCount > 1" class="rounded-lg border border-slate-200 bg-white p-3 dark:border-slate-700 dark:bg-slate-900">
            <div class="mb-3 flex items-center justify-between">
              <h4 class="text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">Per replica</h4>
              <RouterLink
                v-if="selectedService"
                :to="{ name: 'service-pods', params: { name: selectedService.name }, query: { ns: selectedNamespace } }"
                class="text-xs font-medium text-slate-500 hover:text-slate-900 dark:text-slate-400 dark:hover:text-slate-100"
              >
                Open full view →
              </RouterLink>
            </div>
            <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div
                v-for="row in svcPerPodUsage"
                :key="row.pod"
                class="rounded-md border border-slate-200 bg-slate-50 p-3 dark:border-slate-700 dark:bg-slate-800/60"
              >
                <p class="mb-2 truncate text-xs font-medium text-slate-700 dark:text-slate-300" :title="row.pod">{{ row.pod }}</p>
                <div class="grid grid-cols-2 gap-2">
                  <ResourceControl
                    kind="memory"
                    :usage="row.memory"
                    :limit="svcMemoryLimitBytes"
                    size="sm"
                    readonly
                  />
                  <ResourceControl
                    kind="cpu"
                    :usage="row.cpu"
                    :limit="svcCpuLimitMillis"
                    size="sm"
                    readonly
                  />
                </div>
              </div>
            </div>
          </div>

          <!-- Rollout status -->
          <NoticeCallout v-if="svcRolloutPhase === 'restarting'" tone="warning" class="flex items-center gap-2 px-4 py-3">
            <span class="inline-block h-2 w-2 animate-pulse rounded-full bg-amber-500" />
            <span class="text-xs font-medium text-amber-600 dark:text-slate-300">Service is restarting...</span>
          </NoticeCallout>
          <NoticeCallout v-else-if="svcRolloutPhase === 'done'" tone="success" class="flex items-center gap-2 px-4 py-3">
            <span class="text-xs font-medium text-emerald-600 dark:text-slate-300">Service restarted successfully</span>
          </NoticeCallout>

          <!-- Confirmation warning -->
          <NoticeCallout v-if="showSvcConfirm" tone="warning" class="p-4">
            <p class="text-sm font-medium text-amber-600 dark:text-orange-300">This will restart the service</p>
            <p v-if="pendingSvcResize" class="mt-1 text-xs font-medium text-amber-700 dark:text-slate-300">
              {{ pendingSvcResize.kindLabel }}
            </p>
            <p class="mt-1 text-xs text-slate-600 dark:text-slate-400">
              Changing resource limits requires a service restart. The database will be temporarily unavailable (typically 10-30 seconds). Active connections from your apps will be dropped and automatically reconnected.
            </p>
            <div class="mt-3 flex items-center gap-2">
              <button
                @click="saveSvcResources"
                class="rounded-md bg-amber-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-amber-700"
              >
                Yes, restart service
              </button>
              <button
                @click="showSvcConfirm = false; pendingSvcResize = null"
                class="rounded-md border border-slate-300 px-3 py-1.5 text-xs font-medium text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
              >
                Cancel
              </button>
            </div>
          </NoticeCallout>
        </div>
      </div>

      <!-- Migrate data tab -->
      <ServiceMigrateDataPanel
        v-if="svcDetailTab === 'migrate' && selectedService"
        :service-name="selectedService.name"
        :service-type="selectedService.type"
        :target-namespace="selectedNamespace"
      />

      <!-- Share tab -->
      <ServiceSharesPanel
        v-if="svcDetailTab === 'shares' && canShare && selectedService"
        :service-name="selectedService.name"
        :namespace="selectedNamespace"
      />
    </SidePanel>
  </div>
</template>
