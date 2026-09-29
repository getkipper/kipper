// @vitest-environment happy-dom
import { capabilitiesForRole } from '@/utils/testCapabilities'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import AppDetail from '../AppDetail.vue'
import ResourceControl from '../ResourceControl.vue'
import * as appsApi from '@/api/apps'
import * as projectsApi from '@/api/projects'
import { useAuthStore } from '@/stores/auth'
import { useProjectsStore } from '@/stores/projects'
import type { Project } from '@/api/projects'

vi.mock('@/api/client', () => ({
  default: {
    get: vi.fn().mockResolvedValue({ data: {} }),
    put: vi.fn().mockResolvedValue({ data: {} }),
    post: vi.fn().mockResolvedValue({ data: {} }),
    delete: vi.fn().mockResolvedValue({ data: {} }),
  },
}))
vi.mock('@/api/apps')
vi.mock('@/api/services')
vi.mock('@/api/database')
vi.mock('@/api/files')
vi.mock('@/api/mode')
vi.mock('@/api/resources', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/resources')>()),
  fetchResourceUsage: vi.fn().mockResolvedValue({
    metrics_available: false,
    containers: [],
    totals: {
      memory_bytes: 0, memory_limit_bytes: 0, memory_request_bytes: 0,
      cpu_millis: 0, cpu_limit_millis: 0, cpu_request_millis: 0,
      pod_count: 0, container_count: 0, containers_with_metrics: 0,
    },
  }),
}))
vi.mock('@/api/projects', async importOriginal => ({
  ...(await importOriginal<typeof projectsApi>()),
  fetchProjects: vi.fn(),
}))

const Mi = 1024 ** 2

async function openResources(resources?: Awaited<ReturnType<typeof appsApi.fetchResources>> | Error) {
  setActivePinia(createPinia())
  useAuthStore().role = 'admin'
  useProjectsStore().projects = [{
    name: 'shop', role: 'owner', capabilities: capabilitiesForRole('owner'), env_limit: 3,
    environments: [{ name: 'prod', namespace: 'shop-prod', apps: [], status: 'active', order: '0', owned: true }],
  } as Project]
  vi.mocked(appsApi.fetchLogs).mockResolvedValue([])
  vi.mocked(appsApi.updateResources).mockResolvedValue()
  if (resources instanceof Error) {
    vi.mocked(appsApi.fetchResources).mockRejectedValue(resources)
  } else if (resources) {
    vi.mocked(appsApi.fetchResources).mockResolvedValue(resources)
  } else vi.mocked(appsApi.fetchResources).mockResolvedValue({
    memory_request: '512Mi', memory_limit: '2Gi', cpu_request: '100m', cpu_limit: '100m', partial_edits: true,
    memory: {
      mode: 'bounded',
      request: { value: '512Mi', source: 'user' },
      limit: { value: '2Gi', source: 'user' },
      live: { request: '768Mi', limit: '2Gi' },
      pending: false,
    },
    cpu: {
      mode: 'automatic',
      request: { value: '', source: 'unset' },
      limit: { value: '', source: 'unset' },
      live: { request: '200m', limit: '200m' },
      pending: false,
    },
  })

  const wrapper = mount(AppDetail, {
    props: { appName: 'web', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(wrapper.vm as unknown as { activeTab: string }).activeTab = 'resources'
  await flushPromises()
  return wrapper
}

describe('AppDetail resources tab', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  it('says how each resource is sized', async () => {
    await openResources()
    expect(document.body.textContent).toContain('Memory is tuned between your 512Mi and 2Gi, running with 768Mi reserved, up to 2Gi.')
    expect(document.body.textContent).toContain('CPU is sized automatically, running at 200m.')
  })

  // Moving the memory slider must not send CPU: the displayed CPU value would
  // become the user's and Kipper would stop sizing it. With bounds in place
  // the request controls are open, so the slider moves the ceiling and keeps
  // the user's floor.
  it('saves only the resource the slider moved', async () => {
    const wrapper = await openResources()
    const memoryGauge = wrapper.findAllComponents(ResourceControl).find(c => c.props('kind') === 'memory')!
    memoryGauge.vm.$emit('apply', 1024 * Mi)
    await flushPromises()

    expect(appsApi.updateResources).toHaveBeenCalledWith('shop-prod', 'web', { memory_request: '512Mi', memory_limit: '1Gi' })
    const sent = vi.mocked(appsApi.updateResources).mock.calls[0][2]
    expect(Object.keys(sent)).not.toContain('cpu_limit')
    expect(Object.keys(sent)).not.toContain('cpu_request')
  })

  it('hands a resource back to automatic sizing', async () => {
    await openResources()
    const button = document.body.querySelector('[data-testid="resource-mode-automatic"]') as HTMLButtonElement
    button.click()
    await flushPromises()

    expect(appsApi.updateResources).toHaveBeenCalledWith('shop-prod', 'web', { memory_request: '', memory_limit: '' })
  })

  // An older console-api replaces all four values on every save; left out,
  // CPU would be cleared there.
  it('sends the untouched resource along to an older server', async () => {
    const wrapper = await openResources({ memory_request: '256Mi', memory_limit: '256Mi', cpu_request: '100m', cpu_limit: '100m' })
    const memoryGauge = wrapper.findAllComponents(ResourceControl).find(c => c.props('kind') === 'memory')!
    memoryGauge.vm.$emit('apply', 1024 * Mi)
    await flushPromises()

    expect(appsApi.updateResources).toHaveBeenCalledWith('shop-prod', 'web', {
      memory_request: '1Gi', memory_limit: '1Gi', cpu_request: '100m', cpu_limit: '100m',
    })
  })

  // A failed load says nothing about which server answered. An older one
  // would clear the CPU a partial save leaves out, so nothing is written until
  // a read succeeds.
  it('writes nothing while the current resources cannot be read', async () => {
    const wrapper = await openResources(new Error('network'))
    const memoryGauge = wrapper.findAllComponents(ResourceControl).find(c => c.props('kind') === 'memory')!
    memoryGauge.vm.$emit('apply', 1024 * Mi)
    await flushPromises()

    expect(appsApi.fetchResources).toHaveBeenCalledTimes(2)
    expect(appsApi.updateResources).not.toHaveBeenCalled()
  })

  // An uncapped container is shown without a limit, not with the spec's value.
  it('shows no limit for a container that has none', async () => {
    const wrapper = await openResources({
      memory_request: '256Mi', memory_limit: '256Mi', cpu_request: '100m', cpu_limit: '500m', partial_edits: true,
      cpu: {
        mode: 'automatic',
        request: { value: '', source: 'unset' },
        limit: { value: '', source: 'unset' },
        live: { request: '100m', limit: '' },
      pending: false,
      },
      memory: {
        mode: 'automatic',
        request: { value: '', source: 'unset' },
        limit: { value: '', source: 'unset' },
        live: { request: '256Mi', limit: '256Mi' },
      pending: false,
      },
    })
    const cpuGauge = wrapper.findAllComponents(ResourceControl).find(c => c.props('kind') === 'cpu')!
    expect(cpuGauge.props('limit')).toBe(0)
  })

  describe('after a resize', () => {
    // fixed answers for an app whose spec fixes memory at size, running with
    // live, pending while the two differ.
    const fixed = (size: string, live: string) => ({
      memory_request: size, memory_limit: size, cpu_request: '100m', cpu_limit: '100m', partial_edits: true,
      memory: {
        mode: 'fixed' as const,
        request: { value: size, source: 'user' as const },
        limit: { value: size, source: 'user' as const },
        live: { request: live, limit: live },
        pending: size !== live,
      },
      cpu: {
        mode: 'fixed' as const,
        request: { value: '100m', source: 'user' as const },
        limit: { value: '100m', source: 'user' as const },
        live: { request: '100m', limit: '100m' },
        pending: false,
      },
    })
    const memoryGauge = (wrapper: Awaited<ReturnType<typeof openResources>>) =>
      wrapper.findAllComponents(ResourceControl).find(c => c.props('kind') === 'memory')!

    beforeEach(() => { vi.useFakeTimers() })
    afterEach(() => { vi.useRealTimers() })

    // The Deployment is resized after the save returns, so the first read
    // still shows the old container. The panel reads again until the server
    // says the container has the saved size, and then stops.
    it('shows the new live limit once the app has it', async () => {
      const wrapper = await openResources(fixed('256Mi', '256Mi'))
      vi.mocked(appsApi.fetchResources).mockResolvedValue(fixed('1Gi', '256Mi'))
      memoryGauge(wrapper).vm.$emit('apply', 1024 * Mi)
      await vi.advanceTimersByTimeAsync(0)
      expect(memoryGauge(wrapper).props('limit')).toBe(256 * Mi)

      vi.mocked(appsApi.fetchResources).mockResolvedValue(fixed('1Gi', '1Gi'))
      await vi.advanceTimersByTimeAsync(10000)
      expect(memoryGauge(wrapper).props('limit')).toBe(1024 * Mi)
      const reads = vi.mocked(appsApi.fetchResources).mock.calls.length
      await vi.advanceTimersByTimeAsync(30000)
      expect(vi.mocked(appsApi.fetchResources).mock.calls.length).toBe(reads)
    })

    // What was on screen before the save says nothing about the new size, so
    // a failed first read leaves the panel reading.
    it('keeps reading when the first read after the save fails', async () => {
      const wrapper = await openResources(fixed('256Mi', '256Mi'))
      vi.mocked(appsApi.fetchResources).mockRejectedValueOnce(new Error('network'))
      memoryGauge(wrapper).vm.$emit('apply', 1024 * Mi)
      await vi.advanceTimersByTimeAsync(0)

      vi.mocked(appsApi.fetchResources).mockResolvedValue(fixed('1Gi', '1Gi'))
      await vi.advanceTimersByTimeAsync(3000)
      expect(memoryGauge(wrapper).props('limit')).toBe(1024 * Mi)
    })

    // Typing while a read is in flight must survive that read's answer.
    it('keeps what the user types while a read is in flight', async () => {
      const wrapper = await openResources(fixed('256Mi', '256Mi'))
      vi.mocked(appsApi.fetchResources).mockResolvedValue(fixed('1Gi', '256Mi'))
      memoryGauge(wrapper).vm.$emit('apply', 1024 * Mi)
      await vi.advanceTimersByTimeAsync(0)

      let answer: (r: ReturnType<typeof fixed>) => void = () => {}
      vi.mocked(appsApi.fetchResources).mockImplementationOnce(() => new Promise(resolve => { answer = resolve }))
      await vi.advanceTimersByTimeAsync(3000)
      const vm = wrapper.vm as unknown as { memoryLimit: string }
      vm.memoryLimit = '3Gi'
      answer(fixed('1Gi', '1Gi'))
      await vi.advanceTimersByTimeAsync(0)

      expect(vm.memoryLimit).toBe('3Gi')
    })

    // The read straight after a save must not replace what the user types
    // while it is in flight.
    it('keeps what the user types during the first read after a save', async () => {
      const wrapper = await openResources(fixed('256Mi', '256Mi'))
      let answer: (r: ReturnType<typeof fixed>) => void = () => {}
      vi.mocked(appsApi.fetchResources).mockImplementationOnce(() => new Promise(resolve => { answer = resolve }))
      memoryGauge(wrapper).vm.$emit('apply', 1024 * Mi)
      await vi.advanceTimersByTimeAsync(0)

      const vm = wrapper.vm as unknown as { cpuLimit: string, memoryLimit: string, saveResources: () => Promise<void> }
      vm.cpuLimit = '500m'
      answer(fixed('1Gi', '256Mi'))
      await vi.advanceTimersByTimeAsync(0)
      expect(vm.cpuLimit).toBe('500m')
      // Memory was not touched, so it shows what the save set, and saving the
      // CPU change leaves memory alone.
      expect(vm.memoryLimit).toBe('1Gi')
      vi.mocked(appsApi.updateResources).mockClear()
      await vm.saveResources()
      expect(appsApi.updateResources).toHaveBeenCalledWith('shop-prod', 'web', { cpu_request: '100m', cpu_limit: '500m' })
    })

    // A save that finishes after the panel moved to another app reads
    // nothing for that app, so its fields stay as the user left them.
    it('leaves the next app alone when an earlier save finishes', async () => {
      const wrapper = await openResources(fixed('256Mi', '256Mi'))
      let finishPut: () => void = () => {}
      vi.mocked(appsApi.updateResources).mockImplementationOnce(() => new Promise<void>(resolve => { finishPut = resolve }))
      memoryGauge(wrapper).vm.$emit('apply', 1024 * Mi)
      await vi.advanceTimersByTimeAsync(0)

      await wrapper.setProps({ appName: 'checkout' })
      await vi.advanceTimersByTimeAsync(0)
      const vm = wrapper.vm as unknown as { cpuLimit: string }
      vm.cpuLimit = '500m'
      vi.mocked(appsApi.fetchResources).mockResolvedValue(fixed('2Gi', '2Gi'))
      finishPut()
      await vi.advanceTimersByTimeAsync(10000)
      expect(vm.cpuLimit).toBe('500m')
    })

    // The fields a save sent are not a new edit, so a failed read after
    // saving them still leads to another read.
    it('keeps reading after a failed read when typed fields were saved', async () => {
      const wrapper = await openResources(fixed('256Mi', '256Mi'))
      const vm = wrapper.vm as unknown as { memoryRequest: string, memoryLimit: string, saveResources: () => Promise<void> }
      vm.memoryRequest = '1Gi'
      vm.memoryLimit = '1Gi'
      vi.mocked(appsApi.fetchResources).mockRejectedValueOnce(new Error('network'))
      void vm.saveResources()
      await vi.advanceTimersByTimeAsync(0)

      vi.mocked(appsApi.fetchResources).mockResolvedValue(fixed('1Gi', '1Gi'))
      await vi.advanceTimersByTimeAsync(3000)
      expect(memoryGauge(wrapper).props('limit')).toBe(1024 * Mi)
    })

    // The next app's own first read still lands when an earlier save
    // finishes while it is in flight.
    it('lets the next app finish loading when an earlier save finishes', async () => {
      const wrapper = await openResources(fixed('256Mi', '256Mi'))
      let finishPut: () => void = () => {}
      vi.mocked(appsApi.updateResources).mockImplementationOnce(() => new Promise<void>(resolve => { finishPut = resolve }))
      memoryGauge(wrapper).vm.$emit('apply', 1024 * Mi)
      await vi.advanceTimersByTimeAsync(0)

      let answerNext: (r: ReturnType<typeof fixed>) => void = () => {}
      vi.mocked(appsApi.fetchResources).mockImplementationOnce(() => new Promise(resolve => { answerNext = resolve }))
      await wrapper.setProps({ appName: 'checkout' })
      await vi.advanceTimersByTimeAsync(0)
      vi.mocked(appsApi.fetchResources).mockResolvedValue(fixed('2Gi', '2Gi'))
      finishPut()
      await vi.advanceTimersByTimeAsync(0)
      answerNext(fixed('2Gi', '2Gi'))
      await vi.advanceTimersByTimeAsync(0)

      const vm = wrapper.vm as unknown as { memoryLimit: string }
      expect(vm.memoryLimit).toBe('2Gi')
    })

    // A value typed back while the saved one is read in must still be
    // savable: the read moves what the fields are compared against.
    it('saves a value typed back during the read after a save', async () => {
      const wrapper = await openResources(fixed('256Mi', '256Mi'))
      const vm = wrapper.vm as unknown as { memoryRequest: string, memoryLimit: string, saveResources: () => Promise<void> }
      vm.memoryRequest = '1Gi'
      vm.memoryLimit = '1Gi'
      let answer: (r: ReturnType<typeof fixed>) => void = () => {}
      vi.mocked(appsApi.fetchResources).mockImplementationOnce(() => new Promise(resolve => { answer = resolve }))
      const saving = vm.saveResources()
      await vi.advanceTimersByTimeAsync(0)

      vm.memoryRequest = '256Mi'
      vm.memoryLimit = '256Mi'
      answer(fixed('1Gi', '256Mi'))
      await vi.advanceTimersByTimeAsync(0)
      await saving
      vi.mocked(appsApi.updateResources).mockClear()
      await vm.saveResources()

      expect(appsApi.updateResources).toHaveBeenCalledWith('shop-prod', 'web', { memory_request: '256Mi', memory_limit: '256Mi' })
    })

    // A field a read filled in is still untouched, so the next read may
    // replace it again rather than leave a change the user never made.
    it('keeps untouched fields following the server across reads', async () => {
      const wrapper = await openResources(fixed('256Mi', '256Mi'))
      vi.mocked(appsApi.fetchResources).mockResolvedValue(fixed('1Gi', '256Mi'))
      memoryGauge(wrapper).vm.$emit('apply', 1024 * Mi)
      await vi.advanceTimersByTimeAsync(0)

      vi.mocked(appsApi.fetchResources).mockResolvedValue(fixed('2Gi', '256Mi'))
      await vi.advanceTimersByTimeAsync(3000)
      const vm = wrapper.vm as unknown as { memoryLimit: string }
      expect(vm.memoryLimit).toBe('2Gi')
    })

    // The request fields stay open while the user is typing in them, even
    // when the answer would close them.
    it('keeps the request fields open while the user types in them', async () => {
      const wrapper = await openResources()
      let answer: (r: ReturnType<typeof fixed>) => void = () => {}
      vi.mocked(appsApi.fetchResources).mockImplementationOnce(() => new Promise(resolve => { answer = resolve }))
      memoryGauge(wrapper).vm.$emit('apply', 1024 * Mi)
      await vi.advanceTimersByTimeAsync(0)

      const vm = wrapper.vm as unknown as { memoryRequest: string, resourcesAdvanced: boolean }
      vm.memoryRequest = '768Mi'
      answer(fixed('1Gi', '1Gi'))
      await vi.advanceTimersByTimeAsync(0)
      expect(vm.resourcesAdvanced).toBe(true)
    })

    // A slider applies one resource. A change already typed for the other
    // stays in its field and can still be saved.
    it('keeps an unsaved CPU change through a memory slider save', async () => {
      const wrapper = await openResources(fixed('256Mi', '256Mi'))
      const vm = wrapper.vm as unknown as { cpuLimit: string, saveResources: () => Promise<void> }
      vm.cpuLimit = '500m'
      vi.mocked(appsApi.fetchResources).mockResolvedValue(fixed('1Gi', '1Gi'))
      memoryGauge(wrapper).vm.$emit('apply', 1024 * Mi)
      await vi.advanceTimersByTimeAsync(0)

      expect(vm.cpuLimit).toBe('500m')
      vi.mocked(appsApi.updateResources).mockClear()
      await vm.saveResources()
      expect(appsApi.updateResources).toHaveBeenCalledWith('shop-prod', 'web', { cpu_request: '100m', cpu_limit: '500m' })
    })

    // A read begun for an earlier save must not land while a later save is
    // being sent, or its values would look like the user's own edit.
    it('drops an earlier read that answers while a later save is sent', async () => {
      const wrapper = await openResources(fixed('256Mi', '256Mi'))
      vi.mocked(appsApi.fetchResources).mockRejectedValueOnce(new Error('network'))
      memoryGauge(wrapper).vm.$emit('apply', 1024 * Mi)
      await vi.advanceTimersByTimeAsync(0)

      let answerEarlier: (r: ReturnType<typeof fixed>) => void = () => {}
      vi.mocked(appsApi.fetchResources).mockImplementationOnce(() => new Promise(resolve => { answerEarlier = resolve }))
      await vi.advanceTimersByTimeAsync(3000)

      let finishPut: () => void = () => {}
      vi.mocked(appsApi.updateResources).mockImplementationOnce(() => new Promise<void>(resolve => { finishPut = resolve }))
      memoryGauge(wrapper).vm.$emit('apply', 2048 * Mi)
      await vi.advanceTimersByTimeAsync(0)
      answerEarlier(fixed('1Gi', '1Gi'))
      await vi.advanceTimersByTimeAsync(0)
      vi.mocked(appsApi.fetchResources).mockResolvedValue(fixed('2Gi', '2Gi'))
      finishPut()
      await vi.advanceTimersByTimeAsync(0)

      const vm = wrapper.vm as unknown as { memoryLimit: string, cpuLimit: string, saveResources: () => Promise<void> }
      expect(vm.memoryLimit).toBe('2Gi')
      vm.cpuLimit = '500m'
      vi.mocked(appsApi.updateResources).mockClear()
      await vm.saveResources()
      expect(appsApi.updateResources).toHaveBeenCalledWith('shop-prod', 'web', { cpu_request: '100m', cpu_limit: '500m' })
    })

    // A saved field takes the server's spelling of its value, so it does not
    // look like a change afterwards.
    it('shows a saved value as the server writes it', async () => {
      const wrapper = await openResources(fixed('256Mi', '256Mi'))
      const vm = wrapper.vm as unknown as { memoryRequest: string, memoryLimit: string, saveResources: () => Promise<void> }
      vm.memoryRequest = '1024Mi'
      vm.memoryLimit = '1024Mi'
      vi.mocked(appsApi.fetchResources).mockResolvedValue(fixed('1Gi', '1Gi'))
      await vm.saveResources()
      await vi.advanceTimersByTimeAsync(0)

      expect(vm.memoryLimit).toBe('1Gi')
    })

    // A field the save did not send, with no change of the user's in it,
    // follows the server even when someone else changed that value.
    it('shows the server value of a resource the save did not send', async () => {
      const wrapper = await openResources(fixed('256Mi', '256Mi'))
      const answer = fixed('1Gi', '1Gi')
      answer.cpu_request = '200m'
      answer.cpu_limit = '200m'
      vi.mocked(appsApi.fetchResources).mockResolvedValue(answer)
      memoryGauge(wrapper).vm.$emit('apply', 1024 * Mi)
      await vi.advanceTimersByTimeAsync(0)

      const vm = wrapper.vm as unknown as { cpuLimit: string }
      expect(vm.cpuLimit).toBe('200m')
    })

    // Reopening the tab during a save reads the old values in. They are
    // Kipper's, not the user's, so the save's own read still replaces them.
    it('shows the saved values after the tab is reopened during a save', async () => {
      const wrapper = await openResources(fixed('256Mi', '256Mi'))
      const vm = wrapper.vm as unknown as {
        memoryRequest: string, memoryLimit: string, cpuLimit: string, activeTab: string, saveResources: () => Promise<void>
      }
      vm.memoryRequest = '1Gi'
      vm.memoryLimit = '1Gi'
      let finishPut: () => void = () => {}
      vi.mocked(appsApi.updateResources).mockImplementationOnce(() => new Promise<void>(resolve => { finishPut = resolve }))
      const saving = vm.saveResources()
      await vi.advanceTimersByTimeAsync(0)

      vm.activeTab = 'logs'
      await vi.advanceTimersByTimeAsync(0)
      vm.activeTab = 'resources'
      await vi.advanceTimersByTimeAsync(0)
      vi.mocked(appsApi.fetchResources).mockResolvedValue(fixed('1Gi', '1Gi'))
      finishPut()
      await saving
      await vi.advanceTimersByTimeAsync(0)

      expect(vm.memoryLimit).toBe('1Gi')
      vm.cpuLimit = '500m'
      vi.mocked(appsApi.updateResources).mockClear()
      await vm.saveResources()
      expect(appsApi.updateResources).toHaveBeenCalledWith('shop-prod', 'web', { cpu_request: '100m', cpu_limit: '500m' })
    })

    // A save the server rejects wrote nothing, so the earlier save's resize
    // is still read until it lands, and the rejected value stays to fix.
    it('keeps reading an earlier resize after a rejected save', async () => {
      const wrapper = await openResources(fixed('256Mi', '256Mi'))
      vi.mocked(appsApi.fetchResources).mockResolvedValue(fixed('1Gi', '256Mi'))
      memoryGauge(wrapper).vm.$emit('apply', 1024 * Mi)
      await vi.advanceTimersByTimeAsync(0)

      const vm = wrapper.vm as unknown as { cpuLimit: string, saveResources: () => Promise<void> }
      vm.cpuLimit = 'oops'
      vi.mocked(appsApi.updateResources).mockRejectedValueOnce(new Error('400'))
      await vm.saveResources()
      vi.mocked(appsApi.fetchResources).mockResolvedValue(fixed('1Gi', '1Gi'))
      await vi.advanceTimersByTimeAsync(3000)

      expect(memoryGauge(wrapper).props('limit')).toBe(1024 * Mi)
      expect(vm.cpuLimit).toBe('oops')
    })

    // A read begun for an earlier save must not land over a later save's.
    it('keeps the later save when an earlier read answers last', async () => {
      const wrapper = await openResources(fixed('256Mi', '256Mi'))
      vi.mocked(appsApi.fetchResources).mockResolvedValue(fixed('1Gi', '256Mi'))
      memoryGauge(wrapper).vm.$emit('apply', 1024 * Mi)
      await vi.advanceTimersByTimeAsync(0)

      let answerEarlier: (r: ReturnType<typeof fixed>) => void = () => {}
      vi.mocked(appsApi.fetchResources).mockImplementationOnce(() => new Promise(resolve => { answerEarlier = resolve }))
      await vi.advanceTimersByTimeAsync(3000)

      vi.mocked(appsApi.fetchResources).mockResolvedValue(fixed('2Gi', '1Gi'))
      memoryGauge(wrapper).vm.$emit('apply', 2048 * Mi)
      await vi.advanceTimersByTimeAsync(0)
      answerEarlier(fixed('1Gi', '1Gi'))
      await vi.advanceTimersByTimeAsync(0)

      const vm = wrapper.vm as unknown as { memoryLimit: string }
      expect(vm.memoryLimit).toBe('2Gi')
      // Only the later save's reading goes on.
      const reads = vi.mocked(appsApi.fetchResources).mock.calls.length
      await vi.advanceTimersByTimeAsync(3000)
      expect(vi.mocked(appsApi.fetchResources).mock.calls.length).toBe(reads + 1)

      vi.mocked(appsApi.fetchResources).mockResolvedValue(fixed('2Gi', '2Gi'))
      await vi.advanceTimersByTimeAsync(3000)
      expect(memoryGauge(wrapper).props('limit')).toBe(2048 * Mi)
    })
  })

  // Reads after the save go on while the app is resized, but never replace a
  // change the user started typing.
  it('keeps what the user typed while the app is resized', async () => {
    vi.useFakeTimers()
    try {
      const wrapper = await openResources()
      const pendingAnswer = await appsApi.fetchResources('shop-prod', 'web')
      pendingAnswer.memory!.pending = true
      vi.mocked(appsApi.fetchResources).mockResolvedValue(pendingAnswer)
      const memoryGauge = wrapper.findAllComponents(ResourceControl).find(c => c.props('kind') === 'memory')!
      memoryGauge.vm.$emit('apply', 1024 * Mi)
      await vi.advanceTimersByTimeAsync(0)
      const reads = vi.mocked(appsApi.fetchResources).mock.calls.length

      const vm = wrapper.vm as unknown as { memoryLimit: string }
      vm.memoryLimit = '3Gi'
      await vi.advanceTimersByTimeAsync(10000)
      expect(vi.mocked(appsApi.fetchResources).mock.calls.length).toBeGreaterThan(reads)
      expect(vm.memoryLimit).toBe('3Gi')
    } finally {
      vi.useRealTimers()
    }
  })

  // The panel is reused across apps. Values read for one app must never be
  // sent to the next, even when the next app's read fails.
  it('sends nothing read for the previous app to the next one', async () => {
    const wrapper = await openResources({ memory_request: '256Mi', memory_limit: '256Mi', cpu_request: '100m', cpu_limit: '100m' })
    vi.mocked(appsApi.fetchResources).mockRejectedValue(new Error('network'))
    await wrapper.setProps({ appName: 'checkout' })
    await flushPromises()

    const memoryGauge = wrapper.findAllComponents(ResourceControl).find(c => c.props('kind') === 'memory')!
    memoryGauge.vm.$emit('apply', 1024 * Mi)
    await flushPromises()

    expect(appsApi.updateResources).not.toHaveBeenCalled()
  })

  // Values left on screen from the previous app or a failed read never go
  // out. A save that finds the app unread reads it and asks the user to apply
  // again from what is now shown.
  it('does not send an edit built before the app was read', async () => {
    const wrapper = await openResources()
    vi.mocked(appsApi.fetchResources)
      .mockRejectedValueOnce(new Error('network'))
      .mockResolvedValue({ memory_request: '1Gi', memory_limit: '1Gi', cpu_request: '1', cpu_limit: '1', partial_edits: true })
    await wrapper.setProps({ appName: 'checkout' })
    await flushPromises()

    const vm = wrapper.vm as unknown as { memoryLimit: string; memoryRequest: string; cpuLimit: string; saveResources: () => Promise<void> }
    expect(vm.cpuLimit).toBe('')
    vm.memoryRequest = '2Gi'
    vm.memoryLimit = '2Gi'
    await vm.saveResources()
    await flushPromises()

    expect(appsApi.updateResources).not.toHaveBeenCalled()
    expect(vm.cpuLimit).toBe('1')
  })
})
