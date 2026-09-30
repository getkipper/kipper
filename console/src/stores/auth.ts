import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import axios from 'axios'
import client from '@/api/client'
import { openWithSSO } from '@/utils/sso'

// Separate axios instance for the unauthenticated auth routes (/auth/login,
// /auth/callback, /auth/refresh, /auth/logout) which live at the chi router
// root, not under the /api/v1 prefix the main client targets.
const authClient = axios.create({ baseURL: '/' })

// tokenExpiryMs reads the exp claim from a JWT payload for refresh
// scheduling. Scheduling only: the server verifies tokens, the SPA merely
// needs to know when to ask for a new one.
function tokenExpiryMs(token: string): number | null {
  try {
    const payload = JSON.parse(atob(token.split('.')[1].replace(/-/g, '+').replace(/_/g, '/')))
    return typeof payload.exp === 'number' ? payload.exp * 1000 : null
  } catch {
    return null
  }
}

// Start refreshing two minutes before expiry to allow time for the request.
const refreshLeadMs = 2 * 60 * 1000

// Cap delays at the signed 32-bit timer limit to prevent overflow and rapid refresh loops.
const maxTimerDelayMs = 2_147_483_647

export const useAuthStore = defineStore('auth', () => {
  const token = ref<string | null>(localStorage.getItem('kipper_token'))
  const email = ref<string | null>(localStorage.getItem('kipper_email'))
  const role = ref<string | null>(localStorage.getItem('kipper_role'))
  // Set only for admins and monitoring holders while Grafana's route is live.
  const grafanaUrl = ref<string | null>(localStorage.getItem('kipper_grafana_url'))

  const isAuthenticated = computed(() => !!token.value)
  const isAdmin = computed(() => role.value === 'admin')
  const isDeployer = computed(() => role.value === 'admin' || role.value === 'deployer')
  const isViewer = computed(() => !!role.value)

  let refreshTimer: ReturnType<typeof setTimeout> | null = null

  function setToken(newToken: string) {
    token.value = newToken
    localStorage.setItem('kipper_token', newToken)
    scheduleRefresh()
  }

  // Refresh before expiry, with a five-second minimum delay to avoid rapid
  // retries and a timer-safe maximum for tokens with distant expiry dates.
  function scheduleRefresh() {
    if (refreshTimer) {
      clearTimeout(refreshTimer)
      refreshTimer = null
    }
    if (!token.value) return
    const expiry = tokenExpiryMs(token.value)
    if (expiry === null) return
    const delay = Math.min(Math.max(expiry - Date.now() - refreshLeadMs, 5_000), maxTimerDelayMs)
    refreshTimer = setTimeout(() => {
      void refresh()
    }, delay)
  }

  // refresh renews the session through the HttpOnly cookie. The SPA never
  // sees the refresh token itself; a failed renewal means the session is
  // genuinely over, so the local session tears down.
  async function refresh(): Promise<boolean> {
    try {
      const { data } = await authClient.post<{ token: string }>('auth/refresh', null, {
        withCredentials: true,
      })
      setToken(data.token)
      return true
    } catch {
      logout()
      return false
    }
  }

  function login(newToken: string, userEmail: string) {
    email.value = userEmail
    localStorage.setItem('kipper_email', userEmail)
    setToken(newToken)
  }

  function setGrafanaUrl(url: string | null | undefined) {
    grafanaUrl.value = url || null
    if (grafanaUrl.value) {
      localStorage.setItem('kipper_grafana_url', grafanaUrl.value)
    } else {
      localStorage.removeItem('kipper_grafana_url')
    }
  }

  async function fetchRole() {
    try {
      const { data } = await client.get<{ email: string; role: string; grafanaUrl?: string }>('/me')
      role.value = data.role
      localStorage.setItem('kipper_role', data.role)
      setGrafanaUrl(data.grafanaUrl)
    } catch {
      role.value = 'admin' // fallback for pre-RBAC installs
    }
  }

  // fetchGrafanaUrl picks up a monitoring grant or revocation without a new
  // login. Any failure hides the link.
  async function fetchGrafanaUrl() {
    try {
      const { data } = await client.get<{ grafanaUrl?: string }>('/me')
      setGrafanaUrl(data.grafanaUrl)
    } catch {
      setGrafanaUrl(null)
    }
  }

  async function openGrafana(path = '/') {
    if (!grafanaUrl.value) return
    await openWithSSO(grafanaUrl.value.replace(/\/$/, '') + path, mintUICode)
  }

  function logout() {
    // Capture the bearer before tearing down: the backend needs it to
    // identify whose service-UI sessions to revoke.
    const bearer = token.value

    // Clear local session synchronously first. The UI flips to
    // "logged out" the instant the user clicks — no flash of
    // "still authenticated" while the backend call is in flight.
    token.value = null
    email.value = null
    role.value = null
    setGrafanaUrl(null)
    localStorage.removeItem('kipper_token')
    localStorage.removeItem('kipper_email')
    localStorage.removeItem('kipper_role')
    if (refreshTimer) {
      clearTimeout(refreshTimer)
      refreshTimer = null
    }

    // Fire-and-forget the backend call: it clears the HttpOnly refresh
    // cookie and, given the bearer, deletes this operator's service-UI
    // session records so every open service UI (MailHog inbox, etc.)
    // signs out within the record cache window. Best-effort: a failed
    // network call still leaves the local session torn down.
    authClient.post('auth/logout', null, {
      // HttpOnly cookies travel automatically; withCredentials
      // is needed for dev environments where the console and
      // API may sit on different ports.
      withCredentials: true,
      headers: bearer ? { Authorization: `Bearer ${bearer}` } : undefined,
    }).catch(() => {
      // ignore: the user is already logged out locally
    })
  }

  // Distinguish access denial from other SSO failures so callers can explain
  // the denial instead of repeatedly redirecting to login.
  async function requestUICode(host: string): Promise<{ code: string | null; forbidden: boolean }> {
    if (!token.value) return { code: null, forbidden: false }
    try {
      const { data } = await authClient.post<{ code: string }>(
        'auth/ui-code',
        { host },
        { headers: { Authorization: `Bearer ${token.value}` } },
      )
      return { code: data.code, forbidden: false }
    } catch (err) {
      const status = (err as { response?: { status?: number } })?.response?.status
      return { code: null, forbidden: status === 403 }
    }
  }

  // Return null on failure so callers can fall back to the login redirect.
  async function mintUICode(host: string): Promise<string | null> {
    return (await requestUICode(host)).code
  }

  // A page load with a stored token arms the refresh cycle immediately, so
  // a reopened tab keeps its session alive instead of dying at the stored
  // token's expiry.
  scheduleRefresh()

  return { token, email, role, grafanaUrl, isAuthenticated, isAdmin, isDeployer, isViewer, login, fetchRole, fetchGrafanaUrl, openGrafana, logout, refresh, mintUICode, requestUICode }
})
