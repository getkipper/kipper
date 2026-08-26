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
import type { Project } from '@/api/projects'

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
  fetchProjects: vi.fn(),
}))

async function mountWithBuild(phase: string, message: string) {
  setActivePinia(createPinia())
  useAuthStore().role = 'admin'
  useProjectsStore().projects = [{
    name: 'shop', role: 'owner', capabilities: capabilitiesForRole('owner'), env_limit: 3,
    environments: [{ name: 'test', namespace: 'shop-test', apps: [], status: 'active', order: '0', owned: true }],
  } as Project]

  vi.mocked(appsApi.fetchLogs).mockResolvedValue([])
  vi.mocked(appsApi.fetchAppHealth).mockResolvedValue([])
  vi.mocked(appsApi.fetchBuildStatus).mockResolvedValue({
    git_configured: true,
    git_url: 'https://git.example.com/shop/checkout.git',
    git_branch: 'main',
    phase,
    message,
  } as Awaited<ReturnType<typeof appsApi.fetchBuildStatus>>)

  const wrapper = mount(AppDetail, {
    props: { appName: 'checkout', namespace: 'shop-test' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()

  // The build panel lives under the Deploys tab, which is not the one the
  // panel opens on.
  const deploys = [...document.querySelectorAll('button')]
    .find(b => (b.textContent || '').trim() === 'Deploys')
  deploys?.click()
  await flushPromises()
  return wrapper
}

describe('AppDetail discarded build', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  // Alex pushed, the pipeline went green, and nothing appeared. Being unable to
  // tell why is the defect this whole change set started from, so a build whose
  // image was not deployed has to say so where the source is shown.
  it('says why a build produced no deploy', async () => {
    await mountWithBuild(
      'Discarded',
      'The git source changed while this build was running, so it built from settings the app no longer has. Deploy again to build from the current source.',
    )

    const text = document.body.textContent || ''
    expect(text).toContain('Discarded')
    expect(text).toContain('The git source changed while this build was running')
  })
})
