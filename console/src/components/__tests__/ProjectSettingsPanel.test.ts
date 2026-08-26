// @vitest-environment happy-dom
import { capabilitiesForRole } from '@/utils/testCapabilities'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import ProjectSettingsPanel from '../ProjectSettingsPanel.vue'
import ProjectQuota from '../ProjectQuota.vue'
import ProjectApiKeys from '../ProjectApiKeys.vue'
import ProjectMembers from '../ProjectMembers.vue'
import * as projectsApi from '@/api/projects'
import * as apikeysApi from '@/api/apikeys'
import type { Project } from '@/api/projects'

vi.mock('@/api/projects', async importOriginal => ({
  ...(await importOriginal<typeof projectsApi>()),
  fetchQuota: vi.fn(),
  fetchMembers: vi.fn(),
  fetchProjectRoles: vi.fn().mockResolvedValue([]),
}))
vi.mock('@/api/apikeys', async importOriginal => ({
  ...(await importOriginal<typeof apikeysApi>()),
  fetchKeys: vi.fn(),
  fetchPlans: vi.fn(),
  fetchKeyUsage: vi.fn(),
}))

const fetchQuota = vi.mocked(projectsApi.fetchQuota)
const fetchMembers = vi.mocked(projectsApi.fetchMembers)
const fetchKeys = vi.mocked(apikeysApi.fetchKeys)
const fetchPlans = vi.mocked(apikeysApi.fetchPlans)

const project: Project = {
  name: 'shop',
  display_name: 'Web Shop',
  role: 'owner', capabilities: capabilitiesForRole('owner'),
  environments: [
    { name: 'test', namespace: 'shop-test', apps: [], status: 'active', order: '0', owned: true },
    { name: 'prod', namespace: 'shop-prod', apps: [], status: 'active', order: '1', owned: true },
  ],
  env_limit: 3,
}

function mountPanel() {
  return mount(ProjectSettingsPanel, {
    props: { project },
    global: { plugins: [createPinia()], stubs: { teleport: true } },
  })
}

beforeEach(() => {
  vi.clearAllMocks()
  fetchQuota.mockResolvedValue({
    tier: 'small',
    env_limit: 3,
    env_count: 2,
    tiers: {},
    environments: [],
  })
  fetchMembers.mockResolvedValue([])
  fetchKeys.mockResolvedValue([])
  fetchPlans.mockResolvedValue([])
})

describe('ProjectSettingsPanel', () => {
  it('shows the project identity and defaults to the quota tab', async () => {
    const wrapper = mountPanel()
    await flushPromises()
    expect(wrapper.text()).toContain('Web Shop')
    expect(wrapper.text()).toContain('Project settings')
    expect(wrapper.findComponent(ProjectQuota).exists()).toBe(true)
    // Inactive tabs stay unmounted so their API calls only fire on first visit
    expect(wrapper.findComponent(ProjectApiKeys).exists()).toBe(false)
    expect(wrapper.findComponent(ProjectMembers).exists()).toBe(false)
    expect(fetchKeys).not.toHaveBeenCalled()
    expect(fetchMembers).not.toHaveBeenCalled()
  })

  it('mounts a tab only when activated', async () => {
    const wrapper = mountPanel()
    await flushPromises()
    const membersTab = wrapper.findAll('button').find(b => b.text() === 'Members')
    expect(membersTab).toBeDefined()
    await membersTab!.trigger('click')
    await flushPromises()
    expect(wrapper.findComponent(ProjectMembers).exists()).toBe(true)
    expect(wrapper.findComponent(ProjectQuota).exists()).toBe(false)
    expect(fetchMembers).toHaveBeenCalledWith('shop')
  })

  it('emits close when the panel closes', async () => {
    const wrapper = mountPanel()
    await flushPromises()
    const closeButton = wrapper.findAll('button').find(b => b.attributes('title') === 'Close (Esc)')
    expect(closeButton).toBeDefined()
    await closeButton!.trigger('click')
    expect(wrapper.emitted('close')).toBeTruthy()
  })
})

// The panel used to decide what to offer by comparing the role name to 'owner'
// and 'deployer'. It asks what the caller may do now, which is the same thing
// the API gates the matching routes on — and the only thing that can answer for
// a role this build does not enumerate.
describe('gating on capabilities rather than the role name', () => {
  async function membersTabFor(capabilities: string[]) {
    const wrapper = mount(ProjectSettingsPanel, {
      props: { project: { ...project, role: 'auditor', capabilities } },
      global: { plugins: [createPinia()], stubs: { teleport: true } },
    })
    await flushPromises()
    const tab = wrapper.findAll('button').find(b => b.text() === 'Members')
    await tab!.trigger('click')
    await flushPromises()
    return wrapper.findComponent(ProjectMembers)
  }

  it('offers member management to a role it has never heard of that carries it', async () => {
    const members = await membersTabFor(['project.read', 'members.read', 'members.manage'])
    expect(members.props('canManage')).toBe(true)
  })

  it('withholds it from a role that does not, whatever the role is called', async () => {
    const members = await membersTabFor(['project.read', 'members.read'])
    expect(members.props('canManage')).toBe(false)
  })
})
