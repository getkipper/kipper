import type { Project } from '@/api/projects'

/**
 * How many loaded projects claim this namespace.
 *
 * Zero and two mean different things, and `projectInNamespace` answers null to
 * both. Zero says no project the caller belongs to owns this namespace, which
 * for a non-admin is the membership being gone. Two says the response could not
 * settle who owns it — `namespaceOwners` failing leaves every declaration at its
 * optimistic `owned: true`, so a refresh can turn one claimant into two without
 * anything having changed — and that is not evidence of anything.
 */
export function claimantsInNamespace(projects: Project[], namespace: string): number {
  return projects.filter(p => p.environments?.some(e => e.namespace === namespace && e.owned)).length
}

/**
 * The project that owns an environment namespace, or null when nothing the
 * caller can see owns it.
 *
 * Two authorization models meet in the console and answer different questions.
 * The cluster-wide role from `/me` says what someone may do to the platform. A
 * project's capabilities say what they may do inside one project, and that is
 * what the API checks on every route under `/projects/{namespace}`.
 *
 * Reading the cluster role where the server reads the project denies people
 * access the server grants them: a cluster viewer who deploys to this project
 * is authorised by the API and was shown no tab to do it from.
 *
 * Null when no loaded project claims the namespace, which is also what a caller
 * sees before the projects store has loaded. Nothing is granted on a null: the
 * server resolves a non-admin through project membership alone, so with no
 * membership to read there is no access to offer.
 *
 * Null too when more than one project claims it, and that case is why this
 * reads every project rather than stopping at the first match. Two projects can
 * name one namespace — project `shop` with an environment `prod` alongside a
 * project called `shop-prod` — and the reconciler records that as a conflict
 * rather than resolving it. The server does not resolve it from these
 * declarations either: `ProjectAccessResolver` resolves through `nsowner`,
 * which treats the namespace's label as a hint the named project has to back
 * with a record of its own. Nothing in this response carries either, so picking
 * a claimant here is guessing, and guessing wrong hides a tab from someone
 * entitled to it or shows one whose requests come back 403.
 *
 * A claim is an emitted environment namespace the server has not contradicted,
 * and nothing else. The project's name is not a claim, and a declaration marked
 * `owned: false` is not one either.
 *
 * A cluster admin need not consult this at all: the server resolves them as
 * owner of every project.
 */
export function projectInNamespace(projects: Project[], namespace: string): Project | null {
  const claimants = projects.filter(
    p => p.environments?.some(e => e.namespace === namespace && e.owned),
  )
  return claimants.length === 1 ? claimants[0] : null
}

