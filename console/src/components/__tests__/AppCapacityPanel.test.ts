// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'

import AppCapacityPanel from '../AppCapacityPanel.vue'
import * as appsApi from '@/api/apps'
import type { AutoscaleConfig } from '@/api/apps'
import type { App } from '@/api/types'
import { useToast } from '@/composables/useToast'

vi.mock('@/api/apps')

// A console-api that reports the capacity state.
const fixed: AutoscaleConfig = {
  enabled: false, min_replicas: 2, max_replicas: 6, cpu_target: 0, memory_target: 0,
  current_replicas: 0, current_cpu: '', current_memory: '',
  capacity_api: 1, replicas: 3, deployment_replicas: 3, running_replicas: 3, ready_replicas: 3,
  conditions: null, last_scale_time: null, stopped: false, quota_blocked: false, autoscaling_ready: null,
  activity: [], activity_partial: true,
}

const tracking: AutoscaleConfig = {
  ...fixed, enabled: true, cpu_target: 70, current_replicas: 4, current_cpu: '82%',
  replicas: 3, deployment_replicas: 4, running_replicas: 5, ready_replicas: 4,
  conditions: [{ type: 'ScalingActive', status: 'True', reason: 'ValidMetricFound', message: 'ok' }],
}

const noBlock: AutoscaleConfig = { ...fixed, min_replicas: 0, max_replicas: 0 }

// A console-api older than the capacity panel: no capacity_api and no state.
const olderOff: AutoscaleConfig = {
  enabled: false, min_replicas: 2, max_replicas: 6, cpu_target: 0, memory_target: 0,
  current_replicas: 0, current_cpu: '', current_memory: '',
}
const olderOn: AutoscaleConfig = { ...olderOff, enabled: true, cpu_target: 70, current_replicas: 4 }

const listed: App = { name: 'checkout', status: 'running', image: 'checkout:1', replicas: 2, desired_replicas: 3, ready: 2 }

let wrapper: VueWrapper

beforeEach(() => {
  vi.clearAllMocks()
  useToast().toasts.value.splice(0)
  vi.mocked(appsApi.setAutoscale).mockResolvedValue({ status: 'enabled' })
  vi.mocked(appsApi.disableAutoscale).mockResolvedValue({ status: 'disabled', replicas: 4 })
  vi.mocked(appsApi.scaleApp).mockResolvedValue()
  vi.mocked(appsApi.fetchApps).mockResolvedValue([listed])
})

afterEach(() => {
  wrapper?.unmount()
  vi.useRealTimers()
})

async function open(config: AutoscaleConfig, canWrite = true) {
  vi.mocked(appsApi.fetchAutoscale).mockResolvedValue(config)
  wrapper = mount(AppCapacityPanel, { props: { project: 'shop-test', appName: 'checkout', canWrite } })
  await flushPromises()
  return wrapper
}

function find(id: string) {
  return wrapper.find(`[data-testid="${id}"]`)
}

function input(id: string): HTMLInputElement {
  return find(id).element as HTMLInputElement
}

async function type(id: string, value: string | number) {
  await find(id).setValue(String(value))
}

async function click(id: string) {
  await find(id).trigger('click')
  await flushPromises()
}

function toasts(): string[] {
  return useToast().toasts.value.map(t => t.message)
}

describe('AppCapacityPanel', () => {
  it('shows desired, minimum and maximum with the running and ready pods beside them', async () => {
    await open({ ...fixed, running_replicas: 4, ready_replicas: 2 })

    expect(input('capacity-desired').value).toBe('3')
    expect(input('capacity-min').value).toBe('2')
    expect(input('capacity-max').value).toBe('6')
    expect(find('capacity-current').text()).toContain('4 running')
    expect(find('capacity-current').text()).toContain('2 ready')
    expect((find('capacity-policy').element as HTMLSelectElement).value).toBe('none')
    expect(wrapper.text()).toContain("replicas (pods) on this cluster's existing nodes")
  })

  it('shows the desired count read-only as set by autoscaling while tracking is on', async () => {
    await open(tracking)

    expect(find('capacity-desired').exists()).toBe(false)
    expect(find('capacity-desired-auto').text()).toBe('4 (set by autoscaling)')
    expect(input('capacity-cpu').value).toBe('70')
    expect(input('capacity-memory').value).toBe('0')
    expect(wrapper.text()).toContain('Current: 82%')
    expect(input('capacity-min').disabled).toBe(false)
    expect(input('capacity-max').disabled).toBe(false)
  })

  it('leaves the bounds empty with placeholders when no block exists', async () => {
    await open(noBlock)

    expect(input('capacity-min').value).toBe('')
    expect(input('capacity-max').value).toBe('')
    expect(input('capacity-min').placeholder).toBe('None')
    expect(input('capacity-max').placeholder).toBe('None')
  })

  it('enables Save and Cancel only once something changed, and Cancel puts the loaded values back', async () => {
    await open(fixed)
    expect(input('capacity-save').disabled).toBe(true)
    expect(input('capacity-cancel').disabled).toBe(true)

    await type('capacity-desired', 5)
    expect(input('capacity-save').disabled).toBe(false)
    await click('capacity-cancel')

    expect(input('capacity-desired').value).toBe('3')
    expect(input('capacity-save').disabled).toBe(true)
  })

  it('saves a desired change alone through /scale', async () => {
    await open(fixed)

    await type('capacity-desired', 5)
    await click('capacity-save')

    expect(appsApi.scaleApp).toHaveBeenCalledWith('shop-test', 'checkout', 5)
    expect(appsApi.setAutoscale).not.toHaveBeenCalled()
    expect(wrapper.emitted('saved')).toHaveLength(1)
  })

  it('offers to raise the maximum when desired goes above it, and saves both in one PUT', async () => {
    await open(fixed)

    await type('capacity-desired', 8)
    expect(find('capacity-error').text()).toContain('above the maximum of 6')
    expect(input('capacity-save').disabled).toBe(true)
    await click('capacity-raise-max')

    expect(input('capacity-max').value).toBe('8')
    expect(find('capacity-error').exists()).toBe(false)
    await click('capacity-save')
    expect(appsApi.setAutoscale).toHaveBeenCalledWith('shop-test', 'checkout', {
      enabled: false, replicas: 8, min_replicas: 2, max_replicas: 8, cpu_target: 0, memory_target: 0,
    })
    expect(appsApi.scaleApp).not.toHaveBeenCalled()
  })

  it('needs at least one target before target tracking can be saved', async () => {
    await open(fixed)

    await find('capacity-policy').setValue('tracking')
    await type('capacity-cpu', 0)
    expect(find('capacity-error').text()).toContain('Set a CPU or memory target')
    expect(input('capacity-save').disabled).toBe(true)
    await type('capacity-memory', 80)

    await click('capacity-save')
    expect(appsApi.setAutoscale).toHaveBeenCalledWith('shop-test', 'checkout', {
      min_replicas: 2, max_replicas: 6, cpu_target: 0, memory_target: 80,
    })
  })

  it('starts a first policy at 1 to 5 replicas and CPU 70, as kip app autoscale does', async () => {
    await open(noBlock)

    await find('capacity-policy').setValue('tracking')
    expect(input('capacity-min').value).toBe('1')
    expect(input('capacity-max').value).toBe('5')
    expect(input('capacity-cpu').value).toBe('70')
    await click('capacity-save')

    expect(appsApi.setAutoscale).toHaveBeenCalledWith('shop-test', 'checkout', {
      min_replicas: 1, max_replicas: 5, cpu_target: 70, memory_target: 0,
    })
  })

  it('keeps stored bounds and targets when tracking is switched on again', async () => {
    await open({ ...fixed, memory_target: 60 })

    await find('capacity-policy').setValue('tracking')

    expect(input('capacity-min').value).toBe('2')
    expect(input('capacity-max').value).toBe('6')
    expect(input('capacity-cpu').value).toBe('0')
    expect(input('capacity-memory').value).toBe('60')
  })

  it('loads a memory-only policy with CPU at 0 and takes targets above 100', async () => {
    await open({ ...tracking, cpu_target: 0, memory_target: 150 })

    expect(input('capacity-cpu').value).toBe('0')
    expect(input('capacity-memory').value).toBe('150')
    expect(input('capacity-cpu').hasAttribute('max')).toBe(false)
    expect(input('capacity-memory').hasAttribute('max')).toBe(false)
  })

  it('switches tracking off in one PUT and leaves the running count to the server', async () => {
    await open(tracking)
    vi.mocked(appsApi.setAutoscale).mockResolvedValue({ status: 'disabled', replicas: 6, replicas_moved: { from: 9, to: 6 } })

    await find('capacity-policy').setValue('none')
    expect(input('capacity-desired').value).toBe('4')
    vi.mocked(appsApi.fetchAutoscale).mockResolvedValue({ ...fixed, replicas: 3 })
    await click('capacity-save')

    expect(appsApi.setAutoscale).toHaveBeenCalledWith('shop-test', 'checkout', {
      enabled: false, min_replicas: 2, max_replicas: 6, cpu_target: 70, memory_target: 0,
    })
    expect(appsApi.disableAutoscale).not.toHaveBeenCalled()
    expect(toasts()).toContain('Autoscaling switched off; checkout keeps 6 replicas')
    expect(toasts()).toContain('Desired count moved from 9 to 6 to stay within the bounds')
    // The reload can be read before the write reaches it, so the saved count wins.
    expect(input('capacity-desired').value).toBe('6')
  })

  it('switches tracking off for a stopped app, keeping the stored count it starts with', async () => {
    await open({ ...tracking, stopped: true, replicas: 3, deployment_replicas: 0, running_replicas: 0, ready_replicas: 0, current_replicas: 0 })
    expect(find('capacity-desired-auto').text()).toBe('0 (set by autoscaling)')

    await find('capacity-policy').setValue('none')
    expect(input('capacity-desired').value).toBe('3')
    expect(find('capacity-error').exists()).toBe(false)
    await click('capacity-save')

    expect(appsApi.setAutoscale).toHaveBeenCalledWith('shop-test', 'checkout', {
      enabled: false, min_replicas: 2, max_replicas: 6, cpu_target: 70, memory_target: 0,
    })
  })

  it('switches tracking off at a live count of 0, saying the server moves it to the minimum', async () => {
    await open({ ...tracking, deployment_replicas: 0, running_replicas: 0, ready_replicas: 0, current_replicas: 0 })
    vi.mocked(appsApi.setAutoscale).mockResolvedValue({ status: 'disabled', replicas: 2, replicas_moved: { from: 0, to: 2 } })

    await find('capacity-policy').setValue('none')
    expect(input('capacity-desired').value).toBe('0')
    expect(find('capacity-error').exists()).toBe(false)
    expect(find('capacity-bounds-move').text()).toBe('Saving moves the desired count from 0 to 2 to stay within the bounds.')
    await click('capacity-save')

    expect(appsApi.setAutoscale).toHaveBeenCalledWith('shop-test', 'checkout', {
      enabled: false, min_replicas: 2, max_replicas: 6, cpu_target: 70, memory_target: 0,
    })
    expect(toasts()).toContain('Desired count moved from 0 to 2 to stay within the bounds')
  })

  it('switches tracking off with a live count outside newly lowered bounds, leaving the count to the server', async () => {
    await open(tracking)

    await find('capacity-policy').setValue('none')
    await type('capacity-max', 3)
    expect(input('capacity-desired').value).toBe('4')
    expect(find('capacity-error').exists()).toBe(false)
    expect(find('capacity-bounds-move').text()).toBe('Saving moves the desired count from 4 to 3 to stay within the bounds.')
    await click('capacity-save')

    expect(appsApi.setAutoscale).toHaveBeenCalledWith('shop-test', 'checkout', {
      enabled: false, min_replicas: 2, max_replicas: 3, cpu_target: 70, memory_target: 0,
    })
  })

  it('still refuses an edited desired count outside the bounds when switching tracking off', async () => {
    await open({ ...tracking, deployment_replicas: 0, running_replicas: 0, ready_replicas: 0, current_replicas: 0 })

    await find('capacity-policy').setValue('none')
    await type('capacity-desired', 9)
    expect(find('capacity-error').text()).toContain('above the maximum of 6')
    expect(find('capacity-bounds-move').exists()).toBe(false)
    expect(find('capacity-save').attributes('disabled')).toBeDefined()
  })

  it('shows the desired count as unknown when the Deployment could not be read, whatever the autoscaler reports', async () => {
    await open({ ...tracking, deployment_replicas: null, running_replicas: null, ready_replicas: null, current_replicas: 4 })

    expect(find('capacity-desired-auto').text()).toBe('unknown (set by autoscaling)')
  })

  it('switches tracking off with an unknown count, leaving the count to the server', async () => {
    await open({ ...tracking, deployment_replicas: null, running_replicas: null, ready_replicas: null, current_replicas: 4 })

    await find('capacity-policy').setValue('none')
    expect(input('capacity-desired').value).toBe('')
    expect(input('capacity-desired').placeholder).toBe('Unknown')
    expect(find('capacity-error').exists()).toBe(false)
    await click('capacity-save')

    expect(appsApi.setAutoscale).toHaveBeenCalledWith('shop-test', 'checkout', {
      enabled: false, min_replicas: 2, max_replicas: 6, cpu_target: 70, memory_target: 0,
    })
  })

  it('removes the bounds when Min and Max are cleared with policy None', async () => {
    await open(fixed)

    await type('capacity-min', '')
    await type('capacity-max', '')
    await click('capacity-save')

    expect(appsApi.setAutoscale).toHaveBeenCalledWith('shop-test', 'checkout', { enabled: false, cpu_target: 0, memory_target: 0 })
  })

  it('reports a bounds-only save with policy None as saved capacity, keeping the count', async () => {
    await open(fixed)
    vi.mocked(appsApi.setAutoscale).mockResolvedValue({ status: 'disabled', replicas: 3 })

    await type('capacity-max', 8)
    await click('capacity-save')

    expect(toasts()).toContain('Capacity saved; checkout keeps 3 replicas')
    expect(toasts().some(m => m.includes('switched off'))).toBe(false)
  })

  it('says why the API refused a save', async () => {
    await open(fixed)
    vi.mocked(appsApi.scaleApp).mockRejectedValue({
      response: { status: 400, data: { error: 'replicas (5) must be between 2 and 4, the minimum and maximum' } },
    })

    await type('capacity-desired', 5)
    await click('capacity-save')

    expect(toasts().some(m => m.includes('must be between 2 and 4'))).toBe(true)
    expect(wrapper.emitted('saved')).toBeUndefined()
  })

  it('shows badges from the autoscaler state and points at the CPU request at the maximum', async () => {
    await open({
      ...tracking,
      quota_blocked: true,
      conditions: [{ type: 'ScalingLimited', status: 'True', reason: 'TooManyReplicas', message: 'the desired count is more than the maximum' }],
    })

    expect(find('capacity-badge-at-max').text()).toBe('At maximum')
    expect(find('capacity-badge-at-max').attributes('title')).toContain('more than the maximum')
    expect(find('capacity-badge-quota').text()).toBe('Quota blocks new pods')
    expect(find('capacity-at-max-hint').text()).toContain('CPU request')
  })

  it('says a change to a stopped app applies on start', async () => {
    await open({ ...fixed, stopped: true, running_replicas: 0, ready_replicas: 0 })

    expect(find('capacity-badge-stopped').text()).toBe('Stopped, applies on start')
  })

  it('reports an invalid stored policy', async () => {
    await open({ ...tracking, autoscaling_ready: { type: 'AutoscalingReady', status: 'False', reason: 'InvalidPolicy', message: 'minReplicas (7) must not exceed maxReplicas (6)' } })

    expect(find('capacity-badge-policy-invalid').attributes('title')).toContain('must not exceed')
  })

  it('shows a failed autoscaler write with the server message', async () => {
    await open({ ...tracking, autoscaling_ready: { type: 'AutoscalingReady', status: 'False', reason: 'AutoscalerReconcileFailed', message: 'the autoscaler could not be written: forbidden' } })

    expect(find('capacity-badge-autoscaling-not-ready').text()).toBe('Autoscaler not applied')
    expect(find('capacity-badge-autoscaling-not-ready').attributes('title')).toContain('forbidden')
  })

  it('refuses a desired count of 0 without bounds and points at Stop app', async () => {
    await open({ ...noBlock, replicas: 2 })

    await type('capacity-desired', 0)

    expect(find('capacity-error').text()).toContain('Stop app')
    expect(input('capacity-save').disabled).toBe(true)
  })

  it('leaves a bounds move under target tracking to the server', async () => {
    await open(tracking)

    await type('capacity-max', 3)
    expect(find('capacity-bounds-move').exists()).toBe(false)
    await click('capacity-save')

    expect(toasts().some(m => m.includes('Desired count moved'))).toBe(false)
  })

  describe('while the panel is open', () => {
    const later: AutoscaleConfig = {
      ...fixed, replicas: 4, running_replicas: 5, ready_replicas: 4, quota_blocked: true,
      activity: [{ time: '2026-10-04T09:30:00Z', source: 'hpa_event', type: 'Normal', reason: 'SuccessfulRescale', message: 'New size: 4' }],
    }

    function expectLaterObservations() {
      expect(find('capacity-current').text()).toContain('5 running')
      expect(find('capacity-current').text()).toContain('4 ready')
      expect(find('capacity-badge-quota').exists()).toBe(true)
      expect(find('capacity-activity').text()).toContain('New size: 4')
    }

    it('refreshes the observations on an interval and keeps unsaved edits', async () => {
      vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
      await open(fixed)
      await type('capacity-desired', 5)
      await type('capacity-max', 8)

      vi.mocked(appsApi.fetchAutoscale).mockResolvedValue(later)
      vi.advanceTimersByTime(15000)
      await flushPromises()

      expectLaterObservations()
      expect(input('capacity-desired').value).toBe('5')
      expect(input('capacity-max').value).toBe('8')
      expect(input('capacity-save').disabled).toBe(false)
    })

    it('follows the stored values on a refresh when nothing was edited', async () => {
      vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
      await open(fixed)

      vi.mocked(appsApi.fetchAutoscale).mockResolvedValue(later)
      vi.advanceTimersByTime(15000)
      await flushPromises()

      expect(input('capacity-desired').value).toBe('4')
      expect(input('capacity-save').disabled).toBe(true)
    })

    it('refreshes on request and keeps unsaved edits', async () => {
      await open(fixed)
      await type('capacity-desired', 5)

      vi.mocked(appsApi.fetchAutoscale).mockResolvedValue(later)
      await click('capacity-refresh')

      expectLaterObservations()
      expect(input('capacity-desired').value).toBe('5')
    })

    it('keeps the form and the last values when a refresh fails, and says so', async () => {
      await open(fixed)
      await type('capacity-desired', 5)

      vi.mocked(appsApi.fetchAutoscale).mockRejectedValue(new Error('down'))
      await click('capacity-refresh')

      expect(find('capacity-read-error').exists()).toBe(false)
      expect(find('capacity-refresh-failed').exists()).toBe(true)
      expect(input('capacity-desired').value).toBe('5')

      vi.mocked(appsApi.fetchAutoscale).mockResolvedValue(later)
      await click('capacity-refresh')
      expect(find('capacity-refresh-failed').exists()).toBe(false)
    })

    it('stops refreshing once the panel closes', async () => {
      vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
      await open(fixed)
      wrapper.unmount()
      vi.mocked(appsApi.fetchAutoscale).mockClear()

      vi.advanceTimersByTime(60000)
      await flushPromises()

      expect(appsApi.fetchAutoscale).not.toHaveBeenCalled()
    })
  })

  it('lists recent activity, labelled as possibly incomplete', async () => {
    await open({
      ...tracking,
      last_scale_time: '2026-10-04T09:30:00Z',
      activity: [{ time: '2026-10-04T09:30:00Z', source: 'hpa_event', type: 'Normal', reason: 'SuccessfulRescale', message: 'New size: 4; reason: cpu resource utilization above target' }],
    })

    expect(find('capacity-activity').text()).toContain('New size: 4')
    expect(wrapper.text()).toContain('recent and possibly incomplete')
  })

  it('shows the startup metric gaps as one plain line and keeps the raw messages as its title', async () => {
    const raw = 'failed to get cpu utilization: did not receive metrics for targeted pods (pods might be unready)'
    await open({
      ...tracking,
      activity: [
        { time: '2026-10-04T09:32:00Z', source: 'hpa_event', type: 'Normal', reason: 'SuccessfulRescale', message: 'New size: 2' },
        { time: '2026-10-04T09:31:00Z', source: 'hpa_event', type: 'Warning', reason: 'FailedGetResourceMetric', message: raw },
        { time: '2026-10-04T09:30:00Z', source: 'hpa_event', type: 'Warning', reason: 'FailedGetResourceMetric', message: raw },
      ],
    })

    const items = find('capacity-activity').findAll('li')
    expect(items.map(i => i.text())).toEqual([
      expect.stringContaining('New size: 2'),
      expect.stringContaining('Metrics for new pods are not available yet; the autoscaler waits for them'),
    ])
    expect(find('capacity-activity').text()).not.toContain('did not receive metrics')
    expect(items[1].find('[title]').attributes('title')).toBe(raw)
  })

  it('tells an empty history apart from one that could not be read', async () => {
    await open({ ...fixed, activity: [] })
    expect(find('capacity-activity').text()).toContain('No recent scaling activity')

    await open({ ...fixed, activity: null })
    expect(find('capacity-activity').text()).toContain('could not be read')
  })

  it('shows the values without write controls for a reader', async () => {
    await open(tracking, false)

    expect(find('capacity-save').exists()).toBe(false)
    expect(find('capacity-cancel').exists()).toBe(false)
    expect(input('capacity-min').disabled).toBe(true)
    expect(input('capacity-max').disabled).toBe(true)
    expect((find('capacity-policy').element as HTMLSelectElement).disabled).toBe(true)
  })

  it('says when the capacity settings could not be read and offers no save', async () => {
    vi.mocked(appsApi.fetchAutoscale).mockRejectedValue(new Error('down'))
    wrapper = mount(AppCapacityPanel, { props: { project: 'shop-test', appName: 'checkout', canWrite: true } })
    await flushPromises()

    expect(find('capacity-read-error').exists()).toBe(true)
    expect(find('capacity-save').exists()).toBe(false)
  })

  describe('against an older console-api', () => {
    it('reads the desired count and pods from the app list', async () => {
      await open(olderOff)

      expect(input('capacity-desired').value).toBe('3')
      expect(find('capacity-current').text()).toContain('2 running')
      expect(find('capacity-activity').exists()).toBe(false)
    })

    it("shows the autoscaler's count while tracking is on", async () => {
      await open(olderOn)

      expect(find('capacity-desired-auto').text()).toBe('4 (set by autoscaling)')
    })

    it('switches tracking off with DELETE and then sets desired with /scale, saying the save is not atomic', async () => {
      await open(olderOn)

      await find('capacity-policy').setValue('none')
      await type('capacity-desired', 5)
      expect(find('capacity-not-atomic').exists()).toBe(true)
      await click('capacity-save')

      expect(appsApi.disableAutoscale).toHaveBeenCalledWith('shop-test', 'checkout')
      expect(appsApi.scaleApp).toHaveBeenCalledWith('shop-test', 'checkout', 5)
      expect(vi.mocked(appsApi.disableAutoscale).mock.invocationCallOrder[0])
        .toBeLessThan(vi.mocked(appsApi.scaleApp).mock.invocationCallOrder[0])
      expect(appsApi.setAutoscale).not.toHaveBeenCalled()
    })

    it('says what was saved when the second request fails', async () => {
      await open(olderOn)
      vi.mocked(appsApi.scaleApp).mockRejectedValue({ response: { status: 500, data: { error: 'boom' } } })

      await find('capacity-policy').setValue('none')
      await type('capacity-desired', 5)
      await click('capacity-save')

      expect(toasts().some(m => m.includes('Autoscaling was switched off') && m.includes('boom'))).toBe(true)
    })

    it('cannot store bounds without a policy, and says so', async () => {
      await open(olderOff)

      await type('capacity-max', 8)

      expect(find('capacity-error').text()).toContain('older console-api')
      expect(input('capacity-save').disabled).toBe(true)
    })
  })
})
