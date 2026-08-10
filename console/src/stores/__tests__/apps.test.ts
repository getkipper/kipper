import { setActivePinia, createPinia } from 'pinia'
import { describe, it, expect, beforeEach, vi } from 'vitest'
import { useAppsStore } from '../apps'
import * as api from '@/api/apps'

vi.mock('@/api/apps')

describe('apps store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.resetAllMocks()
  })

  it('starts with empty apps', () => {
    const store = useAppsStore()
    expect(store.apps).toEqual([])
    expect(store.loading).toBe(false)
    expect(store.error).toBeNull()
  })

  it('loads apps successfully', async () => {
    const mockApps = [
      { name: 'api', status: 'running' as const, image: 'ghcr.io/acme/api:latest', replicas: 2, ready: 2 },
      { name: 'frontend', status: 'running' as const, image: 'nginx:latest', replicas: 1, ready: 1 },
    ]
    vi.mocked(api.fetchApps).mockResolvedValue(mockApps)

    const store = useAppsStore()
    await store.loadApps('staging')

    expect(store.apps).toHaveLength(2)
    expect(store.apps[0].name).toBe('api')
    expect(store.apps[1].name).toBe('frontend')
  })

  it('replaces apps when switching projects', async () => {
    vi.mocked(api.fetchApps)
      .mockResolvedValueOnce([{ name: 'api', status: 'running' as const, image: 'img1', replicas: 1, ready: 1 }])
      .mockResolvedValueOnce([{ name: 'worker', status: 'running' as const, image: 'img2', replicas: 1, ready: 1 }])

    const store = useAppsStore()
    await store.loadApps('staging')
    expect(store.apps).toHaveLength(1)
    expect(store.apps[0].name).toBe('api')

    await store.loadApps('default')
    expect(store.apps).toHaveLength(1)
    expect(store.apps[0].name).toBe('worker')
  })

  it('replaces apps from the same project on reload', async () => {
    vi.mocked(api.fetchApps)
      .mockResolvedValueOnce([{ name: 'api', status: 'running' as const, image: 'img:v1', replicas: 1, ready: 1 }])
      .mockResolvedValueOnce([{ name: 'api', status: 'running' as const, image: 'img:v2', replicas: 1, ready: 1 }])

    const store = useAppsStore()
    await store.loadApps('staging')
    await store.loadApps('staging')

    expect(store.apps).toHaveLength(1)
    expect(store.apps[0].image).toBe('img:v2')
  })

  it('captures API errors without crashing', async () => {
    vi.mocked(api.fetchApps).mockRejectedValue(new Error('network error'))

    const store = useAppsStore()
    await store.loadApps('staging')

    expect(store.error).toBe('network error')
    expect(store.apps).toHaveLength(0)
  })

  it('deploys an app and adds it to the list', async () => {
    const newApp = { name: 'new-app', status: 'pending' as const, image: 'nginx', replicas: 1, ready: 0 }
    vi.mocked(api.createApp).mockResolvedValue(newApp)

    const store = useAppsStore()
    await store.deployApp('staging', { name: 'new-app', image: 'nginx', port: 80, replicas: 1, env: {} })

    expect(store.apps).toHaveLength(1)
    expect(store.apps[0].name).toBe('new-app')
  })

  it('removes an app from the list on delete', async () => {
    vi.mocked(api.fetchApps).mockResolvedValue([
      { name: 'api', status: 'running' as const, image: 'img', replicas: 1, ready: 1 },
      { name: 'worker', status: 'running' as const, image: 'img', replicas: 1, ready: 1 },
    ])
    vi.mocked(api.deleteApp).mockResolvedValue()

    const store = useAppsStore()
    await store.loadApps('staging')
    await store.removeApp('staging', 'api')

    expect(store.apps).toHaveLength(1)
    expect(store.apps[0].name).toBe('worker')
  })
})
