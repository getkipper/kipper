import type { Capability, Project } from '@/api/projects'
import { can } from '@/utils/capabilities'
import { projectInNamespace } from '@/utils/projectRole'
import { useAuthStore } from '@/stores/auth'
import { useProjectsStore } from '@/stores/projects'

/**
 * What the caller may do inside a project, for the screens that list resources
 * across projects.
 *
 * The cluster role and a project's capabilities answer different questions, and
 * every screen here asks the second one. A list of apps, services, volumes or
 * buckets spans projects, so the answer is per row: the namespace a row sits in
 * names the project whose capabilities decide it.
 *
 * A cluster admin passes everywhere, which is how the server resolves them —
 * as owner of every project.
 */
export function useCapabilities() {
  const auth = useAuthStore()
  const projects = useProjectsStore()

  /**
   * Whether the caller holds this capability in the project owning a namespace.
   *
   * False for a namespace no loaded project claims, and for one two projects
   * claim: projectInNamespace answers null to both, and neither is evidence of
   * access. False before the store has loaded, for the same reason.
   */
  function canInNamespace(namespace: string | null | undefined, capability: Capability): boolean {
    if (auth.isAdmin) return true
    if (!namespace) return false
    return can(projectInNamespace(projects.projects, namespace), capability)
  }

  /** The same question asked of a project the screen already has in hand. */
  function canInProject(project: Project | null | undefined, capability: Capability): boolean {
    return auth.isAdmin || can(project, capability)
  }

  return { canInNamespace, canInProject }
}
