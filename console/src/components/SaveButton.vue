<script setup lang="ts">
import { ref, watch } from 'vue'
import { Loader2, Check } from 'lucide-vue-next'

interface Props {
  saving: boolean
  label?: string
  savedLabel?: string
  disabled?: boolean
}

const props = withDefaults(defineProps<Props>(), {
  label: 'Save',
  savedLabel: 'Saved',
  disabled: false,
})

const showCheck = ref(false)
const wasSaving = ref(false)

watch(() => props.saving, (isSaving) => {
  if (wasSaving.value && !isSaving) {
    showCheck.value = true
    setTimeout(() => { showCheck.value = false }, 2000)
  }
  wasSaving.value = isSaving
})
</script>

<template>
  <button
    :disabled="saving || disabled"
    class="inline-flex items-center gap-1.5 rounded-lg px-4 py-2 text-sm font-medium text-white transition-all disabled:opacity-50"
    :class="showCheck ? 'bg-emerald-600 hover:bg-emerald-600' : 'bg-kipper-600 hover:bg-kipper-700'"
  >
    <Loader2 v-if="saving" class="h-4 w-4 animate-spin" />
    <Check v-else-if="showCheck" class="h-4 w-4" />
    <span>{{ saving ? 'Saving...' : showCheck ? savedLabel : label }}</span>
  </button>
</template>
