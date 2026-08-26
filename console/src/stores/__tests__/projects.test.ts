import { capabilitiesForRole } from '@/utils/testCapabilities'
import { setActivePinia, createPinia } from 'pinia'
import { describe, it, expect, beforeEach, vi } from 'vitest'
import { useProjectsStore } from '../projects'
import * as api from '@/api/projects'

vi.mock('@/api/projects')

describe('projects store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.resetAllMocks()
  })

  it('starts with empty projects', () => {
    const store = useProjectsStore()
    expect(store.projects).toEqual([])
    expect(store.currentProject).toBeNull()
  })

  it('loads projects successfully', async () => {
    vi.mocked(api.fetchProjects).mockResolvedValue([
      { name: 'blog', role: 'owner', capabilities: capabilitiesForRole('owner'), env_limit: 4, environments: [
        { name: 'test', namespace: 'blog-test', status: 'Active', apps: [], order: '0', owned: true },
        { name: 'prod', namespace: 'blog-prod', status: 'Active', apps: [], order: '1', owned: true },
      ]},
    ])

    const store = useProjectsStore()
    await store.loadProjects()

    expect(store.projects).toHaveLength(1)
    expect(store.projects[0].name).toBe('blog')
    expect(store.projects[0].environments).toHaveLength(2)
  })

  it('adds a project and reloads', async () => {
    vi.mocked(api.createProject).mockResolvedValue()
    vi.mocked(api.fetchProjects).mockResolvedValue([
      { name: 'staging', role: 'owner', capabilities: capabilitiesForRole('owner'), env_limit: 4, environments: [{ name: 'default', namespace: 'staging', status: 'Active', apps: [], order: '0', owned: true }] },
    ])

    const store = useProjectsStore()
    await store.addProject('staging')

    expect(api.createProject).toHaveBeenCalledWith('staging', undefined)
    expect(store.projects).toHaveLength(1)
  })

  it('removes a project and clears selection if it was current', async () => {
    vi.mocked(api.fetchProjects).mockResolvedValue([
      { name: 'staging', role: 'owner', capabilities: capabilitiesForRole('owner'), env_limit: 4, environments: [{ name: 'default', namespace: 'staging', status: 'Active', apps: [], order: '0', owned: true }] },
    ])
    vi.mocked(api.deleteProject).mockResolvedValue()

    const store = useProjectsStore()
    await store.loadProjects()
    store.selectProject('staging')

    await store.removeProject('staging')
    expect(store.projects).toHaveLength(0)
    expect(store.currentProject).toBeNull()
  })

  it('selectProject sets the current project', () => {
    const store = useProjectsStore()
    store.selectProject('production')
    expect(store.currentProject).toBe('production')
  })

  it('captures errors on load', async () => {
    vi.mocked(api.fetchProjects).mockRejectedValue(new Error('forbidden'))

    const store = useProjectsStore()
    await store.loadProjects()

    expect(store.error).toBe('forbidden')
    expect(store.projects).toHaveLength(0)
  })
})
