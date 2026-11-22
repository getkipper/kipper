// @vitest-environment happy-dom
import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { AxiosError, AxiosHeaders } from 'axios'

import Backups from '../Backups.vue'
import * as backupsApi from '@/api/backups'
import * as platformApi from '@/api/platform'
import { useAuthStore } from '@/stores/auth'
import { useToast } from '@/composables/useToast'

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

const REASON = "This cluster's backup storage is read-only, as it is on recovery installs, so new backups cannot be saved there. Restores from this storage work as usual."

describe('Creating a backup the server refuses', () => {
  it('shows the reason the server gives', async () => {
    setActivePinia(createPinia())
    useAuthStore().role = 'admin'
    vi.mocked(backupsApi.fetchBackups).mockResolvedValue([])
    vi.mocked(backupsApi.fetchSchedules).mockResolvedValue([])
    vi.mocked(platformApi.fetchPlatformSummary).mockResolvedValue({
      profile: 'nano', available_profiles: [], components: [],
      storage: 'local', shared_volumes: false, backups: true,
    })
    const config = { headers: new AxiosHeaders() }
    vi.mocked(backupsApi.createBackup).mockRejectedValue(new AxiosError('Request failed with status code 409', 'ERR_BAD_REQUEST', config, null, {
      status: 409, statusText: 'Conflict', headers: {}, config, data: { error: REASON },
    }))
    const wrapper = mount(Backups, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
    await flushPromises()

    await wrapper.findAll('button').find(b => b.text() === 'Create backup')!.trigger('click')
    await wrapper.findAll('button').find(b => b.text() === 'Start backup')!.trigger('click')
    await flushPromises()

    expect(useToast().toasts.value.map(t => t.message)).toContain(REASON)
    wrapper.unmount()
  })
})
