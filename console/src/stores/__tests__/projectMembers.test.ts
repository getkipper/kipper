import { setActivePinia, createPinia } from 'pinia'
import { describe, it, expect, beforeEach, vi } from 'vitest'
import { useProjectMembersStore } from '../projectMembers'
import * as api from '@/api/projects'
import type { ProjectMember } from '@/api/projects'

vi.mock('@/api/projects')

const member = (email: string): ProjectMember => ({ email, role: 'deployer' })

describe('projectMembers store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.resetAllMocks()
  })

  it('loads members and captures a load error', async () => {
    vi.mocked(api.fetchMembers).mockResolvedValueOnce([member('a@x.io')])
    const store = useProjectMembersStore()
    await store.loadMembers('shop')
    expect(store.members).toHaveLength(1)
    expect(store.error).toBeNull()

    vi.mocked(api.fetchMembers).mockRejectedValueOnce(new Error('boom'))
    await store.loadMembers('shop')
    expect(store.error).toBe('boom')
  })

  it('reloads after setting a role', async () => {
    vi.mocked(api.setMember).mockResolvedValue()
    vi.mocked(api.fetchMembers).mockResolvedValue([member('a@x.io'), member('b@x.io')])
    const store = useProjectMembersStore()
    await store.loadMembers('shop')
    await store.setMemberRole('shop', 'b@x.io', 'viewer')
    expect(api.setMember).toHaveBeenCalledWith('shop', 'b@x.io', 'viewer')
    expect(store.members).toHaveLength(2)
    expect(api.fetchMembers).toHaveBeenCalledTimes(2)
  })

  // Reloads rather than filtering locally. Dropping the row on the client shows
  // a removal the server may not have made, which is how an API that answered
  // "removed" and removed nobody went unnoticed: the row left the screen and the
  // member kept their access until someone reloaded the page.
  it('reloads after a removal so the list is the server\'s, not the client\'s', async () => {
    vi.mocked(api.fetchMembers)
      .mockResolvedValueOnce([member('a@x.io'), member('b@x.io')])
      .mockResolvedValueOnce([member('b@x.io')])
    vi.mocked(api.removeMember).mockResolvedValue()
    const store = useProjectMembersStore()
    await store.loadMembers('shop')
    await store.removeMember('shop', 'a@x.io')
    expect(store.members.map(m => m.email)).toEqual(['b@x.io'])
    expect(api.fetchMembers).toHaveBeenCalledTimes(2)
  })

  // The point of reloading: if the server did not remove them, the list says so.
  it('keeps a member the server did not remove', async () => {
    vi.mocked(api.fetchMembers)
      .mockResolvedValueOnce([member('a@x.io'), member('b@x.io')])
      .mockResolvedValueOnce([member('a@x.io'), member('b@x.io')])
    vi.mocked(api.removeMember).mockResolvedValue()
    const store = useProjectMembersStore()
    await store.loadMembers('shop')
    await store.removeMember('shop', 'a@x.io')
    expect(store.members.map(m => m.email)).toEqual(['a@x.io', 'b@x.io'])
  })

  it('propagates a mutation error to the caller', async () => {
    vi.mocked(api.removeMember).mockRejectedValueOnce(new Error('denied'))
    const store = useProjectMembersStore()
    await expect(store.removeMember('shop', 'a@x.io')).rejects.toThrow('denied')
  })

  it('drops a stale load that resolves after a newer one', async () => {
    // Project A resolves slowly; project B resolves first. The later A response
    // must not overwrite B's members in the shared store.
    let resolveA!: (v: ProjectMember[]) => void
    vi.mocked(api.fetchMembers)
      .mockImplementationOnce(() => new Promise(res => { resolveA = res }))
      .mockResolvedValueOnce([member('b@x.io')])

    const store = useProjectMembersStore()
    const slow = store.loadMembers('projectA')
    await store.loadMembers('projectB')
    expect(store.members.map(m => m.email)).toEqual(['b@x.io'])

    resolveA([member('a@x.io')])
    await slow
    expect(store.members.map(m => m.email)).toEqual(['b@x.io'])
    expect(store.loading).toBe(false)
  })

  it('ignores a removal that completes after a project switch', async () => {
    // Both projects contain the same email. The removal targeted project A, so
    // it must not drop the row from project B's visible list.
    let resolveRemove!: () => void
    vi.mocked(api.fetchMembers)
      .mockResolvedValueOnce([member('shared@x.io')])
      .mockResolvedValueOnce([member('shared@x.io'), member('b@x.io')])
    vi.mocked(api.removeMember).mockImplementationOnce(() => new Promise(res => { resolveRemove = () => res() }))

    const store = useProjectMembersStore()
    await store.loadMembers('projectA')
    const removal = store.removeMember('projectA', 'shared@x.io')
    await store.loadMembers('projectB')

    resolveRemove()
    await removal
    expect(store.members.map(m => m.email)).toEqual(['shared@x.io', 'b@x.io'])
  })

  it('clears the previous project rows while a new scope loads', async () => {
    let resolveB!: (v: ProjectMember[]) => void
    vi.mocked(api.fetchMembers)
      .mockResolvedValueOnce([member('a@x.io')])
      .mockImplementationOnce(() => new Promise(res => { resolveB = res }))

    const store = useProjectMembersStore()
    await store.loadMembers('projectA')
    const load = store.loadMembers('projectB')
    expect(store.members).toEqual([])
    expect(store.loading).toBe(true)

    resolveB([member('b@x.io')])
    await load
    expect(store.members.map(m => m.email)).toEqual(['b@x.io'])
  })
})
