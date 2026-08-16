// @vitest-environment happy-dom
import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import PlatformComponentCard from '../PlatformComponentCard.vue'
import type { PlatformComponent } from '@/api/platform'

vi.mock('@/composables/useResourceUsage', async () => {
  const { ref, shallowRef } = await import('vue')
  return {
    useResourceUsage: () => ({
      data: shallowRef(null),
      loading: ref(false),
      error: ref(null),
      refresh: vi.fn(),
      start: vi.fn(),
      stop: vi.fn(),
    }),
  }
})

function card(component: Partial<PlatformComponent>) {
  setActivePinia(createPinia())
  return mount(PlatformComponentCard, {
    props: { component: { name: 'grafana', enabled: true, ...component } as PlatformComponent },
    global: { stubs: { RouterLink: true, MetricSparkline: true, ResourceControl: true } },
  })
}

const toggleText = (wrapper: ReturnType<typeof card>) =>
  wrapper.findAll('button').map(b => b.text()).join(' ')

describe('PlatformComponentCard enable/disable', () => {
  it('offers the action for a component that toggles on its own', () => {
    expect(toggleText(card({ name: 'prometheus', toggleable: true }))).toContain('Disable this component')
  })

  // grafana and kube-state-metrics ride with prometheus; the API refuses to
  // toggle them, so a button that only ever returns 400 is worse than no button.
  it('hides the action for a component that shares a chart', () => {
    expect(toggleText(card({ name: 'kube-state-metrics', toggleable: false }))).not.toContain('this component')
  })

  // An API old enough to omit the field rejects the action too, so the absence
  // is treated as "no". Fail-open here would keep showing the buttons this
  // exists to remove for as long as a rollout has the two versions paired.
  it('hides the action when the API does not say', () => {
    expect(toggleText(card({ name: 'grafana' }))).not.toContain('this component')
  })
})
