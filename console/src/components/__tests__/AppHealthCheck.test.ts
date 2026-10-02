// @vitest-environment happy-dom
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import AppHealthCheck from '../AppHealthCheck.vue'
import * as appsApi from '@/api/apps'
import type { HealthCheck, HealthCheckStatus } from '@/api/apps'

vi.mock('@/api/apps')

async function mountWith(health: HealthCheck, status: HealthCheckStatus | null, canWrite = true) {
  vi.mocked(appsApi.fetchHealthCheck).mockResolvedValue({ health, status })
  vi.mocked(appsApi.updateHealthCheck).mockResolvedValue()
  const wrapper = mount(AppHealthCheck, { props: { project: 'shop-test', appName: 'checkout', canWrite } })
  await flushPromises()
  return wrapper
}

describe('AppHealthCheck', () => {
  beforeEach(() => vi.clearAllMocks())

  it('says what Kipper chose for an automatic app', async () => {
    const w = await mountWith({ type: 'auto' }, { type: 'tcp', port: 8080, source: 'inferred' })
    expect(w.text()).toContain('Automatic: port check on 8080')
  })

  it('says why an automatic app has no check yet', async () => {
    const w = await mountWith({ type: 'auto' }, { type: 'none', source: 'pending' })
    expect(w.text()).toContain('next rollout')
  })

  it('explains why automatic inference selected no check', async () => {
    const w = await mountWith({ type: 'auto' }, { type: 'none', source: 'inferred' })
    expect(w.text()).toContain('refused connections')
  })

  it('declares an http check', async () => {
    const w = await mountWith({ type: 'auto' }, { type: 'none', source: 'pending' })
    await w.get('[data-testid="health-type"]').setValue('http')
    await w.get('[data-testid="health-path"]').setValue('/actuator/health/readiness')
    await w.get('[data-testid="health-startup"]').setValue('600')
    await w.get('[data-testid="health-save"]').trigger('click')
    await flushPromises()

    expect(appsApi.updateHealthCheck).toHaveBeenCalledWith('shop-test', 'checkout', {
      type: 'http', path: '/actuator/health/readiness', startup_timeout_seconds: 600,
    })
  })

  it('sends only what the chosen type takes', async () => {
    const w = await mountWith({ type: 'http', path: '/ready', port: 8081, timeout_seconds: 3 }, { type: 'http', port: 8081, path: '/ready', source: 'declared' })
    await w.get('[data-testid="health-type"]').setValue('none')
    await w.get('[data-testid="health-save"]').trigger('click')
    await flushPromises()

    expect(appsApi.updateHealthCheck).toHaveBeenCalledWith('shop-test', 'checkout', { type: 'none' })
  })

  it('keeps a declared timeout when only the startup time changes', async () => {
    const w = await mountWith({ type: 'http', path: '/ready', timeout_seconds: 10 }, { type: 'http', port: 8080, path: '/ready', source: 'declared' })
    await w.get('[data-testid="health-startup"]').setValue('900')
    await w.get('[data-testid="health-save"]').trigger('click')
    await flushPromises()

    expect(appsApi.updateHealthCheck).toHaveBeenCalledWith('shop-test', 'checkout', {
      type: 'http', path: '/ready', timeout_seconds: 10, startup_timeout_seconds: 900,
    })
  })

  it('offers no save while the current check could not be read', async () => {
    vi.mocked(appsApi.fetchHealthCheck).mockRejectedValue(new Error('timeout'))
    const w = mount(AppHealthCheck, { props: { project: 'shop-test', appName: 'checkout', canWrite: true } })
    expect(w.find('[data-testid="health-save"]').exists()).toBe(false)
    await flushPromises()

    expect(w.find('[data-testid="health-save"]').exists()).toBe(false)
    expect(w.text()).toContain('could not be read')
  })

  it('warns that saving restarts the app', async () => {
    const w = await mountWith({ type: 'auto' }, null)
    expect(w.text()).toContain('restarts the app')
  })

  it('offers no save to a reader', async () => {
    const w = await mountWith({ type: 'auto' }, null, false)
    expect(w.find('[data-testid="health-save"]').exists()).toBe(false)
  })
})

describe('AppHealthCheck while a change is on its way', () => {
  it('says a saved check has not reached the pods yet', async () => {
    const w = await mountWith({ type: 'http', path: '/ready' }, { type: 'http', port: 8080, path: '/ready', source: 'applying' })
    expect(w.text()).toContain('not applied to new pods yet')
  })
})
