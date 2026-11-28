// @vitest-environment happy-dom
import { capabilitiesForRole } from '@/utils/testCapabilities'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import ServiceData from '../ServiceData.vue'
import * as dbApi from '@/api/database'
import { useAuthStore } from '@/stores/auth'
import { useModal } from '@/composables/useModal'
import { useProjectsStore } from '@/stores/projects'
import type { Project } from '@/api/projects'

const NS = 'shop-test'

vi.mock('@/composables/useToast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }),
}))
vi.mock('@/api/database')
vi.mock('@/api/services')
const route = vi.hoisted(() => ({ value: null as unknown as { params: { name: string }; query: { namespace: string } } }))
vi.mock('vue-router', async () => {
  const { reactive } = await import('vue')
  route.value = reactive({ params: { name: 'db' }, query: { namespace: 'shop-test' } })
  return {
    useRoute: () => route.value,
    useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  }
})

function project(): Project {
  return {
    name: 'shop', role: 'deployer', capabilities: capabilitiesForRole('deployer'), env_limit: 3,
    environments: [{ name: 'test', namespace: NS, apps: [], status: 'active', order: '0', owned: true }],
  }
}

/**
 * Mounts the page, opens a table in the designer, and queues dropping its note
 * column. The default schema has the tables users and orders in public.
 */
async function queueDrop(
  relations: { schema: string; table: string }[] = [{ schema: 'public', table: 'users' }, { schema: 'public', table: 'orders' }],
  open = 'users',
) {
  setActivePinia(createPinia())
  useAuthStore().role = 'member'
  useProjectsStore().projects = [project()]
  vi.mocked(dbApi.fetchDBDatabases).mockResolvedValue([{ name: 'app', default: true }])
  vi.mocked(dbApi.fetchDBSchema).mockResolvedValue({
    dialect: 'postgres',
    databases: [{
      name: 'app',
      schemas: [...new Set(relations.map((r) => r.schema))].map((name) => ({
        name,
        relations: relations.filter((r) => r.schema === name).map((r) => ({ name: r.table, kind: 'table', columns: 2 })),
      })),
    }],
  })
  vi.mocked(dbApi.fetchDBSnippets).mockResolvedValue([] as never)
  vi.mocked(dbApi.fetchDBHistory).mockResolvedValue([] as never)
  vi.mocked(dbApi.previewDBDDL).mockResolvedValue({ ddl: [] } as never)
  vi.mocked(dbApi.fetchDBRows).mockImplementation(async (_s, _n, schema, table) => ({
    structure: {
      schema, name: table, primary_key: ['id'], foreign_keys: [], indexes: [],
      columns: [
        { name: 'id', type: 'int', nullable: false, generated: false, position: 1 },
        { name: 'note', type: 'text', nullable: true, generated: false, position: 2 },
      ],
    },
    rows: [], total: 0, limit: 50, offset: 0, duration_ms: 1,
  }) as never)

  const wrapper = mount(ServiceData, {
    attachTo: document.body,
    global: { stubs: { RouterLink: true, AIChat: true, Codemirror: true } },
  })
  await flushPromises()
  // The sidebar expands a lone schema by itself and leaves several collapsed.
  const schemas = new Set(relations.map((r) => r.schema))
  if (schemas.size > 1) {
    for (const name of schemas) {
      await wrapper.findAll('button').find((b) => b.find('span').exists() && b.find('span').text() === name)!.trigger('click')
    }
  }
  await flushPromises()
  await tableButton(wrapper, open).trigger('click')
  await flushPromises()
  await wrapper.findAll('button').find((b) => b.text().startsWith('Designer'))!.trigger('click')
  await flushPromises()
  await wrapper.findAll('button').find((b) => b.text() === 'Drop')!.trigger('click')
  ;(useModal().props.value as { onConfirm: () => void }).onConfirm()
  await flushPromises()
  expect(queued(wrapper)).toBe(true)
  return wrapper
}

function tableButton(wrapper: ReturnType<typeof mount>, table: string) {
  return wrapper.findAll('button[title^="Click to browse rows"]').find((b) => b.text() === table)!
}

/** Checks for queued changes using the Designer badge, which is visible on every tab. */
function queued(wrapper: ReturnType<typeof mount>): boolean {
  return wrapper.findAll('button').find((b) => b.text().startsWith('Designer'))!.text() !== 'Designer'
}

beforeEach(() => {
  vi.clearAllMocks()
  route.value.params.name = 'db'
  document.body.innerHTML = ''
})

describe('the designer queue belongs to one table', () => {
  it('is cleared when another table is opened', async () => {
    const wrapper = await queueDrop()
    await tableButton(wrapper, 'orders').trigger('click')
    await flushPromises()
    expect(queued(wrapper)).toBe(false)
  })

  it('is cleared when another service is opened', async () => {
    const wrapper = await queueDrop()
    route.value.params.name = 'otherdb'
    await flushPromises()
    await wrapper.findAll('button').find((b) => b.text().startsWith('users'))?.trigger('click')
    await flushPromises()
    await wrapper.findAll('button').find((b) => b.text().startsWith('Designer'))?.trigger('click')
    await flushPromises()
    expect(queued(wrapper)).toBe(false)
  })

  it('is cleared between tables whose schema and name join to the same text', async () => {
    const wrapper = await queueDrop([{ schema: 'a.b', table: 'c' }, { schema: 'a', table: 'b.c' }], 'c')
    await tableButton(wrapper, 'b.c').trigger('click')
    await flushPromises()
    expect(queued(wrapper)).toBe(false)
  })

  it('does not show a preview that was still loading for the previous table', async () => {
    let resolvePreview: ((r: { ddl: string[] }) => void) | null = null
    const wrapper = await queueDrop()
    vi.mocked(dbApi.previewDBDDL).mockImplementation(() => new Promise((r) => { resolvePreview = r }) as never)
    // Wait for the 200 ms debounce so the users table's preview request is pending.
    await new Promise((r) => setTimeout(r, 250))
    expect(resolvePreview).not.toBeNull()
    const pending = resolvePreview!
    await tableButton(wrapper, 'orders').trigger('click')
    await new Promise((r) => setTimeout(r, 250))
    pending({ ddl: ['ALTER TABLE "public"."users" DROP COLUMN "note"'] })
    await flushPromises()
    await wrapper.findAll('button').find((b) => b.text().startsWith('Designer'))!.trigger('click')
    await flushPromises()
    expect(wrapper.text()).not.toContain('DROP COLUMN')
  })
})
