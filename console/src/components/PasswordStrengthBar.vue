<script setup lang="ts">
import { toRef } from 'vue'
import { usePasswordStrength } from '@/composables/usePasswordStrength'

const props = defineProps<{ password: string }>()
const passwordRef = toRef(props, 'password')
const { strength } = usePasswordStrength(passwordRef)
</script>

<template>
  <div v-if="password.length > 0" class="space-y-2">
    <div class="flex items-center gap-2">
      <div class="h-1.5 flex-1 rounded-full bg-slate-200 dark:bg-slate-700 overflow-hidden">
        <div
          class="h-full rounded-full transition-all duration-300"
          :style="{ width: strength.score + '%', backgroundColor: strength.color }"
        />
      </div>
      <span class="text-[10px] font-semibold" :style="{ color: strength.color }">{{ strength.label }}</span>
    </div>
    <div class="flex flex-wrap gap-x-3 gap-y-1">
      <span
        v-for="check in strength.checks"
        :key="check.label"
        class="text-[10px]"
        :class="check.met ? 'text-emerald-600 dark:text-emerald-400' : 'text-slate-400 dark:text-slate-500'"
      >
        {{ check.met ? '\u2713' : '\u2717' }} {{ check.label }}
      </span>
    </div>
  </div>
</template>
