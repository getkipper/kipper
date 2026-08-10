<script setup lang="ts">
import { Sparkles } from 'lucide-vue-next'
import { useModal } from '@/composables/useModal'
import LogAnalysisModal from '@/components/LogAnalysisModal.vue'

interface Props {
  logs: string
  appName: string
  namespace: string
}

const props = defineProps<Props>()
const modal = useModal()

function analyse() {
  modal.open(LogAnalysisModal, {
    logs: props.logs,
    appName: props.appName,
    namespace: props.namespace,
  })
}
</script>

<template>
  <button
    @click="analyse"
    :disabled="!logs"
    class="inline-flex items-center gap-1 rounded-md px-2 py-1 text-xs font-medium transition-colors text-slate-400 hover:bg-slate-800 hover:text-slate-300 disabled:opacity-40 disabled:pointer-events-none"
    :title="!logs ? 'No logs to analyse' : 'Analyse logs with AI'"
  >
    <Sparkles class="h-3 w-3" />
    Analyse
  </button>
</template>
