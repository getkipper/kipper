// @vitest-environment happy-dom
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import ProjectApiKeys from '../ProjectApiKeys.vue'
import * as apikeysApi from '@/api/apikeys'
import type { ApiKey, KeyUsageResponse, UsagePlan } from '@/api/apikeys'

vi.mock('@/api/apikeys', async importOriginal => ({
  ...(await importOriginal<typeof apikeysApi>()),
  fetchPlans: vi.fn(),
  fetchKeys: vi.fn(),
  fetchKeyUsage: vi.fn(),
}))

const fetchPlans = vi.mocked(apikeysApi.fetchPlans)
const fetchKeys = vi.mocked(apikeysApi.fetchKeys)
const fetchKeyUsage = vi.mocked(apikeysApi.fetchKeyUsage)

const plan: UsagePlan = { name: 'bronze', rate: 10, burst: 20, keys: 1 }
const partnerKey: ApiKey = {
  name: 'key-ab12cd34', display_name: 'Acme partner', plan: 'bronze',
  prefix: 'ab12cd34', enabled: true, apps: [], created: '2026-01-01',
}

function usage(days: KeyUsageResponse['days']): KeyUsageResponse {
  return { from: '2026-04-09', to: '2026-07-08', retention_days: 92, days }
}

function mountPanel() {
  return mount(ProjectApiKeys, {
    props: { project: 'shop', environments: ['prod'], canManage: true },
    global: { plugins: [createPinia()], stubs: { teleport: true } },
  })
}

beforeEach(() => {
  vi.clearAllMocks()
  fetchPlans.mockResolvedValue([plan])
  fetchKeys.mockResolvedValue([partnerKey])
})

describe('ProjectApiKeys usage summary', () => {
  it('sums allowed, rate-denied, and quota-denied and shows the last-used day', async () => {
    fetchKeyUsage.mockResolvedValue(usage([
      { day: '2026-07-08', allowed: 100, denied_rate: 2, denied_quota: 0 },
      { day: '2026-07-01', allowed: 50, denied_rate: 1, denied_quota: 5 },
    ]))
    const wrapper = mountPanel()
    await flushPromises()

    const text = wrapper.text()
    expect(text).toContain('150 ok')
    expect(text).toContain('3 rate')
    expect(text).toContain('5 quota')
    expect(text).toContain('used 2026-07-08')
  })

  it('badges a key as expired or expiring based on expires_at', async () => {
    const soon = new Date(Date.now() + 5 * 86_400_000).toISOString()
    const gone = new Date(Date.now() - 86_400_000).toISOString()
    // Expired only a few hours ago: still expired, must not read as "expires today".
    const justGone = new Date(Date.now() - 3 * 3_600_000).toISOString()
    fetchKeys.mockResolvedValue([
      { ...partnerKey, name: 'key-soon', prefix: 'soon0000', expires_at: soon },
      { ...partnerKey, name: 'key-gone', prefix: 'gone0000', expires_at: gone },
      { ...partnerKey, name: 'key-just', prefix: 'just0000', expires_at: justGone },
    ])
    fetchKeyUsage.mockResolvedValue(usage([]))
    const wrapper = mountPanel()
    await flushPromises()

    const soonRow = wrapper.findAll('li').find(li => li.text().includes('soon0000'))!
    const goneRow = wrapper.findAll('li').find(li => li.text().includes('gone0000'))!
    const justRow = wrapper.findAll('li').find(li => li.text().includes('just0000'))!
    expect(soonRow.text()).toContain(`expires ${soon.slice(0, 10)}`)
    expect(goneRow.text()).toContain(`expired ${gone.slice(0, 10)}`)
    // The expiring-soon badge is amber, the expired badge is red.
    expect(soonRow.find('.bg-amber-100').exists()).toBe(true)
    expect(goneRow.find('.bg-red-100').exists()).toBe(true)
    // A key expired hours ago is red, not amber.
    expect(justRow.find('.bg-red-100').exists()).toBe(true)
    expect(justRow.find('.bg-amber-100').exists()).toBe(false)
  })

  it('marks a key with no traffic as unused and hides zero denial counts', async () => {
    fetchKeyUsage.mockResolvedValue(usage([]))
    const wrapper = mountPanel()
    await flushPromises()

    // Scope to the key's own row so the panel's description copy (which
    // mentions "rate limit") does not trip the negative assertions.
    const row = wrapper.findAll('li').find(li => li.text().includes('ab12cd34'))
    expect(row).toBeDefined()
    const rowText = row!.text()
    expect(rowText).toContain('0 ok')
    expect(rowText).toContain('unused (90d)')
    expect(rowText).not.toContain('rate')
    expect(rowText).not.toContain('quota')
  })
})
