<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { X, Copy, Check, TrendingDown, RotateCw } from 'lucide-vue-next'
import { streamOptimisation } from '@/api/ai-resources'
import { renderMarkdown, handleCodeCopyClick } from '@/utils/markdown'

interface Props {
  project: string
  appName: string
}

const props = defineProps<Props>()
const emit = defineEmits<{ close: [] }>()

const result = ref('')
const loading = ref(true)
const error = ref('')
const copied = ref(false)

async function run() {
  result.value = ''
  error.value = ''
  loading.value = true
  await streamOptimisation(
    props.project, props.appName,
    (content) => { result.value += content },
    () => { loading.value = false },
    (err) => { error.value = err; loading.value = false },
  )
}

onMounted(run)

function retry() { run() }

function copyAll() {
  navigator.clipboard.writeText(result.value)
  copied.value = true
  setTimeout(() => { copied.value = false }, 2000)
}
</script>

<template>
  <div class="max-h-[80vh] w-full max-w-2xl overflow-y-auto rounded-xl border border-slate-700 bg-slate-900 shadow-2xl" @mousedown.stop>
    <div class="sticky top-0 z-10 flex items-center justify-between border-b border-slate-800 bg-slate-900 px-5 py-3">
      <div class="flex items-center gap-1.5 text-sm font-semibold text-emerald-400">
        <TrendingDown class="h-4 w-4" />
        Resource Optimisation — {{ appName }}
      </div>
      <div class="flex items-center gap-1">
        <button
          v-if="!loading"
          @click="retry"
          class="rounded-md p-1 text-slate-500 hover:bg-slate-800 hover:text-slate-300"
          title="Retry"
        >
          <RotateCw class="h-3.5 w-3.5" />
        </button>
        <button
          v-if="result && !loading"
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

    <div class="p-5">
      <div v-if="error" class="text-sm text-red-400">{{ error }}</div>
      <div v-else-if="result" class="ai-prose text-sm leading-relaxed text-slate-300" v-html="renderMarkdown(result)" @click="handleCodeCopyClick" />
      <div v-else-if="loading" class="flex items-center gap-2 py-4 text-sm text-slate-500">
        <span class="inline-block h-2 w-2 animate-pulse rounded-full bg-emerald-500" />
        <span class="inline-block h-2 w-2 animate-pulse rounded-full bg-emerald-500" style="animation-delay: 0.15s" />
        <span class="inline-block h-2 w-2 animate-pulse rounded-full bg-emerald-500" style="animation-delay: 0.3s" />
        <span class="ml-1">Analysing resource usage...</span>
      </div>
    </div>
  </div>
</template>
