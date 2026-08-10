import type { Project, ProjectRole } from '@/api/projects'

/**
 * The caller's role in the project that owns an environment namespace.
 *
 * Two role models meet in the console and answer different questions. The
 * cluster-wide role from `/me` says what someone may do to the platform. A
 * project role says what they may do inside one project, and it is what the API
 * checks on every route under `/projects/{namespace}` — `NamespaceScope`
 * resolves the namespace to its owning project and `RequireProjectRole`
 * evaluates the membership.
 *
 * Reading the cluster role where the server reads the project one denies people
 * access the server grants them: a cluster viewer who deploys to this project is
 * authorised by the API and was shown no tab to do it from.
 *
 * Returns null when no loaded project claims the namespace, which is also what a
 * caller sees before the projects store has loaded. Nothing is granted on a null:
 * the server resolves a non-admin through project membership alone, so with no
 * membership to read there is no access to offer, and offering it on the cluster
 * role instead put every write control in front of a cluster deployer who was a
 * viewer of the project.
 *
 * It also returns null when more than one project claims the namespace, and
 * that case is the reason this reads every project rather than stopping at the
 * first match. Two projects can name one namespace — project `shop` with an
 * environment `prod` alongside a project called `shop-prod` with a default
 * environment — and the reconciler records that as a conflict rather than
 * resolving it. The server does not resolve it from these declarations either:
 * `ProjectAccessResolver` reads the live namespace's own `kipper.run/project`
 * label, which says which project actually got there first. Nothing in this
 * response carries that label, so picking a claimant here is guessing, and
 * guessing wrong in one direction hides a tab from someone entitled to it and in
 * the other shows one whose requests come back 403. Deferring to the cluster
 * role is the answer that claims only what it knows.
 *
 * A claim is an emitted environment namespace that the server has not
 * contradicted, and nothing else.
 *
 * The project's name is not a claim: `ResolveNamespace` gives the bare project
 * name only to a `default` or unnamed environment, and the server emits that
 * namespace for it like any other, so matching on the name counts a second
 * claimant wherever a project is called after another's namespace. `shop`
 * owning `shop-prod` beside a project `shop-prod` owning `shop-prod-test` is a
 * valid, non-colliding pair.
 *
 * A declaration the server marks `owned: false` is not a claim either, and
 * counting claimants cannot stand in for that. This response holds only the
 * projects the caller belongs to, so in a conflict a caller who belongs to the
 * losing project alone sees exactly one claim and no ambiguity to detect. The
 * server decides ownership from the namespace's own label and says so per
 * environment, which is the only answer available that does not depend on
 * seeing every claimant.
 *
 * A cluster admin is the one caller that need not consult this at all: the
 * server resolves them as owner of every project.
 */
export function roleInNamespace(projects: Project[], namespace: string): ProjectRole | null {
  const claimants = projects.filter(
    p => p.environments?.some(e => e.namespace === namespace && e.owned),
  )
  return claimants.length === 1 ? claimants[0].role : null
}

/**
 * How many loaded projects claim this namespace.
 *
 * Zero and two mean different things, and `roleInNamespace` answers null to
 * both. Zero says no project the caller belongs to owns this namespace, which
 * for a non-admin is the membership being gone. Two says the response could not
 * settle who owns it — `namespaceOwners` failing leaves every declaration at its
 * optimistic `owned: true`, so a refresh can turn one claimant into two without
 * anything having changed — and that is not evidence of anything.
 */
export function claimantsInNamespace(projects: Project[], namespace: string): number {
  return projects.filter(p => p.environments?.some(e => e.namespace === namespace && e.owned)).length
}

/** Whether that role may change what runs in the namespace. */
export function canDeployInNamespace(projects: Project[], namespace: string): boolean {
  const role = roleInNamespace(projects, namespace)
  return role === 'owner' || role === 'deployer'
}

/**
 * Whether that role may read a project's configuration.
 *
 * Any role does: the API gates a namespace-scoped read on membership through
 * `NamespaceScope`, and a viewer is a member. Reading how an app is configured
 * is most of what reviewing a deployment consists of, so a viewer who can see
 * logs and nothing else is a role that does less than it is meant to.
 */
export function canReadInNamespace(projects: Project[], namespace: string): boolean {
  return roleInNamespace(projects, namespace) !== null
}
