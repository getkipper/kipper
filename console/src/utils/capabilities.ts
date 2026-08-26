import type { Capability, Project } from '@/api/projects'

/**
 * Whether the current user may do this in this project.
 *
 * The server sends what they may do alongside the project, and it is the same
 * set the API gates its own routes on. Asking it here is what keeps a control
 * from being offered to somebody whose request would be refused, and — the
 * reason this replaced a role comparison — what lets a role the console has
 * never heard of render correctly: it arrives with its capabilities like any
 * other.
 *
 * A project the caller holds nothing in answers false to everything, which is
 * how a member whose role this build does not know is shown as having no
 * access rather than as having a broken screen.
 */
export function can(project: Pick<Project, 'capabilities'> | null | undefined, capability: Capability): boolean {
  return !!project?.capabilities?.includes(capability)
}

/** Whether the user may do every one of these. */
export function canAll(project: Pick<Project, 'capabilities'> | null | undefined, capabilities: Capability[]): boolean {
  return capabilities.every(c => can(project, c))
}
