<script setup lang="ts">
import { ref, onMounted, onUnmounted, computed } from 'vue'
import { Bell, Volume2, VolumeX, RefreshCw, Copy, Check, ArrowRight } from 'lucide-vue-next'
import { RouterLink } from 'vue-router'
import { storeToRefs } from 'pinia'
import { useAlertsStore } from '@/stores/alerts'
import { severityColor, relativeTime } from '@/utils/alerts'

const store = useAlertsStore()
const { alerts, unreadCount, loading } = storeToRefs(store)

const open = ref(false)
const soundEnabled = ref(localStorage.getItem('kipper_alert_sound') !== 'false')
const panelRef = ref<HTMLDivElement>()
let pollInterval: ReturnType<typeof setInterval> | null = null

function playNotificationSound() {
  if (!soundEnabled.value) return
  const ctx = new AudioContext()
  const osc = ctx.createOscillator()
  const gain = ctx.createGain()
  osc.connect(gain)
  gain.connect(ctx.destination)
  osc.frequency.value = 880
  osc.type = 'sine'
  gain.gain.value = 0.08
  gain.gain.exponentialRampToValueAtTime(0.001, ctx.currentTime + 0.3)
  osc.start()
  osc.stop(ctx.currentTime + 0.3)
}

function toggleSound() {
  soundEnabled.value = !soundEnabled.value
  localStorage.setItem('kipper_alert_sound', String(soundEnabled.value))
  if (soundEnabled.value) playNotificationSound()
}

async function pollAlerts() {
  try {
    if (await store.pollForNew()) {
      playNotificationSound()
      if (open.value) store.load()
    }
  } catch {
    // Transient poll failures stay quiet; the dedicated alerts page surfaces
    // a load error when the user opens it.
  }
}

function togglePanel() {
  open.value = !open.value
  // Opening is a glance, not an acknowledgement: keep the unread badge until
  // the user explicitly marks the alerts read.
  if (open.value) {
    store.load()
  }
}

function closePanel() {
  open.value = false
}

async function handleDismiss() {
  try {
    await store.dismiss()
  } catch {
    // ignore
  }
}

// The bell is a quick glance at the most recent alerts; the full history lives
// on the dedicated page.
const displayAlerts = computed(() => alerts.value.slice(0, 20))
const hasMore = computed(() => alerts.value.length > displayAlerts.value.length)
const copied = ref(false)

function copyAlerts() {
  const text = displayAlerts.value.map(a =>
    `[${a.severity}] ${a.app}, ${a.action} (${a.reason}) ${a.time}`
  ).join('\n')
  navigator.clipboard.writeText(text)
  copied.value = true
  setTimeout(() => { copied.value = false }, 2000)
}

function refreshAlerts() {
  store.load()
}

function handleClickOutside() {
  if (!open.value) return
  closePanel()
}

function handleKeyDown(e: KeyboardEvent) {
  if (e.key === 'Escape' && open.value) {
    closePanel()
  }
}

onMounted(() => {
  pollAlerts()
  pollInterval = setInterval(pollAlerts, 30000)
  document.addEventListener('click', handleClickOutside)
  document.addEventListener('keydown', handleKeyDown)
})

onUnmounted(() => {
  if (pollInterval) clearInterval(pollInterval)
  document.removeEventListener('click', handleClickOutside)
  document.removeEventListener('keydown', handleKeyDown)
})
</script>

<template>
  <div class="relative" data-alert-bell>
    <button
      @click.stop="togglePanel"
      class="flex w-full items-center gap-3 rounded-lg px-3 py-2.5 text-sm font-medium text-slate-600 transition-colors hover:bg-slate-100 hover:text-slate-900 dark:text-slate-400 dark:hover:bg-slate-800 dark:hover:text-slate-200"
    >
      <div class="relative">
        <Bell class="h-5 w-5" :stroke-width="1.75" />
        <span
          v-if="unreadCount > 0"
          class="absolute -right-1.5 -top-1.5 flex h-4 min-w-4 items-center justify-center rounded-full bg-red-500 px-1 text-[10px] font-bold text-white"
        >
          {{ unreadCount > 99 ? '99+' : unreadCount }}
        </span>
      </div>
      Alerts
    </button>

    <!-- Alert panel -->
    <Teleport to="body">
      <div
        v-if="open"
        ref="panelRef"
        @click.stop
        class="fixed inset-y-0 right-0 z-50 flex w-full max-w-96 flex-col border-l border-slate-200 bg-white shadow-xl dark:border-slate-700 dark:bg-slate-900"
      >
        <div class="flex items-center justify-between border-b border-slate-200 px-4 py-3 dark:border-slate-700">
          <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Alerts</h3>
          <div class="flex items-center gap-1">
            <button
              @click="copyAlerts"
              class="rounded-md p-1 text-slate-400 hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800 dark:hover:text-slate-300"
              title="Copy alerts to clipboard"
            >
              <Check v-if="copied" class="h-4 w-4 text-emerald-500" />
              <Copy v-else class="h-4 w-4" />
            </button>
            <button
              @click="refreshAlerts"
              class="rounded-md p-1 text-slate-400 hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800 dark:hover:text-slate-300"
              title="Refresh alerts"
            >
              <RefreshCw class="h-4 w-4" />
            </button>
            <button
              @click="toggleSound"
              class="rounded-md p-1 text-slate-400 hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800 dark:hover:text-slate-300"
              :title="soundEnabled ? 'Mute alert sounds' : 'Enable alert sounds'"
            >
              <Volume2 v-if="soundEnabled" class="h-4 w-4" />
              <VolumeX v-else class="h-4 w-4" />
            </button>
            <button
              @click="closePanel"
              class="rounded-md p-1 text-slate-400 hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800 dark:hover:text-slate-300"
            >
              <svg class="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
                <path stroke-linecap="round" stroke-linejoin="round" d="M6 18L18 6M6 6l12 12" />
              </svg>
            </button>
          </div>
        </div>

        <div class="flex-1 overflow-y-auto">
          <div v-if="loading" class="py-12 text-center text-sm text-slate-500 dark:text-slate-400">Loading...</div>

          <div v-else-if="displayAlerts.length === 0" class="py-12 text-center text-sm text-slate-500 dark:text-slate-400">
            No alerts
          </div>

          <div v-else class="divide-y divide-slate-100 dark:divide-slate-800">
            <div
              v-for="alert in displayAlerts"
              :key="alert.id"
              class="px-4 py-3"
            >
              <div class="flex items-start gap-2.5">
                <span :class="severityColor(alert.severity)" class="mt-1.5 h-2 w-2 flex-shrink-0 rounded-full" />
                <div class="min-w-0 flex-1">
                  <div class="flex items-baseline justify-between gap-2">
                    <span class="text-xs font-semibold text-slate-900 dark:text-slate-50">{{ alert.app }}</span>
                    <span class="flex-shrink-0 text-[10px] text-slate-400 dark:text-slate-500">{{ relativeTime(alert.time) }}</span>
                  </div>
                  <p class="mt-0.5 text-xs text-slate-700 dark:text-slate-300">{{ alert.action }}</p>
                  <p class="mt-0.5 text-[11px] text-slate-500 dark:text-slate-400">{{ alert.reason }}</p>
                </div>
              </div>
            </div>
          </div>
        </div>

        <div class="space-y-2 border-t border-slate-200 p-3 dark:border-slate-700">
          <RouterLink
            to="/alerts"
            @click="closePanel"
            class="flex w-full items-center justify-center gap-1.5 rounded-md px-3 py-2 text-xs font-medium text-slate-600 transition-colors hover:bg-slate-100 dark:text-slate-400 dark:hover:bg-slate-800"
          >
            {{ hasMore ? 'View all alerts' : 'Open alerts page' }}
            <ArrowRight class="h-3.5 w-3.5" />
          </RouterLink>
          <button
            v-if="displayAlerts.length > 0 && unreadCount > 0"
            @click="handleDismiss"
            class="w-full rounded-md bg-slate-100 px-3 py-2 text-xs font-medium text-slate-700 transition-colors hover:bg-slate-200 dark:bg-slate-800 dark:text-slate-300 dark:hover:bg-slate-700"
          >
            Mark all read
          </button>
        </div>
      </div>
    </Teleport>
  </div>
</template>
