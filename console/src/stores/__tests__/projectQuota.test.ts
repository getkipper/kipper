import { setActivePinia, createPinia } from 'pinia'
import { describe, it, expect, beforeEach, vi } from 'vitest'
import { useProjectQuotaStore } from '../projectQuota'
import * as api from '@/api/projects'
import type { ProjectQuota } from '@/api/projects'

vi.mock('@/api/projects')

function quota(tier: string): ProjectQuota {
  return { tier, env_limit: 4, env_count: 1, tiers: {}, environments: [] } as unknown as ProjectQuota
}

describe('projectQuota store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.resetAllMocks()
  })

  it('loads quota and captures a load error', async () => {
    vi.mocked(api.fetchQuota).mockResolvedValueOnce(quota('small'))
    const store = useProjectQuotaStore()
    await store.loadQuota('shop')
    expect(store.quota?.tier).toBe('small')
    expect(store.error).toBeNull()

    vi.mocked(api.fetchQuota).mockRejectedValueOnce(new Error('nope'))
    await store.loadQuota('shop')
    expect(store.error).toBe('nope')
  })

  it('saves, reloads from the server, and returns the new value', async () => {
    vi.mocked(api.fetchQuota).mockResolvedValueOnce(quota('small')).mockResolvedValueOnce(quota('medium'))
    vi.mocked(api.updateQuota).mockResolvedValueOnce(quota('medium'))
    const store = useProjectQuotaStore()
    await store.loadQuota('shop')
    const result = await store.saveQuota('shop', { tier: 'medium' })
    expect(result.tier).toBe('medium')
    expect(store.quota?.tier).toBe('medium')
    expect(api.fetchQuota).toHaveBeenCalledTimes(2)
    expect(store.loading).toBe(false)
  })

  it('propagates a save error (e.g. the 409 force-confirm) to the caller', async () => {
    vi.mocked(api.updateQuota).mockRejectedValueOnce(new Error('conflict'))
    const store = useProjectQuotaStore()
    await expect(store.saveQuota('shop', { tier: 'large' })).rejects.toThrow('conflict')
  })

  it('drops a stale load that resolves after a newer one', async () => {
    let resolveA!: (v: ProjectQuota) => void
    vi.mocked(api.fetchQuota)
      .mockImplementationOnce(() => new Promise(res => { resolveA = res }))
      .mockResolvedValueOnce(quota('medium'))

    const store = useProjectQuotaStore()
    const slow = store.loadQuota('projectA')
    await store.loadQuota('projectB')
    expect(store.quota?.tier).toBe('medium')

    resolveA(quota('small'))
    await slow
    expect(store.quota?.tier).toBe('medium')
    expect(store.loading).toBe(false)
  })

  it('does not leave loading stuck when a save interleaves with a load', async () => {
    let resolveLoad!: (v: ProjectQuota) => void
    vi.mocked(api.fetchQuota)
      .mockResolvedValueOnce(quota('small'))
      .mockImplementationOnce(() => new Promise(res => { resolveLoad = res }))
      .mockResolvedValueOnce(quota('medium'))
    vi.mocked(api.updateQuota).mockResolvedValueOnce(quota('medium'))

    const store = useProjectQuotaStore()
    await store.loadQuota('shop')
    const slow = store.loadQuota('shop')
    await store.saveQuota('shop', { tier: 'medium' })
    expect(store.quota?.tier).toBe('medium')
    expect(store.loading).toBe(false)

    resolveLoad(quota('small'))
    await slow
    expect(store.quota?.tier).toBe('medium')
    expect(store.loading).toBe(false)
  })

  it('ignores a save that completes after a project switch', async () => {
    let resolveSave!: (v: ProjectQuota) => void
    vi.mocked(api.fetchQuota).mockResolvedValueOnce(quota('small')).mockResolvedValueOnce(quota('large'))
    vi.mocked(api.updateQuota).mockImplementationOnce(() => new Promise(res => { resolveSave = res }))

    const store = useProjectQuotaStore()
    await store.loadQuota('projectA')
    const save = store.saveQuota('projectA', { tier: 'medium' })
    await store.loadQuota('projectB')

    resolveSave(quota('medium'))
    await save
    expect(store.quota?.tier).toBe('large')
    expect(api.fetchQuota).toHaveBeenCalledTimes(2)
  })

  it('clears the previous project quota while a new scope loads', async () => {
    let resolveB!: (v: ProjectQuota) => void
    vi.mocked(api.fetchQuota)
      .mockResolvedValueOnce(quota('small'))
      .mockImplementationOnce(() => new Promise(res => { resolveB = res }))

    const store = useProjectQuotaStore()
    await store.loadQuota('projectA')
    const load = store.loadQuota('projectB')
    expect(store.quota).toBeNull()
    expect(store.loading).toBe(true)

    resolveB(quota('medium'))
    await load
    expect(store.quota?.tier).toBe('medium')
  })
})
