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
import type { Capability, Project } from '@/api/projects'

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

// A role that may change things but may not read this one thing. Custom roles
// make that ordinary, and it is what turns a swallowed refusal into a write.
function projectWithout(missing: Capability): Project {
  return {
    name: 'shop', role: 'deployer',
    capabilities: capabilitiesForRole('deployer').filter(c => c !== missing),
    env_limit: 3,
    environments: [{ name: 'prod', namespace: NS, apps: [], status: 'active', order: '0', owned: true }],
  }
}

async function mountPanel(project: Project) {
  setActivePinia(createPinia())
  useAuthStore().role = 'member'
  useProjectsStore().projects = [project]

  vi.mocked(appsApi.fetchLogs).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnv).mockResolvedValue({})
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
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

beforeEach(() => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

describe('a read the caller was refused is not an answer', () => {
  it('does not offer to generate a webhook it could not read', async () => {
    // The refused read used to render as "not configured", and Generate then
    // rotated a token that was live, breaking whatever was calling it.
    vi.mocked(appsApi.fetchWebhookConfig).mockRejectedValue({ response: { status: 403 } })
    const wrapper = await mountPanel(projectWithout('webhook.reveal'))

    const vm = wrapper.vm as unknown as {
      loadWebhookConfig: () => Promise<void>
      webhookUnreadable: boolean
      webhookEnabled: boolean
    }
    await vm.loadWebhookConfig()
    await flushPromises()

    expect(vm.webhookUnreadable).toBe(true)
    expect(vm.webhookEnabled).toBe(false)
  })

  it('does not offer to scale by a replica count it could not read', async () => {
    // The refused read left the count at its initial value, so "+" sent an
    // absolute number that scaled a multi-replica app down.
    vi.mocked(appsApi.fetchApps).mockRejectedValue({ response: { status: 403 } })
    const wrapper = await mountPanel(projectWithout('kipper.read'))

    const vm = wrapper.vm as unknown as {
      loadScale: () => Promise<void>
      replicaCountUnreadable: boolean
    }
    await vm.loadScale()
    await flushPromises()

    expect(vm.replicaCountUnreadable).toBe(true)
  })
})
