import type { Capability, Project } from '@/api/projects'

/**
 * Check the server-supplied capability list, supporting roles unknown to this
 * console version. Missing projects or capabilities return false.
 */
export function can(project: Pick<Project, 'capabilities'> | null | undefined, capability: Capability): boolean {
  return !!project?.capabilities?.includes(capability)
}

/** Whether the user may do every one of these. */
export function canAll(project: Pick<Project, 'capabilities'> | null | undefined, capabilities: Capability[]): boolean {
  return capabilities.every(c => can(project, c))
}
