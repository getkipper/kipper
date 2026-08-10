<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import NoticeCallout from '@/components/NoticeCallout.vue'
import { useAuthStore } from '@/stores/auth'
import axios from 'axios'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

const error = ref('')
const loading = ref(false)
// When set, the browser is dropping the per-host session cookie for this UI
// host, so the silent SSO keeps bouncing. We stop and tell the user.
const cookieBlockedHost = ref('')

const authClient = axios.create({ baseURL: '/' })

function firstQuery(value: unknown): string {
  if (Array.isArray(value)) return typeof value[0] === 'string' ? value[0] : ''
  return typeof value === 'string' ? value : ''
}

function hostOf(rawURL: string): string {
  try {
    return new URL(rawURL).host
  } catch {
    return ''
  }
}

function withSSOCode(rawURL: string, code: string): string {
  try {
    const u = new URL(rawURL)
    u.searchParams.set('kipper_sso', code)
    return u.toString()
  } catch {
    return rawURL
  }
}

// The silent SSO can loop when a browser refuses the per-host session
// cookie: open UI → gate bounces to login → mint code → open UI → cookie
// dropped → bounce again. A short-windowed per-host counter in sessionStorage
// caps that at three attempts before we surface an actionable message.
const bounceWindowMs = 60_000
const maxBounces = 3

function bounceKey(host: string): string {
  return `kipper_sso_bounce_${host}`
}

function bounceCount(host: string): number {
  const raw = sessionStorage.getItem(bounceKey(host))
  if (!raw) return 0
  try {
    const { n, t } = JSON.parse(raw) as { n: number; t: number }
    if (Date.now() - t > bounceWindowMs) return 0
    return n
  } catch {
    return 0
  }
}

function recordBounce(host: string): void {
  sessionStorage.setItem(bounceKey(host), JSON.stringify({ n: bounceCount(host) + 1, t: Date.now() }))
}

async function handleLogin() {
  loading.value = true
  error.value = ''

  try {
    // Forward ?next= to the backend so it survives the OIDC
    // state round-trip and the callback can route the user back
    // to a service-UI URL they opened while signed out (e.g.
    // `/login?next=https%3A%2F%2Fmailhog-blog-test.example.com%2F`).
    const next = firstQuery(route.query.next)
    const url = next ? `auth/login?next=${encodeURIComponent(next)}` : 'auth/login'
    const { data } = await authClient.get(url)
    window.location.href = data.url
  } catch {
    error.value = 'Failed to connect to identity provider'
    loading.value = false
  }
}

// openServiceUI mints a single-use SSO code for the target host and navigates
// there with it appended, so the gate can seat a per-host session cookie. On a
// mint failure it navigates without the code and lets the gate's redirect
// dance take over.
async function openServiceUI(nextURL: string) {
  const host = hostOf(nextURL)
  if (host && bounceCount(host) >= maxBounces) {
    cookieBlockedHost.value = host
    loading.value = false
    return
  }
  if (host) recordBounce(host)
  const code = host ? await auth.mintUICode(host) : null
  window.location.href = code ? withSSOCode(nextURL, code) : nextURL
}

onMounted(async () => {
  const code = firstQuery(route.query.code)
  const next = firstQuery(route.query.next)

  // OIDC callback: exchange the code, then hand off to the service UI (if the
  // login started from one) or the dashboard.
  if (code) {
    loading.value = true
    error.value = ''
    try {
      const state = firstQuery(route.query.state)
      const { data } = await authClient.post('auth/callback', { code, state })
      auth.login(data.token, data.email)
      await auth.fetchRole()
      // Backend `next` is the OIDC-state-decoded service UI URL. Absolute
      // cross-subdomain navigation goes through window.location (Vue Router
      // only handles in-app paths).
      if (data.next) {
        await openServiceUI(data.next)
      } else {
        router.push('/')
      }
    } catch {
      error.value = 'Authentication failed'
      loading.value = false
    }
    return
  }

  // Silent SSO: a signed-in operator opened a service UI, the gate bounced
  // them here with ?next=. Refresh to a fresh token (kills the stale-token
  // race), then mint a code and go straight back — no IdP round-trip.
  if (next && auth.isAuthenticated) {
    loading.value = true
    error.value = ''
    await auth.refresh()
    await openServiceUI(next)
  }
})
</script>

<template>
  <div class="flex min-h-screen items-center justify-center bg-slate-50 dark:bg-slate-950">
    <div class="w-full max-w-sm animate-fade-in">
      <!-- Logo -->
      <div class="mb-10 flex flex-col items-center">
        <img src="/logo-stacked-light.svg" alt="Kipper" class="mb-4 h-32 object-contain dark:hidden" />
        <img src="/logo-stacked-dark.svg" alt="Kipper" class="mb-4 hidden h-32 object-contain dark:block" />
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">
          Sign in to manage your cluster
        </p>
      </div>

      <NoticeCallout v-if="error" tone="danger" class="mb-4 px-4 py-3 text-sm text-red-700 dark:text-slate-300">
        {{ error }}
      </NoticeCallout>

      <NoticeCallout v-if="cookieBlockedHost" tone="warning" class="mb-4 px-4 py-3 text-sm text-amber-700 dark:text-slate-300">
        Your browser is blocking sign-in cookies for {{ cookieBlockedHost }}. Allow cookies for {{ cookieBlockedHost }} and reload.
      </NoticeCallout>

      <div v-if="loading" class="text-center text-sm text-slate-500 dark:text-slate-400">
        Authenticating...
      </div>

      <button
        v-else
        @click="handleLogin"
        class="flex w-full items-center justify-center rounded-lg bg-kipper-600 px-4 py-2.5 text-sm font-semibold text-white shadow-sm transition-colors hover:bg-kipper-700 focus:outline-none focus:ring-2 focus:ring-kipper-500/50 dark:bg-kipper-500 dark:hover:bg-kipper-600"
      >
        Sign in
      </button>
    </div>
  </div>
</template>
