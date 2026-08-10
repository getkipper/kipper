<script setup lang="ts">
import { ref, computed, onMounted, onBeforeUnmount, nextTick, useTemplateRef } from 'vue'
import { X, Eye, Copy, Check } from 'lucide-vue-next'
import client from '@/api/client'

interface Props {
  type: 'git' | 'registry' | 'app-git'
  name: string
  server: string
  // Required when type is 'app-git': the app's git token lives under its project.
  project?: string
  app?: string
}

const props = defineProps<Props>()

const kindLabel = computed(() => props.type === 'registry' ? 'Registry credential' : props.type === 'app-git' ? 'Git token' : 'Git credential')
const nounLabel = computed(() => props.type === 'registry' ? 'password' : 'token')
const emit = defineEmits<{ close: [] }>()

const password = ref('')
const submitting = ref(false)
const errorMsg = ref('')
const revealed = ref<string | null>(null)
const copied = ref(false)
const secondsLeft = ref(30)
const passwordInput = useTemplateRef<HTMLInputElement>('passwordInput')

let tickHandle: ReturnType<typeof setInterval> | null = null

onMounted(async () => {
  await nextTick()
  passwordInput.value?.focus()
})

onBeforeUnmount(() => {
  if (tickHandle) clearInterval(tickHandle)
})

async function submit() {
  if (!password.value || submitting.value) return
  submitting.value = true
  errorMsg.value = ''
  try {
    let path: string
    if (props.type === 'app-git') {
      path = `/projects/${encodeURIComponent(props.project ?? '')}/apps/${encodeURIComponent(props.app ?? '')}/git/reveal`
    } else if (props.type === 'git') {
      path = `/settings/git-credentials/${encodeURIComponent(props.name)}/reveal`
    } else {
      path = `/settings/registries/${encodeURIComponent(props.name)}/reveal`
    }
    const { data } = await client.post<{ token?: string; password?: string }>(path, { password: password.value })
    revealed.value = props.type === 'registry' ? (data.password ?? '') : (data.token ?? '')
    startCountdown()
  } catch (e: unknown) {
    const status = (e as { response?: { status?: number } })?.response?.status
    if (status === 401) {
      errorMsg.value = 'Incorrect password.'
    } else if (status === 403) {
      errorMsg.value = 'You do not have permission to reveal this.'
    } else if (status === 404) {
      errorMsg.value = 'Credential no longer exists.'
    } else {
      errorMsg.value = 'Could not reveal credential. Try again.'
    }
    password.value = ''
    await nextTick()
    passwordInput.value?.focus()
  } finally {
    submitting.value = false
  }
}

function startCountdown() {
  secondsLeft.value = 30
  tickHandle = setInterval(() => {
    secondsLeft.value -= 1
    if (secondsLeft.value <= 0) {
      if (tickHandle) clearInterval(tickHandle)
      emit('close')
    }
  }, 1000)
}

function copy() {
  if (!revealed.value) return
  navigator.clipboard.writeText(revealed.value)
  copied.value = true
  setTimeout(() => { copied.value = false }, 2000)
}
</script>

<template>
  <div
    class="fixed inset-0 z-[100] flex items-center justify-center bg-black/40 p-8"
    @mousedown.self="emit('close')"
    @keydown.esc="emit('close')"
  >
    <div class="w-full max-w-md rounded-xl border border-slate-200 bg-white shadow-2xl dark:border-slate-700 dark:bg-slate-900" @mousedown.stop>
      <div class="flex items-center justify-between border-b border-slate-200 px-5 py-3 dark:border-slate-800">
        <div class="flex items-center gap-2 text-sm font-semibold text-slate-900 dark:text-slate-50">
          <Eye class="h-4 w-4 text-kipper-500" />
          Reveal credential
        </div>
        <button @click="emit('close')" class="rounded-lg p-1 text-slate-400 hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800">
          <X class="h-4 w-4" />
        </button>
      </div>

      <div class="px-5 py-4">
        <p class="mb-1 text-xs text-slate-500 dark:text-slate-400">
          {{ kindLabel }}
        </p>
        <p class="mb-4 break-all text-sm font-medium text-slate-900 dark:text-slate-50">{{ props.server }}</p>

        <form v-if="!revealed" @submit.prevent="submit" class="space-y-3">
          <p class="text-sm text-slate-600 dark:text-slate-400">
            Enter your password to reveal the {{ nounLabel }}.
          </p>
          <input
            ref="passwordInput"
            v-model="password"
            type="password"
            autocomplete="current-password"
            class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
            placeholder="Your password"
          />
          <p v-if="errorMsg" class="text-xs text-red-600 dark:text-red-400">{{ errorMsg }}</p>
          <div class="flex justify-end gap-2 pt-1">
            <button type="button" @click="emit('close')" class="rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800">
              Cancel
            </button>
            <button
              type="submit"
              :disabled="!password || submitting"
              class="rounded-lg bg-kipper-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-kipper-700 disabled:opacity-50"
            >
              {{ submitting ? 'Verifying…' : 'Reveal' }}
            </button>
          </div>
        </form>

        <div v-else class="space-y-3">
          <div class="rounded-md border border-slate-200 bg-slate-50 p-3 font-mono text-xs break-all text-slate-900 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50">
            {{ revealed }}
          </div>
          <div class="flex items-center justify-between">
            <span class="text-xs text-slate-500 dark:text-slate-400">Hiding in {{ secondsLeft }}s</span>
            <button
              @click="copy"
              class="inline-flex items-center gap-1.5 rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-800"
            >
              <Check v-if="copied" class="h-3.5 w-3.5 text-emerald-500" />
              <Copy v-else class="h-3.5 w-3.5" />
              {{ copied ? 'Copied' : 'Copy' }}
            </button>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
