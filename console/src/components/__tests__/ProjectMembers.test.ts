// @vitest-environment happy-dom
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import ProjectMembers from '../ProjectMembers.vue'
import * as projectsApi from '@/api/projects'

vi.mock('@/api/projects', async importOriginal => ({
  ...(await importOriginal<typeof projectsApi>()),
  fetchMembers: vi.fn(),
  setMember: vi.fn(),
  removeMember: vi.fn(),
}))

const fetchMembers = vi.mocked(projectsApi.fetchMembers)

function mountPanel(canManage = true) {
  return mount(ProjectMembers, {
    props: { project: 'blog', canManage },
    global: { stubs: { ConfirmDialog: true } },
  })
}

describe('ProjectMembers', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
  })

  // A role this build does not know reaches a Project through kubectl, a
  // restore, or a migration from a cluster that had it. The member holds
  // nothing, and the panel has to say both halves of that: the role exactly as
  // stored, so somebody can find it, and that it grants no access.
  //
  // The failure this guards against is quiet. The role select only ever holds
  // the three built-ins, so an unknown role matches no option and renders as
  // whatever the browser picks — usually the first one, which reads as though
  // the member were an owner.
  it('shows a member holding an unrecognised role as having no access', async () => {
    fetchMembers.mockResolvedValue([
      { email: 'lead@test.com', role: 'owner' },
      { email: 'stranger@test.com', role: 'acme.support', unrecognised: true },
    ])

    const wrapper = mountPanel()
    await flushPromises()

    const text = wrapper.text()
    expect(text).toContain('acme.support')
    expect(text).toContain('no access')

    // One role control in the list, for the owner. The unrecognised member gets
    // a badge instead, because offering a control whose options cannot
    // represent the current value is how the wrong role gets written back.
    expect(wrapper.findAll('li select')).toHaveLength(1)
  })

  it('leaves a member holding a built-in role alone', async () => {
    fetchMembers.mockResolvedValue([{ email: 'lead@test.com', role: 'owner' }])

    const wrapper = mountPanel()
    await flushPromises()

    expect(wrapper.text()).not.toContain('no access')
    expect(wrapper.findAll('li select')).toHaveLength(1)
  })

  // Whoever cannot manage members still needs to see the state, because they
  // are often the person who reports it.
  it('shows the unrecognised state to a member who cannot manage', async () => {
    fetchMembers.mockResolvedValue([
      { email: 'stranger@test.com', role: 'acme.support', unrecognised: true },
    ])

    const wrapper = mountPanel(false)
    await flushPromises()

    expect(wrapper.text()).toContain('acme.support')
    expect(wrapper.text()).toContain('no access')
  })
})
