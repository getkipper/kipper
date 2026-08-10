import { defineStore } from 'pinia'
import { ref } from 'vue'
import * as api from '@/api/apikeys'
import type { ApiKey, KeyUsageDay, UsagePlan } from '@/api/apikeys'

type UsagePlanInput = Omit<UsagePlan, 'keys'>
type CreateKeyPayload = { display_name?: string; plan: string; apps?: string[]; expires_at?: string }

// daysAgo returns the UTC calendar date n days before today, matching the
// YYYY-MM-DD the usage endpoint expects.
function daysAgo(n: number): string {
  return new Date(Date.now() - n * 86_400_000).toISOString().slice(0, 10)
}

export const useProjectApiKeysStore = defineStore('projectApiKeys', () => {
  const plans = ref<UsagePlan[]>([])
  const keys = ref<ApiKey[]>([])
  const usageByKey = ref<Record<string, KeyUsageDay[]>>({})
  const loading = ref(false)
  const error = ref<string | null>(null)

  // The store is a singleton shared across project panels. loadSeq lets the
  // latest load win and drops stale responses; activeScope records which
  // namespace the state belongs to, so a slow response or a dialog completing
  // after an environment switch can never touch the newer view.
  let loadSeq = 0
  let activeScope = ''

  async function loadKeys(namespace: string) {
    if (!namespace) return
    const seq = ++loadSeq
    if (namespace !== activeScope) {
      // Entering a new scope: drop the previous scope's rows so the panel
      // never renders them while this load is in flight.
      activeScope = namespace
      plans.value = []
      keys.value = []
      usageByKey.value = {}
    }
    loading.value = true
    error.value = null
    try {
      const [p, k] = await Promise.all([api.fetchPlans(namespace), api.fetchKeys(namespace)])
      // Pull the full retention window so totals and the last-used date span the
      // three months of history the cluster keeps, not just the last month.
      const window = { from: daysAgo(91) }
      const usage = await Promise.all(
        k.map(key =>
          api.fetchKeyUsage(namespace, key.name, window).then(r => r.days).catch(() => [] as KeyUsageDay[])),
      )
      if (seq !== loadSeq) return
      plans.value = p
      keys.value = k
      usageByKey.value = Object.fromEntries(k.map((key, i) => [key.name, usage[i]]))
    } catch (e) {
      if (seq !== loadSeq) return
      error.value = e instanceof Error ? e.message : 'failed to load API keys'
    } finally {
      if (seq === loadSeq) loading.value = false
    }
  }

  // Mutations reload only while their scope is still the active one: a
  // completion from a panel the user has already left must not overwrite the
  // currently displayed scope with its own reload.
  async function savePlan(namespace: string, plan: UsagePlanInput) {
    await api.upsertPlan(namespace, plan)
    if (namespace === activeScope) await loadKeys(namespace)
  }

  async function removePlan(namespace: string, name: string) {
    await api.deletePlan(namespace, name)
    if (namespace === activeScope) await loadKeys(namespace)
  }

  async function createKey(namespace: string, payload: CreateKeyPayload): Promise<ApiKey> {
    const created = await api.createKey(namespace, payload)
    if (namespace === activeScope) await loadKeys(namespace)
    return created
  }

  async function setKeyEnabled(namespace: string, name: string, enabled: boolean) {
    await api.updateKey(namespace, name, { enabled })
    if (namespace === activeScope) await loadKeys(namespace)
  }

  async function deleteKey(namespace: string, name: string) {
    await api.deleteKey(namespace, name)
    if (namespace === activeScope) await loadKeys(namespace)
  }

  return { plans, keys, usageByKey, loading, error, loadKeys, savePlan, removePlan, createKey, setKeyEnabled, deleteKey }
})
