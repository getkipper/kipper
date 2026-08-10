import { defineStore } from 'pinia'
import { ref } from 'vue'
import * as api from '@/api/projects'
import type { ProjectQuota, QuotaUpdate } from '@/api/projects'

export const useProjectQuotaStore = defineStore('projectQuota', () => {
  const quota = ref<ProjectQuota | null>(null)
  const loading = ref(false)
  const error = ref<string | null>(null)

  // The store is a singleton shared across project panels. loadSeq lets the
  // latest load win and drops stale responses; activeScope records which
  // project the state belongs to, so a slow response or a dialog completing
  // after a project switch can never touch the newer project's view.
  let loadSeq = 0
  let activeScope = ''

  async function loadQuota(project: string) {
    const seq = ++loadSeq
    if (project !== activeScope) {
      // Entering a new scope: drop the previous project's quota so the panel
      // never renders it while this load is in flight.
      activeScope = project
      quota.value = null
    }
    loading.value = true
    error.value = null
    try {
      const result = await api.fetchQuota(project)
      if (seq !== loadSeq) return
      quota.value = result
    } catch (e) {
      if (seq !== loadSeq) return
      error.value = e instanceof Error ? e.message : 'failed to load quota'
    } finally {
      if (seq === loadSeq) loading.value = false
    }
  }

  // Apply a quota change and return the updated quota. Errors (including the 409
  // that carries force-confirm warnings) propagate so the caller can react.
  // The view converges through a reload: any load interleaved with the save
  // carries an older sequence and is dropped, so stale pre-save data cannot
  // stick, and the loading flag stays owned by loadQuota.
  async function saveQuota(project: string, update: QuotaUpdate): Promise<ProjectQuota> {
    const updated = await api.updateQuota(project, update)
    if (project === activeScope) await loadQuota(project)
    return updated
  }

  return { quota, loading, error, loadQuota, saveQuota }
})
