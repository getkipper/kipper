<script setup lang="ts">
import { ref, computed, onMounted, onBeforeUnmount, watch } from 'vue'
import { AlertTriangle, Database, Loader2, RefreshCw } from 'lucide-vue-next'
import { useToast } from '@/composables/useToast'
import {
  fetchServices,
  startServiceMigration,
  fetchServiceMigrationStatus,
  type ServiceStatus,
  type MigrationStatus,
} from '@/api/services'

const props = defineProps<{
  serviceName: string
  serviceType: string
  targetNamespace: string
}>()

const toast = useToast()

const SUPPORTED_TYPES = ['postgres']
const supported = computed(() => SUPPORTED_TYPES.includes(props.serviceType))

const peers = ref<ServiceStatus[]>([])
const status = ref<MigrationStatus | null>(null)
const polling = ref<number | null>(null)
const loadingPeers = ref(false)

const isRunning = computed(() => status.value?.phase === 'Pending' || status.value?.phase === 'Running')

const confirmingFor = ref<ServiceStatus | null>(null)
const confirmText = ref('')
const submitting = ref(false)

async function loadPeers() {
  loadingPeers.value = true
  try {
    // List every same-type service across all namespaces, then filter to peers.
    const all = await fetchServices()
    peers.value = all.filter(
      s => s.name === props.serviceName && s.type === props.serviceType && s.namespace !== props.targetNamespace,
    )
  } catch {
    peers.value = []
  } finally {
    loadingPeers.value = false
  }
}

async function refreshStatus() {
  try {
    status.value = await fetchServiceMigrationStatus(props.serviceName, props.targetNamespace)
  } catch {
    // Don't toast on poll errors — happens during normal cluster blips.
  }
}

function startPolling() {
  stopPolling()
  polling.value = window.setInterval(refreshStatus, 2000)
}

function stopPolling() {
  if (polling.value) {
    window.clearInterval(polling.value)
    polling.value = null
  }
}

watch(isRunning, running => {
  if (running) startPolling()
  else stopPolling()
})

function startConfirm(peer: ServiceStatus) {
  confirmingFor.value = peer
  confirmText.value = ''
}

function cancelConfirm() {
  confirmingFor.value = null
  confirmText.value = ''
}

async function submitMigration() {
  if (!confirmingFor.value) return
  if (confirmText.value !== props.serviceName) return
  submitting.value = true
  const peer = confirmingFor.value
  try {
    const resp = await startServiceMigration(
      props.serviceName,
      props.targetNamespace,
      peer.namespace,
      props.serviceName,
    )
    toast.success(`Migration started — copying ${props.serviceType} data from ${peer.namespace}`)
    status.value = { job_name: resp.job_name, phase: resp.phase }
    startPolling()
    cancelConfirm()
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    toast.error(`Failed to start migration: ${msg}`)
  } finally {
    submitting.value = false
  }
}

const phaseColor = computed(() => {
  switch (status.value?.phase) {
    case 'Succeeded':
      return 'border-emerald-200 bg-emerald-50 text-emerald-800 dark:border-slate-800 dark:bg-slate-900 dark:text-emerald-300 dark:shadow-[inset_3px_0_0_theme(colors.emerald.400)]'
    case 'Failed':
      return 'border-red-300 bg-red-50 text-red-900 dark:border-slate-800 dark:bg-slate-900 dark:text-rose-300 dark:shadow-[inset_3px_0_0_theme(colors.rose.400)]'
    case 'Running':
    case 'Pending':
      return 'border-amber-200 bg-amber-50 text-amber-800 dark:border-slate-800 dark:bg-slate-900 dark:text-orange-300 dark:shadow-[inset_3px_0_0_theme(colors.orange.300)]'
    default:
      return 'border-slate-200 bg-slate-50 text-slate-700 dark:border-slate-800 dark:bg-slate-900 dark:text-slate-300'
  }
})

onMounted(async () => {
  await Promise.all([loadPeers(), refreshStatus()])
  if (isRunning.value) startPolling()
})

onBeforeUnmount(() => stopPolling())
</script>

<template>
  <div class="space-y-4 p-5">
    <div v-if="!supported" class="rounded-lg border border-slate-200 p-4 text-sm text-slate-600 dark:border-slate-700 dark:text-slate-400">
      <p class="font-medium text-slate-900 dark:text-slate-50">Data migration not yet available for {{ serviceType }}</p>
      <p class="mt-1">Postgres ships first; the other types follow in upcoming releases.</p>
    </div>

    <template v-else>
      <!-- Source picker -->
      <div>
        <div class="mb-2 flex items-center justify-between">
          <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Copy data from another environment</h3>
          <button
            @click="loadPeers"
            :disabled="loadingPeers"
            class="rounded p-1 text-slate-400 hover:bg-slate-100 dark:hover:bg-slate-800"
            title="Refresh source list"
          >
            <RefreshCw class="h-3.5 w-3.5" :class="{ 'animate-spin': loadingPeers }" />
          </button>
        </div>
        <p class="mb-3 text-xs text-slate-500 dark:text-slate-400">
          Picks a same-named, same-type {{ serviceType }} service from another environment and replays its data into this one.
          This <span class="font-semibold text-red-600 dark:text-red-400">drops and recreates</span> the target database.
        </p>

        <div v-if="loadingPeers" class="text-sm text-slate-500">Loading sources…</div>
        <div v-else-if="peers.length === 0" class="rounded-lg border border-dashed border-slate-300 p-4 text-center text-sm text-slate-500 dark:border-slate-700">
          No other {{ serviceType }} services named <span class="font-mono">{{ serviceName }}</span> in this cluster.
        </div>
        <div v-else class="space-y-2">
          <div
            v-for="peer in peers"
            :key="peer.namespace"
            class="flex items-center justify-between rounded-lg border border-slate-200 bg-slate-50 px-3 py-2 dark:border-slate-700 dark:bg-slate-800"
          >
            <div class="flex items-center gap-2">
              <Database class="h-4 w-4 text-kipper-500" />
              <div>
                <div class="text-sm font-medium text-slate-900 dark:text-slate-50">{{ peer.name }}</div>
                <div class="font-mono text-xs text-slate-500 dark:text-slate-400">{{ peer.namespace }}</div>
              </div>
            </div>
            <button
              @click="startConfirm(peer)"
              :disabled="isRunning"
              class="rounded-md bg-kipper-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-kipper-700 disabled:opacity-50"
            >
              Copy data here
            </button>
          </div>
        </div>
      </div>

      <!-- Latest run -->
      <div v-if="status?.job_name">
        <div class="mb-2 flex items-center justify-between">
          <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Latest migration</h3>
          <button
            @click="refreshStatus"
            class="rounded p-1 text-slate-400 hover:bg-slate-100 dark:hover:bg-slate-800"
            title="Refresh status"
          >
            <RefreshCw class="h-3.5 w-3.5" />
          </button>
        </div>
        <div class="rounded-lg border p-3 text-xs dark:rounded-l-none" :class="phaseColor">
          <div class="flex items-center gap-2">
            <Loader2 v-if="isRunning" class="h-4 w-4 animate-spin" />
            <span class="font-semibold">{{ status.phase }}</span>
            <span v-if="status.message" class="opacity-75">— {{ status.message }}</span>
          </div>
          <p class="mt-1 font-mono text-[11px] opacity-75">{{ status.job_name }}</p>
        </div>
        <div v-if="status.logs && status.logs.length" class="mt-2 max-h-64 overflow-auto rounded-lg bg-slate-950 p-3 font-mono text-[11px] text-slate-100">
          <div v-for="(line, i) in status.logs" :key="i" class="whitespace-pre-wrap break-all">{{ line }}</div>
        </div>
      </div>
    </template>

    <!-- Confirm modal -->
    <div
      v-if="confirmingFor"
      class="fixed inset-0 z-[120] flex items-center justify-center bg-black/40 p-8"
      @click.self="cancelConfirm"
    >
      <div class="w-full max-w-md rounded-xl bg-white p-6 shadow-xl dark:bg-slate-900" @click.stop>
        <div class="flex items-start gap-4">
          <span class="inline-flex h-10 w-10 shrink-0 items-center justify-center rounded-full bg-red-100 dark:bg-red-950">
            <AlertTriangle class="h-5 w-5 text-red-600 dark:text-red-400" />
          </span>
          <div>
            <h2 class="text-lg font-semibold text-slate-900 dark:text-slate-50">
              Overwrite {{ serviceName }} in this environment?
            </h2>
            <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">
              Drops every table in <span class="font-mono">{{ targetNamespace }}/{{ serviceName }}</span>
              and replays the contents of <span class="font-mono">{{ confirmingFor.namespace }}/{{ confirmingFor.name }}</span> on top.
              The migration runs as a Kubernetes Job in this namespace.
            </p>
          </div>
        </div>
        <div class="mt-5">
          <label class="block text-sm text-slate-700 dark:text-slate-300">
            Type <span class="font-mono font-semibold text-slate-900 dark:text-slate-50">{{ serviceName }}</span> to confirm
          </label>
          <input
            v-model="confirmText"
            type="text"
            :placeholder="serviceName"
            class="mt-1.5 block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
            @keyup.enter="confirmText === serviceName && submitMigration()"
          />
        </div>
        <div class="mt-6 flex justify-end gap-3">
          <button @click="cancelConfirm" class="rounded-lg border border-slate-300 px-4 py-2.5 text-sm font-medium dark:border-slate-600 dark:text-slate-300">
            Cancel
          </button>
          <button
            @click="submitMigration"
            :disabled="confirmText !== serviceName || submitting"
            class="rounded-lg bg-red-600 px-4 py-2.5 text-sm font-semibold text-white hover:bg-red-700 disabled:opacity-40"
          >
            {{ submitting ? 'Starting…' : 'Copy data' }}
          </button>
        </div>
      </div>
    </div>
  </div>
</template>
