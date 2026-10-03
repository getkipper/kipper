<script setup lang="ts">
import { ref } from 'vue'
import { Power } from 'lucide-vue-next'

defineProps<{ appName: string }>()

const emit = defineEmits<{
  close: []
  confirm: [reason: string]
}>()

const reason = ref('')
</script>

<template>
  <div class="w-full max-w-md rounded-xl bg-white p-6 shadow-xl dark:bg-slate-900" @click.stop>
    <div class="flex items-start gap-4">
      <span class="inline-flex h-10 w-10 shrink-0 items-center justify-center rounded-full bg-amber-100 dark:bg-amber-950">
        <Power class="h-5 w-5 text-amber-600 dark:text-amber-400" :stroke-width="1.75" />
      </span>
      <div class="min-w-0">
        <h2 class="text-lg font-semibold text-slate-900 dark:text-slate-50">Stop {{ appName }}?</h2>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">
          Its pods shut down and give their CPU and memory back, and its route answers with a "this app is stopped" page.
        </p>
        <p class="mt-2 text-sm text-slate-500 dark:text-slate-400">
          Bound services, databases and volumes keep running. The replica count, autoscaling, environment and route stay as they are and apply again when you start it.
        </p>
      </div>
    </div>

    <label class="mt-5 block text-sm text-slate-700 dark:text-slate-300" for="stop-reason">Reason (optional)</label>
    <textarea
      id="stop-reason"
      v-model="reason"
      data-testid="app-stop-reason"
      maxlength="500"
      rows="2"
      placeholder="Shown wherever the app is listed"
      class="mt-1.5 block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-amber-500 focus:outline-none focus:ring-2 focus:ring-amber-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
    />

    <div class="mt-6 flex justify-end gap-3">
      <button
        class="rounded-lg border border-slate-300 px-4 py-2.5 text-sm font-medium text-slate-700 transition-colors hover:bg-slate-50 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-800"
        @click="emit('close')"
      >
        Cancel
      </button>
      <button
        data-testid="app-stop-confirm"
        class="rounded-lg bg-amber-600 px-4 py-2.5 text-sm font-semibold text-white transition-colors hover:bg-amber-700"
        @click="emit('confirm', reason.trim())"
      >
        Stop app
      </button>
    </div>
  </div>
</template>
