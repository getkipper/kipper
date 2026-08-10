<script setup lang="ts">
import type { TokenHealth } from '@/api/registry'

interface Props {
  health?: TokenHealth
}
defineProps<Props>()
</script>

<template>
  <template v-if="health">
    <!-- Expired / invalid -->
    <span
      v-if="!health.valid"
      class="inline-flex items-center rounded-full bg-red-100 px-2 py-0.5 text-xs font-medium text-red-700 dark:bg-red-950 dark:text-red-400"
      :title="health.error || 'Authentication failed'"
    >
      Expired
    </span>

    <!-- Expiring soon (≤ 30 days) -->
    <span
      v-else-if="health.expires_at && health.days_remaining !== undefined && health.days_remaining <= 30"
      class="inline-flex items-center rounded-full bg-amber-100 px-2 py-0.5 text-xs font-medium text-amber-700 dark:bg-amber-950 dark:text-amber-400"
      :title="`Expires ${health.expires_at}`"
    >
      Expiring in {{ health.days_remaining }}d
    </span>

    <!-- Valid with known expiry -->
    <span
      v-else-if="health.expires_at"
      class="inline-flex items-center rounded-full bg-emerald-100 px-2 py-0.5 text-xs font-medium text-emerald-700 dark:bg-emerald-950 dark:text-emerald-400"
      :title="`Expires ${health.expires_at}`"
    >
      Valid
    </span>

    <!-- Valid, no expiry info (e.g. GitHub) -->
    <span
      v-else
      class="inline-flex items-center rounded-full bg-emerald-100 px-2 py-0.5 text-xs font-medium text-emerald-700 dark:bg-emerald-950 dark:text-emerald-400"
    >
      Valid
    </span>
  </template>
</template>
