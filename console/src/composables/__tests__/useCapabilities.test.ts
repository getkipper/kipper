// @vitest-environment happy-dom
import { beforeEach, describe, expect, it } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

import { useCapabilities } from '../useCapabilities'
import { useAuthStore } from '@/stores/auth'
import { useProjectsStore } from '@/stores/projects'
import type { Project } from '@/api/projects'

function project(name: string, namespace: string, capabilities: string[]): Project {
  return {
    name,
    role: 'owner',
    capabilities,
    env_limit: 3,
    environments: [{ name: 'prod', namespace, apps: [], status: 'active', order: '0', owned: true }],
  }
}

describe('what the caller may do in the project owning a namespace', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
  })

  it('answers from the project that owns the namespace, not from the cluster role', () => {
    useAuthStore().role = 'viewer'
    useProjectsStore().projects = [project('shop', 'shop-prod', ['kipper.write'])]

    expect(useCapabilities().canInNamespace('shop-prod', 'kipper.write')).toBe(true)
  })

  it('refuses a cluster deployer who holds nothing in the project', () => {
    useAuthStore().role = 'deployer'
    useProjectsStore().projects = [project('shop', 'shop-prod', ['kipper.read'])]

    expect(useCapabilities().canInNamespace('shop-prod', 'kipper.write')).toBe(false)
  })

  it('refuses a namespace no loaded project claims', () => {
    useAuthStore().role = 'deployer'
    useProjectsStore().projects = []

    expect(useCapabilities().canInNamespace('someone-elses-ns', 'kipper.write')).toBe(false)
  })

  // Two projects can name one namespace, and the response carries nothing that
  // settles which owns it. Picking one would guess at authority.
  it('refuses a namespace two projects claim', () => {
    useAuthStore().role = 'viewer'
    useProjectsStore().projects = [
      project('shop', 'shop-prod', ['kipper.write']),
      project('shop-prod', 'shop-prod', ['kipper.write']),
    ]

    expect(useCapabilities().canInNamespace('shop-prod', 'kipper.write')).toBe(false)
  })

  it('refuses an empty namespace rather than reading the first project it finds', () => {
    useAuthStore().role = 'viewer'
    useProjectsStore().projects = [project('shop', 'shop-prod', ['kipper.write'])]

    expect(useCapabilities().canInNamespace('', 'kipper.write')).toBe(false)
    expect(useCapabilities().canInNamespace(null, 'kipper.write')).toBe(false)
  })

  it('passes a cluster admin everywhere, as the server resolves them', () => {
    useAuthStore().role = 'admin'
    useProjectsStore().projects = []

    expect(useCapabilities().canInNamespace('any-ns', 'kipper.write')).toBe(true)
    expect(useCapabilities().canInProject(null, 'project.delete')).toBe(true)
  })
})
