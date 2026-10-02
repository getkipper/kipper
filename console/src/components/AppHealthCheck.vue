<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { HeartPulse } from 'lucide-vue-next'
import SaveButton from '@/components/SaveButton.vue'
import { useToast } from '@/composables/useToast'
import * as api from '@/api/apps'

const props = defineProps<{ project: string; appName: string; canWrite: boolean }>()
const toast = useToast()

const type = ref<api.HealthCheckType>('auto')
const path = ref('')
const port = ref('')
const startup = ref('')
const timeout = ref('')
const status = ref<api.HealthCheckStatus | null>(null)
const saving = ref(false)
// Load the current check before allowing a save, which replaces all settings.
const loaded = ref(false)
const readError = ref('')

const inputClass = 'w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50 dark:placeholder-slate-500'

const current = computed(() => {
  const s = status.value
  if (!s) return ''
  const check = s.type === 'http' ? `HTTP check of ${s.path} on port ${s.port}` : s.type === 'tcp' ? `port check on ${s.port}` : 'no check'
  switch (s.source) {
    case 'inferred':
      return s.type === 'tcp'
        ? `Automatic: port check on ${s.port}, chosen because the port accepted connections.`
        : 'Automatic: no check. A pod running for at least 10 minutes refused connections on the app port. Kipper will check again on the next rollout.'
    case 'pending':
      return 'Automatic: no check yet. On the next rollout, Kipper will test whether running pods accept connections on the app port.'
    case 'applying':
      return `Saved, but not applied to new pods yet: ${check}. Check the app panel for rollout details.`
    case 'building':
      return `Until the first build is deployed: ${check}.`
    default:
      return `In use: ${check}.`
  }
})

async function load() {
  try {
    const data = await api.fetchHealthCheck(props.project, props.appName)
    type.value = data.health.type
    path.value = data.health.path ?? ''
    port.value = data.health.port ? String(data.health.port) : ''
    startup.value = data.health.startup_timeout_seconds ? String(data.health.startup_timeout_seconds) : ''
    timeout.value = data.health.timeout_seconds ? String(data.health.timeout_seconds) : ''
    status.value = data.status
    loaded.value = true
    readError.value = ''
  } catch {
    status.value = null
    loaded.value = false
    readError.value = 'The current health check could not be read. Reload before editing it.'
  }
}

// Send only fields supported by the selected type.
function body(): api.HealthCheck {
  const out: api.HealthCheck = { type: type.value }
  if (type.value === 'auto') return out
  if (type.value === 'http') out.path = path.value.trim()
  if (type.value !== 'none' && port.value) out.port = Number(port.value)
  if (type.value !== 'none' && timeout.value) out.timeout_seconds = Number(timeout.value)
  if (startup.value) out.startup_timeout_seconds = Number(startup.value)
  return out
}

async function save() {
  saving.value = true
  try {
    await api.updateHealthCheck(props.project, props.appName, body())
    toast.success('Health check settings saved')
    await load()
  } catch (e) {
    const detail = (e as { response?: { data?: { error?: string } } })?.response?.data?.error
    toast.error(detail || 'Failed to save the health check')
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800">
    <div class="mb-3 flex items-center gap-3">
      <HeartPulse class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
      <div>
        <p class="text-sm font-medium text-slate-900 dark:text-slate-50">Health check</p>
        <p class="text-xs text-slate-500 dark:text-slate-400">The check a pod must pass before it receives traffic</p>
      </div>
    </div>
    <div class="space-y-3">
      <div>
        <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Check type</label>
        <select v-model="type" data-testid="health-type" :disabled="!canWrite" :class="inputClass">
          <option value="auto">Automatic</option>
          <option value="tcp">Port check (TCP)</option>
          <option value="http">HTTP check</option>
          <option value="none">None (disable the app check)</option>
        </select>
      </div>
      <div v-if="type === 'http'">
        <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Path</label>
        <input v-model="path" data-testid="health-path" type="text" placeholder="/actuator/health/readiness" :disabled="!canWrite" :class="inputClass" />
        <p class="mt-1 text-[10px] text-slate-400 dark:text-slate-500">Must return HTTP 200-399 when the app is ready</p>
      </div>
      <div v-if="type === 'http' || type === 'tcp'">
        <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Port</label>
        <input v-model="port" data-testid="health-port" type="number" placeholder="the app port" :disabled="!canWrite" :class="inputClass" />
      </div>
      <div v-if="type === 'http' || type === 'tcp'">
        <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Check timeout (seconds)</label>
        <input v-model="timeout" data-testid="health-timeout" type="number" placeholder="2" :disabled="!canWrite" :class="inputClass" />
      </div>
      <div v-if="type !== 'auto'">
        <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Startup timeout (seconds)</label>
        <input v-model="startup" data-testid="health-startup" type="number" placeholder="300" :disabled="!canWrite" :class="inputClass" />
        <p class="mt-1 text-[10px] text-slate-400 dark:text-slate-500">Time allowed for a new pod to become ready, including image downloads. After this, it is reported as stuck.</p>
      </div>
      <p v-if="readError" class="text-xs text-amber-700 dark:text-amber-400">{{ readError }}</p>
      <p v-if="current" data-testid="health-current" class="text-xs text-slate-600 dark:text-slate-400">{{ current }}</p>
      <div v-if="canWrite && loaded" class="flex items-center justify-end gap-3">
        <span class="text-[10px] text-slate-400 dark:text-slate-500">Changing the check triggers a rolling restart. Changing only the startup timeout leaves pods running.</span>
        <SaveButton data-testid="health-save" :saving="saving" label="Save health check" @click="save" />
      </div>
    </div>
  </div>
</template>
