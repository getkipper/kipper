import { ref } from 'vue'

export interface Toast {
  id: number
  message: string
  type: 'success' | 'error' | 'info'
}

const toasts = ref<Toast[]>([])
const timers = new Map<number, ReturnType<typeof setTimeout>>()
let nextId = 0

// Success and info are transient; errors stay until dismissed so a failure
// message can't vanish before the user has read it.
const AUTO_DISMISS_MS = 4000

export function useToast() {
  function dismiss(id: number) {
    const timer = timers.get(id)
    if (timer) {
      clearTimeout(timer)
      timers.delete(id)
    }
    toasts.value = toasts.value.filter(t => t.id !== id)
  }

  function show(message: string, type: Toast['type'] = 'info') {
    const id = nextId++
    toasts.value.push({ id, message, type })
    if (type !== 'error') {
      timers.set(id, setTimeout(() => dismiss(id), AUTO_DISMISS_MS))
    }
  }

  function success(message: string) { show(message, 'success') }
  function error(message: string) { show(message, 'error') }
  function info(message: string) { show(message, 'info') }

  return { toasts, success, error, info, dismiss }
}
