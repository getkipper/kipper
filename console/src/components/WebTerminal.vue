<script setup lang="ts">
import { ref, onMounted, onBeforeUnmount, nextTick } from 'vue'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { WebLinksAddon } from '@xterm/addon-web-links'
import '@xterm/xterm/css/xterm.css'

interface Props {
  namespace: string
  appName: string
  pod?: string
}

const props = defineProps<Props>()

const emit = defineEmits<{
  disconnected: []
}>()

const terminalRef = ref<HTMLDivElement>()
const status = ref<'connecting' | 'connected' | 'disconnected' | 'error'>('disconnected')
const errorMessage = ref('')

let terminal: Terminal | null = null
let fitAddon: FitAddon | null = null
let ws: WebSocket | null = null
let resizeObserver: ResizeObserver | null = null

function buildUrl(): string {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const host = window.location.host
  let url = `${protocol}//${host}/api/v1/terminal/${props.namespace}/${props.appName}`
  if (props.pod) {
    url += `?pod=${encodeURIComponent(props.pod)}`
  }
  return url
}

// The JWT rides in the Sec-WebSocket-Protocol header rather than the URL,
// so it stays out of proxy logs and browser history. The server echoes the
// sentinel to complete the handshake.
function authProtocols(): string[] {
  const token = localStorage.getItem('kipper_token')
  return token ? ['kipper.auth', token] : ['kipper.auth']
}

function connect() {
  disconnect()

  status.value = 'connecting'
  errorMessage.value = ''

  terminal = new Terminal({
    cursorBlink: true,
    fontFamily: 'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace',
    fontSize: 13,
    lineHeight: 1.2,
    theme: {
      background: '#020617',
      foreground: '#e2e8f0',
      cursor: '#e2e8f0',
      selectionBackground: '#334155',
      black: '#020617',
      red: '#f87171',
      green: '#4ade80',
      yellow: '#facc15',
      blue: '#60a5fa',
      magenta: '#c084fc',
      cyan: '#22d3ee',
      white: '#e2e8f0',
      brightBlack: '#475569',
      brightRed: '#fca5a5',
      brightGreen: '#86efac',
      brightYellow: '#fde68a',
      brightBlue: '#93c5fd',
      brightMagenta: '#d8b4fe',
      brightCyan: '#67e8f9',
      brightWhite: '#f8fafc',
    },
  })

  fitAddon = new FitAddon()
  terminal.loadAddon(fitAddon)
  terminal.loadAddon(new WebLinksAddon())

  if (terminalRef.value) {
    terminal.open(terminalRef.value)
    fitAddon.fit()
  }

  ws = new WebSocket(buildUrl(), authProtocols())
  ws.binaryType = 'arraybuffer'

  ws.onopen = () => {
    status.value = 'connected'
    if (terminal && fitAddon) {
      sendResize(terminal.cols, terminal.rows)
      terminal.focus()
    }
  }

  ws.onmessage = (event: MessageEvent) => {
    if (!terminal) return
    if (event.data instanceof ArrayBuffer) {
      terminal.write(new Uint8Array(event.data))
    } else {
      terminal.write(event.data as string)
    }
  }

  ws.onerror = () => {
    status.value = 'error'
    errorMessage.value = 'Connection failed'
  }

  ws.onclose = () => {
    if (status.value !== 'error') {
      status.value = 'disconnected'
    }
    emit('disconnected')
  }

  terminal.onData((data: string) => {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(data)
    }
  })

  terminal.onResize(({ cols, rows }) => {
    sendResize(cols, rows)
  })

  // Observe container size changes to re-fit the terminal
  if (terminalRef.value) {
    resizeObserver = new ResizeObserver(() => {
      if (fitAddon) {
        fitAddon.fit()
      }
    })
    resizeObserver.observe(terminalRef.value)
  }
}

function sendResize(cols: number, rows: number) {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: 'resize', cols, rows }))
  }
}

function disconnect() {
  if (resizeObserver) {
    resizeObserver.disconnect()
    resizeObserver = null
  }
  if (ws) {
    // Detach handlers before closing so a late close/error event from the
    // old socket can't overwrite the state of a freshly opened one.
    ws.onopen = null
    ws.onmessage = null
    ws.onerror = null
    ws.onclose = null
    ws.close()
    ws = null
  }
  if (terminal) {
    terminal.dispose()
    terminal = null
  }
  fitAddon = null
  status.value = 'disconnected'
}

function reconnect() {
  connect()
}


onMounted(() => {
  nextTick(() => {
    connect()
  })
})

onBeforeUnmount(() => {
  disconnect()
})

function focus() {
  if (fitAddon) {
    fitAddon.fit()
  }
  if (terminal) {
    terminal.focus()
  }
}

defineExpose({ connect, disconnect, reconnect, focus })
</script>

<template>
  <div class="flex flex-col">
    <div class="mb-2 flex items-center gap-2 text-xs">
      <span
        class="inline-block h-2 w-2 rounded-full"
        :class="{
          'bg-emerald-500': status === 'connected',
          'bg-amber-500 animate-pulse': status === 'connecting',
          'bg-slate-400': status === 'disconnected',
          'bg-red-500': status === 'error',
        }"
      />
      <span class="text-slate-500 dark:text-slate-400">
        {{ status === 'connected' ? 'Connected' : status === 'connecting' ? 'Connecting...' : status === 'error' ? errorMessage : 'Disconnected' }}
      </span>
      <button
        v-if="status === 'disconnected' || status === 'error'"
        class="ml-auto rounded-md border border-slate-300 px-2 py-0.5 text-xs font-medium text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-800"
        @click="reconnect"
      >
        Reconnect
      </button>
    </div>
    <div
      ref="terminalRef"
      class="h-80 w-full overflow-hidden rounded-lg border border-slate-200 bg-[#020617] dark:border-slate-700"
    />
  </div>
</template>
