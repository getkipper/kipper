<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Check, AlertTriangle } from 'lucide-vue-next'
import client from '@/api/client'
import PasswordStrengthBar from '@/components/PasswordStrengthBar.vue'
import { usePasswordStrength } from '@/composables/usePasswordStrength'

const route = useRoute()
const router = useRouter()

const token = route.params.token as string
const loading = ref(true)
const role = ref('')
const expires = ref('')
const error = ref('')
const success = ref(false)

const email = ref('')
const password = ref('')
const confirmPassword = ref('')
const submitting = ref(false)
const { strength } = usePasswordStrength(password)

onMounted(async () => {
  try {
    const { data } = await client.get(`/invites/${token}`)
    role.value = data.role
    expires.value = data.expires
  } catch {
    error.value = 'This invite link is invalid or has expired.'
  } finally {
    loading.value = false
  }
})

async function handleAccept() {
  if (!strength.value.allMet) {
    error.value = 'Password does not meet all requirements'
    return
  }
  if (password.value !== confirmPassword.value) {
    error.value = 'Passwords do not match'
    return
  }

  submitting.value = true
  error.value = ''

  try {
    await client.post(`/invites/${token}/accept`, {
      token: token,
      email: email.value,
      password: password.value,
    })
    success.value = true
    setTimeout(() => router.push('/login'), 3000)
  } catch (e: unknown) {
    const msg = (e as { response?: { data?: { error?: string } } })?.response?.data?.error
    error.value = msg || 'Failed to create account'
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="flex min-h-screen items-center justify-center bg-slate-50 dark:bg-slate-950">
    <div class="w-full max-w-md">
      <!-- Loading -->
      <div v-if="loading" class="text-center text-sm text-slate-500">Verifying invite...</div>

      <!-- Invalid/expired -->
      <div v-else-if="error && !role" class="rounded-xl border border-red-200 bg-white p-8 text-center shadow-lg dark:border-red-800 dark:bg-slate-900">
        <AlertTriangle class="mx-auto mb-4 h-12 w-12 text-red-500" />
        <h1 class="text-lg font-semibold text-slate-900 dark:text-slate-50">Invite not valid</h1>
        <p class="mt-2 text-sm text-slate-500">{{ error }}</p>
        <p class="mt-4 text-xs text-slate-400">Ask your admin for a new invite link.</p>
      </div>

      <!-- Success -->
      <div v-else-if="success" class="rounded-xl border border-emerald-200 bg-white p-8 text-center shadow-lg dark:border-emerald-800 dark:bg-slate-900">
        <Check class="mx-auto mb-4 h-12 w-12 text-emerald-500" />
        <h1 class="text-lg font-semibold text-slate-900 dark:text-slate-50">Account created</h1>
        <p class="mt-2 text-sm text-slate-500">Redirecting to login...</p>
      </div>

      <!-- Accept form -->
      <div v-else class="rounded-xl border border-slate-200 bg-white p-8 shadow-lg dark:border-slate-800 dark:bg-slate-900">
        <div class="mb-6 text-center">
          <img src="/logo.svg" alt="Kipper" class="mx-auto mb-4 h-10 w-10" />
          <h1 class="text-lg font-semibold text-slate-900 dark:text-slate-50">Join this cluster</h1>
          <p class="mt-1 text-sm text-slate-500">
            You've been invited as
            <span class="font-medium" :class="{
              'text-red-600': role === 'admin',
              'text-kipper-600': role === 'deployer',
              'text-slate-600': role === 'viewer',
            }">{{ role }}</span>
          </p>
        </div>

        <div v-if="error" class="mb-4 rounded-lg bg-red-50 p-3 text-xs text-red-600 dark:bg-red-950 dark:text-red-400">
          {{ error }}
        </div>

        <div class="space-y-4">
          <div>
            <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Email</label>
            <input
              v-model="email"
              type="email"
              placeholder="you@example.com"
              class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
            />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Password</label>
            <input
              v-model="password"
              type="password"
              placeholder="Letters, numbers, and symbols"
              class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
            />
            <div class="mt-2">
              <PasswordStrengthBar :password="password" />
            </div>
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Confirm password</label>
            <input
              v-model="confirmPassword"
              type="password"
              placeholder="Repeat password"
              class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
              @keyup.enter="handleAccept"
            />
          </div>
          <button
            @click="handleAccept"
            :disabled="!email || !password || !confirmPassword || !strength.allMet || submitting"
            class="w-full rounded-lg bg-kipper-600 py-2.5 text-sm font-medium text-white hover:bg-kipper-700 disabled:opacity-50"
          >
            {{ submitting ? 'Creating account...' : 'Create account' }}
          </button>
        </div>
      </div>
    </div>
  </div>
</template>
