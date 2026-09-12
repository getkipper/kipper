// @vitest-environment happy-dom
import { capabilitiesForRole } from '@/utils/testCapabilities'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import Services from '../Services.vue'
import * as servicesApi from '@/api/services'
import { useAuthStore } from '@/stores/auth'
import { useProjectsStore } from '@/stores/projects'
import type { Project, ProjectRole } from '@/api/projects'

vi.mock('@/api/client', () => ({
  default: {
    get: vi.fn().mockResolvedValue({ data: {} }),
    put: vi.fn().mockResolvedValue({ data: {} }),
    post: vi.fn().mockResolvedValue({ data: {} }),
    delete: vi.fn().mockResolvedValue({ data: {} }),
  },
}))
vi.mock('@/api/services')
// The usage poll is not the subject and its shape would otherwise have to be
// complete for the sparklines and the pod count to render.
vi.mock('@/composables/useResourceUsage', () => ({
  useResourceUsage: () => ({ data: { value: null }, loading: { value: false }, error: { value: null }, refresh: vi.fn() }),
}))
vi.mock('@/api/projects', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/projects')>()),
  fetchProjects: vi.fn().mockResolvedValue([]),
}))

const NS = 'shop-test'

const SERVICE = {
  name: 'db',
  namespace: NS,
  type: 'postgres',
  status: 'running',
  ready: '1/1',
  storage: '5Gi',
}

// The route behind each control, so a reader can check the gate against it:
//   PUT  /services/{name}/resources     kipper.write
//   POST /services/{name}/diagnose      kipper.write
const WRITE_MARKERS = [
  'data-testid="slider-region"', // the resource sliders' apply region
  'title="AI Diagnose"',
]

function project(role: ProjectRole): Project {
  return {
    name: 'shop', role, capabilities: capabilitiesForRole(role), env_limit: 3,
    environments: [{ name: 'test', namespace: NS, apps: [], status: 'active', order: '0', owned: true }],
  }
}

async function mountServices(role: ProjectRole) {
  setActivePinia(createPinia())
  useAuthStore().role = 'member'
  const projects = useProjectsStore()
  projects.globalNamespace = NS
  projects.projects = [project(role)]

  vi.mocked(servicesApi.fetchServices).mockResolvedValue([SERVICE] as never)
  vi.mocked(servicesApi.fetchServiceInfo).mockResolvedValue({} as never)
  vi.mocked(servicesApi.fetchServiceResources).mockResolvedValue({
    memory_limit: '1Gi', memory_request: '1Gi', cpu_limit: '500m', cpu_request: '500m',
  } as never)

  const wrapper = mount(Services, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
  await flushPromises()
  return wrapper
}

// Open the detail panel on the resources tab, which is where the sliders and
// the diagnose button live.
async function openResources(wrapper: Awaited<ReturnType<typeof mountServices>>) {
  const vm = wrapper.vm as unknown as {
    selectedService: unknown
    selectedNamespace: string
    svcDetailTab: string
  }
  vm.selectedService = SERVICE
  vm.selectedNamespace = NS
  vm.svcDetailTab = 'resources'
  await flushPromises()
}

beforeEach(() => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

describe('the service detail panel offers only what the caller may do', () => {
  it('withholds every write control from a viewer', async () => {
    const wrapper = await mountServices('viewer')
    await openResources(wrapper)

    for (const marker of WRITE_MARKERS) {
      expect(document.body.innerHTML).not.toContain(marker)
    }
  })

  it('offers them to a deployer', async () => {
    // Without this the test above passes for a deployer too, and proves nothing
    // about the role.
    const wrapper = await mountServices('deployer')
    await openResources(wrapper)

    for (const marker of WRITE_MARKERS) {
      expect(document.body.innerHTML).toContain(marker)
    }
  })
})
