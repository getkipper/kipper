<script setup lang="ts">
import { ref, computed } from 'vue'
import { AlertTriangle, Rocket } from 'lucide-vue-next'
import NoticeCallout from '@/components/NoticeCallout.vue'
import type { Environment } from '@/api/projects'

const props = defineProps<{
  projectName: string
  displayName?: string
  environments: Environment[]
}>()

const emit = defineEmits<{
  close: []
  confirm: []
}>()

const confirmText = ref('')

const totalApps = computed(() =>
  props.environments.reduce((sum, env) => sum + env.apps.length, 0)
)

const hasApps = computed(() => totalApps.value > 0)

const canConfirm = computed(() => confirmText.value === props.projectName)
</script>

<template>
  <div
    class="w-full max-w-lg rounded-xl bg-white p-6 shadow-xl dark:bg-slate-900"
    @click.stop
  >
    <!-- Header -->
    <div class="flex items-start gap-4">
      <span class="inline-flex h-10 w-10 shrink-0 items-center justify-center rounded-full bg-red-100 dark:bg-red-950">
        <AlertTriangle class="h-5 w-5 text-red-600 dark:text-red-400" :stroke-width="1.75" />
      </span>
      <div>
        <h2 class="text-lg font-semibold text-slate-900 dark:text-slate-50">
          Delete {{ displayName || projectName }}?
        </h2>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">
          This will permanently delete the project, all environments, and all associated Kubernetes resources.
        </p>
      </div>
    </div>

    <!-- App warning -->
    <NoticeCallout v-if="hasApps" tone="danger" class="mt-5 p-4">
      <p class="text-sm font-medium text-red-800 dark:text-rose-300">
        This project has {{ totalApps }} running {{ totalApps === 1 ? 'app' : 'apps' }} that will be destroyed:
      </p>
      <div class="mt-3 space-y-2">
        <div v-for="env in environments.filter(e => e.apps.length > 0)" :key="env.name">
          <p class="text-xs font-medium uppercase tracking-wide text-red-700/70 dark:text-slate-400">{{ env.name }}</p>
          <div class="mt-1 flex flex-wrap gap-1.5">
            <span
              v-for="app in env.apps"
              :key="app.name"
              class="inline-flex items-center gap-1 rounded-md bg-red-100 px-2 py-0.5 text-xs font-medium text-red-800 dark:bg-red-900/50 dark:text-red-300"
            >
              <Rocket class="h-3 w-3" :stroke-width="1.75" />
              {{ app.name }}
            </span>
          </div>
        </div>
      </div>
    </NoticeCallout>

    <!-- Confirmation input -->
    <div class="mt-5">
      <label class="block text-sm text-slate-700 dark:text-slate-300">
        Type <span class="font-mono font-semibold text-slate-900 dark:text-slate-50">{{ projectName }}</span> to confirm
      </label>
      <input
        v-model="confirmText"
        type="text"
        :placeholder="projectName"
        class="mt-1.5 block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-red-500 focus:outline-none focus:ring-2 focus:ring-red-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
        @keyup.enter="canConfirm && emit('confirm')"
      />
    </div>

    <!-- Actions -->
    <div class="mt-6 flex justify-end gap-3">
      <button
        @click="emit('close')"
        class="rounded-lg border border-slate-300 px-4 py-2.5 text-sm font-medium text-slate-700 transition-colors hover:bg-slate-50 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-800"
      >
        Cancel
      </button>
      <button
        @click="emit('confirm')"
        :disabled="!canConfirm"
        class="rounded-lg bg-red-600 px-4 py-2.5 text-sm font-semibold text-white transition-colors hover:bg-red-700 disabled:opacity-40 disabled:cursor-not-allowed"
      >
        Delete project
      </button>
    </div>
  </div>
</template>
