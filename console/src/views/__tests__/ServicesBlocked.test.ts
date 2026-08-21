// @vitest-environment happy-dom
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import Services from '../Services.vue'
import * as servicesApi from '@/api/services'
import { useProjectsStore } from '@/stores/projects'

vi.mock('@/api/client', () => ({
  default: {
    get: vi.fn().mockResolvedValue({ data: {} }),
    put: vi.fn().mockResolvedValue({ data: {} }),
    post: vi.fn().mockResolvedValue({ data: {} }),
    delete: vi.fn().mockResolvedValue({ data: {} }),
  },
}))
vi.mock('@/api/services')

const HEALTHY = {
  name: 'cache',
  namespace: 'shop-test',
  type: 'redis',
  status: 'running',
  ready: '1/1',
  storage: '1Gi',
}

const BLOCKED = {
  name: 'db',
  namespace: 'shop-test',
  type: 'postgres',
  status: 'failed',
  ready: '0/1',
  storage: '5Gi',
  blockedReason: 'DataWithoutCredentials',
  blockedMessage:
    'service db has data in data-db-0 and no PASSWORD in db-credentials; restore db-credentials from a backup',
}

async function mountServices(services: unknown[]) {
  setActivePinia(createPinia())
  useProjectsStore().globalNamespace = 'shop-test'
  vi.mocked(servicesApi.fetchServices).mockResolvedValue(services as never)

  const wrapper = mount(Services, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
  await flushPromises()
  return wrapper
}

describe('a service whose credentials were refused', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  // The status column says "failed" and nothing an operator can act on. The
  // reconciler writes the reason and the remedy onto the object for exactly
  // this moment, so the console has to show them.
  it('shows the reason and the remedy', async () => {
    const wrapper = await mountServices([BLOCKED])

    const text = wrapper.text()
    expect(text).toContain('DataWithoutCredentials')
    expect(text).toContain('restore db-credentials from a backup')
  })

  // A healthy service carries no condition, and neither does any service on a
  // cluster older than it. Neither should grow a warning.
  it('says nothing about a service that is fine', async () => {
    const wrapper = await mountServices([HEALTHY])

    expect(wrapper.text()).not.toContain('DataWithoutCredentials')
    expect(wrapper.html()).not.toContain('amber-50')
  })
})
