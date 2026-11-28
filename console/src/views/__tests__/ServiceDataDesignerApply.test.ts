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
const toast = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }))

vi.mock('@/composables/useToast', () => ({ useToast: () => toast }))
vi.mock('@/api/database')
vi.mock('@/api/services')
vi.mock('vue-router', () => ({
  useRoute: () => ({ params: { name: 'db' }, query: { namespace: NS } }),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
}))

function project(): Project {
  return {
    name: 'shop', role: 'deployer', capabilities: capabilitiesForRole('deployer'), env_limit: 3,
    environments: [{ name: 'test', namespace: NS, apps: [], status: 'active', order: '0', owned: true }],
  }
}

/**
 * Opens the users table, optionally sorts it by a column, queues dropping its
 * note column in the designer, and confirms Apply with a mocked API that
 * rejects the batch, optionally reporting how many statements committed.
 * Returns the confirmation text, mounted page, and reload and queue status.
 */
async function failApply(dialect: 'postgres' | 'mysql', opts: { committed?: number; sortBy?: string } = {}) {
  setActivePinia(createPinia())
  useAuthStore().role = 'member'
  useProjectsStore().projects = [project()]
  const schemaName = dialect === 'mysql' ? 'app' : 'public'
  vi.mocked(dbApi.fetchDBDatabases).mockResolvedValue([{ name: 'app', default: true }])
  vi.mocked(dbApi.fetchDBSchema).mockResolvedValue({
    dialect,
    databases: [{ name: 'app', schemas: [{ name: schemaName, relations: [{ name: 'users', kind: 'table', columns: 2 }] }] }],
  })
  vi.mocked(dbApi.fetchDBSnippets).mockResolvedValue([] as never)
  vi.mocked(dbApi.fetchDBHistory).mockResolvedValue([] as never)
  vi.mocked(dbApi.previewDBDDL).mockResolvedValue({ ddl: [] } as never)
  vi.mocked(dbApi.fetchDBRows).mockResolvedValue({
    structure: {
      schema: schemaName, name: 'users', primary_key: ['id'], foreign_keys: [], indexes: [],
      columns: [
        { name: 'id', type: 'int', nullable: false, generated: false, position: 1 },
        { name: 'note', type: 'text', nullable: true, generated: false, position: 2 },
      ],
    },
    rows: [], total: 0, limit: 50, offset: 0, duration_ms: 1,
  } as never)
  vi.mocked(dbApi.alterDBTable).mockRejectedValue({
    response: { data: { error: 'exec "ALTER TABLE ...": Data truncated', committed: opts.committed } },
  })

  const wrapper = mount(ServiceData, {
    attachTo: document.body,
    global: { stubs: { RouterLink: true, AIChat: true, Codemirror: true } },
  })
  await flushPromises()
  await wrapper.findAll('button').find((b) => b.text().startsWith('users'))!.trigger('click')
  await flushPromises()
  if (opts.sortBy) {
    await wrapper.findAll('th button').find((b) => b.text().startsWith(opts.sortBy!))!.trigger('click')
    await wrapper.findAll('button').find((b) => b.text() === 'Sort ascending')!.trigger('click')
    await flushPromises()
  }
  await wrapper.findAll('button').find((b) => b.text().startsWith('Designer'))!.trigger('click')
  await flushPromises()
  await wrapper.findAll('button').find((b) => b.text() === 'Drop')!.trigger('click')
  ;(useModal().props.value as { onConfirm: () => void }).onConfirm()
  await flushPromises()
  await wrapper.findAll('button').find((b) => b.text().startsWith('Apply 1 change'))!.trigger('click')
  const confirm = useModal().props.value as { message: string; onConfirm: () => Promise<void> }
  const rowLoadsBefore = vi.mocked(dbApi.fetchDBRows).mock.calls.length
  await confirm.onConfirm()
  await flushPromises()
  return {
    message: confirm.message,
    wrapper,
    reloaded: vi.mocked(dbApi.fetchDBRows).mock.calls.length > rowLoadsBefore,
    lastRowQuery: vi.mocked(dbApi.fetchDBRows).mock.lastCall![4],
    stillQueued: wrapper.findAll('button').some((b) => b.text().startsWith('Apply 1 change')),
  }
}

beforeEach(() => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

describe('applying designer changes that fail', () => {
  it('on MySQL warns that earlier statements commit, then reloads the table and clears the queue', async () => {
    const r = await failApply('mysql')
    expect(r.message).toMatch(/commits each change/i)
    expect(r.reloaded).toBe(true)
    expect(r.stillQueued).toBe(false)
    expect(toast.error).toHaveBeenCalledWith(expect.stringMatching(/may already be applied/i))
  })

  it('on MySQL keeps the queue when no statement committed', async () => {
    const r = await failApply('mysql', { committed: 0 })
    expect(r.reloaded).toBe(false)
    expect(r.stillQueued).toBe(true)
    expect(toast.error).toHaveBeenCalledWith('exec "ALTER TABLE ...": Data truncated')
  })

  it('on MySQL reloads without a sort on a column the batch may have dropped', async () => {
    const r = await failApply('mysql', { committed: 1, sortBy: 'note' })
    expect(r.reloaded).toBe(true)
    expect(r.lastRowQuery).toMatchObject({ order_by: undefined, filter_col: undefined })
  })

  it('on Postgres keeps the one-transaction promise and the queue for a retry', async () => {
    const r = await failApply('postgres')
    expect(r.message).toBe('The changes run as one transaction against the live table.')
    expect(r.reloaded).toBe(false)
    expect(r.stillQueued).toBe(true)
    expect(toast.error).toHaveBeenCalledWith('exec "ALTER TABLE ...": Data truncated')
  })
})
