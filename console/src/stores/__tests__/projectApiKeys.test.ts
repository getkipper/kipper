import { setActivePinia, createPinia } from 'pinia'
import { describe, it, expect, beforeEach, vi } from 'vitest'
import { useProjectApiKeysStore } from '../projectApiKeys'
import * as api from '@/api/apikeys'
import type { ApiKey, UsagePlan, KeyUsageResponse } from '@/api/apikeys'

vi.mock('@/api/apikeys')

const plan = (name: string): UsagePlan => ({ name, rate: 10, burst: 20, keys: 1 })
const key = (name: string): ApiKey =>
  ({ name, plan: 'basic', prefix: 'kp_ab', enabled: true, apps: [], created: '2026-01-01' })
const usage = (allowed: number): KeyUsageResponse => ({
  from: '2026-01-01',
  to: '2026-01-01',
  retention_days: 92,
  days: [{ day: '2026-01-01', allowed, denied_rate: 0, denied_quota: 0 }],
})

describe('projectApiKeys store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.resetAllMocks()
  })

  it('loads plans, keys, and per-key usage', async () => {
    vi.mocked(api.fetchPlans).mockResolvedValue([plan('basic')])
    vi.mocked(api.fetchKeys).mockResolvedValue([key('web'), key('cli')])
    vi.mocked(api.fetchKeyUsage).mockResolvedValueOnce(usage(5)).mockResolvedValueOnce(usage(9))

    const store = useProjectApiKeysStore()
    await store.loadKeys('shop')

    expect(store.plans).toHaveLength(1)
    expect(store.keys.map(k => k.name)).toEqual(['web', 'cli'])
    expect(store.usageByKey.web[0].allowed).toBe(5)
    expect(store.usageByKey.cli[0].allowed).toBe(9)
  })

  it('falls back to empty usage when a per-key usage fetch fails', async () => {
    vi.mocked(api.fetchPlans).mockResolvedValue([])
    vi.mocked(api.fetchKeys).mockResolvedValue([key('web')])
    vi.mocked(api.fetchKeyUsage).mockRejectedValue(new Error('metrics down'))

    const store = useProjectApiKeysStore()
    await store.loadKeys('shop')

    expect(store.error).toBeNull()
    expect(store.usageByKey.web).toEqual([])
  })

  it('ignores an empty namespace', async () => {
    const store = useProjectApiKeysStore()
    await store.loadKeys('')
    expect(api.fetchKeys).not.toHaveBeenCalled()
  })

  it('reloads after creating a key and returns the issued key', async () => {
    vi.mocked(api.fetchPlans).mockResolvedValue([])
    vi.mocked(api.fetchKeys).mockResolvedValue([key('web')])
    vi.mocked(api.fetchKeyUsage).mockResolvedValue(usage(0))
    vi.mocked(api.createKey).mockResolvedValue({ ...key('web'), key: 'kp_secret' })

    const store = useProjectApiKeysStore()
    await store.loadKeys('shop')
    const issued = await store.createKey('shop', { plan: 'basic' })

    expect(issued.key).toBe('kp_secret')
    expect(api.fetchKeys).toHaveBeenCalledTimes(2)
  })

  it('propagates a mutation error to the caller', async () => {
    vi.mocked(api.deleteKey).mockRejectedValueOnce(new Error('denied'))
    const store = useProjectApiKeysStore()
    await expect(store.deleteKey('shop', 'web')).rejects.toThrow('denied')
  })

  it('ignores a delete that completes after an environment switch', async () => {
    // The delete targeted shop-test; its reload must not overwrite the
    // shop-prod view the user has since navigated to.
    let resolveDelete!: () => void
    vi.mocked(api.fetchPlans).mockResolvedValue([])
    vi.mocked(api.fetchKeys)
      .mockResolvedValueOnce([key('web')])
      .mockResolvedValueOnce([key('prod-key')])
    vi.mocked(api.fetchKeyUsage).mockResolvedValue(usage(0))
    vi.mocked(api.deleteKey).mockImplementationOnce(() => new Promise(res => { resolveDelete = () => res() }))

    const store = useProjectApiKeysStore()
    await store.loadKeys('shop-test')
    const removal = store.deleteKey('shop-test', 'web')
    await store.loadKeys('shop-prod')

    resolveDelete()
    await removal
    expect(store.keys.map(k => k.name)).toEqual(['prod-key'])
    expect(api.fetchKeys).toHaveBeenCalledTimes(2)
  })

  it('clears the previous scope rows while a new scope loads', async () => {
    let resolveKeys!: (v: ApiKey[]) => void
    vi.mocked(api.fetchPlans).mockResolvedValue([])
    vi.mocked(api.fetchKeys)
      .mockResolvedValueOnce([key('web')])
      .mockImplementationOnce(() => new Promise(res => { resolveKeys = res }))
    vi.mocked(api.fetchKeyUsage).mockResolvedValue(usage(0))

    const store = useProjectApiKeysStore()
    await store.loadKeys('shop-test')
    const load = store.loadKeys('shop-prod')
    expect(store.keys).toEqual([])
    expect(store.loading).toBe(true)

    resolveKeys([key('prod-key')])
    await load
    expect(store.keys.map(k => k.name)).toEqual(['prod-key'])
  })
})
