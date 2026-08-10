import { defineStore } from 'pinia'
import { ref } from 'vue'
import * as api from '@/api/alerts'
import type { Alert } from '@/api/alerts'

export const useAlertsStore = defineStore('alerts', () => {
  const alerts = ref<Alert[]>([])
  const unreadCount = ref(0)
  const loading = ref(false)
  const error = ref<string | null>(null)

  // loadSeq lets the latest load() win if two overlap, so a slow earlier
  // request can't overwrite newer data.
  let loadSeq = 0

  // Notification-edge state, owned solely by the store so no single view can
  // consume another view's chime. lastNotified advances whenever the user has
  // seen the current alerts (a load) or cleared them (a dismiss), or when the
  // poll has already chimed for them; primed suppresses a chime on first load.
  let lastNotified = 0
  let primed = false

  // dismissEpoch rises on each dismiss so a count fetched before a dismiss
  // can't restore the badge or the edge after the user cleared them.
  let dismissEpoch = 0

  // Fetches the full list and the unread count together so the badge and the
  // list never disagree. The count is best-effort: a count blip must not blank
  // the list.
  async function load() {
    const seq = ++loadSeq
    const epoch = dismissEpoch
    loading.value = true
    error.value = null
    const countPromise = api.fetchUnreadCount().catch(() => null)
    try {
      const [list, count] = await Promise.all([api.fetchAlerts(), countPromise])
      if (seq !== loadSeq) return
      alerts.value = list
      // The list is safe to write regardless, but a count observed before a
      // dismiss must not undo it.
      if (count !== null && epoch === dismissEpoch) {
        unreadCount.value = count
        lastNotified = count
        primed = true
      }
    } catch (e) {
      if (seq !== loadSeq) return
      error.value = e instanceof Error ? e.message : 'Failed to load alerts'
      alerts.value = []
    } finally {
      if (seq === loadSeq) loading.value = false
    }
  }

  // Lightweight count-only refresh for the bell's background poll. Returns true
  // when the unread count has grown since the last observation, so the caller
  // can chime once per new batch. Discards its result if a dismiss landed while
  // the request was in flight, so a stale count can't restore the badge or
  // suppress the next chime.
  async function pollForNew(): Promise<boolean> {
    const epoch = dismissEpoch
    const count = await api.fetchUnreadCount()
    if (epoch !== dismissEpoch) return false
    unreadCount.value = count
    const isNew = primed && count > lastNotified
    lastNotified = count
    primed = true
    return isNew
  }

  async function dismiss() {
    await api.dismissAlerts()
    unreadCount.value = 0
    lastNotified = 0
    dismissEpoch++
  }

  return { alerts, unreadCount, loading, error, load, pollForNew, dismiss }
})
