// @vitest-environment happy-dom
import { capabilitiesForRole } from '@/utils/testCapabilities'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import ServiceData from '../ServiceData.vue'
import * as dbApi from '@/api/database'
import { useAuthStore } from '@/stores/auth'
import { useProjectsStore } from '@/stores/projects'
import type { Project, ProjectRole } from '@/api/projects'

const NS = 'shop-test'

vi.mock('@/composables/useToast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }),
}))
vi.mock('@/api/database')
vi.mock('@/api/services')
vi.mock('vue-router', () => ({
  useRoute: () => ({ params: { name: 'db' }, query: { namespace: NS } }),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
}))

// Every one of these calls a route gated on database.write:
//   POST   /db/query            POST /db/tables        PATCH /db/tables/{...}
//   POST   /db/indexes          POST /db/snippets      DELETE /db/snippets/{...}
//   POST/PATCH/DELETE /db/tables/{schema}/{table}/rows
const WRITE_MARKERS = [
  'title="Create a new table"',
  'title="Run the statement at the cursor',
  'title="Run every statement in the editor',
  'title="Save the current query as a reusable snippet"',
]

// Reached only with a table selected, a row selected or a snippet saved, so
// they are asserted through the component rather than the first render:
// Insert row, Delete n, Save n changes, Drop column, Drop index, Create index,
// and the pin and delete on a snippet. All take canWriteData.

function project(role: ProjectRole): Project {
  return {
    name: 'shop', role, capabilities: capabilitiesForRole(role), env_limit: 3,
    environments: [{ name: 'test', namespace: NS, apps: [], status: 'active', order: '0', owned: true }],
  }
}

async function mountEditor(role: ProjectRole) {
  setActivePinia(createPinia())
  useAuthStore().role = 'member'
  useProjectsStore().projects = [project(role)]

  vi.mocked(dbApi.fetchDBDatabases).mockResolvedValue([{ name: 'app', current: true }] as never)
  vi.mocked(dbApi.fetchDBSchema).mockResolvedValue({ schemas: [] } as never)
  vi.mocked(dbApi.fetchDBSnippets).mockResolvedValue([] as never)
  vi.mocked(dbApi.fetchDBHistory).mockResolvedValue([] as never)

  const wrapper = mount(ServiceData, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
  await flushPromises()
  return wrapper
}

beforeEach(() => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

describe('the database editor offers only what the caller may do', () => {
  it('withholds every write control from a reader', async () => {
    const wrapper = await mountEditor('viewer')

    for (const marker of WRITE_MARKERS) {
      expect(wrapper.html()).not.toContain(marker)
    }
  })

  it('offers them to a deployer', async () => {
    const wrapper = await mountEditor('deployer')

    for (const marker of WRITE_MARKERS) {
      expect(wrapper.html()).toContain(marker)
    }
  })
})
