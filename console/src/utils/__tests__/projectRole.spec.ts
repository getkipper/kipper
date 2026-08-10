import { describe, it, expect } from 'vitest'

import type { Project } from '@/api/projects'
import { canDeployInNamespace, canReadInNamespace, roleInNamespace } from '../projectRole'

function project(name: string, role: Project['role'], namespaces: string[], owned = true): Project {
  return {
    name,
    role,
    env_limit: 3,
    environments: namespaces.map(namespace => ({
      name: namespace, namespace, status: 'ready', apps: [], order: '0', owned,
    })),
  }
}

const projects: Project[] = [
  project('shop', 'deployer', ['shop-test', 'shop-prod']),
  project('billing', 'viewer', ['billing-prod']),
  // A default environment resolves to the project's own name, and the server
  // emits that namespace for it like any other.
  project('scratch', 'owner', ['scratch']),
]

describe('roleInNamespace', () => {
  it('finds the role through the environment that owns the namespace', () => {
    expect(roleInNamespace(projects, 'shop-prod')).toBe('deployer')
    expect(roleInNamespace(projects, 'billing-prod')).toBe('viewer')
  })

  it('matches a default environment, whose namespace is the project name', () => {
    expect(roleInNamespace(projects, 'scratch')).toBe('owner')
  })

  // A project's name is not a claim on its own. `shop` owning `shop-prod` beside
  // a project called `shop-prod` owning `shop-prod-test` is a valid pair the
  // server permits, and counting the name as a claim made it look ambiguous.
  it('does not count a project name that owns no such namespace', () => {
    const pair: Project[] = [
      project('shop', 'deployer', ['shop-prod']),
      project('shop-prod', 'viewer', ['shop-prod-test']),
    ]
    expect(roleInNamespace(pair, 'shop-prod')).toBe('deployer')
    expect(roleInNamespace(pair, 'shop-prod-test')).toBe('viewer')
  })

  it('reports nothing for a namespace no loaded project claims', () => {
    expect(roleInNamespace(projects, 'someone-elses-prod')).toBeNull()
    // Which is also the state before the projects store has loaded.
    expect(roleInNamespace([], 'shop-prod')).toBeNull()
  })
})

// The API gates the env routes on this role rather than on the cluster-wide
// one, so reading the cluster role here denied people access the server grants.
describe('canDeployInNamespace', () => {
  it('lets an owner and a deployer through', () => {
    expect(canDeployInNamespace(projects, 'shop-prod')).toBe(true)
    expect(canDeployInNamespace(projects, 'scratch')).toBe(true)
  })

  it('holds a project viewer back', () => {
    expect(canDeployInNamespace(projects, 'billing-prod')).toBe(false)
  })

  it('holds back a namespace it knows nothing about, so the cluster role decides', () => {
    expect(canDeployInNamespace(projects, 'someone-elses-prod')).toBe(false)
  })
})

// Two projects can emit one namespace: project "shop" with an environment
// "prod", and a project "shop-prod" with a default one. The reconciler records
// that as a conflict rather than resolving it, and the server decides from the
// live namespace's own kipper.run/project label — which is not in this
// response. Picking a claimant here would be guessing, and the tab would either
// hide from someone entitled to it or 403 when they used it.
describe('a namespace two projects claim', () => {
  const collided: Project[] = [
    project('shop', 'viewer', ['shop-test', 'shop-prod']),
    project('shop-prod', 'deployer', ['shop-prod']),
  ]

  it('resolves to nothing, whichever order they arrive in', () => {
    expect(roleInNamespace(collided, 'shop-prod')).toBeNull()
    expect(roleInNamespace([...collided].reverse(), 'shop-prod')).toBeNull()
    expect(canDeployInNamespace(collided, 'shop-prod')).toBe(false)
  })

  it('leaves the namespaces only one of them claims alone', () => {
    expect(roleInNamespace(collided, 'shop-test')).toBe('viewer')
  })
})

// Counting claimants cannot detect a conflict on its own, because the response
// holds only the projects the caller belongs to. Someone who belongs to the
// losing project alone sees one claim and nothing to compare it against, so the
// server marks the claim it knows to be contradicted.
describe('a claim the server has contradicted', () => {
  it('does not count, even when it is the only one visible', () => {
    const losingOnly: Project[] = [project('shop', 'deployer', ['shop-prod'], false)]
    expect(roleInNamespace(losingOnly, 'shop-prod')).toBeNull()
    expect(canDeployInNamespace(losingOnly, 'shop-prod')).toBe(false)
  })

  it('leaves the winner as the single claimant when both are visible', () => {
    const both: Project[] = [
      project('shop', 'viewer', ['shop-prod'], false),
      project('shop-prod', 'deployer', ['shop-prod'], true),
    ]
    expect(roleInNamespace(both, 'shop-prod')).toBe('deployer')
  })
})

// Reading and writing are different questions with different answers, and the
// API already answers them separately: a namespace-scoped GET needs membership,
// a mutation needs the deployer role.
describe('canReadInNamespace', () => {
  it('lets any member read, including a viewer', () => {
    expect(canReadInNamespace(projects, 'shop-prod')).toBe(true)
    expect(canReadInNamespace(projects, 'billing-prod')).toBe(true)
    expect(canDeployInNamespace(projects, 'billing-prod')).toBe(false)
  })

  it('refuses a namespace the caller has no role in', () => {
    expect(canReadInNamespace(projects, 'someone-elses-prod')).toBe(false)
  })

  it('refuses a contradicted claim, like the deploy check', () => {
    const losingOnly: Project[] = [project('shop', 'viewer', ['shop-prod'], false)]
    expect(canReadInNamespace(losingOnly, 'shop-prod')).toBe(false)
  })
})
