// @vitest-environment happy-dom
import { capabilitiesForRole } from '@/utils/testCapabilities'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import AppDetail from '../AppDetail.vue'
import * as appsApi from '@/api/apps'
import * as projectsApi from '@/api/projects'
import * as modeApi from '@/api/mode'
import { useAuthStore } from '@/stores/auth'
import { useProjectsStore } from '@/stores/projects'
import type { AutoscaleConfig } from '@/api/apps'
import type { App } from '@/api/types'
import type { Project, ProjectRole } from '@/api/projects'

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

const fixed: AutoscaleConfig = {
  enabled: false, min_replicas: 2, max_replicas: 6, cpu_target: 0, memory_target: 0,
  current_replicas: 0, current_cpu: '', current_memory: '',
  capacity_api: 1, replicas: 3, deployment_replicas: 3, running_replicas: 3, ready_replicas: 3, stopped: false, activity: [],
}

const running: App = { name: 'checkout', status: 'running', image: 'checkout:1', replicas: 3, ready: 3 }
const stopped: App = { ...running, status: 'stopped', ready: 0, stopped: { reason: 'maintenance' } }

beforeEach(() => {
  document.body.innerHTML = ''
  vi.clearAllMocks()
  vi.mocked(appsApi.fetchLogs).mockResolvedValue([])
  vi.mocked(appsApi.fetchAppHealth).mockResolvedValue([])
  vi.mocked(appsApi.fetchAutoscale).mockResolvedValue(fixed)
  vi.mocked(appsApi.scaleApp).mockResolvedValue()
  vi.mocked(appsApi.startApp).mockResolvedValue({ status: 'starting' })
})

async function openScale(app: App = running, role: ProjectRole = 'owner') {
  setActivePinia(createPinia())
  useAuthStore().role = role === 'owner' ? 'admin' : 'viewer'
  useProjectsStore().projects = [{
    name: 'shop', role, capabilities: capabilitiesForRole(role), env_limit: 3,
    environments: [{ name: 'test', namespace: 'shop-test', apps: [], status: 'active', order: '0', owned: true }],
  } as Project]
  vi.mocked(appsApi.fetchApps).mockResolvedValue([app])
  const wrapper = mount(AppDetail, {
    props: { appName: 'checkout', namespace: 'shop-test' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(wrapper.vm as unknown as { activeTab: string }).activeTab = 'scale'
  await flushPromises()
  return wrapper
}

function byTestId(id: string): HTMLElement | null {
  return document.body.querySelector(`[data-testid="${id}"]`)
}

describe('AppDetail Scale tab', () => {
  it('shows the capacity panel in place of the manual count and the autoscaling toggle', async () => {
    await openScale()

    expect((byTestId('capacity-desired') as HTMLInputElement).value).toBe('3')
    expect(byTestId('capacity-save')).not.toBeNull()
    expect(byTestId('scale-plus')).toBeNull()
    expect(byTestId('autoscale-toggle')).toBeNull()
  })

  it('keeps the write controls from someone who cannot write to the app', async () => {
    await openScale(running, 'viewer')

    expect(byTestId('capacity-desired')).not.toBeNull()
    expect(byTestId('capacity-save')).toBeNull()
    expect((byTestId('capacity-min') as HTMLInputElement).disabled).toBe(true)
  })

  it('refreshes the app after a capacity save', async () => {
    await openScale()
    vi.mocked(appsApi.fetchApps).mockClear()

    const desired = byTestId('capacity-desired') as HTMLInputElement
    desired.value = '5'
    desired.dispatchEvent(new Event('input'))
    await flushPromises()
    byTestId('capacity-save')!.click()
    await flushPromises()

    expect(appsApi.scaleApp).toHaveBeenCalledWith('shop-test', 'checkout', 5)
    expect(appsApi.fetchApps).toHaveBeenCalled()
  })

  it('says that a CPU-tracked app leaves CPU to the autoscaler, without the single-replica note at 3 replicas', async () => {
    vi.mocked(modeApi.getMode).mockResolvedValue({ mode: 'auto' })
    vi.mocked(appsApi.fetchAutoscale).mockResolvedValue({ ...fixed, enabled: true, cpu_target: 70, replicas: 1, deployment_replicas: 3, tracked: { cpu: true, memory: false } })
    await openScale({ ...running, desired_replicas: 1 })

    const notice = byTestId('sizing-notice')!.textContent ?? ''
    expect(notice).toContain('The autoscaler tracks CPU, so CPU is left to it and only memory is adjusted based on usage.')
    expect(notice).not.toContain('single replica')
  })

  it('mentions the single-replica pause when the app runs one replica', async () => {
    vi.mocked(modeApi.getMode).mockResolvedValue({ mode: 'auto' })
    vi.mocked(appsApi.fetchAutoscale).mockResolvedValue({ ...fixed, min_replicas: 0, max_replicas: 0, replicas: 1, deployment_replicas: 1 })
    await openScale({ ...running, replicas: 1, ready: 1 })

    const notice = byTestId('sizing-notice')!.textContent ?? ''
    expect(notice).toContain('CPU and memory are adjusted based on usage.')
    expect(notice).toContain('Scale-down is paused because this app has a single replica. Increases still apply.')
  })

  it('reads the capacity state again after a start', async () => {
    await openScale(stopped)
    vi.mocked(appsApi.fetchAutoscale).mockClear()

    byTestId('app-start')!.click()
    await flushPromises()

    expect(appsApi.fetchAutoscale).toHaveBeenCalledWith('shop-test', 'checkout')
  })
})
