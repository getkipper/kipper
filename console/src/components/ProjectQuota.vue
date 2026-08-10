<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { storeToRefs } from 'pinia'
import { Gauge, Pencil, RotateCcw, X } from 'lucide-vue-next'
import { isAxiosError } from 'axios'
import NoticeCallout from '@/components/NoticeCallout.vue'
import type {
  EnvironmentQuota,
  QuotaDimensions,
  QuotaUpdate,
  QuotaWarning,
} from '@/api/projects'
import { useProjectQuotaStore } from '@/stores/projectQuota'
import { useToast } from '@/composables/useToast'

interface Props {
  project: string
  canManage: boolean
}
const props = defineProps<Props>()

const toast = useToast()
const store = useProjectQuotaStore()
const { quota, loading, error } = storeToRefs(store)
const saving = ref(false)

const editingEnv = ref<string | null>(null)
const editValues = ref<QuotaDimensions>({ cpu_request: '', cpu_limit: '', memory_request: '', memory_limit: '' })

// A refused change (409) is held here with its warnings until the user
// confirms or cancels; confirming retries with force.
const pendingUpdate = ref<QuotaUpdate | null>(null)
const pendingWarnings = ref<QuotaWarning[]>([])

// selectedTier drives the tier <select>. Binding the native select straight to
// quota.tier means a cancelled or failed change leaves the DOM showing the
// picked tier, because the bound value never changed for Vue to reset. Tracking
// it in its own ref lets us snap the control back to the tier actually in
// effect. Kept in sync whenever the quota loads or changes.
const selectedTier = ref('')
watch(quota, q => { selectedTier.value = q?.tier ?? '' })

const tierNames = computed(() => Object.keys(quota.value?.tiers ?? {}).sort(
  (a, b) => tierRank(a) - tierRank(b),
))

const dimensionLabels: { key: keyof QuotaDimensions; label: string }[] = [
  { key: 'cpu_request', label: 'CPU requests' },
  { key: 'cpu_limit', label: 'CPU limits' },
  { key: 'memory_request', label: 'Memory requests' },
  { key: 'memory_limit', label: 'Memory limits' },
]

function tierRank(tier: string): number {
  return { small: 0, medium: 1, large: 2 }[tier] ?? 99
}

function tierLabel(tier: string): string {
  if (tier === '') return 'No tier — cluster-wide limits only'
  const dims = quota.value?.tiers[tier]
  if (!dims) return tier
  return `${tier} — ${dims.cpu_request} CPU / ${dims.memory_request} memory`
}

// parseQuantity converts a Kubernetes quantity string into a plain number
// for the usage bars. Only the suffixes Kipper actually emits are handled;
// anything unparseable yields NaN and the bar is skipped.
function parseQuantity(value: string): number {
  if (!value) return NaN
  const match = /^([0-9.]+)(m|Ki|Mi|Gi|Ti|k|M|G|T)?$/.exec(value)
  if (!match) return NaN
  const base = parseFloat(match[1])
  const scale: Record<string, number> = {
    m: 1e-3,
    k: 1e3, M: 1e6, G: 1e9, T: 1e12,
    Ki: 1024, Mi: 1024 ** 2, Gi: 1024 ** 3, Ti: 1024 ** 4,
  }
  return base * (match[2] ? scale[match[2]] : 1)
}

function usagePercent(env: EnvironmentQuota, key: keyof QuotaDimensions): number | null {
  if (!env.used) return null
  const used = parseQuantity(env.used[key])
  const hard = parseQuantity(env.hard[key])
  if (Number.isNaN(used) || Number.isNaN(hard) || hard <= 0) return null
  return Math.round((used / hard) * 100)
}

function barClass(percent: number): string {
  if (percent >= 100) return 'bg-red-500'
  if (percent >= 80) return 'bg-amber-500'
  return 'bg-kipper-500'
}

function load() {
  return store.loadQuota(props.project)
}

async function apply(update: QuotaUpdate) {
  saving.value = true
  try {
    await store.saveQuota(props.project, update)
    editingEnv.value = null
    pendingUpdate.value = null
    pendingWarnings.value = []
    toast.success('Quota updated')
  } catch (e) {
    if (isAxiosError(e) && e.response?.status === 409) {
      pendingUpdate.value = update
      pendingWarnings.value = (e.response.data?.warnings as QuotaWarning[]) ?? []
      return
    }
    const detail = isAxiosError(e) ? e.response?.data?.error : null
    toast.error(typeof detail === 'string' ? detail : 'failed to update quota')
  } finally {
    saving.value = false
  }
}

async function changeTier(tier: string) {
  if (!quota.value || tier === quota.value.tier) {
    return
  }
  await apply({ tier })
  // Snap the control back to the tier that is actually in effect: the new one
  // on success, the unchanged one on a refusal or failure.
  selectedTier.value = quota.value?.tier ?? ''
}

function startEdit(env: EnvironmentQuota) {
  editingEnv.value = env.environment
  editValues.value = { ...env.hard }
  pendingUpdate.value = null
  pendingWarnings.value = []
}

function saveOverride(env: EnvironmentQuota) {
  void apply({ environments: [{ name: env.environment, quota: { ...editValues.value } }] })
}

function resetToTier(env: EnvironmentQuota) {
  void apply({ environments: [{ name: env.environment, quota: null }] })
}

function confirmPending() {
  if (!pendingUpdate.value) return
  void apply({ ...pendingUpdate.value, force: true })
}

function cancelPending() {
  pendingUpdate.value = null
  pendingWarnings.value = []
}

onMounted(load)
watch(() => props.project, load)
</script>

<template>
  <div class="p-5">
    <div class="mb-3 flex items-center gap-2">
      <Gauge class="h-4 w-4 text-slate-400" :stroke-width="2" />
      <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Resource quota</h3>
    </div>

    <p class="mb-4 text-sm text-slate-500 dark:text-slate-400">
      Optional. Without a tier this project simply uses whatever the cluster has free. Set a
      tier to give each environment a fixed CPU and memory budget that apps cannot exceed.
    </p>

    <p v-if="loading" class="text-sm text-slate-500 dark:text-slate-400">Loading quota…</p>
    <p v-else-if="error" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>

    <div v-else-if="quota" class="space-y-4">
      <!-- Tier -->
      <div class="flex flex-wrap items-center gap-2">
        <span class="text-sm text-slate-600 dark:text-slate-300">Tier</span>
        <select
          v-if="canManage"
          v-model="selectedTier"
          :disabled="saving"
          @change="changeTier(selectedTier)"
          class="rounded-md border border-slate-300 bg-white px-2 py-1 text-sm text-slate-900 focus:border-kipper-500 focus:outline-none dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
        >
          <option value="">{{ tierLabel('') }}</option>
          <option v-for="tier in tierNames" :key="tier" :value="tier">{{ tierLabel(tier) }}</option>
        </select>
        <span
          v-else
          class="rounded-full bg-slate-200 px-2 py-0.5 text-xs font-medium capitalize text-slate-600 dark:bg-slate-700 dark:text-slate-300"
        >
          {{ quota.tier || 'no tier' }}
        </span>
        <span v-if="quota.tier" class="text-xs text-slate-500 dark:text-slate-400">Each environment gets its own quota of this size</span>
      </div>

      <!-- Below-usage confirmation -->
      <NoticeCallout
        v-if="pendingWarnings.length"
        tone="warning"
        class="p-3"
      >
        <p class="text-sm font-medium text-amber-800 dark:text-orange-300">
          The new caps are below what these environments currently use. Nothing gets evicted, but new pods will be rejected until usage drops:
        </p>
        <ul class="mt-1.5 space-y-0.5 text-xs text-amber-700 dark:text-slate-400">
          <li v-for="w in pendingWarnings" :key="`${w.environment}-${w.dimension}`">
            <span class="font-mono">{{ w.environment }}</span> — {{ w.dimension }} uses {{ w.used }}, new cap {{ w.new_cap }}
          </li>
        </ul>
        <div class="mt-2 flex gap-2">
          <button
            @click="confirmPending"
            :disabled="saving"
            class="rounded-md bg-amber-600 px-3 py-1 text-xs font-medium text-white hover:bg-amber-700 disabled:opacity-50"
          >
            Apply anyway
          </button>
          <button
            @click="cancelPending"
            class="rounded-md border border-amber-300 px-3 py-1 text-xs font-medium text-amber-800 hover:bg-amber-100 dark:border-amber-700 dark:text-amber-200 dark:hover:bg-amber-900/40"
          >
            Cancel
          </button>
        </div>
      </NoticeCallout>

      <!-- Per-environment cards -->
      <div class="grid gap-3 md:grid-cols-2">
        <div
          v-for="env in quota.environments"
          :key="env.environment"
          class="rounded-lg border border-slate-200 p-3 dark:border-slate-700"
        >
          <div class="mb-2 flex items-center justify-between">
            <div class="flex items-center gap-2">
              <span class="text-sm font-medium capitalize text-slate-900 dark:text-slate-50">{{ env.environment }}</span>
              <span
                v-if="env.source === 'override'"
                class="rounded-full bg-kipper-100 px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wide text-kipper-700 dark:bg-kipper-900/50 dark:text-kipper-300"
              >
                Override
              </span>
              <span
                v-if="env.over_quota"
                class="rounded-full bg-red-100 px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wide text-red-700 dark:bg-red-900/50 dark:text-red-300"
              >
                Over quota
              </span>
            </div>
            <div v-if="canManage" class="flex items-center gap-1">
              <button
                v-if="editingEnv !== env.environment"
                @click="startEdit(env)"
                class="rounded p-1 text-slate-400 transition-colors hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800 dark:hover:text-slate-300"
                :title="`Edit ${env.environment} quota`"
              >
                <Pencil class="h-3.5 w-3.5" :stroke-width="2" />
              </button>
              <button
                v-if="env.source === 'override'"
                @click="resetToTier(env)"
                :disabled="saving"
                class="rounded p-1 text-slate-400 transition-colors hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800 dark:hover:text-slate-300"
                title="Reset to tier default"
              >
                <RotateCcw class="h-3.5 w-3.5" :stroke-width="2" />
              </button>
            </div>
          </div>

          <!-- Edit form -->
          <form
            v-if="editingEnv === env.environment"
            @submit.prevent="saveOverride(env)"
            class="space-y-2"
          >
            <div v-for="dim in dimensionLabels" :key="dim.key" class="flex items-center justify-between gap-2">
              <label class="text-xs text-slate-600 dark:text-slate-300">{{ dim.label }}</label>
              <input
                v-model="editValues[dim.key]"
                type="text"
                :placeholder="env.hard[dim.key]"
                class="w-24 rounded-md border border-slate-300 bg-white px-2 py-1 text-right font-mono text-xs text-slate-900 focus:border-kipper-500 focus:outline-none dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
              />
            </div>
            <div class="flex justify-end gap-1.5 pt-1">
              <button
                type="button"
                @click="editingEnv = null"
                class="rounded p-1 text-slate-400 hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800 dark:hover:text-slate-300"
                title="Cancel"
              >
                <X class="h-4 w-4" :stroke-width="2" />
              </button>
              <button
                type="submit"
                :disabled="saving"
                class="rounded-md bg-kipper-600 px-3 py-1 text-xs font-medium text-white hover:bg-kipper-700 disabled:opacity-50"
              >
                Save
              </button>
            </div>
          </form>

          <!-- Tierless environment without an override: nothing to meter -->
          <p v-else-if="env.source === 'none'" class="text-xs text-slate-500 dark:text-slate-400">
            No caps set. Apps here share the cluster's free resources; use the pencil to give
            this environment its own budget.
          </p>

          <!-- Usage bars -->
          <div v-else class="space-y-1.5">
            <div v-for="dim in dimensionLabels" :key="dim.key">
              <div class="flex items-center justify-between text-xs">
                <span class="text-slate-500 dark:text-slate-400">{{ dim.label }}</span>
                <span class="font-mono text-slate-600 dark:text-slate-300">
                  <template v-if="env.used">{{ env.used[dim.key] || '0' }} / </template>{{ env.hard[dim.key] }}
                </span>
              </div>
              <div
                v-if="usagePercent(env, dim.key) !== null"
                class="mt-0.5 h-1.5 overflow-hidden rounded-full bg-slate-100 dark:bg-slate-800"
              >
                <div
                  class="h-full rounded-full transition-all"
                  :class="barClass(usagePercent(env, dim.key)!)"
                  :style="{ width: Math.min(usagePercent(env, dim.key)!, 100) + '%' }"
                />
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
