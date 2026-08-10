import { ref, onUnmounted } from 'vue'

export function useLogStream() {
  const lines = ref<string[]>([])
  const connected = ref(false)
  const error = ref<string | null>(null)

  let ws: WebSocket | null = null

  function connect(project: string, app: string, pod?: string, tail?: number) {
    disconnect()

    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    const host = window.location.host
    const params = new URLSearchParams()
    if (pod) params.set('pod', pod)
    if (tail && tail !== 100) params.set('tail', String(tail))
    const qs = params.toString()
    const url = `${protocol}//${host}/api/v1/projects/${project}/apps/${app}/logs${qs ? '?' + qs : ''}`

    // The JWT rides in the Sec-WebSocket-Protocol header, not the URL, so it
    // stays out of proxy logs and browser history.
    const token = localStorage.getItem('kipper_token')
    const protocols = token ? ['kipper.auth', token] : ['kipper.auth']

    ws = new WebSocket(url, protocols)

    ws.onopen = () => {
      connected.value = true
      error.value = null
    }

    ws.onmessage = (event) => {
      lines.value.push(event.data)
      // Keep a reasonable buffer
      if (lines.value.length > 1000) {
        lines.value = lines.value.slice(-500)
      }
    }

    ws.onerror = () => {
      error.value = 'Connection error'
      connected.value = false
    }

    ws.onclose = () => {
      connected.value = false
    }
  }

  function disconnect() {
    if (ws) {
      ws.close()
      ws = null
    }
    connected.value = false
  }

  function clear() {
    lines.value = []
  }

  onUnmounted(() => {
    disconnect()
  })

  return { lines, connected, error, connect, disconnect, clear }
}
