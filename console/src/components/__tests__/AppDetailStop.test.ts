// @vitest-environment happy-dom
import { capabilitiesForRole } from '@/utils/testCapabilities'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import AppDetail from '../AppDetail.vue'
import ModalContainer from '../ModalContainer.vue'
import * as appsApi from '@/api/apps'
import * as projectsApi from '@/api/projects'
import { useAuthStore } from '@/stores/auth'
import { useProjectsStore } from '@/stores/projects'
import type { App } from '@/api/types'
import type { Project } from '@/api/projects'

vi.mock('@/api/client', () => ({
  default: {
    get: vi.fn().mockResolvedValue({ data: {} }),
    put: vi.fn().mockResolvedValue({ data: {} }),
    post: vi.fn().mockResolvedValue({ data: {} }),
    delete: vi.fn().mockResolvedValue({ data: {} }),
  },
}))
vi.mock('@/api/apps')
vi.mock('@/api/services')
vi.mock('@/api/database')
vi.mock('@/api/files')
vi.mock('@/api/mode')
vi.mock('@/api/projects', async importOriginal => ({
  ...(await importOriginal<typeof projectsApi>()),
  fetchProjects: vi.fn(),
}))

beforeEach(() => {
  document.body.innerHTML = ''
  vi.mocked(appsApi.fetchLogs).mockResolvedValue([])
  vi.mocked(appsApi.fetchAppHealth).mockResolvedValue([])
  vi.mocked(appsApi.stopApp).mockResolvedValue()
  vi.mocked(appsApi.startApp).mockResolvedValue({ status: 'starting' })
})

async function mountWith(app: App) {
  setActivePinia(createPinia())
  useAuthStore().role = 'admin'
  useProjectsStore().projects = [{
    name: 'shop', role: 'owner', capabilities: capabilitiesForRole('owner'), env_limit: 3,
    environments: [{ name: 'test', namespace: 'shop-test', apps: [], status: 'active', order: '0', owned: true }],
  } as Project]
  vi.mocked(appsApi.fetchApps).mockResolvedValue([app])
  mount(ModalContainer, { attachTo: document.body })
  mount(AppDetail, {
    props: { appName: 'checkout', namespace: 'shop-test' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
}

const running: App = { name: 'checkout', status: 'running', image: 'checkout:1', replicas: 2, ready: 2 }
const stopped: App = {
  name: 'checkout', status: 'stopped', image: 'checkout:1', replicas: 2, ready: 0,
  stopped: { reason: 'freeing memory', by: 'alice@example.com', at: '2026-10-03T09:00:00Z' },
}

function byTestId(id: string): HTMLElement | null {
  return document.body.querySelector(`[data-testid="${id}"]`)
}

describe('AppDetail stop and start', () => {
  it('stops a running app with a reason after saying what keeps running', async () => {
    await mountWith(running)

    byTestId('app-stop')!.click()
    await flushPromises()
    const text = document.body.textContent || ''
    expect(text).toContain('Bound services')
    expect(text).toContain('volumes')

    const reason = byTestId('app-stop-reason') as HTMLTextAreaElement
    reason.value = 'freeing memory'
    reason.dispatchEvent(new Event('input'))
    byTestId('app-stop-confirm')!.click()
    await flushPromises()

    expect(appsApi.stopApp).toHaveBeenCalledWith('shop-test', 'checkout', 'freeing memory')
  })

  it('shows who stopped it and why, and starts it again', async () => {
    await mountWith(stopped)

    const banner = byTestId('app-stopped-banner')
    expect(banner).not.toBeNull()
    expect(banner!.textContent).toContain('alice@example.com')
    expect(banner!.textContent).toContain('freeing memory')
    expect(byTestId('app-stop')).toBeNull()

    byTestId('app-start')!.click()
    await flushPromises()

    expect(appsApi.startApp).toHaveBeenCalledWith('shop-test', 'checkout')
  })

  it('offers no restart while the app is stopped', async () => {
    await mountWith(stopped)

    expect(document.body.querySelector('[title="Rolling restart"]')).toBeNull()
  })
})
