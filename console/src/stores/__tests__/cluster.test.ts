import { setActivePinia, createPinia } from 'pinia'
import { describe, it, expect, beforeEach, vi } from 'vitest'
import { useClusterStore } from '../cluster'
import * as api from '@/api/cluster'

vi.mock('@/api/cluster')

describe('cluster store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.resetAllMocks()
  })

  it('starts with null status and empty nodes', () => {
    const store = useClusterStore()
    expect(store.status).toBeNull()
    expect(store.nodes).toEqual([])
    expect(store.loading).toBe(false)
    expect(store.error).toBeNull()
  })

  it('loads cluster status successfully', async () => {
    vi.mocked(api.fetchClusterStatus).mockResolvedValue({
      health: 'healthy',
      nodes: [{ name: 'node1', status: 'Ready', role: 'master', version: 'v1.34.5', ip: '10.0.0.1' }],
    })

    const store = useClusterStore()
    await store.loadStatus()

    expect(store.status?.health).toBe('healthy')
    expect(store.status?.nodes).toHaveLength(1)
    expect(store.loading).toBe(false)
    expect(store.error).toBeNull()
  })

  it('sets loading state while fetching', async () => {
    vi.mocked(api.fetchClusterStatus).mockResolvedValue({ health: 'healthy', nodes: [] })

    const store = useClusterStore()
    const promise = store.loadStatus()
    expect(store.loading).toBe(true)
    await promise
    expect(store.loading).toBe(false)
  })

  it('captures errors without crashing', async () => {
    vi.mocked(api.fetchClusterStatus).mockRejectedValue(new Error('connection refused'))

    const store = useClusterStore()
    await store.loadStatus()

    expect(store.error).toBe('connection refused')
    expect(store.status).toBeNull()
    expect(store.loading).toBe(false)
  })

  it('loads nodes successfully', async () => {
    vi.mocked(api.fetchNodes).mockResolvedValue([
      { name: 'node1', status: 'Ready', role: 'master', version: 'v1.34.5', ip: '10.0.0.1' },
      { name: 'node2', status: 'Ready', role: 'worker', version: 'v1.34.5', ip: '10.0.0.2' },
    ])

    const store = useClusterStore()
    await store.loadNodes()

    expect(store.nodes).toHaveLength(2)
    expect(store.nodes[0].name).toBe('node1')
    expect(store.nodes[1].role).toBe('worker')
  })
})
