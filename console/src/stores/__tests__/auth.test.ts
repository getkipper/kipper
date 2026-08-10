// @vitest-environment happy-dom
import { setActivePinia, createPinia } from 'pinia'
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'

// axios.create binds methods at instance creation, so the transport is
// mocked at the module boundary: every instance the store (and the api
// client it imports) creates shares these fns.
const postMock = vi.hoisted(() => vi.fn().mockResolvedValue({ data: {} }))
const getMock = vi.hoisted(() => vi.fn().mockResolvedValue({ data: {} }))
vi.mock('axios', () => ({
  default: {
    create: () => ({
      post: postMock,
      get: getMock,
      interceptors: { request: { use: vi.fn() }, response: { use: vi.fn() } },
    }),
  },
}))
import { useAuthStore } from '../auth'

describe('auth store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    localStorage.clear()
  })

  it('starts unauthenticated when no token in localStorage', () => {
    const store = useAuthStore()
    expect(store.isAuthenticated).toBe(false)
    expect(store.token).toBeNull()
    expect(store.email).toBeNull()
  })

  it('restores token from localStorage', () => {
    localStorage.setItem('kipper_token', 'stored-token')
    localStorage.setItem('kipper_email', 'admin@test.com')
    const store = useAuthStore()
    expect(store.isAuthenticated).toBe(true)
    expect(store.token).toBe('stored-token')
    expect(store.email).toBe('admin@test.com')
  })

  it('login sets token and email', () => {
    const store = useAuthStore()
    store.login('new-token', 'user@test.com')
    expect(store.isAuthenticated).toBe(true)
    expect(store.token).toBe('new-token')
    expect(store.email).toBe('user@test.com')
    expect(localStorage.getItem('kipper_token')).toBe('new-token')
    expect(localStorage.getItem('kipper_email')).toBe('user@test.com')
  })

  it('logout clears token and email', () => {
    const store = useAuthStore()
    store.login('token', 'user@test.com')
    store.logout()
    expect(store.isAuthenticated).toBe(false)
    expect(store.token).toBeNull()
    expect(store.email).toBeNull()
    expect(localStorage.getItem('kipper_token')).toBeNull()
  })
})

describe('silent refresh', () => {
  function jwtWithExp(expSecondsFromNow: number): string {
    const payload = btoa(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + expSecondsFromNow }))
      .replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
    return `h.${payload}.s`
  }

  beforeEach(() => {
    setActivePinia(createPinia())
    localStorage.clear()
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.useRealTimers()
    postMock.mockReset()
    postMock.mockResolvedValue({ data: {} })
  })

  it('schedules a refresh before the token expires and applies the new token', async () => {
    const store = useAuthStore()
    const renewed = jwtWithExp(900)
    postMock.mockResolvedValue({ data: { token: renewed } })

    store.login(jwtWithExp(900), 'user@test.com')

    // 15m token, 2m lead: the timer must fire at ~13m, not at expiry.
    await vi.advanceTimersByTimeAsync(13 * 60 * 1000 + 1000)
    expect(postMock).toHaveBeenCalledWith('auth/refresh', null, expect.objectContaining({ withCredentials: true }))
    expect(store.token).toBe(renewed)
  })

  it('logs out when the refresh is rejected', async () => {
    const store = useAuthStore()
    postMock.mockRejectedValue(new Error('401'))

    store.login(jwtWithExp(900), 'user@test.com')
    await vi.advanceTimersByTimeAsync(14 * 60 * 1000)

    expect(store.isAuthenticated).toBe(false)
    expect(localStorage.getItem('kipper_token')).toBeNull()
  })

  it('arms the refresh cycle for a token restored from localStorage', async () => {
    localStorage.setItem('kipper_token', jwtWithExp(900))
    const renewed = jwtWithExp(900)
    postMock.mockResolvedValue({ data: { token: renewed } })

    const store = useAuthStore()
    await vi.advanceTimersByTimeAsync(13 * 60 * 1000 + 1000)

    expect(postMock).toHaveBeenCalled()
    expect(store.token).toBe(renewed)
  })

  it('never schedules for a malformed token', () => {
    localStorage.setItem('kipper_token', 'not-a-jwt')
    useAuthStore()
    expect(vi.getTimerCount()).toBe(0)
  })
})
