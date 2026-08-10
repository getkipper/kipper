// @vitest-environment happy-dom
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { defineComponent, h, ref } from 'vue'
import { useResourceUsage } from '../useResourceUsage'
import type { UsageResponse, UsageScope } from '@/api/resources'

const stubResponse: UsageResponse = {
  metrics_available: true,
  containers: [
    {
      pod: 'prom-0',
      namespace: 'monitoring',
      name: 'prometheus',
      metrics_present: true,
      memory_bytes: 100,
      memory_limit_bytes: 200,
      memory_request_bytes: 50,
      cpu_millis: 10,
      cpu_limit_millis: 100,
      cpu_request_millis: 50,
    },
  ],
  totals: {
    memory_bytes: 100,
    memory_limit_bytes: 200,
    memory_request_bytes: 50,
    cpu_millis: 10,
    cpu_limit_millis: 100,
    cpu_request_millis: 50,
    pod_count: 1,
    container_count: 1,
    containers_with_metrics: 1,
  },
}

const fetchMock = vi.fn<(scope: UsageScope) => Promise<UsageResponse>>()

vi.mock('@/api/resources', () => ({
  fetchResourceUsage: (scope: UsageScope) => fetchMock(scope),
}))

function mountHarness(scopeRef: ReturnType<typeof ref<UsageScope | null>>, opts?: { pollMs?: number }) {
  const harness = defineComponent({
    setup() {
      const usage = useResourceUsage(scopeRef, opts)
      return { usage }
    },
    render() {
      return h('div')
    },
  })
  return mount(harness)
}

describe('useResourceUsage', () => {
  beforeEach(() => {
    fetchMock.mockReset()
    fetchMock.mockResolvedValue(stubResponse)
    vi.useRealTimers()
  })

  it('fetches on mount with a non-null scope', async () => {
    const scope = ref<UsageScope | null>({ namespace: 'monitoring', selector: 'app=prom' })
    const wrapper = mountHarness(scope, { pollMs: 0 })
    await flushPromises()
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(fetchMock.mock.calls[0][0]).toEqual({ namespace: 'monitoring', selector: 'app=prom' })
    expect((wrapper.vm as unknown as { usage: { data: { value: UsageResponse | null } } }).usage.data.value).toEqual(stubResponse)
  })

  it('skips fetch when scope is null', async () => {
    const scope = ref<UsageScope | null>(null)
    mountHarness(scope, { pollMs: 0 })
    await flushPromises()
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('re-fetches when scope changes from null to non-null', async () => {
    const scope = ref<UsageScope | null>(null)
    mountHarness(scope, { pollMs: 0 })
    await flushPromises()
    expect(fetchMock).not.toHaveBeenCalled()

    scope.value = { namespace: 'monitoring', selector: 'app=prom' }
    await flushPromises()
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('clears data when scope becomes null again', async () => {
    const scope = ref<UsageScope | null>({ namespace: 'monitoring', selector: 'app=prom' })
    const wrapper = mountHarness(scope, { pollMs: 0 })
    await flushPromises()
    expect((wrapper.vm as unknown as { usage: { data: { value: UsageResponse | null } } }).usage.data.value).not.toBeNull()

    scope.value = null
    await flushPromises()
    expect((wrapper.vm as unknown as { usage: { data: { value: UsageResponse | null } } }).usage.data.value).toBeNull()
  })

  it('polls at the configured interval and stops on unmount', async () => {
    vi.useFakeTimers()
    const scope = ref<UsageScope | null>({ namespace: 'monitoring', selector: 'app=prom' })
    const wrapper = mountHarness(scope, { pollMs: 1000 })

    // Initial fetch fires synchronously on mount.
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))

    await vi.advanceTimersByTimeAsync(1000)
    expect(fetchMock).toHaveBeenCalledTimes(2)

    await vi.advanceTimersByTimeAsync(1000)
    expect(fetchMock).toHaveBeenCalledTimes(3)

    wrapper.unmount()
    await vi.advanceTimersByTimeAsync(5000)
    expect(fetchMock).toHaveBeenCalledTimes(3)
  })

  it('records error string when fetch rejects', async () => {
    fetchMock.mockRejectedValueOnce(new Error('boom'))
    const scope = ref<UsageScope | null>({ namespace: 'monitoring', selector: 'app=prom' })
    const wrapper = mountHarness(scope, { pollMs: 0 })
    await flushPromises()
    const u = (wrapper.vm as unknown as { usage: { error: { value: string | null } } }).usage
    expect(u.error.value).toBe('boom')
  })

  it('accepts a plain (non-ref) scope', async () => {
    const wrapper = mount(
      defineComponent({
        setup() {
          const usage = useResourceUsage({ namespace: 'monitoring' }, { pollMs: 0 })
          return { usage }
        },
        render() {
          return h('div')
        },
      }),
    )
    await flushPromises()
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(fetchMock.mock.calls[0][0]).toEqual({ namespace: 'monitoring' })
    wrapper.unmount()
  })
})
