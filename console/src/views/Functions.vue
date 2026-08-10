<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { useRouter } from 'vue-router'
import { Zap, Plus, Trash2, RefreshCw, ExternalLink, Sparkles, Settings2 } from 'lucide-vue-next'
import DiagnoseModal from '@/components/DiagnoseModal.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import { useModal } from '@/composables/useModal'
import { useToast } from '@/composables/useToast'
import { fetchFunctions, deleteFunction, type FunctionInfo } from '@/api/functions'
import { useProjectsStore } from '@/stores/projects'
import { useAuthStore } from '@/stores/auth'

const router = useRouter()
const toast = useToast()
const projectsStore = useProjectsStore()
const authStore = useAuthStore()
const modal = useModal()

const selectedNamespace = computed(() => projectsStore.globalNamespace || 'default')

const functions = ref<FunctionInfo[]>([])
const loading = ref(false)
const refreshing = ref(false)

onMounted(loadFunctions)
watch(selectedNamespace, loadFunctions)

async function loadFunctions() {
  loading.value = true
  try {
    functions.value = await fetchFunctions(selectedNamespace.value)
  } catch {
    toast.error('Failed to load functions')
  } finally {
    loading.value = false
  }
}

async function refreshList() {
  refreshing.value = true
  try {
    functions.value = await fetchFunctions(selectedNamespace.value)
    toast.success('Functions refreshed')
  } catch {
    toast.error('Failed to refresh')
  } finally {
    refreshing.value = false
  }
}

function handleDelete(name: string) {
  modal.open(ConfirmDialog, {
    title: `Delete function ${name}?`,
    message: 'This permanently removes the function and its route. This cannot be undone.',
    confirmLabel: 'Delete function',
    onConfirm: async () => {
      modal.close()
      try {
        await deleteFunction(selectedNamespace.value, name)
        toast.success(`Function ${name} deleted`)
        await loadFunctions()
      } catch {
        toast.error(`Failed to delete ${name}`)
      }
    },
  })
}

function openDiagnose(fnName: string) {
  modal.open(DiagnoseModal, {
    project: selectedNamespace.value,
    appName: fnName,
    kind: 'function',
  })
}

function newFunction() {
  router.push({ name: 'function-new' })
}

function openFn(name: string) {
  router.push({ name: 'function-edit', params: { fn: name } })
}

function statusColor(status: string): string {
  switch (status) {
    case 'running': return 'text-emerald-700 dark:text-emerald-400'
    case 'scaling': return 'text-amber-700 dark:text-amber-400'
    case 'idle': return 'text-kipper-700 dark:text-kipper-400'
    default: return 'text-slate-500'
  }
}

function statusDot(status: string): string {
  switch (status) {
    case 'running': return 'bg-emerald-500'
    case 'scaling': return 'bg-amber-500'
    case 'idle': return 'bg-kipper-500'
    default: return 'bg-slate-400'
  }
}

function displayImage(image: string): string {
  if (image.includes('kipper-runtime-node')) return 'inline (Node.js)'
  if (image.includes('kipper-runtime-python')) return 'inline (Python)'
  return image
}
</script>

<template>
  <div class="animate-fade-in">
    <!-- Header -->
    <div class="mb-8 flex items-center justify-between">
      <div>
        <h1 class="text-2xl font-semibold tracking-tight text-slate-900 dark:text-slate-50">Functions</h1>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Serverless functions with automatic scale-to-zero</p>
      </div>
      <div class="flex items-center gap-2">
        <button
          v-if="authStore.isDeployer"
          @click="newFunction"
          class="inline-flex items-center gap-1.5 rounded-lg bg-kipper-600 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-kipper-700"
        >
          <Plus class="h-4 w-4" />
          New function
        </button>
        <button
          @click="refreshList"
          :disabled="refreshing"
          class="rounded-lg border border-slate-300 p-2.5 text-slate-600 transition-colors hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
        >
          <RefreshCw class="h-4 w-4" :class="refreshing ? 'animate-spin' : ''" :stroke-width="1.75" />
        </button>
      </div>
    </div>

    <div v-if="loading" class="py-12 text-center text-sm text-slate-500 dark:text-slate-400">Loading...</div>

    <div v-else-if="functions.length" class="space-y-3">
      <div
        v-for="fn in functions"
        :key="fn.name"
        class="group rounded-xl border border-slate-200 bg-white p-5 transition-colors hover:border-kipper-300 dark:border-slate-800 dark:bg-slate-900 dark:hover:border-kipper-700 cursor-pointer"
        @click="openFn(fn.name)"
      >
        <div class="flex items-center justify-between">
          <div class="flex min-w-0 items-center gap-4">
            <span class="inline-flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-slate-100 dark:bg-slate-800">
              <Zap class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
            </span>
            <div class="min-w-0">
              <div class="flex flex-wrap items-center gap-2">
                <span class="text-sm font-semibold text-slate-900 dark:text-slate-50">{{ fn.name }}</span>
                <span class="rounded-full bg-kipper-100 px-2 py-0.5 text-xs font-medium text-kipper-700 dark:bg-kipper-900 dark:text-kipper-300">
                  {{ fn.trigger }} trigger
                </span>
              </div>
              <div class="mt-1 flex flex-col gap-0.5 text-xs text-slate-500 dark:text-slate-400">
                <span class="break-all font-mono">{{ displayImage(fn.image) }}</span>
                <a
                  v-if="fn.url"
                  :href="fn.url"
                  target="_blank"
                  rel="noopener"
                  class="break-all text-kipper-600 hover:text-kipper-700 dark:text-kipper-400"
                  @click.stop
                >{{ fn.url }} <ExternalLink class="inline h-3 w-3" /></a>
              </div>
            </div>
          </div>

          <div class="flex items-center gap-4">
            <span class="inline-flex items-center gap-1.5 text-xs" :class="statusColor(fn.status)">
              <span class="inline-block h-2 w-2 rounded-full" :class="statusDot(fn.status)" />
              {{ fn.status }}
            </span>
            <button
              @click.stop="openDiagnose(fn.name)"
              class="rounded-lg p-1.5 text-slate-400 md:opacity-0 transition-opacity hover:bg-amber-50 hover:text-amber-600 group-hover:opacity-100 dark:hover:bg-amber-950 dark:hover:text-amber-400"
              title="AI Diagnose"
            >
              <Sparkles class="h-4 w-4" />
            </button>
            <button
              @click.stop="openFn(fn.name)"
              class="rounded-lg p-1.5 text-slate-400 md:opacity-0 transition-opacity hover:bg-kipper-50 hover:text-kipper-600 group-hover:opacity-100 dark:hover:bg-kipper-950 dark:hover:text-kipper-400"
              title="Configure"
            >
              <Settings2 class="h-4 w-4" />
            </button>
            <button
              v-if="authStore.isDeployer"
              @click.stop="handleDelete(fn.name)"
              class="rounded-lg p-1.5 text-slate-400 md:opacity-0 transition-opacity hover:bg-red-50 hover:text-red-600 group-hover:opacity-100 dark:hover:bg-red-950 dark:hover:text-red-400"
              title="Delete"
            >
              <Trash2 class="h-4 w-4" />
            </button>
          </div>
        </div>
      </div>
    </div>

    <div v-else class="rounded-xl border border-dashed border-slate-300 py-16 text-center dark:border-slate-700">
      <Zap class="mx-auto mb-3 h-10 w-10 text-slate-400 dark:text-slate-500" :stroke-width="1.5" />
      <p class="text-sm font-medium text-slate-900 dark:text-slate-50">No functions deployed</p>
      <p class="mt-1 max-w-sm mx-auto text-sm text-slate-500 dark:text-slate-400">
        Click <span class="font-medium">New function</span> to build one in the console, or use
        <code class="rounded bg-slate-100 px-1.5 py-0.5 font-mono text-xs dark:bg-slate-800">kip function create</code> from the CLI.
      </p>
    </div>
  </div>
</template>
