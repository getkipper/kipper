// @vitest-environment happy-dom
import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import ResourceControl from '../ResourceControl.vue'
import { CPU_STOPS, MEMORY_STOPS } from '@/utils/resources'

const Gi = 1024 ** 3

function mountMemory(overrides: Record<string, unknown> = {}) {
  return mount(ResourceControl, {
    props: {
      kind: 'memory',
      usage: 0.4 * Gi,
      limit: 2 * Gi,
      ...overrides,
    },
  })
}

describe('ResourceControl', () => {
  it('renders usage/limit labels formatted by kind', () => {
    const wrapper = mountMemory()
    expect(wrapper.get('[data-testid="usage-label"]').text()).toBe('410 Mi')
    expect(wrapper.get('[data-testid="limit-label"]').text()).toBe('2 Gi')
  })

  it('snaps the slider to the stop closest to the current limit', () => {
    const wrapper = mountMemory({ limit: 2 * Gi })
    const slider = wrapper.get('[data-testid="slider"]').element as HTMLInputElement
    // MEMORY_STOPS[4] === 2Gi
    expect(slider.value).toBe('4')
  })

  it('previews a new limit on the gauge denominator as the slider moves', async () => {
    const wrapper = mountMemory({ limit: 2 * Gi })
    const slider = wrapper.get('[data-testid="slider"]')
    await slider.setValue('5') // 4Gi
    expect(wrapper.get('[data-testid="limit-label"]').text()).toBe('4 Gi')
    // gauge should not yet have emitted apply
    expect(wrapper.emitted('apply')).toBeUndefined()
  })

  it('rotates the needle toward the left (green) at low usage', () => {
    const wrapper = mountMemory({ usage: 0.05 * Gi, limit: 2 * Gi })
    const needleGroup = wrapper.get('[data-testid="gauge-needle"]').element.parentElement!
    const transform = needleGroup.getAttribute('transform') || ''
    const match = transform.match(/rotate\(([-0-9.]+)/)
    const angle = match ? parseFloat(match[1]) : NaN
    // Green sits on the left now — 0% maps to -90°, low usage stays sharply negative.
    expect(angle).toBeLessThan(-60)
  })

  it('rotates the needle toward the right (red) at high usage', () => {
    const wrapper = mountMemory({ usage: 1.9 * Gi, limit: 2 * Gi })
    const needleGroup = wrapper.get('[data-testid="gauge-needle"]').element.parentElement!
    const transform = needleGroup.getAttribute('transform') || ''
    const match = transform.match(/rotate\(([-0-9.]+)/)
    const angle = match ? parseFloat(match[1]) : NaN
    // Red sits on the right — 100% maps to +90°, near-100% stays sharply positive.
    expect(angle).toBeGreaterThan(60)
  })

  it('disables apply when no change is pending', () => {
    const wrapper = mountMemory({ limit: 2 * Gi })
    const apply = wrapper.get('[data-testid="apply-button"]').element as HTMLButtonElement
    expect(apply.disabled).toBe(true)
  })

  it('emits apply with the pending limit', async () => {
    const wrapper = mountMemory({ limit: 2 * Gi })
    await wrapper.get('[data-testid="slider"]').setValue('5') // 4Gi
    await wrapper.get('[data-testid="apply-button"]').trigger('click')
    const emitted = wrapper.emitted('apply')
    expect(emitted).toBeTruthy()
    expect(emitted![0]).toEqual([4 * Gi])
  })

  it('reset snaps the slider back to the current limit', async () => {
    const wrapper = mountMemory({ limit: 2 * Gi })
    const slider = wrapper.get('[data-testid="slider"]')
    await slider.setValue('6') // 8Gi
    expect(wrapper.get('[data-testid="limit-label"]').text()).toBe('8 Gi')
    await wrapper.get('[data-testid="reset-button"]').trigger('click')
    expect(wrapper.get('[data-testid="limit-label"]').text()).toBe('2 Gi')
  })

  it('re-snaps the slider when the limit prop updates from outside', async () => {
    const wrapper = mountMemory({ limit: 2 * Gi })
    await wrapper.setProps({ limit: 4 * Gi })
    await nextTick()
    const slider = wrapper.get('[data-testid="slider"]').element as HTMLInputElement
    // MEMORY_STOPS[5] === 4Gi
    expect(slider.value).toBe('5')
  })

  it('hides the slider region when readonly', () => {
    const wrapper = mountMemory({ readonly: true })
    expect(wrapper.find('[data-testid="slider-region"]').exists()).toBe(false)
  })

  it('renders the throttling badge for CPU when above 5%', () => {
    const wrapper = mount(ResourceControl, {
      props: {
        kind: 'cpu',
        usage: 800,
        limit: 1000,
        throttlingPct: 12.5,
        label: 'CPU',
      },
    })
    const badge = wrapper.find('[data-testid="throttling-badge"]')
    expect(badge.exists()).toBe(true)
    expect(badge.text()).toBe('throttled 13%')
  })

  it('hides the throttling badge for memory even when set', () => {
    const wrapper = mountMemory({ throttlingPct: 50, label: 'Memory' })
    expect(wrapper.find('[data-testid="throttling-badge"]').exists()).toBe(false)
  })

  it('clamps stops to [min,max] when provided', () => {
    const wrapper = mountMemory({ min: 1 * Gi, max: 4 * Gi, limit: 2 * Gi })
    const slider = wrapper.get('[data-testid="slider"]').element as HTMLInputElement
    // Three stops in range: 1Gi, 2Gi, 4Gi → indices 0,1,2; 2Gi is index 1
    expect(slider.max).toBe('2')
    expect(slider.value).toBe('1')
  })

  it('honors custom stops', () => {
    const stops = [100 * 1024 ** 2, 200 * 1024 ** 2, 400 * 1024 ** 2]
    const wrapper = mountMemory({ stops, limit: 200 * 1024 ** 2 })
    const slider = wrapper.get('[data-testid="slider"]').element as HTMLInputElement
    expect(slider.max).toBe('2')
    expect(slider.value).toBe('1')
  })

  it('shows an over-limit chip when usage exceeds the limit', () => {
    const wrapper = mountMemory({ usage: 3 * Gi, limit: 2 * Gi })
    expect(wrapper.text()).toContain('over')
  })

  it('renders all default memory stops by default', () => {
    const wrapper = mountMemory()
    const text = wrapper.get('[data-testid="slider-region"]').text()
    expect(text).toContain('128 Mi')
    expect(text).toContain('16 Gi')
    expect(MEMORY_STOPS.length).toBe(8)
  })

  it('renders all default CPU stops for CPU kind', () => {
    const wrapper = mount(ResourceControl, {
      props: { kind: 'cpu', usage: 100, limit: 500 },
    })
    const text = wrapper.get('[data-testid="slider-region"]').text()
    expect(text).toContain('50m')
    expect(text).toContain('4')
    expect(CPU_STOPS.length).toBe(7)
  })

  it('preserves a non-stop limit until the user moves the slider', async () => {
    // 384Mi is not in MEMORY_STOPS. The slider's index should snap to the
    // nearest stop for display, but pendingLimit must stay at 384Mi and
    // Apply must stay disabled until the user actually moves the slider.
    const oddLimit = 384 * 1024 ** 2
    const wrapper = mountMemory({ limit: oddLimit })
    expect(wrapper.get('[data-testid="limit-label"]').text()).toBe('384 Mi')
    const apply = wrapper.get('[data-testid="apply-button"]').element as HTMLButtonElement
    expect(apply.disabled).toBe(true)
    await wrapper.get('[data-testid="apply-button"]').trigger('click')
    expect(wrapper.emitted('apply')).toBeUndefined()

    await wrapper.get('[data-testid="slider"]').setValue('4') // 2Gi
    expect(wrapper.get('[data-testid="limit-label"]').text()).toBe('2 Gi')
    await wrapper.get('[data-testid="apply-button"]').trigger('click')
    expect(wrapper.emitted('apply')![0]).toEqual([2 * Gi])
  })

  it('honors min/max bounds even when no preset stop falls inside', () => {
    const wrapper = mountMemory({
      min: 300 * 1024 ** 2,
      max: 400 * 1024 ** 2,
      limit: 350 * 1024 ** 2,
    })
    const slider = wrapper.get('[data-testid="slider"]').element as HTMLInputElement
    // Effective stops should be the bounds themselves [300Mi, 400Mi].
    expect(slider.max).toBe('1')
    const region = wrapper.get('[data-testid="slider-region"]').text()
    expect(region).toContain('300 Mi')
    expect(region).toContain('400 Mi')
    // Slider cannot push pendingLimit outside the bounds.
    expect(slider.min).toBe('0')
  })

  it('reset clears the pending change for non-stop limits too', async () => {
    const oddLimit = 384 * 1024 ** 2
    const wrapper = mountMemory({ limit: oddLimit })
    await wrapper.get('[data-testid="slider"]').setValue('4')
    expect(wrapper.get('[data-testid="limit-label"]').text()).toBe('2 Gi')
    await wrapper.get('[data-testid="reset-button"]').trigger('click')
    expect(wrapper.get('[data-testid="limit-label"]').text()).toBe('384 Mi')
    const apply = wrapper.get('[data-testid="apply-button"]').element as HTMLButtonElement
    expect(apply.disabled).toBe(true)
  })

  it('does not emit apply while applying is true', async () => {
    const wrapper = mountMemory({ limit: 2 * Gi, applying: true })
    await wrapper.get('[data-testid="slider"]').setValue('5')
    await wrapper.get('[data-testid="apply-button"]').trigger('click')
    expect(wrapper.emitted('apply')).toBeUndefined()
  })
})
