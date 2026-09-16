import type { Capability, Project } from '@/api/projects'
import { can } from '@/utils/capabilities'
import { projectInNamespace } from '@/utils/projectRole'
import { useAuthStore } from '@/stores/auth'
import { useProjectsStore } from '@/stores/projects'

/**
 * Resolve project capabilities for resource rows across namespaces, with a
 * cluster-admin override.
 */
export function useCapabilities() {
  const auth = useAuthStore()
  const projects = useProjectsStore()

  /**
   * Require a single loaded project claiming the namespace and the requested
   * capability, unless the caller is a cluster admin.
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
