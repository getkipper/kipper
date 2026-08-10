<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { RefreshCw, Check } from 'lucide-vue-next'
import { storeToRefs } from 'pinia'
import { useAlertsStore } from '@/stores/alerts'
import { severityColor, severityLabel, relativeTime } from '@/utils/alerts'
import { useToast } from '@/composables/useToast'

const store = useAlertsStore()
const toast = useToast()
const { alerts, loading, error, unreadCount } = storeToRefs(store)

type Filter = 'all' | 'critical' | 'warning' | 'info'
const filter = ref<Filter>('all')

const filters: { value: Filter; label: string }[] = [
  { value: 'all', label: 'All' },
  { value: 'critical', label: 'Critical' },
  { value: 'warning', label: 'Warning' },
  { value: 'info', label: 'Info' },
]

const counts = computed(() => {
  const c = { all: alerts.value.length, critical: 0, warning: 0, info: 0 }
  for (const a of alerts.value) {
    if (a.severity === 'critical') c.critical++
    else if (a.severity === 'warning') c.warning++
    else c.info++
  }
  return c
})

const visibleAlerts = computed(() =>
  filter.value === 'all' ? alerts.value : alerts.value.filter(a => a.severity === filter.value),
)

async function markAllRead() {
  try {
    await store.dismiss()
  } catch {
    toast.error('Could not mark alerts as read. Try again.')
  }
}

onMounted(() => {
  store.load()
})
</script>

<template>
  <div class="animate-fade-in">
    <div class="mb-8 flex flex-wrap items-center justify-between gap-3">
      <div>
        <h1 class="text-2xl font-semibold tracking-tight text-slate-900 dark:text-slate-50">Alerts</h1>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">
          Events from your projects, newest first
        </p>
      </div>
      <div class="flex items-center gap-2">
        <button
          v-if="unreadCount > 0"
          @click="markAllRead"
          class="inline-flex items-center gap-1.5 rounded-lg bg-slate-100 px-4 py-2 text-sm font-medium text-slate-700 transition-colors hover:bg-slate-200 dark:bg-slate-800 dark:text-slate-300 dark:hover:bg-slate-700"
        >
          <Check class="h-4 w-4" />
          Mark all read
        </button>
        <button
          @click="store.load()"
          class="inline-flex items-center gap-1.5 rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 transition-colors hover:bg-slate-50 dark:border-slate-700 dark:text-slate-300 dark:hover:bg-slate-800"
        >
          <RefreshCw class="h-4 w-4" :class="{ 'animate-spin': loading }" />
          Refresh
        </button>
      </div>
    </div>

    <div class="mb-4 flex flex-wrap gap-2">
      <button
        v-for="f in filters"
        :key="f.value"
        @click="filter = f.value"
        class="rounded-full px-3 py-1 text-xs font-medium transition-colors"
        :class="filter === f.value
          ? 'bg-kipper-600 text-white'
          : 'bg-slate-100 text-slate-600 hover:bg-slate-200 dark:bg-slate-800 dark:text-slate-400 dark:hover:bg-slate-700'"
      >
        {{ f.label }} ({{ counts[f.value] }})
      </button>
    </div>

    <div class="rounded-xl border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900">
      <div v-if="loading && alerts.length === 0" class="py-16 text-center text-sm text-slate-500 dark:text-slate-400">
        Loading...
      </div>

      <div v-else-if="error" class="py-16 text-center">
        <p class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
        <button
          @click="store.load()"
          class="mt-3 rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50 dark:border-slate-700 dark:text-slate-300 dark:hover:bg-slate-800"
        >
          Try again
        </button>
      </div>

      <div v-else-if="visibleAlerts.length === 0" class="py-16 text-center text-sm text-slate-500 dark:text-slate-400">
        {{ filter === 'all' ? 'No alerts' : `No ${filter} alerts` }}
      </div>

      <div v-else class="divide-y divide-slate-100 dark:divide-slate-800">
        <div v-for="alert in visibleAlerts" :key="alert.id" class="flex items-start gap-3 px-5 py-4">
          <span :class="severityColor(alert.severity)" class="mt-1.5 h-2 w-2 flex-shrink-0 rounded-full" />
          <div class="min-w-0 flex-1">
            <div class="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
              <div class="flex min-w-0 items-center gap-2">
                <span class="truncate text-sm font-semibold text-slate-900 dark:text-slate-50">{{ alert.app }}</span>
                <span v-if="alert.namespace" class="truncate text-xs text-slate-400 dark:text-slate-500">{{ alert.namespace }}</span>
              </div>
              <span class="flex-shrink-0 text-xs text-slate-400 dark:text-slate-500">
                {{ severityLabel(alert.severity) }} · {{ relativeTime(alert.time) }}
              </span>
            </div>
            <p class="mt-1 text-sm text-slate-700 dark:text-slate-300">{{ alert.action }}</p>
            <p class="mt-0.5 text-xs text-slate-500 dark:text-slate-400">{{ alert.reason }}</p>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
