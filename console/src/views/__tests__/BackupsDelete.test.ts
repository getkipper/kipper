// @vitest-environment happy-dom
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { AxiosError, AxiosHeaders } from 'axios'

import Backups from '../Backups.vue'
import * as backupsApi from '@/api/backups'
import * as platformApi from '@/api/platform'
import { useAuthStore } from '@/stores/auth'
import { useModal } from '@/composables/useModal'
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

async function confirmDelete() {
  const wrapper = mount(Backups, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
  await flushPromises()
  await wrapper.find('button[title="Delete backup"]').trigger('click')
  const props = useModal().props.value as { message: string, onConfirm: () => Promise<void> }
  const message = props.message
  await props.onConfirm()
  await flushPromises()
  wrapper.unmount()
  return { message, toasts: useToast().toasts.value.map(t => t.message) }
}

describe('Deleting a backup', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    useAuthStore().role = 'admin'
    vi.mocked(backupsApi.fetchBackups).mockResolvedValue([
      { name: 'nightly-1', status: 'Completed', namespaces: '*', created: '2026-11-22 03:00', ttl: '720h', reason: '' },
    ])
    vi.mocked(backupsApi.fetchSchedules).mockResolvedValue([])
    vi.mocked(platformApi.fetchPlatformSummary).mockResolvedValue({
      profile: 'nano', available_profiles: [], components: [],
      storage: 'local', shared_volumes: false, backups: true,
    })
  })

  it('says that its data in the backup storage goes too', async () => {
    vi.mocked(backupsApi.deleteBackup).mockResolvedValue({ queued: true })

    const { message, toasts } = await confirmDelete()

    expect(message).toContain('backup storage')
    expect(toasts).toContain('Velero is deleting backup nightly-1 and its data in the backup storage')
  })

  it('says a backup that never started is gone', async () => {
    vi.mocked(backupsApi.deleteBackup).mockResolvedValue({ queued: false })

    const { toasts } = await confirmDelete()

    expect(toasts).toContain('Backup nightly-1 deleted')
  })

  it('shows the reason the server gives for refusing', async () => {
    const reason = 'Backup nightly-1 cannot be deleted now because Velero is not running.'
    const config = { headers: new AxiosHeaders() }
    vi.mocked(backupsApi.deleteBackup).mockRejectedValue(new AxiosError('Request failed with status code 409', 'ERR_BAD_REQUEST', config, null, {
      status: 409, statusText: 'Conflict', headers: {}, config, data: { error: reason },
    }))

    const { toasts } = await confirmDelete()

    expect(toasts).toContain(reason)
  })
})
