<script setup lang="ts">
import { AlertTriangle, X } from 'lucide-vue-next'

import type { ContainerHealth } from '@/api/apps'
import ContainerFailureEntry from '@/components/ContainerFailureEntry.vue'

interface Props {
  title: string
  failures: { pod: string; container: ContainerHealth }[]
}
defineProps<Props>()

const emit = defineEmits<{ close: [] }>()
</script>

<template>
  <div
    data-testid="container-errors-modal"
    class="flex max-h-[80vh] w-full max-w-3xl flex-col overflow-hidden rounded-xl bg-white shadow-2xl dark:border dark:border-slate-700 dark:bg-slate-900"
    @mousedown.stop
  >
    <div class="flex items-center justify-between border-b border-slate-200 px-5 py-3 dark:border-slate-800">
      <div class="flex items-center gap-1.5 text-sm font-semibold text-red-700 dark:text-red-400">
        <AlertTriangle class="h-4 w-4" :stroke-width="1.75" />
        {{ title }}
      </div>
      <button
        @click="emit('close')"
        title="Close"
        class="rounded-lg p-1 text-slate-400 hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800 dark:hover:text-slate-300"
      >
        <X class="h-4 w-4" />
      </button>
    </div>
    <div class="space-y-4 overflow-y-auto p-5">
      <ContainerFailureEntry
        v-for="entry in failures"
        :key="`${entry.pod}/${entry.container.name}`"
        :pod="entry.pod"
        :container="entry.container"
      />
    </div>
  </div>
</template>
