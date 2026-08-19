<script setup lang="ts">
import type { ContainerHealth } from '@/api/apps'

interface Props {
  pod: string
  container: ContainerHealth
}
defineProps<Props>()

// The line that names the cause. A container between restarts reports only
// CrashLoopBackOff in its current state; the exit code and the process's own
// message are in the previous termination, so prefer those.
function failureDetail(container: ContainerHealth): string {
  const previous = container.last_termination
  if (previous) {
    const message = previous.message?.trim()
    const reason = previous.reason || 'exited'
    return message ? `${reason} (exit ${previous.exit_code}): ${message}` : `${reason} (exit ${previous.exit_code})`
  }
  if (container.exit_code !== undefined) {
    const message = container.message?.trim()
    return message ? `exit ${container.exit_code}: ${message}` : `exit ${container.exit_code}`
  }
  return container.message?.trim() || ''
}
</script>

<template>
  <div class="text-xs">
    <div class="flex flex-wrap items-center gap-x-2 gap-y-1">
      <span class="font-mono font-medium text-red-900 dark:text-red-200">{{ container.name }}</span>
      <span class="rounded bg-red-100 px-1.5 py-0.5 font-medium text-red-700 dark:bg-red-900/60 dark:text-red-300">
        {{ container.reason || container.state }}
      </span>
      <span v-if="container.restarts > 0" class="text-red-700 dark:text-red-400">
        restarted {{ container.restarts }}&times;
      </span>
      <span class="font-mono text-red-500 dark:text-red-500">{{ pod }}</span>
    </div>
    <p
      v-if="failureDetail(container)"
      class="mt-0.5 break-words font-mono text-red-800 dark:text-red-300"
    >{{ failureDetail(container) }}</p>
    <pre
      v-if="container.log"
      data-testid="app-health-log"
      class="mt-1 max-h-40 overflow-auto rounded bg-red-100/60 p-2 font-mono text-[11px] leading-relaxed text-red-900 dark:bg-red-950/60 dark:text-red-200"
    >{{ container.log }}</pre>
  </div>
</template>
