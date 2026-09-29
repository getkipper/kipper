import { describe, expect, it } from 'vitest'
import { automatic, changedResources, confirm, fixedSize, forLegacyApi, formValues, isEmptyEdit, resourcesPending } from '../resourceEdits'
import type { ResourceDetail } from '@/api/resources'

const loaded = { memoryRequest: '512Mi', memoryLimit: '2Gi', cpuRequest: '100m', cpuLimit: '100m' }

describe('changedResources', () => {
  it('sends only the resource that changed, with both of its values', () => {
    expect(changedResources(loaded, { ...loaded, memoryRequest: '768Mi' })).toEqual({
      memory_request: '768Mi',
      memory_limit: '2Gi',
    })
  })

  it('sends nothing when nothing changed', () => {
    expect(isEmptyEdit(changedResources(loaded, { ...loaded }))).toBe(true)
  })

  it('sends both resources when both changed', () => {
    const edit = changedResources(loaded, { ...loaded, memoryLimit: '4Gi', cpuLimit: '1' })
    expect(Object.keys(edit).sort()).toEqual(['cpu_limit', 'cpu_request', 'memory_limit', 'memory_request'])
  })
})

describe('single-resource edits', () => {
  it('fixes one resource at one size', () => {
    expect(fixedSize('cpu', '250m')).toEqual({ cpu_request: '250m', cpu_limit: '250m' })
  })

  it('hands one resource back to automatic sizing', () => {
    expect(automatic('memory')).toEqual({ memory_request: '', memory_limit: '' })
  })

  it('confirms a held resource as it is', () => {
    const held: ResourceDetail = {
      mode: 'held',
      request: { value: '300Mi', source: 'held' },
      limit: { value: '1Gi', source: 'held' },
      live: { request: '300Mi', limit: '1Gi' },
      pending: false,
    }
    expect(confirm('memory', held)).toEqual({ memory_request: '300Mi', memory_limit: '1Gi' })
  })
})

describe('forLegacyApi', () => {
  // An older console-api replaces all four values on every save, so an edit
  // that leaves one out would clear it there.
  it('fills in the untouched resource for a server that replaces all four values', () => {
    expect(forLegacyApi({ memory_request: '1Gi', memory_limit: '1Gi' }, loaded)).toEqual({
      memory_request: '1Gi', memory_limit: '1Gi', cpu_request: '100m', cpu_limit: '100m',
    })
  })
})

describe('formValues', () => {
  it('turns a changed simple-mode limit into a fixed size', () => {
    const typed = { ...loaded, memoryLimit: '1Gi' }
    expect(formValues(loaded, typed, false)).toMatchObject({ memoryRequest: '1Gi', memoryLimit: '1Gi' })
  })

  // Switching the form to simple mode is not an edit: a CPU range the user did
  // not touch stays a range.
  it('keeps an untouched range when the form shows simple mode', () => {
    const ranged = { memoryRequest: '256Mi', memoryLimit: '256Mi', cpuRequest: '100m', cpuLimit: '1000m' }
    const typed = { ...ranged, memoryLimit: '512Mi' }
    const edit = changedResources(ranged, formValues(ranged, typed, false))
    expect(edit).toEqual({ memory_request: '512Mi', memory_limit: '512Mi' })
  })

  it('saves what advanced mode shows as typed', () => {
    const typed = { ...loaded, cpuRequest: '250m', cpuLimit: '1' }
    expect(formValues(loaded, typed, true)).toEqual(typed)
  })
})

describe('resourcesPending', () => {
  const detail = (pending: boolean): ResourceDetail => ({
    mode: 'fixed',
    request: { value: '1Gi', source: 'user' },
    limit: { value: '1Gi', source: 'user' },
    live: { request: '256Mi', limit: '256Mi' },
    pending,
  })

  it('is pending while either resource is', () => {
    expect(resourcesPending({ partial_edits: true, memory: detail(false), cpu: detail(true) })).toBe(true)
    expect(resourcesPending({ partial_edits: true, memory: detail(true), cpu: detail(false) })).toBe(true)
    expect(resourcesPending({ partial_edits: true, memory: detail(false), cpu: detail(false) })).toBe(false)
  })

  // An older server sends no details, so there is nothing to wait for.
  it('never waits on an older server', () => {
    expect(resourcesPending({})).toBe(false)
  })

  // A current server that could not read the workload says nothing about it.
  it('keeps waiting when a current server sends no details', () => {
    expect(resourcesPending({ partial_edits: true })).toBe(true)
    expect(resourcesPending({ partial_edits: true, memory: detail(false) })).toBe(true)
  })
})
