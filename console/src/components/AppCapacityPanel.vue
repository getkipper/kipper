<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { RefreshCw } from 'lucide-vue-next'
import SaveButton from '@/components/SaveButton.vue'
import { useToast } from '@/composables/useToast'
import * as api from '@/api/apps'
import { formatDateTime } from '@/utils/datetime'
import { activityLines, boundsMove, capacityBadges, checkCapacity, planCapacitySave, type CapacityForm, type CapacitySaveStep } from '@/utils/capacity'

const props = defineProps<{ project: string; appName: string; canWrite: boolean }>()
const emit = defineEmits<{ saved: []; loaded: [config: api.AutoscaleConfig] }>()
const toast = useToast()

const config = ref<api.AutoscaleConfig | null>(null)
const readError = ref(false)
const refreshFailed = ref(false)
const refreshing = ref(false)
const saving = ref(false)
// Pods from the app list, for a console-api that does not report them itself.
const listedRunning = ref<number | null>(null)
const listedReady = ref<number | null>(null)

const emptyForm: CapacityForm = { policy: 'none', desired: null, min: null, max: null, cpu: 0, memory: 0 }
const saved = ref<CapacityForm>({ ...emptyForm })
// Observed desired count while tracking is on. Stopped apps instead use
// their stored restart count when switching off.
const autoscaledCount = ref<number | null>(null)
// Number inputs hand back '' when cleared, so the form holds raw values.
const draft = ref<Record<keyof CapacityForm, string | number | null>>({ ...emptyForm })

const inputClass = 'w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none disabled:opacity-60 dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50 dark:placeholder-slate-500'
const labelClass = 'mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400'

const capacityApi = computed(() => (config.value?.capacity_api ?? 0) >= 1)

function count(v: string | number | null): number | null {
  if (v === '' || v === null) return null
  return Number(v)
}

const form = computed<CapacityForm>(() => ({
  policy: draft.value.policy === 'tracking' ? 'tracking' : 'none',
  desired: count(draft.value.desired),
  min: count(draft.value.min),
  max: count(draft.value.max),
  cpu: count(draft.value.cpu) ?? 0,
  memory: count(draft.value.memory) ?? 0,
}))

const dirty = computed(() => JSON.stringify(form.value) !== JSON.stringify(saved.value))
const check = computed(() => checkCapacity(form.value, saved.value))
const plan = computed(() => planCapacitySave(saved.value, form.value, capacityApi.value))
const problem = computed(() => check.value.error ?? ('unsupported' in plan.value ? plan.value.unsupported : undefined))
const move = computed(() => (problem.value ? undefined : boundsMove(saved.value, plan.value)))
const notAtomic = computed(() => !problem.value && 'steps' in plan.value && !plan.value.atomic)
const canSave = computed(() => dirty.value && !problem.value && !saving.value)

const running = computed(() => config.value?.running_replicas ?? (capacityApi.value ? null : listedRunning.value))
const ready = computed(() => config.value?.ready_replicas ?? (capacityApi.value ? null : listedReady.value))
const badges = computed(() => (config.value ? capacityBadges(config.value) : []))
const atMaxWithCpu = computed(() => badges.value.some(b => b.key === 'at-max') && (config.value?.cpu_target ?? 0) > 0)
const autoDesired = computed(() => {
  if (saved.value.policy !== 'tracking') return `${saved.value.desired ?? 'unknown'} (set by autoscaling once saved)`
  return `${autoscaledCount.value ?? 'unknown'} (set by autoscaling)`
})

const badgeClass: Record<string, string> = {
  danger: 'border-red-200 bg-red-50 text-red-700 dark:border-rose-800 dark:bg-rose-950 dark:text-rose-300',
  warning: 'border-amber-200 bg-amber-50 text-amber-700 dark:border-orange-800 dark:bg-orange-950 dark:text-orange-300',
  info: 'border-sky-200 bg-sky-50 text-sky-700 dark:border-sky-800 dark:bg-sky-950 dark:text-sky-300',
}

// A slower response for an earlier load must not overwrite a later one.
let loadSeq = 0

/**
 * Reads the capacity state and resets the form to it.
 *
 * @param keepEdits refresh only what is observed when the form holds unsaved
 *   edits, and keep the last values read if the read fails
 */
async function load(keepEdits = false): Promise<void> {
  const seq = ++loadSeq
  try {
    const data = await api.fetchAutoscale(props.project, props.appName)
    let listedDesired: number | null = null
    if ((data.capacity_api ?? 0) < 1) {
      const app = (await api.fetchApps(props.project)).find(a => a.name === props.appName)
      // The running pods lag a pending scale and include surge pods, so the
      // desired count starts from the stored one.
      listedDesired = app ? app.desired_replicas ?? app.replicas : null
      listedRunning.value = app ? app.replicas : null
      listedReady.value = app ? app.ready : null
    }
    if (seq !== loadSeq) return
    config.value = data
    emit('loaded', data)
    readError.value = false
    refreshFailed.value = false
    autoscaledCount.value = autoscaledFrom(data)
    if (!keepEdits || !dirty.value) reset(formFrom(data, listedDesired))
  } catch {
    if (seq !== loadSeq) return
    if (keepEdits && config.value) {
      refreshFailed.value = true
      return
    }
    config.value = null
    readError.value = true
  }
}

const refreshIntervalMs = 15000
let refreshTimer: ReturnType<typeof setInterval> | undefined

async function refresh(): Promise<void> {
  if (saving.value || refreshing.value) return
  refreshing.value = true
  try {
    await load(true)
  } finally {
    refreshing.value = false
  }
}

// An older console-api reports only the autoscaler's own count. A newer one
// reports the Deployment's, and null there means unknown.
function autoscaledFrom(data: api.AutoscaleConfig): number | null {
  if ((data.capacity_api ?? 0) >= 1) return data.deployment_replicas ?? null
  return data.current_replicas > 0 ? data.current_replicas : null
}

function formFrom(data: api.AutoscaleConfig, listedDesired: number | null): CapacityForm {
  const hasBounds = data.max_replicas > 0
  // While tracking is on, desired starts at the count a switch-off keeps: what
  // the autoscaler last set, or the stored count for a stopped app.
  let desired: number | null
  if (data.enabled) {
    desired = data.stopped ? data.replicas ?? null : autoscaledFrom(data)
  } else {
    desired = data.replicas ?? listedDesired
  }
  return {
    policy: data.enabled ? 'tracking' : 'none',
    desired,
    min: hasBounds ? data.min_replicas || 1 : null,
    max: hasBounds ? data.max_replicas : null,
    cpu: data.cpu_target || 0,
    memory: data.memory_target || 0,
  }
}

function reset(to: CapacityForm) {
  saved.value = { ...to }
  draft.value = { ...to }
}

function cancel() {
  draft.value = { ...saved.value }
}

// Match CLI defaults when bounds or targets are absent.
function policyChanged() {
  if (form.value.policy !== 'tracking') return
  if (form.value.min === null && form.value.max === null) {
    draft.value.min = 1
    draft.value.max = 5
  }
  if (form.value.cpu === 0 && form.value.memory === 0) draft.value.cpu = 70
}

function raiseMax() {
  if (check.value.raiseMaxTo !== undefined) draft.value.max = check.value.raiseMaxTo
}

function errorDetail(e: unknown): string | undefined {
  return (e as { response?: { data?: { error?: string } } })?.response?.data?.error
}

async function run(step: CapacitySaveStep): Promise<api.AutoscaleSaveResult | undefined> {
  switch (step.kind) {
    case 'put':
      return api.setAutoscale(props.project, props.appName, step.body)
    case 'delete':
      return api.disableAutoscale(props.project, props.appName)
    case 'scale':
      await api.scaleApp(props.project, props.appName, step.replicas)
      return { status: 'scaled', replicas: step.replicas }
  }
}

function whatSaved(step: CapacitySaveStep): string {
  switch (step.kind) {
    case 'put':
      return step.body.enabled === false ? 'The capacity settings were saved' : 'Target tracking was saved'
    case 'delete':
      return 'Autoscaling was switched off'
    case 'scale':
      return `The desired count was set to ${step.replicas}`
  }
}

function report(step: CapacitySaveStep, result: api.AutoscaleSaveResult | undefined) {
  const stopped = config.value?.stopped === true
  if (step.kind === 'scale') {
    toast.success(stopped
      ? `${props.appName} is stopped; it runs ${step.replicas} replicas when started`
      : `Scaled ${props.appName} to ${step.replicas} replicas`)
  } else if (step.kind === 'put' && step.body.enabled !== false) {
    toast.success('Target tracking saved')
  } else {
    // saved still holds the form as loaded, before this save.
    const done = saved.value.policy === 'tracking' ? 'Autoscaling switched off' : 'Capacity saved'
    toast.success(result?.replicas !== undefined
      ? `${done}; ${props.appName} keeps ${result.replicas} replicas`
      : done)
  }
  if (result?.replicas_moved) {
    toast.info(`Desired count moved from ${result.replicas_moved.from} to ${result.replicas_moved.to} to stay within the bounds`)
  }
  if (result?.warning) toast.info(result.warning)
  if (result?.note) toast.info(result.note)
}

async function save() {
  if (!canSave.value || !('steps' in plan.value)) return
  const steps = plan.value.steps
  const policyOff = form.value.policy === 'none'
  saving.value = true
  let done: CapacitySaveStep | undefined
  let kept: number | undefined
  try {
    for (const step of steps) {
      const result = await run(step)
      report(step, result)
      if (policyOff && result?.replicas !== undefined) kept = result.replicas
      done = step
    }
    await load()
    // The reload can be read before the write reaches it.
    if (kept !== undefined && saved.value.policy === 'none') reset({ ...saved.value, desired: kept })
    emit('saved')
  } catch (e) {
    const detail = errorDetail(e)
    const failure = detail ? `Failed to save capacity: ${detail}` : 'Failed to save capacity'
    toast.error(done ? `${whatSaved(done)}, but the rest of the save failed. ${failure}` : failure)
    if (done) {
      await load()
      emit('saved')
    }
  } finally {
    saving.value = false
  }
}

onMounted(() => {
  load()
  refreshTimer = setInterval(refresh, refreshIntervalMs)
})
onUnmounted(() => clearInterval(refreshTimer))

defineExpose({ load })
</script>

<template>
  <div class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800">
    <div class="mb-4 flex flex-wrap items-start justify-between gap-2">
      <div>
        <p class="text-sm font-medium text-slate-900 dark:text-slate-50">Capacity</p>
        <p class="text-xs text-slate-500 dark:text-slate-400">Counts are replicas (pods) on this cluster's existing nodes.</p>
      </div>
      <div class="flex flex-wrap items-center gap-1.5">
        <span
          v-for="badge in badges"
          :key="badge.key"
          :data-testid="`capacity-badge-${badge.key}`"
          :title="badge.title"
          class="rounded-full border px-2 py-0.5 text-xs font-medium"
          :class="badgeClass[badge.tone]"
        >{{ badge.label }}</span>
        <button
          data-testid="capacity-refresh"
          type="button"
          :disabled="refreshing || saving"
          class="rounded p-1 text-slate-500 hover:bg-slate-200 disabled:opacity-50 dark:text-slate-400 dark:hover:bg-slate-700"
          title="Refresh the counts, badges and activity"
          @click="refresh"
        >
          <RefreshCw class="h-3.5 w-3.5" :stroke-width="2" :class="{ 'animate-spin': refreshing }" />
        </button>
      </div>
    </div>

    <p v-if="readError" data-testid="capacity-read-error" class="text-sm text-red-600 dark:text-rose-300">
      The capacity settings could not be read. Reload before editing them.
    </p>

    <div v-else-if="config" class="space-y-4">
      <p v-if="refreshFailed" data-testid="capacity-refresh-failed" class="text-xs text-amber-700 dark:text-orange-300">
        The latest refresh failed, so the counts and badges show the last values read.
      </p>
      <div class="grid grid-cols-2 gap-4 sm:grid-cols-4">
        <div>
          <label :class="labelClass" for="capacity-desired">Desired</label>
          <input
            v-if="form.policy === 'none'"
            id="capacity-desired"
            v-model.number="draft.desired"
            data-testid="capacity-desired"
            type="number"
            min="0"
            placeholder="Unknown"
            :disabled="!canWrite"
            :class="inputClass"
          />
          <p v-else data-testid="capacity-desired-auto" class="py-2 text-sm text-slate-700 dark:text-slate-300">{{ autoDesired }}</p>
        </div>
        <div>
          <label :class="labelClass" for="capacity-min">Minimum</label>
          <input
            id="capacity-min"
            v-model.number="draft.min"
            data-testid="capacity-min"
            type="number"
            min="1"
            :placeholder="form.policy === 'tracking' ? '1' : 'None'"
            :disabled="!canWrite"
            :class="inputClass"
          />
        </div>
        <div>
          <label :class="labelClass" for="capacity-max">Maximum</label>
          <input
            id="capacity-max"
            v-model.number="draft.max"
            data-testid="capacity-max"
            type="number"
            min="1"
            placeholder="None"
            :disabled="!canWrite"
            :class="inputClass"
          />
        </div>
        <div>
          <p :class="labelClass">Current</p>
          <p data-testid="capacity-current" class="py-2 text-sm text-slate-700 dark:text-slate-300">
            {{ running ?? 'unknown' }} running / {{ ready ?? 'unknown' }} ready
          </p>
        </div>
      </div>

      <div>
        <label :class="labelClass" for="capacity-policy">Scaling policy</label>
        <select id="capacity-policy" v-model="draft.policy" data-testid="capacity-policy" @change="policyChanged" :disabled="!canWrite" :class="inputClass" class="sm:w-64">
          <option value="none">None (fixed count)</option>
          <option value="tracking">Target tracking</option>
        </select>
      </div>

      <div v-if="form.policy === 'tracking'" class="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div>
          <label :class="labelClass" for="capacity-cpu">CPU target (% of request)</label>
          <input id="capacity-cpu" v-model.number="draft.cpu" data-testid="capacity-cpu" type="number" min="0" placeholder="0 = unused" :disabled="!canWrite" :class="inputClass" />
          <p v-if="config.enabled && config.current_cpu" class="mt-1 text-xs text-slate-500">Current: {{ config.current_cpu }}</p>
        </div>
        <div>
          <label :class="labelClass" for="capacity-memory">Memory target (% of request)</label>
          <input id="capacity-memory" v-model.number="draft.memory" data-testid="capacity-memory" type="number" min="0" placeholder="0 = unused" :disabled="!canWrite" :class="inputClass" />
          <p v-if="config.enabled && config.current_memory" class="mt-1 text-xs text-slate-500">Current: {{ config.current_memory }}</p>
        </div>
      </div>

      <p v-if="atMaxWithCpu" data-testid="capacity-at-max-hint" class="text-xs text-amber-700 dark:text-orange-300">
        Autoscaling wants more replicas than the maximum allows. Raise the maximum, or raise the CPU request so each replica handles more load.
      </p>

      <template v-if="canWrite">
        <div v-if="dirty && problem" data-testid="capacity-error" class="flex flex-wrap items-center gap-2 text-xs text-red-600 dark:text-rose-300">
          <span>{{ problem }}</span>
          <button
            v-if="check.raiseMaxTo !== undefined"
            data-testid="capacity-raise-max"
            type="button"
            class="rounded-md border border-slate-300 px-2 py-1 font-medium text-slate-700 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-700"
            @click="raiseMax"
          >Raise maximum to {{ check.raiseMaxTo }}</button>
        </div>
        <p v-if="dirty && move" data-testid="capacity-bounds-move" class="text-xs text-slate-600 dark:text-slate-400">
          Saving moves the desired count from {{ move.from }} to {{ move.to }} to stay within the bounds.
        </p>
        <p v-if="dirty && notAtomic" data-testid="capacity-not-atomic" class="text-xs text-amber-700 dark:text-orange-300">
          This cluster runs an older console-api, so the save goes out as separate requests and can stop half way.
        </p>
        <div class="flex justify-end gap-2">
          <button
            data-testid="capacity-cancel"
            type="button"
            :disabled="!dirty || saving"
            class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-100 disabled:opacity-50 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-700"
            @click="cancel"
          >Cancel</button>
          <SaveButton data-testid="capacity-save" :saving="saving" :disabled="!canSave" label="Save capacity" @click="save" />
        </div>
      </template>

      <div v-if="capacityApi" class="border-t border-slate-200 pt-4 dark:border-slate-700">
        <p class="text-xs font-medium text-slate-700 dark:text-slate-300">Scaling activity</p>
        <p class="text-xs text-slate-500 dark:text-slate-400">
          Showing recent and possibly incomplete activity, because events and log entries expire.
          <span v-if="config.last_scale_time">Last scaled {{ formatDateTime(config.last_scale_time) }}.</span>
        </p>
        <div data-testid="capacity-activity" class="mt-2 text-xs text-slate-600 dark:text-slate-400">
          <p v-if="config.activity === null || config.activity === undefined">Scaling activity could not be read.</p>
          <p v-else-if="config.activity.length === 0">No recent scaling activity.</p>
          <ul v-else class="space-y-1">
            <li v-for="(line, i) in activityLines(config.activity)" :key="i" class="flex gap-2">
              <span class="shrink-0 font-mono text-slate-500">{{ formatDateTime(line.time) }}</span>
              <span :title="line.title">{{ line.text }}</span>
            </li>
          </ul>
        </div>
      </div>
    </div>
  </div>
</template>
