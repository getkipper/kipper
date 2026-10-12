// @vitest-environment happy-dom
import { beforeEach, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import FunctionForm from '../FunctionForm.vue'
import * as inlineApi from '@/api/inline-functions'
import * as fnApi from '@/api/functions'
import * as servicesApi from '@/api/services'

const toast = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }))
vi.mock('@/composables/useToast', () => ({ useToast: () => toast }))
vi.mock('vue-router', () => ({
  useRoute: () => ({ name: 'function-edit', params: { fn: 'update-tlds' }, query: {} }),
  useRouter: () => ({ replace: vi.fn(), push: vi.fn() }),
}))
vi.mock('@/api/inline-functions', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/inline-functions')>()),
  getInlineFunctionCode: vi.fn(),
  updateInlineFunctionCode: vi.fn(),
}))
vi.mock('@/api/functions', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/functions')>()),
  fetchFunctions: vi.fn(),
  fetchFunctionEnv: vi.fn(),
  fetchFunctionSecretKeys: vi.fn(),
  fetchFunctionDependencies: vi.fn(),
  fetchFunctionResources: vi.fn(),
  updateFunctionEnv: vi.fn(),
  updateFunctionDependencies: vi.fn(),
  setFunctionSecrets: vi.fn(),
}))

vi.mock('@/api/services', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/services')>()),
  fetchServices: vi.fn(),
  fetchFunctionBindings: vi.fn(),
}))

beforeEach(() => {
  vi.clearAllMocks()
  setActivePinia(createPinia())
  vi.mocked(inlineApi.getInlineFunctionCode).mockResolvedValue({ name: 'update-tlds', runtime: 'python', code: 'print(0)', trigger: 'cron', schedule: '0 3 * * *' } as Awaited<ReturnType<typeof inlineApi.getInlineFunctionCode>>)
  vi.mocked(inlineApi.updateInlineFunctionCode).mockResolvedValue(undefined)
  vi.mocked(fnApi.fetchFunctions).mockResolvedValue([])
  vi.mocked(fnApi.fetchFunctionEnv).mockResolvedValue({ REGISTRAR: 'api.example.com' })
  vi.mocked(fnApi.fetchFunctionSecretKeys).mockResolvedValue([])
  vi.mocked(fnApi.fetchFunctionDependencies).mockResolvedValue({ requests: '2.32' })
  vi.mocked(servicesApi.fetchFunctionBindings).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
  vi.mocked(fnApi.fetchFunctionResources).mockRejectedValue(new Error('not needed'))
  vi.mocked(fnApi.updateFunctionEnv).mockResolvedValue(undefined)
  vi.mocked(fnApi.updateFunctionDependencies).mockResolvedValue(undefined)
})

async function editedForm() {
  const w = mount(FunctionForm, { global: { stubs: { RouterLink: true, Codemirror: true, AIChat: true } } })
  await flushPromises()
  return w.vm as unknown as { code: string; deps: { name: string; version: string }[]; save: () => Promise<void> }
}

it('saves only the parts that changed', async () => {
  const vm = await editedForm()
  vm.code = 'print(1)'

  await vm.save()
  await flushPromises()

  expect(inlineApi.updateInlineFunctionCode).toHaveBeenCalledOnce()
  expect(fnApi.updateFunctionEnv).not.toHaveBeenCalled()
  expect(fnApi.updateFunctionDependencies).not.toHaveBeenCalled()
  expect(toast.error).not.toHaveBeenCalled()
})

it('says what was saved when a later part fails', async () => {
  vi.mocked(fnApi.updateFunctionDependencies).mockRejectedValue({ response: { data: { error: 'failed to update dependencies' } } })
  const vm = await editedForm()
  vm.code = 'print(1)'
  vm.deps = [{ name: 'requests', version: '2.33' }]

  await vm.save()
  await flushPromises()

  expect(toast.error).toHaveBeenCalledWith('Saved code. Could not save dependencies: failed to update dependencies.')
  expect(toast.success).not.toHaveBeenCalled()
})

it('refuses to replace environment variables it could not read', async () => {
  vi.mocked(fnApi.fetchFunctionEnv).mockRejectedValue(new Error('timeout'))
  const vm = await editedForm() as unknown as { code: string; env: { key: string; value: string }[]; save: () => Promise<void> }
  vm.code = 'print(1)'
  vm.env = [{ key: 'NEW', value: '1' }]

  await vm.save()
  await flushPromises()

  expect(fnApi.updateFunctionEnv).not.toHaveBeenCalled()
  expect(toast.error).toHaveBeenCalledWith(expect.stringContaining('Could not save environment variables'))
})

it('leaves an unread part alone when it was not edited', async () => {
  vi.mocked(fnApi.fetchFunctionDependencies).mockRejectedValue(new Error('timeout'))
  const vm = await editedForm()
  vm.code = 'print(1)'

  await vm.save()
  await flushPromises()

  expect(fnApi.updateFunctionDependencies).not.toHaveBeenCalled()
  expect(toast.error).not.toHaveBeenCalled()
})

it('discards a dependency reload a save overtook', async () => {
  let answer: (deps: Record<string, string>) => void = () => {}
  const vm = await editedForm() as unknown as { deps: { name: string; version: string }[]; code: string; save: () => Promise<void>; reloadDeps: () => Promise<void> }
  vi.mocked(fnApi.fetchFunctionDependencies).mockImplementation(() => new Promise((resolve) => { answer = resolve }))
  const reload = vm.reloadDeps()
  vm.deps = [{ name: 'requests', version: '2.33' }]
  await vm.save()
  answer({ requests: '2.32' })
  await reload
  await flushPromises()

  expect(vm.deps).toEqual([{ name: 'requests', version: '2.33' }])
})

it('discards a dependency reload that answers during a save', async () => {
  let answer: (deps: Record<string, string>) => void = () => {}
  let secretWritten: () => void = () => {}
  const vm = await editedForm() as unknown as { deps: { name: string; version: string }[]; secretsToSet: { key: string; value: string }[]; save: () => Promise<void>; reloadDeps: () => Promise<void> }
  vi.mocked(fnApi.fetchFunctionDependencies).mockImplementation(() => new Promise((resolve) => { answer = resolve }))
  vi.mocked(fnApi.setFunctionSecrets).mockImplementation(() => new Promise((resolve) => { secretWritten = () => resolve(undefined) }))
  const reload = vm.reloadDeps()
  vm.deps = [{ name: 'requests', version: '2.33' }]
  vm.secretsToSet = [{ key: 'TOKEN', value: 'example' }]

  const saving = vm.save()
  await flushPromises()
  expect(fnApi.updateFunctionDependencies).toHaveBeenCalledWith(expect.anything(), 'update-tlds', { requests: '2.33' })
  answer({ requests: '2.32' })
  await reload
  secretWritten()
  await saving
  await flushPromises()

  expect(vm.deps).toEqual([{ name: 'requests', version: '2.33' }])
  vi.mocked(fnApi.updateFunctionDependencies).mockClear()
  vm.deps = [{ name: 'requests', version: '2.32' }]
  await vm.save()
  expect(fnApi.updateFunctionDependencies).toHaveBeenCalledWith(expect.anything(), 'update-tlds', { requests: '2.32' })
})
