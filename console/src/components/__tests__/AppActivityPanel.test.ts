// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'

import AppActivityPanel from '../AppActivityPanel.vue'
import * as activityApi from '@/api/activity'
import type { AppActivity } from '@/api/activity'
import type { AutoscaleConfig } from '@/api/apps'

vi.mock('@/api/activity', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/activity')>()),
  fetchAppActivity: vi.fn(),
}))

const tracking: AutoscaleConfig = {
  enabled: true, min_replicas: 1, max_replicas: 3, cpu_target: 70, memory_target: 0,
  current_replicas: 2, current_cpu: '', current_memory: '', stopped: false, conditions: [],
}

const three = (v: number | null) => [v, v, v]

function response(over: Partial<AppActivity> = {}): AppActivity {
  return {
    available: true,
    range: '1h',
    step_seconds: 30,
    timestamps: [1000, 1030, 1060],
    traffic: 'available',
    requests_per_min: { ok: [10, 20, 30], redirect: three(0), client_error: three(0), aborted: three(0), server_error: three(0) },
    requests_peak_per_min: [12, 25, 40],
    cpu_pct_of_request: [40, 50, 60],
    cpu_peak_pct_of_request: [45, 55, 90],
    cpu_used_millis: [144, 180, 216],
    cpu_requested_millis: three(360),
    memory_pct_of_request: three(30),
    memory_used_bytes: three(100),
    memory_requested_bytes: three(300),
    replicas: [2, 2, 3],
    policy: { cpu_target_pct: three(70), memory_target_pct: three(null), min: three(1), max: three(3) },
    changes: [{
      time: 1060, from: 2, to: 3, cause: 'autoscaler', autoscaled: true,
      around: { window_seconds: 120, cpu: { peak_pct: 97, avg_pct: 41, target_pct: 70 }, memory: null, requests: null },
    }],
    changes_detailed: 1,
    degraded: [],
    ...over,
  }
}

let wrapper: VueWrapper

beforeEach(() => {
  vi.clearAllMocks()
  localStorage.clear()
})

afterEach(() => {
  wrapper?.unmount()
  vi.useRealTimers()
})

async function open(data: AppActivity, config: AutoscaleConfig | null = tracking) {
  vi.mocked(activityApi.fetchAppActivity).mockResolvedValue(data)
  wrapper = mount(AppActivityPanel, { props: { project: 'shop-test', appName: 'checkout', config } })
  await flushPromises()
  return wrapper
}

function find(id: string) {
  return wrapper.find(`[data-testid="${id}"]`)
}

describe('AppActivityPanel', () => {
  it('charts requests, usage and pods with the change marked', async () => {
    await open(response())
    expect(activityApi.fetchAppActivity).toHaveBeenCalledWith('shop-test', 'checkout', '1h')
    expect(find('chart-requests').exists()).toBe(true)
    expect(find('chart-usage').exists()).toBe(true)
    expect(find('chart-pods').exists()).toBe(true)
    expect(wrapper.findAll('[data-testid="chart-marker-solid"]')).toHaveLength(3)
    expect(find('activity-guide').text()).toContain('pods may be added above about 77% (277m of 360m)')
  })

  it('remembers the chosen range in this browser', async () => {
    await open(response())
    await find('activity-range-6h').trigger('click')
    await flushPromises()
    expect(activityApi.fetchAppActivity).toHaveBeenLastCalledWith('shop-test', 'checkout', '6h')
    wrapper.unmount()
    await open(response())
    expect(activityApi.fetchAppActivity).toHaveBeenLastCalledWith('shop-test', 'checkout', '6h')
  })

  it('falls back to 1h when the stored range is unknown', async () => {
    localStorage.setItem('kipper_activity_range', '7d')
    await open(response())
    expect(activityApi.fetchAppActivity).toHaveBeenCalledWith('shop-test', 'checkout', '1h')
  })

  it('shows a change observation for the marker under the pointer', async () => {
    await open(response())
    const svg = find('chart-pods').find('svg')
    svg.element.getBoundingClientRect = () => ({ left: 0, width: 300, top: 0, height: 64, right: 300, bottom: 64, x: 0, y: 0, toJSON: () => ({}) })
    await svg.trigger('mousemove', { clientX: 299 })
    expect(find('activity-change').text()).toContain('The autoscaler changed the desired count from 2 pods to 3 pods.')
    expect(find('activity-change').text()).toContain('CPU peaked at 97% of its request')
    expect(find('activity-readout').text()).toContain('30 requests/min (peak 40)')
  })

  it('explains a requests chart without data in one sentence', async () => {
    await open(response({ traffic: 'no_route' }))
    expect(find('chart-requests').exists()).toBe(false)
    expect(find('activity-traffic-note').text()).toContain('no route')
  })

  it('asks for a newer cluster version when the API has no activity endpoint', async () => {
    vi.mocked(activityApi.fetchAppActivity).mockRejectedValue({ response: { status: 404, data: '404 page not found\n' } })
    wrapper = mount(AppActivityPanel, { props: { project: 'shop-test', appName: 'checkout', config: tracking } })
    await flushPromises()
    expect(find('activity-unsupported').exists()).toBe(true)
    expect(find('activity-error').exists()).toBe(false)
  })

  it('treats a 404 from the handler as an error, not an old cluster', async () => {
    vi.mocked(activityApi.fetchAppActivity).mockRejectedValue({ response: { status: 404, data: { error: 'app not found' } } })
    wrapper = mount(AppActivityPanel, { props: { project: 'shop-test', appName: 'checkout', config: tracking } })
    await flushPromises()
    expect(find('activity-unsupported').exists()).toBe(false)
    expect(find('activity-error').exists()).toBe(true)
  })

  it('shows the charts once the cluster serves the activity endpoint', async () => {
    vi.useFakeTimers()
    vi.mocked(activityApi.fetchAppActivity).mockRejectedValueOnce({ response: { status: 404, data: '404 page not found\n' } }).mockResolvedValue(response())
    wrapper = mount(AppActivityPanel, { props: { project: 'shop-test', appName: 'checkout', config: tracking } })
    await flushPromises()
    expect(find('activity-unsupported').exists()).toBe(true)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(find('activity-unsupported').exists()).toBe(false)
    expect(find('chart-pods').exists()).toBe(true)
  })

  it('says so when monitoring is off', async () => {
    await open({ available: false, reason: 'Monitoring is not enabled on this cluster.', changes: [], changes_detailed: 0, degraded: [] })
    expect(find('activity-unavailable').text()).toBe('Monitoring is not enabled on this cluster.')
  })

  it('leaves out the target guide while the app is stopped', async () => {
    await open(response(), { ...tracking, stopped: true })
    expect(find('activity-guide').exists()).toBe(false)
  })

  it('opens on memory when memory is the only autoscaled metric', async () => {
    await open(response(), { ...tracking, cpu_target: 0, memory_target: 80 })
    expect(find('chart-usage').attributes('data-testid')).toBe('chart-usage')
    expect(wrapper.text()).toContain('Memory use (% of resource request)')
  })

  it('refreshes every minute while the page is visible', async () => {
    vi.useFakeTimers()
    await open(response())
    expect(activityApi.fetchAppActivity).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(activityApi.fetchAppActivity).toHaveBeenCalledTimes(2)
    Object.defineProperty(document, 'visibilityState', { value: 'hidden', configurable: true })
    await vi.advanceTimersByTimeAsync(60_000)
    expect(activityApi.fetchAppActivity).toHaveBeenCalledTimes(2)
    Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true })
  })

  it('ignores a slower response for an earlier range', async () => {
    let resolveFirst: (v: AppActivity) => void = () => {}
    vi.mocked(activityApi.fetchAppActivity)
      .mockImplementationOnce(() => new Promise(r => { resolveFirst = r }))
      .mockResolvedValueOnce(response({ range: '6h', replicas: [5, 5, 5] }))
    wrapper = mount(AppActivityPanel, { props: { project: 'shop-test', appName: 'checkout', config: tracking } })
    await find('activity-range-6h').trigger('click')
    await flushPromises()
    resolveFirst(response({ replicas: [1, 1, 1] }))
    await flushPromises()
    const svg = find('chart-pods').find('svg')
    svg.element.getBoundingClientRect = () => ({ left: 0, width: 300, top: 0, height: 64, right: 300, bottom: 64, x: 0, y: 0, toJSON: () => ({}) })
    await svg.trigger('mousemove', { clientX: 0 })
    expect(find('activity-readout').text()).toContain('5 pods')
  })

  it('keeps the last charts when a refresh fails', async () => {
    vi.useFakeTimers()
    await open(response())
    vi.mocked(activityApi.fetchAppActivity).mockRejectedValueOnce(new Error('down'))
    await vi.advanceTimersByTimeAsync(60_000)
    expect(find('activity-refresh-failed').exists()).toBe(true)
    expect(find('chart-pods').exists()).toBe(true)
  })
})
