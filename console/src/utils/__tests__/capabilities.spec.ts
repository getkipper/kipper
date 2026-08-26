import { describe, it, expect } from 'vitest'

import type { Project } from '@/api/projects'
import { can, canAll } from '../capabilities'

const project = (capabilities: string[]) => ({ capabilities }) as Pick<Project, 'capabilities'>

describe('can', () => {
  it('answers from what the server said the caller may do', () => {
    expect(can(project(['kipper.write']), 'kipper.write')).toBe(true)
    expect(can(project(['kipper.write']), 'members.manage')).toBe(false)
  })

  // The point of the change: a role this build has never heard of arrives with
  // its capabilities like any other, so the controls it may use are offered and
  // the ones it may not are not. A role-name comparison could only ever have
  // answered for the three names it was written with.
  it('gates a role it has never heard of on what that role carries', () => {
    const auditor = { role: 'auditor', capabilities: ['project.read', 'workloads.read'] } as Project
    expect(can(auditor, 'workloads.read')).toBe(true)
    expect(can(auditor, 'kipper.write')).toBe(false)
  })

  // A member whose role means nothing to the cluster holds nothing, and the
  // console has to render that as no access rather than as a broken screen.
  it('grants nothing when the caller holds nothing', () => {
    expect(can(project([]), 'project.read')).toBe(false)
  })

  // Before the projects call returns there is no project, and offering a
  // control then would show it and then take it away.
  it('grants nothing before the project has loaded', () => {
    expect(can(null, 'project.read')).toBe(false)
    expect(can(undefined, 'project.read')).toBe(false)
  })

  // An older console talking to a newer cluster, or the reverse: a response
  // without the field is not a licence to assume access.
  it('grants nothing when the server did not say', () => {
    expect(can({} as Pick<Project, 'capabilities'>, 'project.read')).toBe(false)
  })
})

describe('canAll', () => {
  it('needs every one of them', () => {
    const p = project(['env.read', 'env.write'])
    expect(canAll(p, ['env.read', 'env.write'])).toBe(true)
    expect(canAll(p, ['env.read', 'env.reveal'])).toBe(false)
  })
})
