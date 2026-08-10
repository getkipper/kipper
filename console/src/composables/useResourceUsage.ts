import { isRef, onMounted, onUnmounted, ref, shallowRef, watch, type Ref } from 'vue'
import { fetchResourceUsage, type UsageResponse, type UsageScope } from '@/api/resources'

export interface UseResourceUsageOptions {
  // Polling interval in milliseconds. 0 disables polling (single fetch).
  pollMs?: number
  // Skip the initial fetch on mount. Useful when scope is not yet resolved.
  manual?: boolean
}

const DEFAULT_POLL_MS = 15000

type MaybeScope = UsageScope | null | undefined
type ScopeInput = MaybeScope | Ref<MaybeScope>

// useResourceUsage fetches /api/v1/resources/usage on mount and at a
// regular cadence. The scope may be a plain object (static) or a Ref
// (reactive — a null value pauses polling, useful when the consumer
// needs to wait for config to resolve or for a component to be enabled).
//
// Polling pauses while the browser tab is hidden so an idle window does
// not hammer the API. The interval is cleared on unmount.
export function useResourceUsage(scope: ScopeInput, options: UseResourceUsageOptions = {}) {
  const data = shallowRef<UsageResponse | null>(null)
  const loading = ref(false)
  const error = ref<string | null>(null)

  let timer: ReturnType<typeof setInterval> | null = null
  let abandoned = false

  const currentScope: Ref<MaybeScope> = isRef(scope) ? scope : ref(scope)

  async function refresh() {
    const s = currentScope.value
    if (!s) return
    loading.value = true
    error.value = null
    try {
      const resp = await fetchResourceUsage(s)
      if (!abandoned) data.value = resp
    } catch (e) {
      if (!abandoned) error.value = e instanceof Error ? e.message : 'failed to fetch usage'
    } finally {
      if (!abandoned) loading.value = false
    }
  }

  function start() {
    stop()
    const period = options.pollMs ?? DEFAULT_POLL_MS
    if (period > 0) {
      timer = setInterval(() => {
        if (typeof document !== 'undefined' && document.hidden) return
        refresh()
      }, period)
    }
  }

  function stop() {
    if (timer !== null) {
      clearInterval(timer)
      timer = null
    }
  }

  onMounted(() => {
    if (!options.manual && currentScope.value) refresh()
    if (currentScope.value) start()
  })

  // When the scope appears (null → non-null) start polling. When it
  // disappears, stop and clear stale data so the consumer can render a
  // disabled state cleanly.
  watch(
    currentScope,
    (next, prev) => {
      if (next) {
        refresh()
        start()
      } else if (prev) {
        stop()
        data.value = null
      }
    },
    { deep: true },
  )

  onUnmounted(() => {
    abandoned = true
    stop()
  })

  return { data, loading, error, refresh, start, stop }
}
