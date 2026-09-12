// @vitest-environment happy-dom
import { capabilitiesForRole } from '@/utils/testCapabilities'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import AppDetail from '../AppDetail.vue'
import * as appsApi from '@/api/apps'
import * as projectsApi from '@/api/projects'
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
vi.mock('@/api/apps')
vi.mock('@/api/services')
vi.mock('@/api/database')
vi.mock('@/api/files')
vi.mock('@/api/mode')
vi.mock('@/api/projects', async importOriginal => ({
  ...(await importOriginal<typeof projectsApi>()),
  fetchProjects: vi.fn().mockResolvedValue([]),
}))

const NS = 'shop-prod'

function projectWithRole(role: ProjectRole): Project {
  return {
    name: 'shop', role, capabilities: capabilitiesForRole(role), env_limit: 3,
    environments: [{ name: 'prod', namespace: NS, apps: [], status: 'active', order: '0', owned: true }],
  }
}

async function mountPanel(role: ProjectRole) {
  setActivePinia(createPinia())
  useAuthStore().role = 'member'
  useProjectsStore().projects = [projectWithRole(role)]

  vi.mocked(appsApi.fetchLogs).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnv).mockResolvedValue({ LOG_LEVEL: 'debug' })
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvPreview).mockResolvedValue({ variables: [], available: [], snippets: [] })

  const wrapper = mount(AppDetail, {
    props: { appName: 'api', namespace: NS },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  return wrapper
}

// Withdrawing env.reveal clears what is on screen. A read issued a moment
// earlier, under the role that still held it, must not put it back.
describe('a reveal in flight when the capability is withdrawn', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  it('does not publish the secret it was authorised to read', async () => {
    const wrapper = await mountPanel('owner')
    let release: (v: string) => void = () => {}
    vi.mocked(appsApi.revealSecret).mockReturnValue(new Promise<string>(resolve => { release = resolve }))

    const vm = wrapper.vm as unknown as {
      revealSecret: (key: string) => Promise<void>
      revealedSecrets: Record<string, string>
    }
    const revealing = vm.revealSecret('DATABASE_URL')

    useProjectsStore().projects = [projectWithRole('viewer')]
    await flushPromises()

    release('postgres://real:secret@db/app')
    await revealing
    await flushPromises()

    expect(vm.revealedSecrets).toEqual({})
  })

  it('does not publish the env preview it was authorised to read', async () => {
    const wrapper = await mountPanel('owner')
    let release: (v: unknown) => void = () => {}
    vi.mocked(appsApi.fetchEnvPreview).mockReturnValue(new Promise(resolve => { release = resolve }) as never)

    const vm = wrapper.vm as unknown as {
      loadEnvPreview: () => Promise<void>
      envPreview: unknown
    }
    const loading = vm.loadEnvPreview()

    useProjectsStore().projects = [projectWithRole('viewer')]
    await flushPromises()

    release({ variables: [{ name: 'DATABASE_URL', value: 'postgres://real:secret@db/app' }], available: [], snippets: [] })
    await loading
    await flushPromises()

    expect(vm.envPreview).toBeNull()
  })
})
