import { describe, expect, it } from 'vitest'
import { useResourceForm, type LoadedResources } from '../useResourceForm'

const fixed256: LoadedResources = {
  memory_request: '256Mi', memory_limit: '256Mi', cpu_request: '100m', cpu_limit: '100m', partial_edits: true,
  memory: { mode: 'fixed', request: { value: '256Mi', source: 'user' }, limit: { value: '256Mi', source: 'user' }, live: { request: '256Mi', limit: '256Mi' }, pending: false },
}

describe('useResourceForm', () => {
  it('sends only the resource that changed', () => {
    const f = useResourceForm()
    f.load(fixed256)
    f.memoryLimit.value = '512Mi'
    const pending = f.pendingEdit()
    expect(pending).not.toBe('unchanged')
    expect(pending).not.toBe('unknown')
    expect((pending as { edit: unknown }).edit).toEqual({ memory_request: '512Mi', memory_limit: '512Mi' })
  })

  // After a simple-mode save the fields hold what was saved, so opening the
  // advanced fields and saving again changes nothing.
  it('shows what was saved, so a later switch to advanced mode is not an edit', () => {
    const f = useResourceForm()
    f.load(fixed256)
    f.memoryLimit.value = '512Mi'
    const pending = f.pendingEdit() as { values: Parameters<typeof f.saved>[0] }
    f.saved(pending.values)

    f.advanced.value = true
    expect(f.memoryRequest.value).toBe('512Mi')
    expect(f.pendingEdit()).toBe('unchanged')
  })

  it('refuses to send a change when the current resources were never read', () => {
    const f = useResourceForm()
    f.load(null)
    f.memoryLimit.value = '512Mi'
    expect(f.pendingEdit()).toBe('unknown')
  })

  it('has nothing to send after a failed read the user left alone', () => {
    const f = useResourceForm()
    f.load(null)
    expect(f.pendingEdit()).toBe('unchanged')
  })

  it('sends the untouched resource along to an older server', () => {
    const f = useResourceForm()
    f.load({ memory_request: '256Mi', memory_limit: '256Mi', cpu_request: '100m', cpu_limit: '100m' })
    f.memoryLimit.value = '512Mi'
    expect((f.pendingEdit() as { edit: unknown }).edit).toEqual({
      memory_request: '512Mi', memory_limit: '512Mi', cpu_request: '100m', cpu_limit: '100m',
    })
  })

  // A current server that could not build the details still takes partial
  // edits; sending the untouched CPU would make its value the user's.
  it('sends only the change to a current server that returned no details', () => {
    const f = useResourceForm()
    f.load({ memory_request: '256Mi', memory_limit: '256Mi', cpu_request: '100m', cpu_limit: '100m', partial_edits: true })
    f.memoryLimit.value = '512Mi'
    expect((f.pendingEdit() as { edit: unknown }).edit).toEqual({ memory_request: '512Mi', memory_limit: '512Mi' })
  })
})
