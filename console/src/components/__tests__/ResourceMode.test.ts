// @vitest-environment happy-dom
import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import ResourceMode from '../ResourceMode.vue'
import type { ResourceDetail } from '@/api/resources'

function detail(overrides: Partial<ResourceDetail>): ResourceDetail {
  return {
    mode: 'automatic',
    request: { value: '', source: 'unset' },
    limit: { value: '', source: 'unset' },
    live: { request: '256Mi', limit: '256Mi' },
    pending: false,
    ...overrides,
  }
}

describe('ResourceMode', () => {
  it('says the bounds and what the container runs with', () => {
    const wrapper = mount(ResourceMode, {
      props: {
        kind: 'memory',
        canWrite: true,
        detail: detail({
          mode: 'bounded',
          request: { value: '512Mi', source: 'user' },
          limit: { value: '2Gi', source: 'user' },
          live: { request: '768Mi', limit: '2Gi' },
        }),
      },
    })
    expect(wrapper.get('[data-testid="resource-mode-summary"]').text()).toBe(
      'Memory is tuned between your 512Mi and 2Gi, running with 768Mi reserved, up to 2Gi.',
    )
    expect(wrapper.find('[data-testid="resource-mode-confirm"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="resource-mode-automatic"]').exists()).toBe(true)
  })

  it('offers no actions when sizing is already automatic', () => {
    const wrapper = mount(ResourceMode, { props: { kind: 'cpu', canWrite: true, detail: detail({}) } })
    expect(wrapper.get('[data-testid="resource-mode-summary"]').text()).toBe(
      'CPU is sized automatically, running at 256Mi.',
    )
    expect(wrapper.find('button').exists()).toBe(false)
  })

  it('lets the user keep or hand back a held value', async () => {
    const wrapper = mount(ResourceMode, {
      props: {
        kind: 'memory',
        canWrite: true,
        detail: detail({ mode: 'held', request: { value: '1Gi', source: 'held' }, limit: { value: '1Gi', source: 'held' } }),
      },
    })
    await wrapper.get('[data-testid="resource-mode-confirm"]').trigger('click')
    await wrapper.get('[data-testid="resource-mode-automatic"]').trigger('click')
    expect(wrapper.emitted('confirm')).toHaveLength(1)
    expect(wrapper.emitted('automatic')).toHaveLength(1)
  })

  it('shows no actions to someone who may not change the app', () => {
    const wrapper = mount(ResourceMode, {
      props: { kind: 'memory', canWrite: false, detail: detail({ mode: 'fixed', limit: { value: '1Gi', source: 'user' } }) },
    })
    expect(wrapper.find('button').exists()).toBe(false)
  })

  // A fixed size comes from the user's own value; the other side of the pair
  // may still be the old auto-sizer's.
  it('names the value the user set when only one side is theirs', () => {
    const requestIsTheirs = mount(ResourceMode, {
      props: { kind: 'memory', detail: detail({
        mode: 'fixed', request: { value: '512Mi', source: 'user' }, limit: { value: '2Gi', source: 'automatic' },
      }) },
    })
    expect(requestIsTheirs.get('[data-testid="resource-mode-summary"]').text()).toBe('Memory is fixed at the 512Mi you set.')

    const limitIsTheirs = mount(ResourceMode, {
      props: { kind: 'memory', detail: detail({
        mode: 'fixed', request: { value: '256Mi', source: 'automatic' }, limit: { value: '1Gi', source: 'user' },
      }) },
    })
    expect(limitIsTheirs.get('[data-testid="resource-mode-summary"]').text()).toBe('Memory is fixed at the 1Gi you set.')
  })

  it('says when the container has no cap', () => {
    const wrapper = mount(ResourceMode, { props: { kind: 'cpu', detail: detail({ live: { request: '100m', limit: '' } }) } })
    expect(wrapper.get('[data-testid="resource-mode-summary"]').text()).toBe(
      'CPU is sized automatically, running with 100m reserved and no cap.',
    )
  })
})
