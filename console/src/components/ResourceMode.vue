<script setup lang="ts">
import { computed } from 'vue'
import type { ResourceDetail } from '@/api/resources'
import type { ResourceKind } from '@/utils/resources'

const props = defineProps<{
  kind: ResourceKind
  detail?: ResourceDetail
  canWrite?: boolean
  busy?: boolean
}>()

const emit = defineEmits<{
  confirm: []
  automatic: []
}>()

const name = computed(() => (props.kind === 'memory' ? 'Memory' : 'CPU'))

const running = computed(() => {
  const live = props.detail?.live
  if (!live?.request && !live?.limit) return ''
  if (!live.limit) return `running with ${live.request} reserved and no cap`
  if (!live.request || live.request === live.limit) return `running at ${live.limit}`
  return `running with ${live.request} reserved, up to ${live.limit}`
})

// Resolve a fixed size from the user-owned limit, or otherwise the user-owned
// request, matching the server's resolver.
function fixedValue(d: ResourceDetail): string {
  return d.limit.source === 'user' ? d.limit.value : d.request.value
}

const summary = computed(() => {
  const d = props.detail
  if (!d) return ''
  switch (d.mode) {
    case 'automatic':
      return `${name.value} is sized automatically, ${running.value || 'no size yet'}.`
    case 'bounded':
      return `${name.value} is tuned between your ${d.request.value} and ${d.limit.value}, ${running.value}.`
    case 'fixed':
      return `${name.value} is fixed at the ${fixedValue(d)} you set.`
    case 'held':
      return `${name.value} is held at ${d.limit.value || d.request.value}. Kipper can't tell who set this value, so it leaves it alone until you confirm it or hand it to automatic sizing.`
    default:
      return ''
  }
})
</script>

<template>
  <div v-if="detail" class="mt-2 space-y-1 text-xs text-slate-600 dark:text-slate-400" data-testid="resource-mode">
    <p data-testid="resource-mode-summary">{{ summary }}</p>
    <div v-if="canWrite && detail.mode !== 'automatic'" class="flex gap-3">
      <button
        v-if="detail.mode === 'held'"
        type="button"
        data-testid="resource-mode-confirm"
        :disabled="busy"
        class="font-medium text-kipper-700 underline decoration-kipper-700/40 underline-offset-2 hover:text-kipper-800 hover:decoration-current disabled:cursor-not-allowed disabled:opacity-50 dark:text-kipper-400 dark:decoration-kipper-400/40 dark:hover:text-kipper-300"
        @click="emit('confirm')"
      >Keep this value</button>
      <button
        type="button"
        data-testid="resource-mode-automatic"
        :disabled="busy"
        class="font-medium text-kipper-700 underline decoration-kipper-700/40 underline-offset-2 hover:text-kipper-800 hover:decoration-current disabled:cursor-not-allowed disabled:opacity-50 dark:text-kipper-400 dark:decoration-kipper-400/40 dark:hover:text-kipper-300"
        @click="emit('automatic')"
      >Size automatically</button>
    </div>
  </div>
</template>
