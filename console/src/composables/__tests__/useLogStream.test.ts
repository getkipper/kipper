// @vitest-environment happy-dom
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { useLogStream } from '../useLogStream'

class MockWebSocket {
  onopen: (() => void) | null = null
  onmessage: ((event: { data: string }) => void) | null = null
  onclose: (() => void) | null = null
  onerror: (() => void) | null = null
  closed = false

  close() {
    this.closed = true
    this.onclose?.()
  }

  simulateOpen() {
    this.onopen?.()
  }

  simulateMessage(data: string) {
    this.onmessage?.({ data })
  }

  simulateError() {
    this.onerror?.()
  }
}

let lastWebSocket: MockWebSocket

vi.stubGlobal('WebSocket', vi.fn().mockImplementation(() => {
  lastWebSocket = new MockWebSocket()
  return lastWebSocket
}))

describe('useLogStream', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  it('starts disconnected with empty lines', () => {
    const { lines, connected } = useLogStream()
    expect(lines.value).toEqual([])
    expect(connected.value).toBe(false)
  })

  it('connects and receives messages', () => {
    const { lines, connected, connect } = useLogStream()

    connect('default', 'api')
    lastWebSocket.simulateOpen()

    expect(connected.value).toBe(true)

    lastWebSocket.simulateMessage('log line 1')
    lastWebSocket.simulateMessage('log line 2')

    expect(lines.value).toEqual(['log line 1', 'log line 2'])
  })

  it('disconnects cleanly', () => {
    const { connected, connect, disconnect } = useLogStream()

    connect('default', 'api')
    lastWebSocket.simulateOpen()
    expect(connected.value).toBe(true)

    disconnect()
    expect(connected.value).toBe(false)
  })

  it('clears lines', () => {
    const { lines, connect, clear } = useLogStream()

    connect('default', 'api')
    lastWebSocket.simulateOpen()
    lastWebSocket.simulateMessage('line 1')

    expect(lines.value).toHaveLength(1)

    clear()
    expect(lines.value).toHaveLength(0)
  })

  it('sets error on connection failure', () => {
    const { error, connect } = useLogStream()

    connect('default', 'api')
    lastWebSocket.simulateError()

    expect(error.value).toBe('Connection error')
  })
})
