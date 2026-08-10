<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { ShieldAlert, X } from 'lucide-vue-next'
import { RouterLink } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { getTwoFactorStatus } from '@/api/twofa'

// Nudges admins without a 2FA factor towards enrolling. Gentle and
// dismissible at first; after two weeks the dismissal only lasts the
// session, so the banner returns until a factor exists. Enrolling early
// matters: the factor must be a week old before it can authorise a
// migration, and an unenrolled account is the one an attacker can enroll
// their own device on.
const FIRST_SEEN_KEY = 'kipper_2fa_nudge_first_seen'
const DISMISSED_KEY = 'kipper_2fa_nudge_dismissed'
const ESCALATE_AFTER_DAYS = 14

const auth = useAuthStore()
const unenrolled = ref(false)
const dismissedThisSession = ref(sessionStorage.getItem(DISMISSED_KEY) === '1')

const escalated = computed(() => {
  const firstSeen = localStorage.getItem(FIRST_SEEN_KEY)
  if (!firstSeen) return false
  return Date.now() - Date.parse(firstSeen) > ESCALATE_AFTER_DAYS * 24 * 60 * 60 * 1000
})

const visible = computed(() => {
  if (!unenrolled.value || dismissedThisSession.value) return false
  // Before escalation a dismissal sticks; afterwards only for the session.
  if (!escalated.value && localStorage.getItem(DISMISSED_KEY) === '1') return false
  return true
})

function dismiss() {
  dismissedThisSession.value = true
  sessionStorage.setItem(DISMISSED_KEY, '1')
  localStorage.setItem(DISMISSED_KEY, '1')
}

onMounted(async () => {
  if (!auth.isAdmin) return
  try {
    const status = await getTwoFactorStatus()
    if (status.state !== 'active') {
      unenrolled.value = true
      if (!localStorage.getItem(FIRST_SEEN_KEY)) {
        localStorage.setItem(FIRST_SEEN_KEY, new Date().toISOString())
      }
    }
  } catch {
    // Status being unavailable is not worth a banner.
  }
})
</script>

<template>
  <!-- Dark mode renders the banner as a neutral card with a coloured accent
       edge instead of a tinted wash: peach for the nudge, rose once escalated. -->
  <div
    v-if="visible"
    class="flex items-center gap-3 border-b px-4 py-2.5 text-sm dark:border-slate-800 dark:bg-slate-900 dark:text-slate-300"
    :class="escalated
      ? 'border-red-200 bg-red-50 text-red-800 dark:shadow-[inset_3px_0_0_theme(colors.rose.400)]'
      : 'border-amber-200 bg-amber-50 text-amber-800 dark:shadow-[inset_3px_0_0_theme(colors.orange.300)]'"
  >
    <ShieldAlert
      class="h-4 w-4 shrink-0"
      :class="escalated ? 'dark:text-rose-300' : 'dark:text-orange-300'"
      :stroke-width="1.75"
    />
    <p class="flex-1">
      <template v-if="escalated">
        Your admin account still has no two-factor authentication. Until a factor is enrolled, a stolen password is all it takes to act as you.
      </template>
      <template v-else>
        Set up two-factor authentication for your admin account. It keeps the cluster's most sensitive operations tied to a device you hold.
      </template>
      <RouterLink
        to="/settings"
        class="ml-1 font-semibold underline underline-offset-2"
        :class="escalated ? 'dark:text-rose-300' : 'dark:text-orange-300'"
      >Enroll in Settings</RouterLink>
    </p>
    <button @click="dismiss" class="shrink-0 rounded p-1 hover:bg-black/5 dark:hover:bg-white/10" aria-label="Dismiss">
      <X class="h-4 w-4" :stroke-width="1.75" />
    </button>
  </div>
</template>
