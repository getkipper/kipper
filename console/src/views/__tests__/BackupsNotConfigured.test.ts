// @vitest-environment happy-dom
import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import Backups from '../Backups.vue'
import * as backupsApi from '@/api/backups'
import * as platformApi from '@/api/platform'
import { useAuthStore } from '@/stores/auth'

vi.mock('@/api/client', () => ({
  default: {
    get: vi.fn().mockResolvedValue({ data: {} }),
    post: vi.fn().mockResolvedValue({ data: {} }),
    put: vi.fn().mockResolvedValue({ data: {} }),
    delete: vi.fn().mockResolvedValue({ data: {} }),
  },
}))
vi.mock('@/api/backups')
vi.mock('@/api/platform')
vi.mock('@/api/projects', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/projects')>()),
  fetchProjects: vi.fn().mockResolvedValue([]),
}))

const NOTE = 'Backups are not configured. This cluster uses local storage and needs external storage for backups.'

async function mountBackups(backups: boolean) {
  setActivePinia(createPinia())
  useAuthStore().role = 'admin'
  vi.mocked(backupsApi.fetchBackups).mockResolvedValue([])
  vi.mocked(backupsApi.fetchSchedules).mockResolvedValue([])
  vi.mocked(platformApi.fetchPlatformSummary).mockResolvedValue({
    profile: 'nano', available_profiles: [], components: [],
    storage: 'local', shared_volumes: false, backups, backups_note: backups ? undefined : NOTE,
  })
  const wrapper = mount(Backups, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
  await flushPromises()
  return wrapper
}

describe('Backups on a cluster without backups', () => {
  it('offers no backup and explains why instead of suggesting kip backup create', async () => {
    const wrapper = await mountBackups(false)
    expect(wrapper.findAll('button').map(b => b.text())).not.toContain('Create backup')
    expect(wrapper.text()).toContain(NOTE)
    expect(wrapper.text()).not.toContain('kip backup create')
    wrapper.unmount()
  })

  it('keeps the create action on a cluster with backups', async () => {
    const wrapper = await mountBackups(true)
    expect(wrapper.findAll('button').map(b => b.text())).toContain('Create backup')
    expect(wrapper.text()).toContain('kip backup create')
    wrapper.unmount()
  })
})
