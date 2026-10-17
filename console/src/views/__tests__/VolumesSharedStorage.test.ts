// @vitest-environment happy-dom
import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import Volumes from '../Volumes.vue'
import * as volumesApi from '@/api/volumes'
import * as appsApi from '@/api/apps'
import * as platformApi from '@/api/platform'
import type { Project } from '@/api/projects'
import { capabilitiesForRole } from '@/utils/testCapabilities'
import { useAuthStore } from '@/stores/auth'
import { useProjectsStore } from '@/stores/projects'

vi.mock('@/api/client', () => ({
  default: {
    get: vi.fn().mockResolvedValue({ data: {} }),
    post: vi.fn().mockResolvedValue({ data: {} }),
    delete: vi.fn().mockResolvedValue({ data: {} }),
  },
}))
vi.mock('@/api/volumes')
vi.mock('@/api/apps')
vi.mock('@/api/platform')
vi.mock('@/api/projects', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/projects')>()),
  fetchProjects: vi.fn().mockResolvedValue([]),
}))

const NS = 'shop-test'
const NOTE = 'This cluster uses local storage. Shared volumes (ReadWriteMany) require Longhorn.'

async function mountVolumes(sharedVolumes: boolean, volumes: volumesApi.Volume[] = []) {
  setActivePinia(createPinia())
  useAuthStore().role = 'member'
  const projects = useProjectsStore()
  projects.globalNamespace = NS
  projects.projects = [{
    name: 'shop', role: 'deployer', capabilities: capabilitiesForRole('deployer'), env_limit: 3,
    environments: [{ name: 'test', namespace: NS, apps: [], status: 'active', order: '0', owned: true }],
  } as Project]

  vi.mocked(volumesApi.fetchVolumes).mockResolvedValue(volumes)
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(platformApi.fetchPlatformSummary).mockResolvedValue({
    profile: 'nano', available_profiles: [], components: [],
    storage: sharedVolumes ? 'longhorn' : 'local', shared_volumes: sharedVolumes,
    shared_volumes_note: sharedVolumes ? undefined : NOTE,
  })

  const wrapper = mount(Volumes, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
  await flushPromises()
  return wrapper
}

describe('Volumes on a cluster without shared volumes', () => {
  it('offers no create action and explains why instead of suggesting kip volume create', async () => {
    const wrapper = await mountVolumes(false)
    expect(wrapper.findAll('button').map(b => b.text())).not.toContain('Create')
    expect(wrapper.text()).toContain(NOTE)
    expect(wrapper.text()).not.toContain('kip volume create')
    wrapper.unmount()
  })

  it('keeps explaining once a refused volume is listed, and offers no mount', async () => {
    const wrapper = await mountVolumes(false, [{ name: 'uploads', size: '1Gi', status: 'Pending', access: 'ReadWriteMany', mounts: [] }])
    const buttons = wrapper.findAll('button').map(b => b.text())
    expect(buttons).not.toContain('Create')
    expect(buttons).not.toContain('Mount')
    expect(wrapper.text()).toContain('uploads')
    expect(wrapper.text()).toContain(NOTE)
    wrapper.unmount()
  })

  it('keeps the create action and the hint on Longhorn', async () => {
    const wrapper = await mountVolumes(true)
    expect(wrapper.findAll('button').map(b => b.text())).toContain('Create')
    expect(wrapper.text()).toContain('kip volume create')
    wrapper.unmount()
  })
})
