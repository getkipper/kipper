// @vitest-environment happy-dom
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

const postMock = vi.hoisted(() => vi.fn())
vi.mock('axios', () => ({
  default: {
    create: () => ({
      post: postMock,
      get: vi.fn().mockResolvedValue({ data: {} }),
      interceptors: { request: { use: vi.fn() }, response: { use: vi.fn() } },
    }),
  },
}))
const next = 'https://grafana.example.com/explore'
vi.mock('vue-router', () => ({
  useRoute: () => ({ query: { next } }),
  useRouter: () => ({ push: vi.fn() }),
}))

import Login from '../Login.vue'

// Authentication is mocked; the store only decodes exp to schedule refreshes.
const unexpiredToken = ['header', btoa(JSON.stringify({ exp: 9999999999 })), 'sig'].join('.')

describe('Login with a host the user may not open', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    localStorage.clear()
    sessionStorage.clear()
    localStorage.setItem('kipper_token', unexpiredToken)
    postMock.mockReset()
    postMock.mockImplementation((url: string) => {
      if (url === 'auth/refresh') return Promise.resolve({ data: { token: unexpiredToken } })
      if (url === 'auth/ui-code') return Promise.reject({ response: { status: 403 } })
      return Promise.resolve({ data: {} })
    })
  })

  it('explains the refusal instead of sending the user back into the gate', async () => {
    const before = window.location.href
    const wrapper = mount(Login, { global: { stubs: { NoticeCallout: { template: '<div><slot /></div>' } } } })
    await flushPromises()

    expect(wrapper.text()).toContain("Your account can't open grafana.example.com")
    expect(wrapper.text()).not.toContain('blocking sign-in cookies')
    expect(window.location.href).toBe(before)
  })
})
