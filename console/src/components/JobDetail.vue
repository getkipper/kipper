<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { Play, CheckCircle, XCircle, Terminal, Clock } from 'lucide-vue-next'
import SidePanel from '@/components/SidePanel.vue'
import SaveButton from '@/components/SaveButton.vue'
import LogAnalysis from '@/components/LogAnalysis.vue'
import TabBar from '@/components/TabBar.vue'
import { useToast } from '@/composables/useToast'
import { useLogStream } from '@/composables/useLogStream'
import { useCapabilities } from '@/composables/useCapabilities'
import { fetchJobHistory, triggerJob, fetchJobResources, updateJobResources, type Job, type JobResources } from '@/api/jobs'

const toast = useToast()
const { canInNamespace } = useCapabilities()

const props = defineProps<{
  jobName: string
  jobType: string
  schedule?: string
  namespace: string
}>()

const emit = defineEmits<{
  close: []
  refresh: []
}>()

const activeTab = ref<'logs' | 'history' | 'resources'>('logs')
const jobTabs = [
  { key: 'history', label: 'History' },
  { key: 'logs', label: 'Logs' },
  { key: 'resources', label: 'Resources' },
]
const history = ref<Job[]>([])
const historyLoading = ref(false)
const triggering = ref(false)

// Resources
const jobResources = ref<JobResources>({ memory_limit: '', memory_request: '', cpu_limit: '', cpu_request: '' })
const jobResourcesLoading = ref(false)
const jobResourcesSaving = ref(false)
const jobMemoryLimit = ref('')
const jobCPULimit = ref('')

const { lines, connected, connect, disconnect, clear } = useLogStream()

const jobLogsText = computed(() => lines.value.join('\n'))

async function loadHistory() {
  historyLoading.value = true
  try {
    history.value = await fetchJobHistory(props.jobName)
  } catch {
    // ignore
  } finally {
    historyLoading.value = false
  }
}

async function handleTrigger() {
  triggering.value = true
  try {
    await triggerJob(props.jobName)
    toast.success(`${props.jobName} triggered, running now`)
    emit('refresh')
    await loadHistory()
  } catch {
    toast.error(`Failed to trigger ${props.jobName}`)
  } finally {
    triggering.value = false
  }
}

function connectLogs() {
  // Jobs run in various namespaces — try to find the pod
  connect(props.namespace, props.jobName)
}

async function loadJobResources() {
  jobResourcesLoading.value = true
  try {
    jobResources.value = await fetchJobResources(props.jobName)
    jobMemoryLimit.value = jobResources.value.memory_limit || ''
    jobCPULimit.value = jobResources.value.cpu_limit || ''
  } catch {
    jobResources.value = { memory_limit: '', memory_request: '', cpu_limit: '', cpu_request: '' }
  } finally {
    jobResourcesLoading.value = false
  }
}

async function saveJobResources() {
  jobResourcesSaving.value = true
  try {
    await updateJobResources(props.jobName, {
      memory_limit: jobMemoryLimit.value,
      cpu_limit: jobCPULimit.value,
    })
    toast.success('Resources updated: next job run will use new limits')
  } catch {
    toast.error('Failed to update resources')
  } finally {
    jobResourcesSaving.value = false
  }
}

watch(activeTab, (tab) => {
  if (tab === 'logs') {
    connectLogs()
  } else {
    disconnect()
  }
  if (tab === 'history') loadHistory()
  if (tab === 'resources') loadJobResources()
})

watch(() => props.jobName, () => {
  clear()
  if (activeTab.value === 'logs') connectLogs()
  if (activeTab.value === 'history') loadHistory()
  if (activeTab.value === 'resources') loadJobResources()
})

onMounted(() => {
  loadHistory()
})

function statusColor(status: string): string {
  switch (status) {
    case 'completed': return 'text-emerald-600 dark:text-emerald-400'
    case 'failed': return 'text-red-600 dark:text-red-400'
    case 'running': return 'text-amber-600 dark:text-amber-400'
    default: return 'text-slate-500'
  }
}
</script>

<template>
  <SidePanel :open="true" :label="`Job details for ${jobName}`" @close="emit('close')">
    <template #header>
      <h2 class="text-sm font-semibold text-slate-900 dark:text-slate-50">{{ jobName }}</h2>
      <div class="mt-0.5 flex items-center gap-2 text-xs text-slate-500 dark:text-slate-400">
        <span class="rounded-full bg-slate-100 px-1.5 py-0.5 font-medium dark:bg-slate-800">{{ jobType }}</span>
        <span v-if="schedule" class="flex items-center gap-1">
          <Clock class="h-3 w-3" />
          {{ schedule }}
        </span>
      </div>
    </template>
    <template #actions>
      <button
        v-if="canInNamespace(props.namespace, 'kipper.write') && jobType === 'cronjob'"
        @click="handleTrigger"
        :disabled="triggering"
        class="inline-flex items-center gap-1 rounded-lg bg-kipper-600 px-3 py-1.5 text-xs font-medium text-white transition-colors hover:bg-kipper-700 disabled:opacity-50"
        title="Run now"
      >
        <Play class="h-3 w-3" :stroke-width="2" />
        Run now
      </button>
    </template>

    <!-- Tabs -->
    <TabBar
      :tabs="jobTabs"
      :model-value="activeTab"
      aria-label="Job detail sections"
      @update:model-value="(k) => (activeTab = k as typeof activeTab.value)"
    />

    <!-- Content -->
    <div class="flex-1 overflow-y-auto">
      <!-- History tab -->
      <div v-if="activeTab === 'history'" class="p-5">
        <div v-if="historyLoading" class="text-sm text-slate-500 dark:text-slate-400">Loading...</div>

        <div v-else-if="history.length" class="space-y-2">
          <div
            v-for="h in history"
            :key="h.name"
            class="flex items-center justify-between rounded-lg border border-slate-100 bg-slate-50 px-4 py-3 dark:border-slate-700 dark:bg-slate-800"
          >
            <span class="truncate font-mono text-xs text-slate-600 dark:text-slate-400">{{ h.name }}</span>
            <div class="flex items-center gap-3">
              <span class="text-xs text-slate-500">{{ h.last }}</span>
              <span class="inline-flex items-center gap-1 text-xs" :class="statusColor(h.status)">
                <CheckCircle v-if="h.status === 'completed'" class="h-3.5 w-3.5" />
                <XCircle v-else-if="h.status === 'failed'" class="h-3.5 w-3.5" />
                {{ h.status }}
              </span>
            </div>
          </div>
        </div>

        <div v-else class="py-8 text-center text-sm text-slate-500 dark:text-slate-400">
          No executions yet
        </div>
      </div>

      <!-- Logs tab -->
      <div v-if="activeTab === 'logs'" class="flex h-full flex-col">
        <div class="flex items-center justify-between border-b border-slate-100 px-5 py-2 dark:border-slate-800">
          <span class="flex items-center gap-2 text-xs text-slate-500 dark:text-slate-400">
            <span class="inline-block h-2 w-2 rounded-full" :class="connected ? 'bg-emerald-500' : 'bg-slate-400'" />
            {{ connected ? 'Connected' : 'Disconnected' }}
          </span>
          <button @click="clear" class="text-xs text-slate-400 hover:text-slate-600 dark:hover:text-slate-300">Clear</button>
          <LogAnalysis
            :logs="jobLogsText"
            :app-name="jobName"
            namespace="default"
          />
        </div>
        <div class="flex-1 overflow-y-auto bg-slate-950 p-4 font-mono text-xs leading-relaxed text-slate-300">
          <div v-for="(line, i) in lines" :key="i">{{ line }}</div>
          <div v-if="!lines.length" class="text-slate-600">
            <Terminal class="mb-2 h-5 w-5" />
            Waiting for logs...
          </div>
        </div>
      </div>
    </div>

    <!-- Resources tab -->
    <div v-if="activeTab === 'resources'" class="flex-1 overflow-y-auto p-5">
      <div v-if="jobResourcesLoading" class="text-sm text-slate-500 dark:text-slate-400">Loading...</div>
      <div v-else class="space-y-4">
        <p class="text-xs text-slate-500 dark:text-slate-400">Resource limits apply to the next job run.</p>
        <div>
          <label class="mb-1.5 block text-xs font-medium text-slate-600 dark:text-slate-400">Memory limit</label>
          <input
            v-model="jobMemoryLimit"
            type="text"
            placeholder="e.g. 256Mi, 1Gi"
            class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
          />
          <p v-if="jobResources.memory_request" class="mt-1 text-xs text-slate-500 dark:text-slate-400">
            Current request: {{ jobResources.memory_request }}
          </p>
        </div>
        <div>
          <label class="mb-1.5 block text-xs font-medium text-slate-600 dark:text-slate-400">CPU limit</label>
          <input
            v-model="jobCPULimit"
            type="text"
            placeholder="e.g. 100m, 500m, 1"
            class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
          />
          <p v-if="jobResources.cpu_request" class="mt-1 text-xs text-slate-500 dark:text-slate-400">
            Current request: {{ jobResources.cpu_request }}
          </p>
        </div>
        <SaveButton v-if="canInNamespace(props.namespace, 'kipper.write')" :saving="jobResourcesSaving" @click="saveJobResources" />
      </div>
    </div>
  </SidePanel>
</template>
