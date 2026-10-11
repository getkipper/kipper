// @vitest-environment happy-dom
import { beforeEach, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import FunctionForm from '../FunctionForm.vue'
import * as inlineApi from '@/api/inline-functions'
import * as servicesApi from '@/api/services'

const toast = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }))
vi.mock('@/composables/useToast', () => ({ useToast: () => toast }))
vi.mock('vue-router', () => ({
  useRoute: () => ({ name: 'function-new', params: {}, query: {} }),
  useRouter: () => ({ replace: vi.fn(), push: vi.fn() }),
}))
vi.mock('@/api/inline-functions', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/inline-functions')>()),
  createInlineFunction: vi.fn(),
}))
vi.mock('@/api/services', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/services')>()),
  fetchServices: vi.fn().mockResolvedValue([]),
}))

beforeEach(() => {
  vi.clearAllMocks()
  setActivePinia(createPinia())
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
})

it('says when a binding database was not created with the function', async () => {
  vi.mocked(inlineApi.createInlineFunction).mockResolvedValue({
    status: 'created', name: 'sync', runtime: 'node', url: '',
    binding_warnings: ['db: database "sync_db" was not created in db: pod not running'],
  })
  const w = mount(FunctionForm, { global: { stubs: { RouterLink: true, Codemirror: true, AIChat: true } } })
  await flushPromises()
  const vm = w.vm as unknown as { name: string; code: string; save: () => Promise<void> }
  vm.name = 'sync'
  vm.code = 'module.exports = () => 1'

  await vm.save()
  await flushPromises()

  expect(toast.error).toHaveBeenCalledWith(expect.stringContaining('"sync_db" was not created'))
  expect(toast.success).not.toHaveBeenCalled()
})
