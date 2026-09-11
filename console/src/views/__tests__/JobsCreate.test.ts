// @vitest-environment happy-dom
import { capabilitiesForRole } from '@/utils/testCapabilities'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import Jobs from '../Jobs.vue'
import * as jobsApi from '@/api/jobs'
import { useAuthStore } from '@/stores/auth'
import { useProjectsStore } from '@/stores/projects'
import type { Project, ProjectRole } from '@/api/projects'

vi.mock('@/composables/useToast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }),
}))
vi.mock('@/api/jobs')
vi.mock('@/api/projects', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/projects')>()),
  fetchProjects: vi.fn().mockResolvedValue([]),
}))

const SHOP_NS = 'shop-prod'
const BLOG_NS = 'blog-prod'
const JOB = 'nightly-cleanup'

function project(name: string, namespace: string, role: ProjectRole): Project {
  return {
    name, role, capabilities: capabilitiesForRole(role), env_limit: 3,
    environments: [{ name: 'prod', namespace, apps: [], status: 'active', order: '0', owned: true }],
  }
}

function job(namespace: string) {
  return {
    name: JOB, type: 'cronjob', namespace, schedule: '0 3 * * *',
    last: 'never', status: 'pending', image: 'busybox',
  }
}

async function mountJobs() {
  setActivePinia(createPinia())
  useAuthStore().role = 'member'
  useProjectsStore().projects = [project('shop', SHOP_NS, 'deployer'), project('blog', BLOG_NS, 'deployer')]
  const wrapper = mount(Jobs, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
  await flushPromises()
  return wrapper
}

beforeEach(() => {
  vi.clearAllMocks()
})

describe('the job list tells two same-named jobs apart', () => {
  it('renders a row for each, in its own namespace', async () => {
    vi.mocked(jobsApi.fetchJobs).mockResolvedValue([job(SHOP_NS), job(BLOG_NS)])
    const wrapper = await mountJobs()

    const text = wrapper.text()
    expect(text).toContain(SHOP_NS)
    expect(text).toContain(BLOG_NS)
  })

  it('waits for the created job rather than a namesake in another project', async () => {
    // The poll asked whether any job of that name was listed. With the name
    // already taken in another project it answered yes on the first pass, so
    // creating a job appeared to finish before the cluster had it.
    vi.mocked(jobsApi.fetchJobs).mockResolvedValue([job(BLOG_NS)])
    vi.mocked(jobsApi.createJob).mockResolvedValue(undefined)
    const wrapper = await mountJobs()
    // The namesake in blog-prod is listed throughout; the new job in shop-prod
    // only appears on the second poll.
    vi.mocked(jobsApi.fetchJobs)
      .mockResolvedValueOnce([job(BLOG_NS)])
      .mockResolvedValue([job(BLOG_NS), job(SHOP_NS)])

    const vm = wrapper.vm as unknown as {
      newName: string; newImage: string; newNamespace: string
      handleCreate: () => Promise<void>
    }
    vm.newName = JOB
    vm.newImage = 'busybox'
    vm.newNamespace = SHOP_NS
    vi.mocked(jobsApi.fetchJobs).mockClear()

    await vm.handleCreate()

    // Two polls: the first sees only the namesake, the second sees the new job.
    expect(vi.mocked(jobsApi.fetchJobs).mock.calls.length).toBe(2)
  })
})
