<script setup lang="ts">
import { ref, onMounted, watch, nextTick } from 'vue'
import { ShieldCheck, Copy, Check, KeyRound, RefreshCw } from 'lucide-vue-next'
import QRCode from 'qrcode'
import NoticeCallout from '@/components/NoticeCallout.vue'
import { useToast } from '@/composables/useToast'
import {
  getTwoFactorStatus,
  enrollTwoFactor,
  confirmTwoFactor,
  resetTwoFactor,
  type TwoFactorStatus,
  type TwoFactorEnrollment,
} from '@/api/twofa'
import { formatDateTime } from '@/utils/datetime'

const toast = useToast()

const status = ref<TwoFactorStatus | null>(null)
const loading = ref(false)

// Enrollment flow
const bootstrapCode = ref('')
const enrolling = ref(false)
const enrollment = ref<TwoFactorEnrollment | null>(null)
const confirmCode = ref('')
const confirming = ref(false)
const recoveryCodes = ref<string[]>([])
const recoveryCopied = ref(false)
const qrCanvas = ref<HTMLCanvasElement | null>(null)

// Reset flow
const showReset = ref(false)
const resetCode = ref('')
const resetRecoveryCode = ref('')
const resetting = ref(false)

async function loadStatus() {
  loading.value = true
  try {
    status.value = await getTwoFactorStatus()
  } catch {
    toast.error('Failed to load 2FA status')
  } finally {
    loading.value = false
  }
}

function apiError(e: unknown, fallback: string): string {
  const axiosErr = e as { response?: { data?: { error?: string } } }
  return axiosErr.response?.data?.error || fallback
}

async function handleEnroll() {
  if (!bootstrapCode.value) return
  enrolling.value = true
  try {
    enrollment.value = await enrollTwoFactor(bootstrapCode.value.trim())
    bootstrapCode.value = ''
  } catch (e: unknown) {
    toast.error(apiError(e, 'Enrollment failed'))
  } finally {
    enrolling.value = false
  }
}

async function handleConfirm() {
  if (!confirmCode.value) return
  confirming.value = true
  try {
    const result = await confirmTwoFactor(confirmCode.value.trim())
    recoveryCodes.value = result.recovery_codes
    enrollment.value = null
    confirmCode.value = ''
    await loadStatus()
    toast.success('Two-factor authentication is active')
  } catch (e: unknown) {
    toast.error(apiError(e, 'Confirmation failed'))
  } finally {
    confirming.value = false
  }
}

async function handleReset() {
  resetting.value = true
  try {
    enrollment.value = await resetTwoFactor(
      resetCode.value ? { code: resetCode.value.trim() } : { recovery_code: resetRecoveryCode.value.trim() },
    )
    showReset.value = false
    resetCode.value = ''
    resetRecoveryCode.value = ''
    recoveryCodes.value = []
    await loadStatus()
    toast.success('Factor reset: scan the new QR code and confirm')
  } catch (e: unknown) {
    toast.error(apiError(e, 'Reset failed'))
  } finally {
    resetting.value = false
  }
}

async function copyRecoveryCodes() {
  await navigator.clipboard.writeText(recoveryCodes.value.join('\n'))
  recoveryCopied.value = true
  setTimeout(() => { recoveryCopied.value = false }, 2000)
}

// Render the QR whenever a fresh enrollment appears.
watch(enrollment, async (value) => {
  if (!value) return
  await nextTick()
  if (qrCanvas.value) {
    try {
      await QRCode.toCanvas(qrCanvas.value, value.otpauth_uri, { width: 192, margin: 1 })
    } catch {
      // The manual secret below the canvas covers a failed render.
    }
  }
})

onMounted(loadStatus)
</script>

<template>
  <div class="rounded-xl border border-slate-200 bg-white p-6 dark:border-slate-800 dark:bg-slate-900">
    <div class="mb-1 flex items-center gap-2">
      <ShieldCheck class="h-4 w-4 text-kipper-600 dark:text-kipper-400" :stroke-width="1.75" />
      <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Two-factor authentication</h3>
    </div>
    <p class="mb-4 text-sm text-slate-500 dark:text-slate-400">
      Destructive operations like cluster migration require a code from your authenticator app on top of your login.
    </p>

    <div v-if="loading" class="py-2 text-sm text-slate-500">Loading...</div>

    <!-- Active factor -->
    <div v-else-if="status?.state === 'active' && !enrollment" class="space-y-3">
      <NoticeCallout tone="success" class="flex items-center justify-between px-4 py-3">
        <div>
          <p class="text-sm font-medium text-emerald-800 dark:text-emerald-300">Enrolled</p>
          <p class="mt-0.5 text-xs text-emerald-700 dark:text-slate-400">
            Since {{ status.enrolled_at ? formatDateTime(status.enrolled_at) : 'unknown' }}.
            <template v-if="status.eligible">Eligible to authorise migrations.</template>
            <template v-else-if="status.eligible_at">
              Can authorise migrations from {{ formatDateTime(status.eligible_at) }} — new factors wait {{ status.min_age_days }} days.
            </template>
          </p>
        </div>
        <Check class="h-5 w-5 text-emerald-500 dark:text-emerald-300" :stroke-width="2" />
      </NoticeCallout>

      <button
        @click="showReset = !showReset"
        class="inline-flex items-center gap-1.5 text-sm text-slate-500 hover:text-slate-700 dark:hover:text-slate-300"
      >
        <RefreshCw class="h-3.5 w-3.5" :stroke-width="1.75" /> Replace this factor
      </button>

      <div v-if="showReset" class="space-y-3 rounded-lg border border-slate-200 p-4 dark:border-slate-700">
        <p class="text-xs text-slate-500 dark:text-slate-400">
          Replacing the factor voids the old one and its recovery codes. The new factor waits the full {{ status.min_age_days }} days before it can authorise a migration.
          Confirm with a current code, or a recovery code if the device is gone.
        </p>
        <div class="flex flex-wrap gap-3">
          <input
            v-model="resetCode"
            placeholder="Current 6-digit code"
            inputmode="numeric"
            class="w-44 rounded-lg border border-slate-300 bg-white px-3 py-2 font-mono text-sm dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
          />
          <input
            v-model="resetRecoveryCode"
            placeholder="or recovery code"
            class="w-56 rounded-lg border border-slate-300 bg-white px-3 py-2 font-mono text-sm dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
          />
          <button
            @click="handleReset"
            :disabled="resetting || (!resetCode && !resetRecoveryCode)"
            class="rounded-lg bg-red-600 px-4 py-2 text-sm font-semibold text-white hover:bg-red-700 disabled:opacity-40"
          >
            {{ resetting ? 'Replacing...' : 'Replace factor' }}
          </button>
        </div>
        <p class="text-xs text-slate-400 dark:text-slate-500">
          Lost the device and the recovery codes? A host operator runs
          <span class="font-mono">kip 2fa remove</span> and issues a fresh enrollment code.
        </p>
      </div>
    </div>

    <!-- Enrollment: bootstrap code entry -->
    <div v-else-if="!enrollment" class="space-y-3">
      <NoticeCallout tone="warning" class="px-4 py-3">
        <p class="text-sm font-medium text-amber-800 dark:text-orange-300">Not enrolled</p>
        <p class="mt-1 text-xs text-amber-700 dark:text-slate-400">
          Cluster migrations stay blocked until a factor is enrolled and {{ status?.min_age_days ?? 7 }} days old.
        </p>
      </NoticeCallout>
      <p class="text-sm text-slate-600 dark:text-slate-400">
        Enrollment needs a one-time code issued from the cluster host. Ask an operator with kubeconfig access to run
        <span class="font-mono text-xs">kip 2fa bootstrap &lt;your email&gt;</span> and enter the code within 15 minutes.
      </p>
      <div class="flex gap-3">
        <input
          v-model="bootstrapCode"
          placeholder="XXXX-XXXX-XXXX-XXXX"
          class="w-64 rounded-lg border border-slate-300 bg-white px-3 py-2 font-mono text-sm uppercase dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
          @keyup.enter="handleEnroll"
        />
        <button
          @click="handleEnroll"
          :disabled="enrolling || !bootstrapCode"
          class="rounded-lg bg-kipper-600 px-4 py-2 text-sm font-semibold text-white hover:bg-kipper-700 disabled:opacity-40"
        >
          {{ enrolling ? 'Checking...' : 'Start enrollment' }}
        </button>
      </div>
    </div>

    <!-- Enrollment: QR + confirm -->
    <div v-if="enrollment" class="mt-2 space-y-4">
      <p class="text-sm text-slate-600 dark:text-slate-400">
        Scan the QR code with your authenticator app (or enter the secret manually), then confirm with the first code it shows.
      </p>
      <div class="flex flex-wrap items-start gap-6">
        <canvas ref="qrCanvas" class="rounded-lg border border-slate-200 dark:border-slate-700" />
        <div class="space-y-3">
          <div>
            <p class="mb-1 text-xs font-medium text-slate-500 dark:text-slate-400">Manual entry secret</p>
            <code class="break-all rounded bg-slate-100 px-2 py-1 font-mono text-xs text-slate-700 dark:bg-slate-800 dark:text-slate-300">{{ enrollment.secret }}</code>
          </div>
          <div class="flex gap-3">
            <input
              v-model="confirmCode"
              placeholder="6-digit code"
              inputmode="numeric"
              maxlength="6"
              class="w-36 rounded-lg border border-slate-300 bg-white px-3 py-2 font-mono text-sm dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
              @keyup.enter="handleConfirm"
            />
            <button
              @click="handleConfirm"
              :disabled="confirming || confirmCode.length !== 6"
              class="rounded-lg bg-kipper-600 px-4 py-2 text-sm font-semibold text-white hover:bg-kipper-700 disabled:opacity-40"
            >
              {{ confirming ? 'Confirming...' : 'Confirm' }}
            </button>
          </div>
          <p class="text-xs text-slate-400 dark:text-slate-500">The enrollment expires after 15 minutes, and a wrong code voids it, take the current code straight from the app.</p>
        </div>
      </div>
    </div>

    <!-- Recovery codes, shown exactly once -->
    <div v-if="recoveryCodes.length > 0" class="mt-4 rounded-lg border border-kipper-200 bg-kipper-50 p-4 dark:border-kipper-800 dark:bg-kipper-950/30">
      <div class="mb-2 flex items-center gap-2">
        <KeyRound class="h-4 w-4 text-kipper-600 dark:text-kipper-400" :stroke-width="1.75" />
        <p class="text-sm font-semibold text-slate-900 dark:text-slate-50">Recovery codes, save them now</p>
      </div>
      <p class="mb-3 text-xs text-slate-600 dark:text-slate-400">
        Each works once to replace the factor if the device is lost. They are shown only this once.
      </p>
      <div class="grid grid-cols-2 gap-1 sm:grid-cols-4">
        <code v-for="code in recoveryCodes" :key="code" class="rounded bg-white px-2 py-1 text-center font-mono text-xs text-slate-700 dark:bg-slate-800 dark:text-slate-300">{{ code }}</code>
      </div>
      <button
        @click="copyRecoveryCodes"
        class="mt-3 inline-flex items-center gap-1.5 rounded-lg border border-kipper-300 px-3 py-1.5 text-xs font-medium text-kipper-700 hover:bg-kipper-100 dark:border-kipper-700 dark:text-kipper-300 dark:hover:bg-kipper-900/40"
      >
        <Check v-if="recoveryCopied" class="h-3.5 w-3.5" />
        <Copy v-else class="h-3.5 w-3.5" :stroke-width="1.75" />
        {{ recoveryCopied ? 'Copied' : 'Copy all' }}
      </button>
    </div>
  </div>
</template>
