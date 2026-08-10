<script setup lang="ts">
import { ref, computed } from 'vue'
import { AlertTriangle } from 'lucide-vue-next'

interface Props {
  title: string
  message?: string
  confirmLabel?: string
  // When set, the user must type this exact phrase to enable the confirm
  // button. Use it for high-blast-radius actions such as destroying data.
  confirmPhrase?: string
  danger?: boolean
}

const props = withDefaults(defineProps<Props>(), {
  confirmLabel: 'Delete',
  danger: true,
})

const emit = defineEmits<{
  close: []
  confirm: []
}>()

const typed = ref('')
const canConfirm = computed(() => !props.confirmPhrase || typed.value === props.confirmPhrase)
</script>

<template>
  <div class="w-full max-w-md rounded-xl bg-white p-6 shadow-xl dark:bg-slate-900" @click.stop>
    <div class="flex items-start gap-4">
      <span
        class="inline-flex h-10 w-10 shrink-0 items-center justify-center rounded-full"
        :class="danger ? 'bg-red-100 dark:bg-red-950' : 'bg-amber-100 dark:bg-amber-950'"
      >
        <AlertTriangle
          class="h-5 w-5"
          :class="danger ? 'text-red-600 dark:text-red-400' : 'text-amber-600 dark:text-amber-400'"
          :stroke-width="1.75"
        />
      </span>
      <div class="min-w-0">
        <h2 class="text-lg font-semibold text-slate-900 dark:text-slate-50">{{ title }}</h2>
        <p v-if="message" class="mt-1 text-sm text-slate-500 dark:text-slate-400">{{ message }}</p>
      </div>
    </div>

    <div v-if="confirmPhrase" class="mt-5">
      <label class="block text-sm text-slate-700 dark:text-slate-300">
        Type
        <span class="font-mono font-semibold text-slate-900 dark:text-slate-50">{{ confirmPhrase }}</span>
        to confirm
      </label>
      <input
        v-model="typed"
        type="text"
        :placeholder="confirmPhrase"
        class="mt-1.5 block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-red-500 focus:outline-none focus:ring-2 focus:ring-red-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
        @keyup.enter="canConfirm && emit('confirm')"
      />
    </div>

    <div class="mt-6 flex justify-end gap-3">
      <button
        class="rounded-lg border border-slate-300 px-4 py-2.5 text-sm font-medium text-slate-700 transition-colors hover:bg-slate-50 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-800"
        @click="emit('close')"
      >
        Cancel
      </button>
      <button
        :disabled="!canConfirm"
        class="rounded-lg px-4 py-2.5 text-sm font-semibold text-white transition-colors disabled:cursor-not-allowed disabled:opacity-40"
        :class="danger ? 'bg-red-600 hover:bg-red-700' : 'bg-amber-600 hover:bg-amber-700'"
        @click="emit('confirm')"
      >
        {{ confirmLabel }}
      </button>
    </div>
  </div>
</template>
