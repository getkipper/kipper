// @vitest-environment happy-dom
import { capabilitiesForRole } from '@/utils/testCapabilities'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import Projects from '../Projects.vue'
import ProjectSettingsPanel from '@/components/ProjectSettingsPanel.vue'
import AppDetail from '@/components/AppDetail.vue'
import * as projectsApi from '@/api/projects'
import type { Project } from '@/api/projects'

vi.mock('@/api/projects', async importOriginal => ({
  ...(await importOriginal<typeof projectsApi>()),
  fetchProjects: vi.fn(),
  fetchQuota: vi.fn().mockResolvedValue({
    tier: 'small',
    env_limit: 3,
    env_count: 1,
    tiers: {},
    environments: [],
  }),
}))

const fetchProjects = vi.mocked(projectsApi.fetchProjects)

function shopProject(): Project {
  return {
    name: 'shop',
    display_name: 'Web Shop',
    role: 'owner', capabilities: capabilitiesForRole('owner'),
    env_limit: 3,
    environments: [
      {
        name: 'prod',
        namespace: 'shop-prod',
        status: 'active',
        order: '0',
        owned: true,
        apps: [
          {
            name: 'api',
            image: 'ghcr.io/acme/api:latest',
            status: 'running',
            ready: '1/1',
            replicas: 1,
            url: 'https://api-prod.acme.dev',
          },
          {
            name: 'worker',
            image: 'ghcr.io/acme/worker:latest',
            status: 'running',
            ready: '1/1',
            replicas: 1,
          },
        ],
      },
    ],
  }
}

function mountView() {
  return mount(Projects, {
    global: {
      plugins: [createPinia()],
      stubs: { teleport: true, AppDetail: true, ProjectSettingsPanel: true },
    },
  })
}

async function expandShop(wrapper: ReturnType<typeof mountView>) {
  await wrapper.find('.group.flex.items-center.justify-between').trigger('click')
  await flushPromises()
}

beforeEach(() => {
  vi.clearAllMocks()
  fetchProjects.mockResolvedValue([shopProject()])
})

describe('Projects view', () => {
  it('renders a direct open link only for apps with a url', async () => {
    const wrapper = mountView()
    await flushPromises()
    await expandShop(wrapper)

    const links = wrapper.findAll('a[target="_blank"]')
    expect(links).toHaveLength(1)
    expect(links[0].attributes('href')).toBe('https://api-prod.acme.dev')
    expect(links[0].attributes('rel')).toBe('noopener noreferrer')
    expect(links[0].text()).toContain('api-prod.acme.dev')
  })

  it('opens the settings panel from the gear without expanding the card', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.findComponent(ProjectSettingsPanel).exists()).toBe(false)
    await wrapper.find('button[aria-label="Project settings"]').trigger('click')
    await flushPromises()

    const panel = wrapper.findComponent(ProjectSettingsPanel)
    expect(panel.exists()).toBe(true)
    expect((panel.props('project') as Project).name).toBe('shop')
    // The gear must not toggle the card open
    expect(wrapper.text()).not.toContain('No apps deployed')
  })

  it('opening an app closes the settings panel', async () => {
    const wrapper = mountView()
    await flushPromises()
    await expandShop(wrapper)
    await wrapper.find('button[aria-label="Project settings"]').trigger('click')
    await flushPromises()
    expect(wrapper.findComponent(ProjectSettingsPanel).exists()).toBe(true)

    const appRow = wrapper
      .findAll('div.cursor-pointer')
      .find(d => d.classes().includes('rounded-lg') && d.text().includes('api'))
    expect(appRow).toBeDefined()
    await appRow!.trigger('click')
    await flushPromises()

    expect(wrapper.findComponent(AppDetail).exists()).toBe(true)
    expect(wrapper.findComponent(ProjectSettingsPanel).exists()).toBe(false)
  })

  it('closes the settings panel when the project disappears from a reload', async () => {
    const wrapper = mountView()
    await flushPromises()
    await wrapper.find('button[aria-label="Project settings"]').trigger('click')
    await flushPromises()
    expect(wrapper.findComponent(ProjectSettingsPanel).exists()).toBe(true)

    fetchProjects.mockResolvedValue([])
    await wrapper.find('button[title="Refresh"]').trigger('click')
    await flushPromises()

    expect(wrapper.findComponent(ProjectSettingsPanel).exists()).toBe(false)
  })
})
