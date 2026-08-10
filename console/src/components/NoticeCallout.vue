<script setup lang="ts">
interface Props {
  tone: 'info' | 'success' | 'warning' | 'danger'
}
defineProps<Props>()

// Light mode keeps the classic tinted wash; dark mode renders a neutral
// card with a coloured accent edge, matching the TwoFactorNudge banner.
// Call sites own their padding and layout classes.
const toneClasses: Record<Props['tone'], string> = {
  info: 'border-sky-200 bg-sky-50 dark:shadow-[inset_3px_0_0_theme(colors.sky.400)]',
  success: 'border-emerald-200 bg-emerald-50 dark:shadow-[inset_3px_0_0_theme(colors.emerald.400)]',
  warning: 'border-amber-200 bg-amber-50 dark:shadow-[inset_3px_0_0_theme(colors.orange.300)]',
  danger: 'border-red-200 bg-red-50 dark:shadow-[inset_3px_0_0_theme(colors.rose.400)]',
}
</script>

<template>
  <!-- Square left corners in dark mode keep the accent edge a straight line. -->
  <div class="rounded-lg border dark:rounded-l-none dark:border-slate-800 dark:bg-slate-900" :class="toneClasses[tone]">
    <slot />
  </div>
</template>
