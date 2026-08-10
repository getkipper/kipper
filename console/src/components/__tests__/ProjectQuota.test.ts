// @vitest-environment happy-dom
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { AxiosError, AxiosHeaders } from 'axios'
import ProjectQuota from '../ProjectQuota.vue'
import * as projectsApi from '@/api/projects'
import type { ProjectQuota as ProjectQuotaResponse } from '@/api/projects'

vi.mock('@/api/projects', async importOriginal => ({
  ...(await importOriginal<typeof projectsApi>()),
  fetchQuota: vi.fn(),
  updateQuota: vi.fn(),
}))

const fetchQuota = vi.mocked(projectsApi.fetchQuota)
const updateQuota = vi.mocked(projectsApi.updateQuota)

const dims = (cpuReq: string, cpuLim: string, memReq: string, memLim: string) => ({
  cpu_request: cpuReq,
  cpu_limit: cpuLim,
  memory_request: memReq,
  memory_limit: memLim,
})

function quotaResponse(): ProjectQuotaResponse {
  return {
    tier: 'small',
    env_limit: 4,
    env_count: 2,
    tiers: {
      small: dims('2', '6', '6Gi', '12Gi'),
      medium: dims('4', '12', '12Gi', '24Gi'),
      large: dims('8', '24', '24Gi', '48Gi'),
    },
    environments: [
      {
        environment: 'test',
        namespace: 'shop-test',
        source: 'tier',
        hard: dims('2', '6', '6Gi', '12Gi'),
        used: dims('500m', '7', '3Gi', '6Gi'),
        over_quota: true,
      },
      {
        environment: 'prod',
        namespace: 'shop-prod',
        source: 'override',
        hard: dims('6', '12', '12Gi', '24Gi'),
        over_quota: false,
      },
    ],
  }
}

function mountPanel(canManage = false) {
  // A fresh Pinia per mount so the quota store never leaks state between tests.
  return mount(ProjectQuota, {
    props: { project: 'shop', canManage },
    global: { plugins: [createPinia()] },
  })
}

beforeEach(() => {
  vi.clearAllMocks()
  fetchQuota.mockResolvedValue(quotaResponse())
})

describe('ProjectQuota', () => {
  it('renders per-environment caps, usage, and badges', async () => {
    const wrapper = mountPanel()
    await flushPromises()

    expect(wrapper.text()).toContain('Resource quota')
    expect(wrapper.text()).toContain('500m / 2')
    expect(wrapper.text()).toContain('3Gi / 6Gi')
    expect(wrapper.text()).toContain('Over quota')
    expect(wrapper.text()).toContain('Override')
    // A read-only user gets a tier badge instead of a selector.
    expect(wrapper.find('select').exists()).toBe(false)
    expect(wrapper.text()).toContain('small')
  })

  it('sizes usage bars from parsed quantities and colours over-quota red', async () => {
    const wrapper = mountPanel()
    await flushPromises()

    const bars = wrapper.findAll('.h-full.rounded-full')
    // test env has all four dims with usage; prod has none.
    expect(bars).toHaveLength(4)
    // 500m of 2 CPU = 25%.
    expect(bars[0].attributes('style')).toContain('width: 25%')
    // limits.cpu 7 of 6 is over: clamped to 100% and red.
    expect(bars[1].attributes('style')).toContain('width: 100%')
    expect(bars[1].classes()).toContain('bg-red-500')
    // 3Gi of 6Gi = 50%.
    expect(bars[2].attributes('style')).toContain('width: 50%')
  })

  it('lets an admin change the tier', async () => {
    updateQuota.mockResolvedValue(quotaResponse())
    const wrapper = mountPanel(true)
    await flushPromises()

    await wrapper.get('select').setValue('medium')
    await flushPromises()

    expect(updateQuota).toHaveBeenCalledWith('shop', { tier: 'medium' })
  })

  it('surfaces below-usage warnings from a 409 and retries with force', async () => {
    const conflict = new AxiosError('conflict', '409', undefined, undefined, {
      status: 409,
      statusText: 'Conflict',
      headers: new AxiosHeaders(),
      config: { headers: new AxiosHeaders() },
      data: {
        warnings: [
          { environment: 'test', dimension: 'requests.cpu', used: '3', new_cap: '2' },
        ],
      },
    })
    updateQuota.mockRejectedValueOnce(conflict).mockResolvedValueOnce(quotaResponse())

    const wrapper = mountPanel(true)
    await flushPromises()

    await wrapper.get('select').setValue('medium')
    await flushPromises()

    expect(wrapper.text()).toContain('requests.cpu uses 3, new cap 2')

    const apply = wrapper.findAll('button').find(b => b.text() === 'Apply anyway')
    expect(apply).toBeDefined()
    await apply!.trigger('click')
    await flushPromises()

    expect(updateQuota).toHaveBeenLastCalledWith('shop', { tier: 'medium', force: true })
    expect(wrapper.text()).not.toContain('Apply anyway')
  })

  it('renders the tierless state and lets an admin clear the tier', async () => {
    const tierless = quotaResponse()
    tierless.tier = ''
    tierless.env_limit = 6
    tierless.environments = [
      {
        environment: 'test',
        namespace: 'shop-test',
        source: 'none',
        hard: dims('', '', '', ''),
        over_quota: false,
      },
    ]
    fetchQuota.mockResolvedValue(tierless)
    updateQuota.mockResolvedValue(tierless)

    const wrapper = mountPanel(true)
    await flushPromises()

    expect(wrapper.text()).toContain('No caps set')
    const select = wrapper.get('select')
    expect((select.element as HTMLSelectElement).value).toBe('')

    // Assigning and clearing a tier round-trips through the API with the
    // pointer semantics the handler expects (tier "" clears).
    fetchQuota.mockResolvedValue(quotaResponse())
    updateQuota.mockResolvedValue(quotaResponse())
    await select.setValue('small')
    await flushPromises()
    expect(updateQuota).toHaveBeenCalledWith('shop', { tier: 'small' })

    expect((select.element as HTMLSelectElement).value).toBe('small')

    fetchQuota.mockResolvedValue(tierless)
    updateQuota.mockResolvedValue(tierless)
    await wrapper.get('select').setValue('')
    await flushPromises()
    expect(updateQuota).toHaveBeenCalledTimes(2)
    expect(updateQuota).toHaveBeenLastCalledWith('shop', { tier: '' })
  })

  it('snaps the tier selector back to the current tier when a change is refused', async () => {
    const conflict = new AxiosError('conflict', '409', undefined, undefined, {
      status: 409,
      statusText: 'Conflict',
      headers: new AxiosHeaders(),
      config: { headers: new AxiosHeaders() },
      data: { warnings: [{ environment: 'test', dimension: 'requests.cpu', used: '3', new_cap: '2' }] },
    })
    updateQuota.mockRejectedValueOnce(conflict)

    const wrapper = mountPanel(true)
    await flushPromises()

    const select = wrapper.get('select')
    await select.setValue('large')
    await flushPromises()

    // The change was refused, so the control must show the tier still in effect,
    // not the one the user picked.
    expect((select.element as HTMLSelectElement).value).toBe('small')
  })
})
