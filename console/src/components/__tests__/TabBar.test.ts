// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import TabBar from '../TabBar.vue'

// happy-dom lacks ResizeObserver. The component falls back to window resize
// when it is absent, so we exercise the same path with no extra stub.
beforeEach(() => {
  // @ts-expect-error force-clear so component takes the window-resize path
  globalThis.ResizeObserver = undefined
})

afterEach(() => {
  vi.restoreAllMocks()
})

const tabs = [
  { key: 'logs', label: 'Logs' },
  { key: 'deploys', label: 'Deploys' },
  { key: 'scale', label: 'Scale' },
  { key: 'env', label: 'Env' },
]

describe('TabBar', () => {
  it('renders all tabs and marks the active one', () => {
    const wrapper = mount(TabBar, {
      props: { tabs, modelValue: 'deploys' },
    })
    const buttons = wrapper.findAll('button[role="tab"]')
    expect(buttons.length).toBe(tabs.length)
    const active = buttons.find((b) => b.attributes('aria-selected') === 'true')
    expect(active?.text()).toBe('Deploys')
  })

  it('emits update:modelValue when a tab is clicked', async () => {
    const wrapper = mount(TabBar, {
      props: { tabs, modelValue: 'logs' },
    })
    await wrapper.findAll('button[role="tab"]')[2].trigger('click')
    expect(wrapper.emitted('update:modelValue')?.[0]).toEqual(['scale'])
  })

  it('hides the overflow trigger when nothing overflows', () => {
    const wrapper = mount(TabBar, {
      props: { tabs, modelValue: 'logs' },
    })
    expect(wrapper.find('[data-testid="tabbar-overflow"]').exists()).toBe(false)
  })

  it('shows the overflow trigger when tabs do not fit', async () => {
    // Force a tight container and wide tab buttons so overflow kicks in.
    const wrapper = mount(TabBar, {
      props: { tabs, modelValue: 'logs' },
      attachTo: document.body,
    })
    const root = wrapper.element as HTMLElement
    Object.defineProperty(root, 'clientWidth', { configurable: true, value: 200 })
    wrapper.findAll('button[role="tab"]').forEach((b) => {
      Object.defineProperty(b.element, 'offsetWidth', { configurable: true, value: 80 })
    })

    window.dispatchEvent(new Event('resize'))
    await flushPromises()
    await flushPromises()

    expect(wrapper.find('[data-testid="tabbar-overflow"]').exists()).toBe(true)
    // Some tabs should be hidden from the inline row.
    const hidden = wrapper.findAll('button[role="tab"]').filter((b) => b.classes().includes('hidden'))
    expect(hidden.length).toBeGreaterThan(0)
    wrapper.unmount()
  })

  it('opens and closes the overflow popover and emits selection', async () => {
    const wrapper = mount(TabBar, {
      props: { tabs, modelValue: 'logs' },
      attachTo: document.body,
    })
    const root = wrapper.element as HTMLElement
    Object.defineProperty(root, 'clientWidth', { configurable: true, value: 150 })
    wrapper.findAll('button[role="tab"]').forEach((b) => {
      Object.defineProperty(b.element, 'offsetWidth', { configurable: true, value: 80 })
    })
    window.dispatchEvent(new Event('resize'))
    await flushPromises()
    await flushPromises()

    const trigger = wrapper.find('[data-testid="tabbar-overflow"] button')
    expect(trigger.exists()).toBe(true)
    await trigger.trigger('click')
    const menu = wrapper.find('[data-testid="tabbar-overflow-menu"]')
    expect(menu.exists()).toBe(true)

    // Click a hidden tab from the popover.
    const menuItems = menu.findAll('button[role="menuitem"]')
    expect(menuItems.length).toBeGreaterThan(0)
    await menuItems[menuItems.length - 1].trigger('click')
    const emitted = wrapper.emitted('update:modelValue')
    expect(emitted).toBeTruthy()
    expect(emitted![0][0]).toBe(menuItems[menuItems.length - 1].text().toLowerCase())
    // Popover closes after selection.
    expect(wrapper.find('[data-testid="tabbar-overflow-menu"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('highlights the overflow trigger when the active tab is hidden', async () => {
    const wrapper = mount(TabBar, {
      props: { tabs, modelValue: 'env' },
      attachTo: document.body,
    })
    const root = wrapper.element as HTMLElement
    Object.defineProperty(root, 'clientWidth', { configurable: true, value: 150 })
    wrapper.findAll('button[role="tab"]').forEach((b) => {
      Object.defineProperty(b.element, 'offsetWidth', { configurable: true, value: 80 })
    })
    window.dispatchEvent(new Event('resize'))
    await flushPromises()
    await flushPromises()

    const trigger = wrapper.find('[data-testid="tabbar-overflow"] button')
    expect(trigger.exists()).toBe(true)
    // The trigger should be flagged as carrying the active item.
    expect(trigger.classes().some((c) => c.includes('border-kipper-500'))).toBe(true)
    wrapper.unmount()
  })

  it('closes the popover on Escape', async () => {
    const wrapper = mount(TabBar, {
      props: { tabs, modelValue: 'logs' },
      attachTo: document.body,
    })
    const root = wrapper.element as HTMLElement
    Object.defineProperty(root, 'clientWidth', { configurable: true, value: 150 })
    wrapper.findAll('button[role="tab"]').forEach((b) => {
      Object.defineProperty(b.element, 'offsetWidth', { configurable: true, value: 80 })
    })
    window.dispatchEvent(new Event('resize'))
    await flushPromises()
    await flushPromises()

    await wrapper.find('[data-testid="tabbar-overflow"] button').trigger('click')
    expect(wrapper.find('[data-testid="tabbar-overflow-menu"]').exists()).toBe(true)

    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
    await flushPromises()
    expect(wrapper.find('[data-testid="tabbar-overflow-menu"]').exists()).toBe(false)
    wrapper.unmount()
  })
})
