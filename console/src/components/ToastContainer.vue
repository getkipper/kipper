<script setup lang="ts">
import { useToast } from '@/composables/useToast'
import { CheckCircle, XCircle, Info, X } from 'lucide-vue-next'

const { toasts, dismiss } = useToast()
</script>

<template>
  <div class="fixed bottom-4 right-4 z-[200] flex flex-col gap-2">
    <transition-group name="toast">
      <div
        v-for="toast in toasts"
        :key="toast.id"
        :role="toast.type === 'error' ? 'alert' : 'status'"
        class="flex items-center gap-2.5 rounded-lg py-3 pl-4 pr-2.5 text-sm font-medium shadow-lg"
        :class="{
          'bg-emerald-600 text-white': toast.type === 'success',
          'bg-red-600 text-white': toast.type === 'error',
          'bg-slate-800 text-white dark:bg-slate-700': toast.type === 'info',
        }"
      >
        <CheckCircle v-if="toast.type === 'success'" class="h-4 w-4 shrink-0" :stroke-width="2" />
        <XCircle v-if="toast.type === 'error'" class="h-4 w-4 shrink-0" :stroke-width="2" />
        <Info v-if="toast.type === 'info'" class="h-4 w-4 shrink-0" :stroke-width="2" />
        <span class="min-w-0">{{ toast.message }}</span>
        <button
          type="button"
          aria-label="Dismiss"
          class="ml-1 shrink-0 rounded p-0.5 text-white/70 transition-colors hover:bg-white/15 hover:text-white"
          @click="dismiss(toast.id)"
        >
          <X class="h-3.5 w-3.5" :stroke-width="2.5" />
        </button>
      </div>
    </transition-group>
  </div>
</template>

<style scoped>
.toast-enter-active {
  transition: all 0.3s ease-out;
}
.toast-leave-active {
  transition: all 0.2s ease-in;
}
.toast-enter-from {
  transform: translateX(100%);
  opacity: 0;
}
.toast-leave-to {
  transform: translateX(100%);
  opacity: 0;
}
</style>
