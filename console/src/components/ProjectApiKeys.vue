<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { storeToRefs } from 'pinia'
import { Copy, KeyRound, Plus, Trash2 } from 'lucide-vue-next'
import type { ApiKey, UsagePlan } from '@/api/apikeys'
import { useProjectApiKeysStore } from '@/stores/projectApiKeys'
import { useToast } from '@/composables/useToast'
import { useModal } from '@/composables/useModal'
import ConfirmDialog from '@/components/ConfirmDialog.vue'

interface Props {
  project: string
  environments: string[]
  canManage: boolean
}
const props = defineProps<Props>()

const toast = useToast()
const modal = useModal()
const store = useProjectApiKeysStore()
const { plans, keys, usageByKey, loading, error } = storeToRefs(store)
const environment = ref(props.environments[0] ?? '')
const saving = ref(false)

// The issued key, shown exactly once after creation.
const revealedKey = ref<ApiKey | null>(null)

const showPlanForm = ref(false)
const planForm = ref({ name: '', rate: 10, burst: 20, quotaRequests: 0, quotaPeriod: 'month' as 'day' | 'week' | 'month' })

const showKeyForm = ref(false)
const keyForm = ref({ display_name: '', plan: '', apps: '', expires: '' })

const namespace = computed(() => {
  const env = environment.value
  if (!env || env === 'default') return props.project
  return `${props.project}-${env}`
})

// Per-key totals over the fetched window plus the last day with any traffic,
// so an operator can spot throttled keys and judge whether an old key is safe
// to revoke.
const usageSummary = computed(() => {
  const out: Record<string, { allowed: number; deniedRate: number; deniedQuota: number; lastUsed: string | null }> = {}
  for (const key of keys.value) {
    let allowed = 0, deniedRate = 0, deniedQuota = 0
    let lastUsed: string | null = null
    for (const d of usageByKey.value[key.name] ?? []) {
      allowed += d.allowed
      deniedRate += d.denied_rate
      deniedQuota += d.denied_quota
      if ((d.allowed > 0 || d.denied_rate > 0 || d.denied_quota > 0) && (lastUsed === null || d.day > lastUsed)) {
        lastUsed = d.day
      }
    }
    out[key.name] = { allowed, deniedRate, deniedQuota, lastUsed }
  }
  return out
})

// A per-key expiry badge: red once expired, amber within two weeks, plain
// otherwise. Null when the key never expires.
const expiryByKey = computed(() => {
  const out: Record<string, { label: string; cls: string } | null> = {}
  const now = Date.now()
  for (const key of keys.value) {
    if (!key.expires_at) {
      out[key.name] = null
      continue
    }
    const date = key.expires_at.slice(0, 10)
    const expiresMs = new Date(key.expires_at).getTime()
    // Compare the instant, not a rounded day count: a key expired a few
    // hours ago must read as expired, not "expires today".
    if (expiresMs <= now) {
      out[key.name] = { label: `expired ${date}`, cls: 'bg-red-100 text-red-700 dark:bg-red-900/50 dark:text-red-300' }
    } else if (expiresMs - now <= 14 * 86_400_000) {
      out[key.name] = { label: `expires ${date}`, cls: 'bg-amber-100 text-amber-700 dark:bg-amber-900/50 dark:text-amber-300' }
    } else {
      out[key.name] = { label: `expires ${date}`, cls: 'bg-slate-100 text-slate-500 dark:bg-slate-800 dark:text-slate-400' }
    }
  }
  return out
})

function load() {
  return store.loadKeys(namespace.value)
}

async function savePlan() {
  if (!planForm.value.name.trim()) return
  saving.value = true
  try {
    await store.savePlan(namespace.value, {
      name: planForm.value.name.trim(),
      rate: planForm.value.rate,
      burst: planForm.value.burst,
      quota: planForm.value.quotaRequests > 0
        ? { requests: planForm.value.quotaRequests, period: planForm.value.quotaPeriod }
        : null,
    })
    showPlanForm.value = false
    planForm.value = { name: '', rate: 10, burst: 20, quotaRequests: 0, quotaPeriod: 'month' }
    toast.success('Usage plan saved')
  } catch (e) {
    toast.error(e instanceof Error ? e.message : 'failed to save plan')
  } finally {
    saving.value = false
  }
}

async function removePlan(plan: UsagePlan) {
  try {
    await store.removePlan(namespace.value, plan.name)
  } catch (e) {
    toast.error(e instanceof Error ? e.message : `failed to delete plan ${plan.name}`)
  }
}

async function issueKey() {
  if (!keyForm.value.plan) return
  saving.value = true
  try {
    const apps = keyForm.value.apps.split(',').map(a => a.trim()).filter(Boolean)
    // A date-only expiry means valid through the end of that UTC day.
    const expiresAt = keyForm.value.expires ? `${keyForm.value.expires}T23:59:59Z` : undefined
    revealedKey.value = await store.createKey(namespace.value, {
      display_name: keyForm.value.display_name.trim() || undefined,
      plan: keyForm.value.plan,
      apps: apps.length ? apps : undefined,
      expires_at: expiresAt,
    })
    showKeyForm.value = false
    keyForm.value = { display_name: '', plan: '', apps: '', expires: '' }
  } catch (e) {
    toast.error(e instanceof Error ? e.message : 'failed to create key')
  } finally {
    saving.value = false
  }
}

async function copyKey() {
  if (!revealedKey.value?.key) return
  try {
    await navigator.clipboard.writeText(revealedKey.value.key)
    toast.success('Key copied to clipboard')
  } catch {
    toast.error('Could not copy. Select the key and copy it manually before closing.')
  }
}

async function toggleKey(key: ApiKey) {
  try {
    await store.setKeyEnabled(namespace.value, key.name, !key.enabled)
  } catch (e) {
    toast.error(e instanceof Error ? e.message : 'failed to update key')
  }
}

function revokeKey(key: ApiKey) {
  const label = key.display_name || key.name
  modal.open(ConfirmDialog, {
    title: `Revoke ${label}?`,
    message: 'Every client using this key stops working immediately, and the key cannot be recovered. You would have to issue and distribute a new one.',
    confirmLabel: 'Revoke key',
    onConfirm: async () => {
      modal.close()
      try {
        // The store reloads, so plan key-counts (and their delete buttons)
        // stay accurate.
        await store.deleteKey(namespace.value, key.name)
        toast.success(`${label} revoked`)
      } catch (e) {
        toast.error(e instanceof Error ? e.message : 'failed to revoke key')
      }
    },
  })
}

function planLabel(plan: UsagePlan): string {
  const quota = plan.quota ? `, ${plan.quota.requests.toLocaleString()}/${plan.quota.period}` : ''
  return `${plan.rate} rps (burst ${plan.burst})${quota}`
}

onMounted(load)
watch(() => props.project, () => {
  environment.value = props.environments[0] ?? ''
  // The revealed key belongs to the previous project/environment; clear it so
  // it isn't shown above another scope's keys.
  revealedKey.value = null
  void load()
})
watch(environment, () => {
  revealedKey.value = null
  void load()
})
</script>

<template>
  <div class="p-5">
    <div class="mb-3 flex flex-wrap items-center justify-between gap-2">
      <div class="flex items-center gap-2">
        <KeyRound class="h-4 w-4 text-slate-400" :stroke-width="2" />
        <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-50">API keys</h3>
      </div>
      <select
        v-if="environments.length > 1"
        v-model="environment"
        class="rounded-md border border-slate-300 bg-white px-2 py-1 text-xs text-slate-700 focus:border-kipper-500 focus:outline-none dark:border-slate-700 dark:bg-slate-800 dark:text-slate-200"
      >
        <option v-for="env in environments" :key="env" :value="env">{{ env }}</option>
      </select>
    </div>

    <p class="mb-4 text-sm text-slate-500 dark:text-slate-400">
      Put your apps behind Kipper's API gateway. Mark an app as requiring a key, create a usage
      plan with the rate limit and daily quota you want, then issue keys to your API consumers.
    </p>

    <!-- Reveal-once banner. Kept outside the loading/error branch so a failed
         reload right after creating a key can never hide the one-time secret. -->
    <div
      v-if="revealedKey"
      class="mb-4 rounded-lg border border-kipper-300 bg-kipper-50 p-3 dark:border-kipper-700 dark:bg-kipper-950/40"
    >
      <p class="text-sm font-medium text-kipper-800 dark:text-kipper-200">
        Key created. Copy it now: it is shown only this once.
      </p>
      <div class="mt-2 flex items-center gap-2">
        <code class="flex-1 overflow-x-auto rounded bg-white px-2 py-1.5 font-mono text-xs text-slate-800 dark:bg-slate-900 dark:text-slate-200">{{ revealedKey.key }}</code>
        <button
          @click="copyKey"
          class="rounded-md bg-kipper-600 p-1.5 text-white hover:bg-kipper-700"
          title="Copy key"
        >
          <Copy class="h-4 w-4" :stroke-width="2" />
        </button>
        <button
          @click="revealedKey = null"
          class="rounded-md border border-kipper-300 px-2 py-1 text-xs font-medium text-kipper-800 hover:bg-kipper-100 dark:border-kipper-700 dark:text-kipper-200 dark:hover:bg-kipper-900/40"
        >
          Done
        </button>
      </div>
    </div>

    <p v-if="loading" class="text-sm text-slate-500 dark:text-slate-400">Loading API keys…</p>
    <p v-else-if="error" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>

    <div v-else class="space-y-4">
      <!-- Usage plans -->
      <div>
        <div class="mb-1.5 flex items-center justify-between">
          <span class="text-xs font-medium uppercase tracking-wide text-slate-500 dark:text-slate-400">Usage plans</span>
          <button
            v-if="canManage"
            @click="showPlanForm = !showPlanForm"
            class="inline-flex items-center gap-1 rounded-md px-2 py-1 text-xs font-medium text-kipper-600 hover:bg-kipper-50 dark:text-kipper-400 dark:hover:bg-kipper-950/40"
          >
            <Plus class="h-3.5 w-3.5" :stroke-width="2" />
            Plan
          </button>
        </div>

        <form
          v-if="showPlanForm"
          @submit.prevent="savePlan"
          class="mb-2 flex flex-wrap items-end gap-2 rounded-lg bg-slate-50 p-3 dark:bg-slate-800/50"
        >
          <label class="text-xs text-slate-600 dark:text-slate-300">
            Name
            <input v-model="planForm.name" type="text" placeholder="bronze" class="mt-0.5 block w-28 rounded-md border border-slate-300 bg-white px-2 py-1 text-xs dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50" />
          </label>
          <label class="text-xs text-slate-600 dark:text-slate-300">
            Rate (rps)
            <input v-model.number="planForm.rate" type="number" min="1" class="mt-0.5 block w-20 rounded-md border border-slate-300 bg-white px-2 py-1 text-xs dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50" />
          </label>
          <label class="text-xs text-slate-600 dark:text-slate-300">
            Burst
            <input v-model.number="planForm.burst" type="number" min="1" class="mt-0.5 block w-20 rounded-md border border-slate-300 bg-white px-2 py-1 text-xs dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50" />
          </label>
          <label class="text-xs text-slate-600 dark:text-slate-300">
            Quota (0 = none)
            <input v-model.number="planForm.quotaRequests" type="number" min="0" class="mt-0.5 block w-24 rounded-md border border-slate-300 bg-white px-2 py-1 text-xs dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50" />
          </label>
          <label v-if="planForm.quotaRequests > 0" class="text-xs text-slate-600 dark:text-slate-300">
            Per
            <select v-model="planForm.quotaPeriod" class="mt-0.5 block rounded-md border border-slate-300 bg-white px-2 py-1 text-xs dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50">
              <option value="day">day</option>
              <option value="week">week</option>
              <option value="month">month</option>
            </select>
          </label>
          <button
            type="submit"
            :disabled="saving || !planForm.name.trim()"
            class="rounded-md bg-kipper-600 px-3 py-1 text-xs font-medium text-white hover:bg-kipper-700 disabled:opacity-50"
          >
            Save
          </button>
          <p class="w-full text-xs text-slate-500 dark:text-slate-400">
            Saving a plan re-applies its rate limit and quota to every key on it straight away.
          </p>
        </form>

        <p v-if="!plans.length" class="text-sm text-slate-500 dark:text-slate-400">
          No usage plans yet. A plan sets the rate limit and quota its keys get.
        </p>
        <ul v-else class="space-y-1">
          <li
            v-for="plan in plans"
            :key="plan.name"
            class="flex items-center justify-between gap-2 rounded-lg bg-slate-50 px-3 py-1.5 dark:bg-slate-800/50"
          >
            <div class="flex items-baseline gap-2 text-sm">
              <span class="font-medium text-slate-800 dark:text-slate-100">{{ plan.display_name || plan.name }}</span>
              <span class="text-xs text-slate-500 dark:text-slate-400">{{ planLabel(plan) }}</span>
            </div>
            <div class="flex items-center gap-2">
              <span class="text-xs text-slate-400">{{ plan.keys }} {{ plan.keys === 1 ? 'key' : 'keys' }}</span>
              <button
                v-if="canManage && plan.keys === 0"
                @click="removePlan(plan)"
                class="rounded p-1 text-slate-400 hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-950 dark:hover:text-red-400"
                :title="`Delete ${plan.name}`"
              >
                <Trash2 class="h-3.5 w-3.5" :stroke-width="2" />
              </button>
            </div>
          </li>
        </ul>
      </div>

      <!-- Keys -->
      <div>
        <div class="mb-1.5 flex items-center justify-between">
          <span class="text-xs font-medium uppercase tracking-wide text-slate-500 dark:text-slate-400">Keys</span>
          <button
            v-if="canManage && plans.length"
            @click="showKeyForm = !showKeyForm"
            class="inline-flex items-center gap-1 rounded-md px-2 py-1 text-xs font-medium text-kipper-600 hover:bg-kipper-50 dark:text-kipper-400 dark:hover:bg-kipper-950/40"
          >
            <Plus class="h-3.5 w-3.5" :stroke-width="2" />
            Key
          </button>
        </div>

        <form
          v-if="showKeyForm"
          @submit.prevent="issueKey"
          class="mb-2 flex flex-wrap items-end gap-2 rounded-lg bg-slate-50 p-3 dark:bg-slate-800/50"
        >
          <label class="text-xs text-slate-600 dark:text-slate-300">
            Name
            <input v-model="keyForm.display_name" type="text" placeholder="acme partner" class="mt-0.5 block w-36 rounded-md border border-slate-300 bg-white px-2 py-1 text-xs dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50" />
          </label>
          <label class="text-xs text-slate-600 dark:text-slate-300">
            Plan
            <select v-model="keyForm.plan" class="mt-0.5 block rounded-md border border-slate-300 bg-white px-2 py-1 text-xs dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50">
              <option value="" disabled>choose…</option>
              <option v-for="plan in plans" :key="plan.name" :value="plan.name">{{ plan.name }}</option>
            </select>
          </label>
          <label class="text-xs text-slate-600 dark:text-slate-300">
            Apps (empty = all gated apps)
            <input v-model="keyForm.apps" type="text" placeholder="api, webhooks" class="mt-0.5 block w-44 rounded-md border border-slate-300 bg-white px-2 py-1 text-xs dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50" />
          </label>
          <label class="text-xs text-slate-600 dark:text-slate-300">
            Expires (optional)
            <input v-model="keyForm.expires" type="date" class="mt-0.5 block rounded-md border border-slate-300 bg-white px-2 py-1 text-xs dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50" />
          </label>
          <button
            type="submit"
            :disabled="saving || !keyForm.plan"
            class="rounded-md bg-kipper-600 px-3 py-1 text-xs font-medium text-white hover:bg-kipper-700 disabled:opacity-50"
          >
            Create
          </button>
        </form>

        <p v-if="!keys.length" class="text-sm text-slate-500 dark:text-slate-400">
          No API keys yet. Create a usage plan above, then issue the first key from it.
        </p>
        <ul v-else class="space-y-1">
          <li
            v-for="key in keys"
            :key="key.name"
            class="flex flex-wrap items-center justify-between gap-2 rounded-lg bg-slate-50 px-3 py-1.5 dark:bg-slate-800/50"
          >
            <div class="flex items-baseline gap-2 text-sm">
              <span class="font-medium text-slate-800 dark:text-slate-100">{{ key.display_name || key.name }}</span>
              <code class="font-mono text-xs text-slate-500 dark:text-slate-400">kip_{{ key.prefix }}_…</code>
              <span class="text-xs text-slate-400">{{ key.plan }}</span>
              <span
                v-if="!key.enabled"
                class="rounded-full bg-amber-100 px-1.5 py-0.5 text-[10px] font-medium uppercase text-amber-700 dark:bg-amber-900/50 dark:text-amber-300"
              >
                Disabled
              </span>
              <span
                v-if="expiryByKey[key.name]"
                :class="expiryByKey[key.name]!.cls"
                class="rounded-full px-1.5 py-0.5 text-[10px] font-medium uppercase"
              >
                {{ expiryByKey[key.name]!.label }}
              </span>
            </div>
            <div class="flex items-center gap-2">
              <div class="flex items-center gap-1.5 text-xs text-slate-400">
                <span :title="'Allowed requests over the last 90 days'">
                  {{ usageSummary[key.name].allowed.toLocaleString() }} ok
                </span>
                <span
                  v-if="usageSummary[key.name].deniedRate"
                  class="text-amber-600 dark:text-amber-400"
                  :title="'Requests turned away for exceeding the rate limit, last 90 days'"
                >
                  {{ usageSummary[key.name].deniedRate.toLocaleString() }} rate
                </span>
                <span
                  v-if="usageSummary[key.name].deniedQuota"
                  class="text-amber-600 dark:text-amber-400"
                  :title="'Requests turned away for exceeding the quota, last 90 days'"
                >
                  {{ usageSummary[key.name].deniedQuota.toLocaleString() }} quota
                </span>
                <span class="text-slate-400" :title="'Most recent day with any traffic'">
                  {{ usageSummary[key.name].lastUsed ? `used ${usageSummary[key.name].lastUsed}` : 'unused (90d)' }}
                </span>
              </div>
              <button
                v-if="canManage"
                @click="toggleKey(key)"
                class="rounded-md border border-slate-300 px-2 py-0.5 text-xs text-slate-600 hover:bg-slate-100 dark:border-slate-700 dark:text-slate-300 dark:hover:bg-slate-800"
              >
                {{ key.enabled ? 'Disable' : 'Enable' }}
              </button>
              <button
                v-if="canManage"
                @click="revokeKey(key)"
                class="rounded p-1 text-slate-400 hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-950 dark:hover:text-red-400"
                :title="`Revoke ${key.display_name || key.name}`"
              >
                <Trash2 class="h-3.5 w-3.5" :stroke-width="2" />
              </button>
            </div>
          </li>
        </ul>
      </div>
    </div>
  </div>
</template>
