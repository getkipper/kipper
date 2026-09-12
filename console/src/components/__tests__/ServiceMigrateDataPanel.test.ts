// @vitest-environment happy-dom
import { capabilitiesForRole } from '@/utils/testCapabilities'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import ServiceMigrateDataPanel from '../ServiceMigrateDataPanel.vue'
import * as servicesApi from '@/api/services'
import { useAuthStore } from '@/stores/auth'
import { useProjectsStore } from '@/stores/projects'
import type { Project, ProjectRole } from '@/api/projects'

vi.mock('@/composables/useToast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }),
}))
vi.mock('@/api/services')

const NS = 'shop-prod'
const PEER_NS = 'shop-test'

function project(role: ProjectRole): Project {
  return {
    name: 'shop', role, capabilities: capabilitiesForRole(role), env_limit: 3,
    environments: [
      { name: 'prod', namespace: NS, apps: [], status: 'active', order: '0', owned: true },
      { name: 'test', namespace: PEER_NS, apps: [], status: 'active', order: '1', owned: true },
    ],
  }
}

async function mountPanel(role: ProjectRole) {
  setActivePinia(createPinia())
  useAuthStore().role = 'member'
  useProjectsStore().projects = [project(role)]

  // The panel finds its peers by listing services of the same type elsewhere.
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([
    { name: 'db', namespace: NS, type: 'postgres', status: 'running', ready: '1/1', storage: '5Gi' },
    { name: 'db', namespace: PEER_NS, type: 'postgres', status: 'running', ready: '1/1', storage: '5Gi' },
  ] as never)
  vi.mocked(servicesApi.fetchServiceMigrationStatus).mockResolvedValue({} as never)

  const wrapper = mount(ServiceMigrateDataPanel, {
    props: { serviceName: 'db', serviceType: 'postgres', targetNamespace: NS },
    attachTo: document.body,
  })
  await flushPromises()
  return wrapper
}

beforeEach(() => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

// POST /services/{name}/migrate-data takes kipper.write, and it overwrites the
// target service's data with a peer's. Offering it to someone the server will
// refuse is the worst version of that: the confirmation is the only part that
// tells them what it does.
describe('copying data between services is offered only to a writer', () => {
  it('withholds the copy from a viewer', async () => {
    const wrapper = await mountPanel('viewer')
    expect(wrapper.text()).not.toContain('Copy data here')
  })

  it('offers it to a deployer', async () => {
    const wrapper = await mountPanel('deployer')
    expect(wrapper.text()).toContain('Copy data here')
  })
})
