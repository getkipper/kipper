<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { Sparkles, X, Copy, Check, RotateCw } from 'lucide-vue-next'
import { streamLogAnalysis } from '@/api/ai-logs'
import { renderMarkdown, handleCodeCopyClick } from '@/utils/markdown'

interface Props {
  logs: string
  appName: string
  namespace: string
}

const props = defineProps<Props>()
const emit = defineEmits<{ close: [] }>()

const analysis = ref('')
const analysing = ref(true)
const error = ref('')
const copied = ref(false)

async function run() {
  analysis.value = ''
  error.value = ''
  analysing.value = true
  await streamLogAnalysis(
    { logs: props.logs, app_name: props.appName, namespace: props.namespace },
    (content) => { analysis.value += content },
    () => { analysing.value = false },
    (err) => { error.value = err; analysing.value = false },
  )
}

onMounted(run)

function retry() { run() }

function copyAll() {
  navigator.clipboard.writeText(analysis.value)
  copied.value = true
  setTimeout(() => { copied.value = false }, 2000)
}
</script>

<template>
  <div class="max-h-[80vh] w-full max-w-2xl overflow-y-auto rounded-xl border border-slate-700 bg-slate-900 shadow-2xl" @mousedown.stop>
    <!-- Header -->
    <div class="sticky top-0 z-10 flex items-center justify-between border-b border-slate-800 bg-slate-900 px-5 py-3">
      <div class="flex items-center gap-1.5 text-sm font-semibold text-kipper-400">
        <Sparkles class="h-4 w-4" />
        AI Analysis
      </div>
      <div class="flex items-center gap-1">
        <button
          v-if="!analysing"
          @click="retry"
          class="rounded-md p-1 text-slate-500 hover:bg-slate-800 hover:text-slate-300"
          title="Retry analysis"
        >
          <RotateCw class="h-3.5 w-3.5" />
        </button>
        <button
          v-if="analysis && !analysing"
          @click="copyAll"
          class="inline-flex items-center gap-1 rounded-md px-2 py-1 text-xs text-slate-400 hover:bg-slate-800 hover:text-slate-300"
        >
          <Check v-if="copied" class="h-3.5 w-3.5 text-emerald-400" />
          <Copy v-else class="h-3.5 w-3.5" />
          {{ copied ? 'Copied' : 'Copy all' }}
        </button>
        <button @click="emit('close')" class="rounded-lg p-1 text-slate-500 hover:bg-slate-800 hover:text-slate-400">
          <X class="h-4 w-4" />
        </button>
      </div>
    </div>

    <!-- Content -->
    <div class="p-5">
      <div v-if="error" class="text-sm text-red-400">{{ error }}</div>

      <div v-else-if="analysis" class="ai-prose text-sm leading-relaxed text-slate-300" v-html="renderMarkdown(analysis)" @click="handleCodeCopyClick" />

      <div v-else-if="analysing" class="flex items-center gap-2 py-4 text-sm text-slate-500">
        <span class="inline-block h-2 w-2 animate-pulse rounded-full bg-kipper-500" />
        <span class="inline-block h-2 w-2 animate-pulse rounded-full bg-kipper-500" style="animation-delay: 0.15s" />
        <span class="inline-block h-2 w-2 animate-pulse rounded-full bg-kipper-500" style="animation-delay: 0.3s" />
        <span class="ml-1">Analysing logs...</span>
      </div>
    </div>
  </div>
</template>
