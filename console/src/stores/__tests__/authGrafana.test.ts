// @vitest-environment happy-dom
import { setActivePinia, createPinia } from 'pinia'
import { describe, it, expect, beforeEach, vi } from 'vitest'

const postMock = vi.hoisted(() => vi.fn().mockResolvedValue({ data: {} }))
const getMock = vi.hoisted(() => vi.fn().mockResolvedValue({ data: {} }))
vi.mock('axios', () => ({
  default: {
    create: () => ({
      post: postMock,
      get: getMock,
      interceptors: { request: { use: vi.fn() }, response: { use: vi.fn() } },
    }),
  },
}))
import { useAuthStore } from '../auth'

describe('auth store Grafana link', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    localStorage.clear()
    getMock.mockReset()
  })

  it('fetchRole keeps the Grafana URL /me reports', async () => {
    getMock.mockResolvedValue({ data: { email: 'dev@test.com', role: 'deployer', grafanaUrl: 'https://grafana.example.com' } })
    const store = useAuthStore()

    await store.fetchRole()

    expect(store.grafanaUrl).toBe('https://grafana.example.com')
    expect(localStorage.getItem('kipper_grafana_url')).toBe('https://grafana.example.com')
  })

  it('fetchGrafanaUrl clears the link when /me reports none', async () => {
    localStorage.setItem('kipper_grafana_url', 'https://grafana.example.com')
    getMock.mockResolvedValue({ data: { email: 'dev@test.com', role: 'viewer' } })
    const store = useAuthStore()

    await store.fetchGrafanaUrl()

    expect(store.grafanaUrl).toBeNull()
    expect(localStorage.getItem('kipper_grafana_url')).toBeNull()
  })

  it('fetchGrafanaUrl hides the link when /me fails', async () => {
    localStorage.setItem('kipper_grafana_url', 'https://grafana.example.com')
    getMock.mockRejectedValue(new Error('network'))
    const store = useAuthStore()

    await store.fetchGrafanaUrl()

    expect(store.grafanaUrl).toBeNull()
  })

  it('logout clears the Grafana link', () => {
    localStorage.setItem('kipper_grafana_url', 'https://grafana.example.com')
    const store = useAuthStore()
    store.login('token', 'dev@test.com')

    store.logout()

    expect(store.grafanaUrl).toBeNull()
    expect(localStorage.getItem('kipper_grafana_url')).toBeNull()
  })
})

describe('auth store SSO codes', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    localStorage.clear()
    postMock.mockReset()
  })

  it('requestUICode reports a refusal separately from other failures', async () => {
    postMock.mockRejectedValueOnce({ response: { status: 403 } })
    const store = useAuthStore()
    store.login('token', 'viewer@test.com')

    expect(await store.requestUICode('grafana.example.com')).toEqual({ code: null, forbidden: true })

    postMock.mockRejectedValueOnce(new Error('network'))
    expect(await store.requestUICode('grafana.example.com')).toEqual({ code: null, forbidden: false })

    postMock.mockResolvedValueOnce({ data: { code: 'c1' } })
    expect(await store.requestUICode('grafana.example.com')).toEqual({ code: 'c1', forbidden: false })
  })
})
