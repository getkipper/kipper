import type { Project } from '@/api/projects'

/** Count loaded projects claiming the namespace through an owned environment. */
export function claimantsInNamespace(projects: Project[], namespace: string): number {
  return projects.filter(p => p.environments?.some(e => e.namespace === namespace && e.owned)).length
}

/**
 * Return the sole loaded project claiming this namespace, or null for zero or
 * multiple claimants. Environment ownership determines claims; project names
 * alone do not. Callers can use claimantsInNamespace to distinguish absent
 * membership from ambiguous ownership.
 *
 * Project capabilities govern project actions independently of cluster roles.
 * Cluster admins receive access through the server's separate admin override.
 */
export function projectInNamespace(projects: Project[], namespace: string): Project | null {
  const claimants = projects.filter(
    p => p.environments?.some(e => e.namespace === namespace && e.owned),
  )
  return claimants.length === 1 ? claimants[0] : null
}

