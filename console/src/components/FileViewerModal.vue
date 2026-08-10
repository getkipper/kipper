<script setup lang="ts">
import { ref, onMounted, onBeforeUnmount } from 'vue'
import { File, X, Pencil, Download } from 'lucide-vue-next'
import SaveButton from '@/components/SaveButton.vue'
import * as filesApi from '@/api/files'
import { useToast } from '@/composables/useToast'

interface Props {
  project: string
  appName: string
  filePath: string
  fileName: string
  fileSize: number
}

const props = defineProps<Props>()
const emit = defineEmits<{ close: [] }>()

const toast = useToast()
const content = ref('')
const loading = ref(true)
const editing = ref(false)
const saving = ref(false)
const tooLarge = ref(false)

onMounted(async () => {
  if (props.fileSize > 1024 * 1024) {
    tooLarge.value = true
    loading.value = false
    return
  }

  try {
    content.value = await filesApi.readFileContent(props.project, props.appName, props.filePath)
  } catch {
    content.value = 'Failed to load file content'
  } finally {
    loading.value = false
  }
})

async function save() {
  saving.value = true
  try {
    await filesApi.saveFileContent(props.project, props.appName, props.filePath, content.value)
    toast.success('File saved')
    editing.value = false
  } catch {
    toast.error('Failed to save file')
  } finally {
    saving.value = false
  }
}

async function download() {
  try {
    await filesApi.downloadFile(props.project, props.appName, props.filePath)
  } catch {
    toast.error('Failed to download')
  }
}

function onKeydown(e: KeyboardEvent) {
  if (e.key === 'Escape') {
    e.stopImmediatePropagation()
    emit('close')
  }
}

onMounted(() => {
  document.addEventListener('keydown', onKeydown, true)
})

onBeforeUnmount(() => {
  document.removeEventListener('keydown', onKeydown, true)
})
</script>

<template>
  <div class="mx-4 flex max-h-[85vh] w-full max-w-3xl flex-col rounded-xl border border-slate-700 bg-white shadow-2xl dark:bg-slate-900" @mousedown.stop>
    <!-- Header -->
    <div class="flex items-center justify-between border-b border-slate-200 px-5 py-3 dark:border-slate-700">
      <div class="flex min-w-0 items-center gap-2">
        <File class="h-4 w-4 shrink-0 text-slate-400" />
        <span class="truncate font-mono text-sm font-medium text-slate-900 dark:text-slate-50">{{ fileName }}</span>
      </div>
      <div class="flex items-center gap-1">
        <button
          v-if="!tooLarge && !loading"
          @click="download"
          class="rounded-md p-1.5 text-slate-400 hover:bg-slate-100 dark:hover:bg-slate-800"
          title="Download"
        >
          <Download class="h-4 w-4" />
        </button>
        <button
          v-if="!tooLarge && !loading"
          @click="editing = !editing"
          class="rounded-md p-1.5 transition-colors"
          :class="editing ? 'bg-kipper-100 text-kipper-600 dark:bg-kipper-900 dark:text-kipper-400' : 'text-slate-400 hover:bg-slate-100 dark:hover:bg-slate-800'"
          :title="editing ? 'View mode' : 'Edit mode'"
        >
          <Pencil class="h-4 w-4" />
        </button>
        <SaveButton
          v-if="editing"
          :saving="saving"
          label="Save"
          @click="save"
        />
        <button @click="emit('close')" class="rounded-md p-1.5 text-slate-400 hover:bg-slate-100 dark:hover:bg-slate-800">
          <X class="h-4 w-4" />
        </button>
      </div>
    </div>

    <!-- Content -->
    <div class="flex-1 overflow-auto">
      <div v-if="loading" class="p-5 text-sm text-slate-500">Loading file content...</div>
      <div v-else-if="tooLarge" class="p-8 text-center">
        <p class="text-sm text-slate-500 dark:text-slate-400">File too large to preview (exceeds 1MB)</p>
        <button
          @click="download"
          class="mt-3 inline-flex items-center gap-1.5 rounded-md bg-kipper-600 px-4 py-2 text-sm font-medium text-white hover:bg-kipper-700"
        >
          <Download class="h-4 w-4" />
          Download file
        </button>
      </div>
      <div v-else-if="editing" class="flex flex-1 flex-col">
        <div class="bg-amber-50 px-4 py-1.5 text-[10px] text-amber-700 dark:bg-slate-900 dark:text-slate-400 dark:shadow-[inset_3px_0_0_theme(colors.orange.300)]">
          Changes are saved to all running pods but will be lost on the next deployment.
        </div>
        <textarea
          v-model="content"
          class="flex-1 min-h-[400px] w-full resize-none bg-slate-950 p-4 font-mono text-xs leading-relaxed text-slate-300 outline-none"
        />
      </div>
      <pre v-else class="overflow-auto bg-slate-950 p-4 font-mono text-xs leading-relaxed text-slate-300">{{ content }}</pre>
    </div>
  </div>
</template>
