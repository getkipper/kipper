// @vitest-environment happy-dom
import { capabilitiesForRole } from '@/utils/testCapabilities'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import ServiceData from '../ServiceData.vue'
import * as dbApi from '@/api/database'
import { useAuthStore } from '@/stores/auth'
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

async function dialectShown(schema: dbApi.DBSchema): Promise<string> {
  setActivePinia(createPinia())
  useAuthStore().role = 'member'
  useProjectsStore().projects = [project()]
  vi.mocked(dbApi.fetchDBDatabases).mockResolvedValue([{ name: 'app', default: true }])
  vi.mocked(dbApi.fetchDBSchema).mockResolvedValue(schema)
  vi.mocked(dbApi.fetchDBSnippets).mockResolvedValue([] as never)
  vi.mocked(dbApi.fetchDBHistory).mockResolvedValue([] as never)

  const wrapper = mount(ServiceData, {
    attachTo: document.body,
    global: { stubs: { RouterLink: true, AIChat: true, Codemirror: true } },
  })
  await flushPromises()
  const aiToggle = wrapper.findAll('button').find((b) => b.text() === 'AI')
  await aiToggle!.trigger('click')
  const match = wrapper.text().match(/Dialect:\s*(postgres|mysql)/)
  return match ? match[1] : ''
}

beforeEach(() => {
  vi.clearAllMocks()
  route.value.params.name = 'db'
  document.body.innerHTML = ''
})

describe('the database editor takes its SQL dialect from the server', () => {
  it('treats a Postgres database without a public schema as Postgres', async () => {
    const dialect = await dialectShown({
      dialect: 'postgres',
      databases: [{ name: 'app', schemas: [{ name: 'billing', relations: [] }] }],
    })
    expect(dialect).toBe('postgres')
  })

  it('treats a MySQL database as MySQL', async () => {
    const dialect = await dialectShown({
      dialect: 'mysql',
      databases: [{ name: 'app', schemas: [{ name: 'app', relations: [] }] }],
    })
    expect(dialect).toBe('mysql')
  })
})

describe('a new table on MySQL', () => {
  function fieldValues(wrapper: ReturnType<typeof mount>): string[] {
    return [...wrapper.findAll('input'), ...wrapper.findAll('select')].map(
      (el) => (el.element as HTMLInputElement | HTMLSelectElement).value,
    )
  }

  it('starts with MySQL defaults even when New table is clicked before the schema arrives', async () => {
    setActivePinia(createPinia())
    useAuthStore().role = 'member'
    useProjectsStore().projects = [project()]
    vi.mocked(dbApi.fetchDBDatabases).mockResolvedValue([{ name: 'app', default: true }])
    let resolveSchema: (s: dbApi.DBSchema) => void = () => {}
    vi.mocked(dbApi.fetchDBSchema).mockReturnValue(new Promise((r) => { resolveSchema = r }))
    vi.mocked(dbApi.fetchDBSnippets).mockResolvedValue([] as never)
    vi.mocked(dbApi.fetchDBHistory).mockResolvedValue([] as never)
    vi.mocked(dbApi.previewDBDDL).mockResolvedValue({ ddl: [] } as never)

    const wrapper = mount(ServiceData, {
      attachTo: document.body,
      global: { stubs: { RouterLink: true, AIChat: true, Codemirror: true } },
    })
    await flushPromises()
    await wrapper.find('button[title="Create a new table"]').trigger('click')
    resolveSchema({ dialect: 'mysql', databases: [{ name: 'app', schemas: [{ name: 'app', relations: [] }] }] })
    await flushPromises()
    expect(fieldValues(wrapper)).not.toContain('bigserial')
    expect(fieldValues(wrapper)).not.toContain('timestamptz')

    await wrapper.find('button[title="Create a new table"]').trigger('click')
    await flushPromises()
    expect(fieldValues(wrapper)).toContain('bigint unsigned auto_increment')
    expect(fieldValues(wrapper)).toContain('app')
  })
})

describe('a new MySQL table on a server with several databases', () => {
  it('goes into the open database', async () => {
    setActivePinia(createPinia())
    useAuthStore().role = 'member'
    useProjectsStore().projects = [project()]
    ;(route.value.query as Record<string, string>).database = 'shop'
    vi.mocked(dbApi.fetchDBDatabases).mockResolvedValue([{ name: 'analytics', default: false }, { name: 'shop', default: true }])
    vi.mocked(dbApi.fetchDBSchema).mockResolvedValue({
      dialect: 'mysql',
      databases: [
        { name: 'analytics', schemas: [{ name: 'analytics', relations: [] }] },
        { name: 'shop', schemas: [{ name: 'shop', relations: [] }] },
      ],
    })
    vi.mocked(dbApi.fetchDBSnippets).mockResolvedValue([] as never)
    vi.mocked(dbApi.fetchDBHistory).mockResolvedValue([] as never)
    vi.mocked(dbApi.previewDBDDL).mockResolvedValue({ ddl: [] } as never)

    const wrapper = mount(ServiceData, {
      attachTo: document.body,
      global: { stubs: { RouterLink: true, AIChat: true, Codemirror: true } },
    })
    await flushPromises()
    await wrapper.find('button[title="Create a new table"]').trigger('click')
    await flushPromises()
    delete (route.value.query as Record<string, string>).database
    const schemaField = wrapper.findAll('input').find((i) => i.attributes('placeholder') === 'public')!
    expect((schemaField.element as HTMLInputElement).value).toBe('shop')
  })
})

describe('switching services', () => {
  it("waits for the new service's dialect before New table starts a draft", async () => {
    setActivePinia(createPinia())
    useAuthStore().role = 'member'
    useProjectsStore().projects = [project()]
    vi.mocked(dbApi.fetchDBDatabases).mockResolvedValue([{ name: 'app', default: true }])
    vi.mocked(dbApi.fetchDBSnippets).mockResolvedValue([] as never)
    vi.mocked(dbApi.fetchDBHistory).mockResolvedValue([] as never)
    vi.mocked(dbApi.previewDBDDL).mockResolvedValue({ ddl: [] } as never)
    let resolveMySQL: (s: dbApi.DBSchema) => void = () => {}
    vi.mocked(dbApi.fetchDBSchema).mockImplementation((service: string) =>
      service === 'db'
        ? Promise.resolve({ dialect: 'postgres', databases: [{ name: 'app', schemas: [{ name: 'public', relations: [] }] }] })
        : new Promise((r) => { resolveMySQL = r }),
    )

    const wrapper = mount(ServiceData, {
      attachTo: document.body,
      global: { stubs: { RouterLink: true, AIChat: true, Codemirror: true } },
    })
    await flushPromises()
    route.value.params.name = 'shopdb'
    await flushPromises()
    await wrapper.find('button[title="Create a new table"]').trigger('click')
    resolveMySQL({ dialect: 'mysql', databases: [{ name: 'app', schemas: [{ name: 'app', relations: [] }] }] })
    await flushPromises()

    const values = [...wrapper.findAll('input'), ...wrapper.findAll('select')].map(
      (el) => (el.element as HTMLInputElement | HTMLSelectElement).value,
    )
    expect(values).not.toContain('bigserial')
    expect(values).not.toContain('timestamptz')
  })
})

async function indexFormFor(dialect: 'postgres' | 'mysql') {
  setActivePinia(createPinia())
  useAuthStore().role = 'member'
  useProjectsStore().projects = [project()]
  const schemaName = dialect === 'mysql' ? 'app' : 'public'
  vi.mocked(dbApi.fetchDBDatabases).mockResolvedValue([{ name: 'app', default: true }])
  vi.mocked(dbApi.fetchDBSchema).mockResolvedValue({
    dialect,
    databases: [{ name: 'app', schemas: [{ name: schemaName, relations: [{ name: 'users', kind: 'table', columns: 1 }] }] }],
  })
  vi.mocked(dbApi.fetchDBSnippets).mockResolvedValue([] as never)
  vi.mocked(dbApi.fetchDBHistory).mockResolvedValue([] as never)
  vi.mocked(dbApi.fetchDBRows).mockResolvedValue({
    structure: {
      schema: schemaName, name: 'users', primary_key: ['id'], foreign_keys: [], indexes: [],
      columns: [{ name: 'id', type: 'int', nullable: false, generated: false, position: 1 }],
    },
    rows: [], total: 0, limit: 50, offset: 0, duration_ms: 1,
  } as never)

  const wrapper = mount(ServiceData, {
    attachTo: document.body,
    global: { stubs: { RouterLink: true, AIChat: true, Codemirror: true } },
  })
  await flushPromises()
  await wrapper.findAll('button').find((b) => b.text().startsWith('users'))!.trigger('click')
  await flushPromises()
  await wrapper.findAll('button').find((b) => b.text() === 'Indexes')!.trigger('click')
  await wrapper.findAll('button').find((b) => b.text().includes('Create index'))!.trigger('click')
  return {
    methods: wrapper.findAll('option').map((o) => o.attributes('value')).filter((v) => ['btree', 'hash', 'gin', 'gist'].includes(v!)),
    text: wrapper.text(),
  }
}

describe('the index form follows the dialect', () => {
  it('offers MySQL only the methods and options it supports', async () => {
    const form = await indexFormFor('mysql')
    expect(form.methods).toEqual(['btree', 'hash'])
    expect(form.text).not.toContain('Concurrent')
    expect(form.text).not.toContain('WHERE clause')
  })

  it('keeps the Postgres methods, partial indexes and concurrent builds', async () => {
    const form = await indexFormFor('postgres')
    expect(form.methods).toEqual(['btree', 'gin', 'gist', 'hash'])
    expect(form.text).toContain('Concurrent')
    expect(form.text).toContain('WHERE clause')
  })
})
