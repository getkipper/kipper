// @vitest-environment happy-dom
import { capabilitiesForRole } from '@/utils/testCapabilities'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import Services from '../Services.vue'
import * as servicesApi from '@/api/services'
import { useAuthStore } from '@/stores/auth'
import { useProjectsStore } from '@/stores/projects'
import type { Project } from '@/api/projects'
import type { ServiceResources } from '@/api/services'

vi.mock('@/api/client', () => ({
  default: {
    get: vi.fn().mockResolvedValue({ data: {} }),
    put: vi.fn().mockResolvedValue({ data: {} }),
    post: vi.fn().mockResolvedValue({ data: {} }),
    delete: vi.fn().mockResolvedValue({ data: {} }),
  },
}))
vi.mock('@/api/services')
vi.mock('@/composables/useResourceUsage', () => ({
  useResourceUsage: () => ({ data: { value: null }, loading: { value: false }, error: { value: null }, refresh: vi.fn() }),
}))
vi.mock('@/api/projects', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/projects')>()),
  fetchProjects: vi.fn().mockResolvedValue([]),
}))

const NS = 'shop-test'
const Mi = 1024 ** 2

interface ServicesVm {
  svcMemoryLimit: string
  showInfo: (name: string, namespace: string) => Promise<void>
  switchSvcTab: (tab: string) => Promise<void>
  requestSvcMemoryApply: (bytes: number) => void | Promise<void>
  saveSvcResources: () => Promise<void>
}

async function mountServices() {
  setActivePinia(createPinia())
  useAuthStore().role = 'member'
  const projects = useProjectsStore()
  projects.globalNamespace = NS
  projects.projects = [{
    name: 'shop', role: 'deployer', capabilities: capabilitiesForRole('deployer'), env_limit: 3,
    environments: [{ name: 'test', namespace: NS, apps: [], status: 'active', order: '0', owned: true }],
  } as Project]
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServiceInfo).mockImplementation(async name => ({ name }) as never)
  vi.mocked(servicesApi.updateServiceResources).mockResolvedValue()
  const wrapper = mount(Services, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
  await flushPromises()
  return wrapper.vm as unknown as ServicesVm
}

beforeEach(() => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

describe('service resources', () => {
  // A slow read for one service must not stand in for the next one's.
  it('sends nothing to a service built from another service\'s late answer', async () => {
    const vm = await mountServices()
    let answerA: (r: ServiceResources) => void = () => {}
    vi.mocked(servicesApi.fetchServiceResources).mockImplementationOnce(() => new Promise(resolve => { answerA = resolve }))

    await vm.showInfo('a', NS)
    void vm.switchSvcTab('resources')
    await flushPromises()

    vi.mocked(servicesApi.fetchServiceResources).mockRejectedValue(new Error('network'))
    await vm.showInfo('b', NS)
    await vm.switchSvcTab('resources')
    // An older server's answer for A arrives after B's read failed.
    answerA({ memory_limit: '256Mi', memory_request: '256Mi', cpu_limit: '100m', cpu_request: '100m' })
    await flushPromises()

    await vm.requestSvcMemoryApply(1024 * Mi)
    await vm.saveSvcResources()
    await flushPromises()

    expect(servicesApi.updateServiceResources).not.toHaveBeenCalled()
  })

  // Resources read for the service on screen let a change through, carrying
  // only the resource that changed.
  it('sends only the changed resource once the service was read', async () => {
    const vm = await mountServices()
    vi.mocked(servicesApi.fetchServiceResources).mockResolvedValue({
      memory_limit: '256Mi', memory_request: '256Mi', cpu_limit: '100m', cpu_request: '100m', partial_edits: true,
    })
    await vm.showInfo('b', NS)
    await vm.switchSvcTab('resources')

    await vm.requestSvcMemoryApply(1024 * Mi)
    // A successful save then polls the rollout; only the write matters here.
    void vm.saveSvcResources()
    await flushPromises()

    expect(servicesApi.updateServiceResources).toHaveBeenCalledWith('b', NS, { memory_request: '1Gi', memory_limit: '1Gi' })
  })

  // Two reads of the same service can overlap. Only the newest may change
  // anything: an older one failing late must not blank what the newer one read.
  it('ignores an older read of the same service that fails late', async () => {
    const vm = await mountServices()
    await vm.showInfo('b', NS)
    let failOlder: (e: Error) => void = () => {}
    vi.mocked(servicesApi.fetchServiceResources)
      .mockImplementationOnce(() => new Promise((_, reject) => { failOlder = reject }))
      .mockResolvedValue({ memory_limit: '256Mi', memory_request: '256Mi', cpu_limit: '1', cpu_request: '1', partial_edits: true })

    void vm.switchSvcTab('resources')
    await vm.switchSvcTab('connection')
    await vm.switchSvcTab('resources')
    failOlder(new Error('network'))
    await flushPromises()

    await vm.requestSvcMemoryApply(1024 * Mi)
    void vm.saveSvcResources()
    await flushPromises()

    expect(servicesApi.updateServiceResources).toHaveBeenCalledWith('b', NS, { memory_request: '1Gi', memory_limit: '1Gi' })
  })

  describe('after a resize', () => {
    const detail = (size: string, pending: boolean) => ({
      mode: 'fixed' as const,
      request: { value: '1Gi', source: 'user' as const },
      limit: { value: '1Gi', source: 'user' as const },
      live: { request: size, limit: size },
      pending,
    })
    const cpu = {
      mode: 'fixed' as const,
      request: { value: '1', source: 'user' as const },
      limit: { value: '1', source: 'user' as const },
      live: { request: '1', limit: '1' },
      pending: false,
    }
    const base = { cpu_limit: '1', cpu_request: '1', partial_edits: true, cpu }
    const before = { ...base, memory_limit: '256Mi', memory_request: '256Mi', memory: detail('256Mi', false) }
    const applying = { ...base, memory_limit: '256Mi', memory_request: '256Mi', memory: detail('256Mi', true) }
    const applied = { ...base, memory_limit: '1Gi', memory_request: '1Gi', memory: detail('1Gi', false) }

    async function resize() {
      const vm = await mountServices()
      vi.mocked(servicesApi.fetchServiceResources).mockResolvedValue(before)
      vi.mocked(servicesApi.fetchRolloutStatus).mockResolvedValue({ ready: true } as never)
      await vm.showInfo('b', NS)
      await vm.switchSvcTab('resources')
      await vm.requestSvcMemoryApply(1024 * Mi)
      vi.mocked(servicesApi.fetchServiceResources).mockResolvedValue(applying)
      const saving = vm.saveSvcResources()
      await vi.advanceTimersByTimeAsync(0)
      return { vm: vm as ServicesVm & { svcRolloutPhase: string }, saving }
    }

    beforeEach(() => { vi.useFakeTimers() })
    afterEach(() => { vi.useRealTimers() })

    // A healthy StatefulSet reports ready before its reconciler has applied
    // the edit, so ready alone proves nothing. The panel keeps reading until
    // the server says the container has the saved size.
    it('waits for the saved size, not just a ready rollout', async () => {
      const { vm, saving } = await resize()
      await vi.advanceTimersByTimeAsync(5000)
      expect(vm.svcMemoryLimit).toBe('256Mi')
      expect(vm.svcRolloutPhase).toBe('restarting')

      vi.mocked(servicesApi.fetchServiceResources).mockResolvedValue(applied)
      await vi.advanceTimersByTimeAsync(5000)
      await saving
      expect(vm.svcMemoryLimit).toBe('1Gi')
      expect(vm.svcRolloutPhase).toBe('done')
    })

    // A failed read says nothing about the container, so it cannot finish
    // the wait.
    it('keeps waiting through a failed read', async () => {
      const { vm, saving } = await resize()
      vi.mocked(servicesApi.fetchServiceResources).mockRejectedValue(new Error('network'))
      await vi.advanceTimersByTimeAsync(5000)
      expect(vm.svcRolloutPhase).toBe('restarting')

      vi.mocked(servicesApi.fetchServiceResources).mockResolvedValue(applied)
      await vi.advanceTimersByTimeAsync(5000)
      await saving
      expect(vm.svcMemoryLimit).toBe('1Gi')
      expect(vm.svcRolloutPhase).toBe('done')
    })

    // The wait belongs to the service that was saved. Opening another one
    // ends it, whenever that happens, and that service's resources or the
    // saved one's rollout never finish it on the other's panel.
    async function openA() {
      const vm = await mountServices() as ServicesVm & { svcRolloutPhase: string }
      vi.mocked(servicesApi.fetchServiceResources).mockResolvedValue(before)
      vi.mocked(servicesApi.fetchRolloutStatus).mockResolvedValue({ ready: true } as never)
      await vm.showInfo('a', NS)
      await vm.switchSvcTab('resources')
      await vm.requestSvcMemoryApply(1024 * Mi)
      return vm
    }
    async function openB(vm: ServicesVm) {
      await vm.showInfo('c', NS)
      await vm.switchSvcTab('resources')
    }

    it('ends the wait when another service is opened during the save', async () => {
      const vm = await openA()
      let finishPut: () => void = () => {}
      vi.mocked(servicesApi.updateServiceResources).mockImplementationOnce(() => new Promise<void>(resolve => { finishPut = resolve }))
      const saving = vm.saveSvcResources()
      await vi.advanceTimersByTimeAsync(0)

      await openB(vm)
      finishPut()
      await vi.advanceTimersByTimeAsync(0)
      expect(vm.svcRolloutPhase).toBe('idle')
      await vi.advanceTimersByTimeAsync(10000)
      await saving
      expect(servicesApi.fetchRolloutStatus).not.toHaveBeenCalled()
    })

    it('ends the wait when another service is opened between polls', async () => {
      const vm = await openA()
      vi.mocked(servicesApi.fetchServiceResources).mockResolvedValue(applying)
      const saving = vm.saveSvcResources()
      await vi.advanceTimersByTimeAsync(0)

      vi.mocked(servicesApi.fetchServiceResources).mockResolvedValue(before)
      await openB(vm)
      await vi.advanceTimersByTimeAsync(10000)
      await saving
      expect(servicesApi.fetchRolloutStatus).not.toHaveBeenCalled()
      expect(vm.svcRolloutPhase).toBe('idle')
    })

    it('ends the wait when another service is opened during the rollout check', async () => {
      const vm = await openA()
      vi.mocked(servicesApi.fetchServiceResources).mockResolvedValue(applied)
      let answerRollout: (s: never) => void = () => {}
      vi.mocked(servicesApi.fetchRolloutStatus).mockImplementationOnce(() => new Promise(resolve => { answerRollout = resolve }))
      const saving = vm.saveSvcResources()
      await vi.advanceTimersByTimeAsync(5000)

      await openB(vm)
      answerRollout({ ready: true } as never)
      await vi.advanceTimersByTimeAsync(0)
      await saving
      expect(vm.svcRolloutPhase).toBe('idle')
    })

    // A ready answer read before the new template was in place describes the
    // old pods. The one that finishes the wait comes after the read that saw
    // the saved size.
    it('asks whether the rollout is ready only after the saved size is in place', async () => {
      const { saving } = await resize()
      vi.mocked(servicesApi.fetchServiceResources).mockResolvedValue(applied)
      await vi.advanceTimersByTimeAsync(5000)
      await saving
      const lastRead = Math.max(...vi.mocked(servicesApi.fetchServiceResources).mock.invocationCallOrder)
      const lastReady = Math.max(...vi.mocked(servicesApi.fetchRolloutStatus).mock.invocationCallOrder)
      expect(lastReady).toBeGreaterThan(lastRead)
    })
  })
})
