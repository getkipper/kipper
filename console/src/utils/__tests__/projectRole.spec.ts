import { describe, it, expect } from 'vitest'

import type { Project } from '@/api/projects'
import { claimantsInNamespace, projectInNamespace } from '../projectRole'
import { can } from '@/utils/capabilities'
import { capabilitiesForRole } from '@/utils/testCapabilities'

function project(name: string, role: Project['role'], namespaces: string[], owned = true): Project {
  return {
    name,
    role,
    capabilities: capabilitiesForRole(role),
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

// The API gates the env routes on this role rather than on the cluster-wide
// one, so reading the cluster role here denied people access the server grants.

// Namespace claims can collide: shop/prod and shop-prod/default both name
// shop-prod. Count claimants so the UI can distinguish missing from ambiguous
// ownership while projectInNamespace returns null for either.
describe('claimantsInNamespace', () => {
  it('counts the projects whose claim the server has not contradicted', () => {
    expect(claimantsInNamespace(projects, 'shop-prod')).toBe(1)
    expect(claimantsInNamespace(projects, 'scratch')).toBe(1)
    expect(claimantsInNamespace(projects, 'nobodys')).toBe(0)
  })

  it('counts two when two projects claim one namespace', () => {
    const collided: Project[] = [
      project('shop', 'viewer', ['shop-prod']),
      project('shop-prod', 'deployer', ['shop-prod']),
    ]
    expect(claimantsInNamespace(collided, 'shop-prod')).toBe(2)
  })

  it('does not count a claim the server marked contradicted', () => {
    const losing: Project[] = [project('shop', 'deployer', ['shop-prod'], false)]
    expect(claimantsInNamespace(losing, 'shop-prod')).toBe(0)
  })
})

// A single claimant resolves, and its capabilities are what gate the tab.
describe('projectInNamespace', () => {
  it('returns the one project that claims it, with what the caller may do there', () => {
    const resolved = projectInNamespace(projects, 'shop-prod')
    expect(resolved?.name).toBe('shop')
    expect(can(resolved, 'kipper.write')).toBe(true)
  })

  it('returns null for a namespace nothing the caller belongs to claims', () => {
    expect(projectInNamespace(projects, 'nobodys')).toBeNull()
  })
})

describe('a namespace two projects claim', () => {
  const collided: Project[] = [
    project('shop', 'viewer', ['shop-test', 'shop-prod']),
    project('shop-prod', 'deployer', ['shop-prod']),
  ]

  it('resolves to nothing, whichever order they arrive in', () => {
    expect((projectInNamespace(collided, 'shop-prod')?.role ?? null)).toBeNull()
    expect(projectInNamespace([...collided].reverse(), 'shop-prod')?.role ?? null).toBeNull()
    expect(can(projectInNamespace(collided, 'shop-prod'), 'kipper.write')).toBe(false)
  })

  it('leaves the namespaces only one of them claims alone', () => {
    expect((projectInNamespace(collided, 'shop-test')?.role ?? null)).toBe('viewer')
  })
})

// Counting claimants cannot detect a conflict on its own, because the response
// holds only the projects the caller belongs to. Someone who belongs to the
// losing project alone sees one claim and nothing to compare it against, so the
// server marks the claim it knows to be contradicted.
describe('a claim the server has contradicted', () => {
  it('does not count, even when it is the only one visible', () => {
    const losingOnly: Project[] = [project('shop', 'deployer', ['shop-prod'], false)]
    expect((projectInNamespace(losingOnly, 'shop-prod')?.role ?? null)).toBeNull()
    expect(can(projectInNamespace(losingOnly, 'shop-prod'), 'kipper.write')).toBe(false)
  })

  it('leaves the winner as the single claimant when both are visible', () => {
    const both: Project[] = [
      project('shop', 'viewer', ['shop-prod'], false),
      project('shop-prod', 'deployer', ['shop-prod'], true),
    ]
    expect((projectInNamespace(both, 'shop-prod')?.role ?? null)).toBe('deployer')
  })
})

// Reading and writing are different questions with different answers, and the
// API already answers them separately: a namespace-scoped GET needs membership,
// a mutation needs the deployer role.
