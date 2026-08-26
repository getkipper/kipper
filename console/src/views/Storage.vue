<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { FolderOpen, File, Folder, Upload, Trash2, Share2, FolderPlus, RefreshCw, ChevronRight, Copy, X, Download, Globe, Lock, ExternalLink } from 'lucide-vue-next'
import { useToast } from '@/composables/useToast'
import { useModal } from '@/composables/useModal'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import { formatDateTime } from '@/utils/datetime'
import { useCapabilities } from '@/composables/useCapabilities'
import { fetchServices, type ServiceStatus } from '@/api/services'
import {
  fetchBuckets,
  createBucket,
  createFolder,
  fetchObjects,
  uploadFile,
  downloadFile,
  deleteObject,
  shareObject,
  makePublic,
  makePrivate,
  type Bucket,
  type StorageObject,
  type ShareLink,
} from '@/api/storage'

const toast = useToast()
const modal = useModal()
const { canInNamespace } = useCapabilities()
const route = useRoute()
const router = useRouter()

const services = ref<ServiceStatus[]>([])
const selectedService = ref((route.query.service as string) || '')
// Storage endpoints require an explicit namespace, so a service name that
// collides across projects resolves to exactly one instance. The selector binds
// a composite key so two same-named services in different namespaces stay
// distinct, and the URL carries the namespace so a bookmark restores the exact one.
const selectedNamespace = ref((route.query.namespace as string) || '')
const selectedKey = computed<string>({
  get: () => (selectedService.value ? `${selectedNamespace.value}/${selectedService.value}` : ''),
  set: (key) => {
    const svc = services.value.find(s => `${s.namespace}/${s.name}` === key)
    selectedService.value = svc?.name ?? ''
    selectedNamespace.value = svc?.namespace ?? ''
  },
})
const buckets = ref<Bucket[]>([])
const selectedBucket = ref((route.query.bucket as string) || '')
const objects = ref<StorageObject[]>([])
const prefix = ref((route.query.prefix as string) || '')
const loading = ref(false)
const loadingObjects = ref(false)
const refreshing = ref(false)

function syncURL() {
  const query: Record<string, string> = {}
  if (selectedService.value) query.service = selectedService.value
  if (selectedNamespace.value) query.namespace = selectedNamespace.value
  if (selectedBucket.value) query.bucket = selectedBucket.value
  if (prefix.value) query.prefix = prefix.value
  router.replace({ query })
}

const showCreateBucket = ref(false)
const newBucketName = ref('')
const creatingBucket = ref(false)

const showCreateFolder = ref(false)
const newFolderName = ref('')
const creatingFolder = ref(false)

const uploading = ref(false)
const fileInput = ref<HTMLInputElement | null>(null)

const confirmDelete = ref<string | null>(null)
const shareLink = ref<ShareLink | null>(null)
const shareKey = ref('')

// Bulk selection
const selectedKeys = ref<Set<string>>(new Set())
const bulkRunning = ref(false)
const bulkAction = ref('')
const bulkTotal = ref(0)
const bulkProcessed = ref(0)
const bulkCurrentFile = ref('')

const allFilesSelected = computed(() => {
  const files = sortedObjects.value.filter(o => !o.is_dir)
  return files.length > 0 && files.every(o => selectedKeys.value.has(o.key))
})

function toggleSelect(key: string) {
  const s = new Set(selectedKeys.value)
  if (s.has(key)) {
    s.delete(key)
  } else {
    s.add(key)
  }
  selectedKeys.value = s
}

function toggleSelectAll() {
  const files = sortedObjects.value.filter(o => !o.is_dir)
  if (allFilesSelected.value) {
    selectedKeys.value = new Set()
  } else {
    selectedKeys.value = new Set(files.map(o => o.key))
  }
}

function clearSelection() {
  selectedKeys.value = new Set()
}

function confirmBulkDelete() {
  const count = selectedKeys.value.size
  if (count === 0) return
  modal.open(ConfirmDialog, {
    title: `Delete ${count} file${count === 1 ? '' : 's'}?`,
    message: 'The selected files are permanently deleted from this bucket. This cannot be undone.',
    confirmLabel: 'Delete files',
    onConfirm: () => {
      modal.close()
      bulkDelete()
    },
  })
}

async function bulkDelete() {
  if (!selectedService.value || !selectedBucket.value) return
  const keys = [...selectedKeys.value]
  bulkAction.value = 'Deleting'
  bulkTotal.value = keys.length
  bulkProcessed.value = 0
  bulkRunning.value = true

  let failed = 0
  for (const key of keys) {
    bulkCurrentFile.value = objectName(key)
    try {
      await deleteObject(selectedService.value, selectedNamespace.value, selectedBucket.value, key)
    } catch {
      failed++
    }
    bulkProcessed.value++
  }

  bulkRunning.value = false
  clearSelection()
  await loadObjects()
  if (failed > 0) {
    toast.error(`Deleted ${keys.length - failed} files, ${failed} failed`)
  } else {
    toast.success(`Deleted ${keys.length} files`)
  }
}

async function bulkMakePublic() {
  if (!selectedService.value || !selectedBucket.value) return
  const keys = [...selectedKeys.value]
  bulkAction.value = 'Making public'
  bulkTotal.value = keys.length
  bulkProcessed.value = 0
  bulkRunning.value = true

  for (const key of keys) {
    bulkCurrentFile.value = objectName(key)
    try {
      await makePublic(selectedService.value, selectedNamespace.value, selectedBucket.value, key)
    } catch { /* continue */ }
    bulkProcessed.value++
  }

  bulkRunning.value = false
  clearSelection()
  await loadObjects()
  toast.success(`${keys.length} files made public`)
}

async function bulkMakePrivate() {
  if (!selectedService.value || !selectedBucket.value) return
  const keys = [...selectedKeys.value]
  bulkAction.value = 'Making private'
  bulkTotal.value = keys.length
  bulkProcessed.value = 0
  bulkRunning.value = true

  for (const key of keys) {
    bulkCurrentFile.value = objectName(key)
    try {
      await makePrivate(selectedService.value, selectedNamespace.value, selectedBucket.value, key)
    } catch { /* continue */ }
    bulkProcessed.value++
  }

  bulkRunning.value = false
  clearSelection()
  await loadObjects()
  toast.success(`${keys.length} files made private`)
}

async function bulkDownload() {
  if (!selectedService.value || !selectedBucket.value) return
  const keys = [...selectedKeys.value]
  bulkAction.value = 'Downloading'
  bulkTotal.value = keys.length
  bulkProcessed.value = 0
  bulkRunning.value = true

  for (const key of keys) {
    bulkCurrentFile.value = objectName(key)
    try {
      await downloadFile(selectedService.value, selectedNamespace.value, selectedBucket.value, key)
    } catch { /* continue */ }
    bulkProcessed.value++
  }

  bulkRunning.value = false
  clearSelection()
  toast.success(`Downloaded ${keys.length} files`)
}

const minioServices = computed(() =>
  services.value.filter((s) => s.type === 'minio'),
)

// Service names are unique only within a namespace. When the same name appears in
// more than one accessible namespace, show the namespace in the selector so the
// two stay distinguishable.
const duplicateServiceNames = computed(() => {
  const counts = new Map<string, number>()
  for (const s of minioServices.value) counts.set(s.name, (counts.get(s.name) ?? 0) + 1)
  return new Set([...counts.entries()].filter(([, n]) => n > 1).map(([n]) => n))
})

const breadcrumbs = computed(() => {
  if (!prefix.value) return []
  const parts = prefix.value.split('/').filter(Boolean)
  return parts.map((part, i) => ({
    label: part,
    prefix: parts.slice(0, i + 1).join('/') + '/',
  }))
})

const sortedObjects = computed(() => {
  const dirs = objects.value.filter((o) => o.is_dir)
  const files = objects.value.filter((o) => !o.is_dir)
  return [...dirs, ...files]
})

onMounted(async () => {
  loading.value = true
  try {
    const all = await fetchServices()
    services.value = all

    // Restore from URL (service + namespace identify one instance) or default to
    // the first service.
    const qService = (route.query.service as string) || ''
    const qNamespace = (route.query.namespace as string) || ''
    const qBucket = (route.query.bucket as string) || ''
    const qPrefix = (route.query.prefix as string) || ''

    const restored = minioServices.value.find(s => s.name === qService && s.namespace === qNamespace)
    if (restored) {
      selectedService.value = restored.name
      selectedNamespace.value = restored.namespace
      await loadBuckets()
      if (qBucket && buckets.value.some(b => b.name === qBucket)) {
        selectedBucket.value = qBucket
        prefix.value = qPrefix
        await loadObjects()
      }
    } else {
      const first = minioServices.value[0]
      if (first) {
        selectedService.value = first.name
        selectedNamespace.value = first.namespace
      }
    }
  } catch {
    toast.error('Failed to load services')
  } finally {
    loading.value = false
  }
})

watch([selectedService, selectedNamespace], async ([svc]) => {
  if (!svc) return
  selectedBucket.value = ''
  prefix.value = ''
  objects.value = []
  syncURL()
  await loadBuckets()
})

watch(selectedBucket, async (bucket) => {
  if (!bucket) return
  prefix.value = ''
  syncURL()
  await loadObjects()
})

async function loadBuckets() {
  if (!selectedService.value) return
  try {
    buckets.value = await fetchBuckets(selectedService.value, selectedNamespace.value)
    if (buckets.value.length && !selectedBucket.value) {
      selectedBucket.value = buckets.value[0].name
    }
  } catch {
    toast.error('Failed to load buckets')
  }
}

async function loadObjects() {
  if (!selectedService.value || !selectedBucket.value) return
  loadingObjects.value = true
  try {
    const result = await fetchObjects(selectedService.value, selectedNamespace.value, selectedBucket.value, prefix.value)
    objects.value = result.objects || []
  } catch {
    toast.error('Failed to load objects')
  } finally {
    loadingObjects.value = false
  }
}

async function handleRefresh() {
  refreshing.value = true
  try {
    await loadObjects()
    toast.success('Refreshed')
  } finally {
    refreshing.value = false
  }
}

async function handleCreateBucket() {
  if (!newBucketName.value || !selectedService.value) return
  creatingBucket.value = true
  try {
    await createBucket(selectedService.value, selectedNamespace.value, newBucketName.value)
    toast.success(`Bucket "${newBucketName.value}" created`)
    newBucketName.value = ''
    showCreateBucket.value = false
    await loadBuckets()
  } catch {
    toast.error('Failed to create bucket')
  } finally {
    creatingBucket.value = false
  }
}

async function handleCreateFolder() {
  if (!newFolderName.value || !selectedService.value || !selectedBucket.value) return
  creatingFolder.value = true
  try {
    const folderPrefix = prefix.value + newFolderName.value
    await createFolder(selectedService.value, selectedNamespace.value, selectedBucket.value, folderPrefix)
    toast.success(`Folder "${newFolderName.value}" created`)
    newFolderName.value = ''
    showCreateFolder.value = false
    await loadObjects()
  } catch {
    toast.error('Failed to create folder')
  } finally {
    creatingFolder.value = false
  }
}

function navigateToFolder(folderPrefix: string) {
  prefix.value = folderPrefix
  clearSelection()
  syncURL()
  loadObjects()
}

function navigateUp() {
  if (!prefix.value) return
  const parts = prefix.value.split('/').filter(Boolean)
  parts.pop()
  prefix.value = parts.length ? parts.join('/') + '/' : ''
  clearSelection()
  syncURL()
  loadObjects()
}

function objectName(key: string): string {
  const withoutPrefix = key.startsWith(prefix.value) ? key.slice(prefix.value.length) : key
  return withoutPrefix.replace(/\/$/, '')
}

function formatSize(bytes: number): string {
  if (bytes === 0) return '—'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  const value = bytes / Math.pow(1024, i)
  return `${value.toFixed(i > 0 ? 1 : 0)} ${units[i]}`
}

function formatDate(iso: string): string {
  if (!iso) return '—'
  return formatDateTime(iso)
}

const dragging = ref(false)

function triggerUpload() {
  fileInput.value?.click()
}

function onDragOver(e: DragEvent) {
  e.preventDefault()
  dragging.value = true
}

function onDragLeave() {
  dragging.value = false
}

async function onDrop(e: DragEvent) {
  e.preventDefault()
  dragging.value = false
  const files = e.dataTransfer?.files
  if (!files || !selectedService.value || !selectedBucket.value) return

  for (const file of Array.from(files)) {
    const key = prefix.value + file.name
    uploading.value = true
    try {
      await uploadFile(selectedService.value, selectedNamespace.value, selectedBucket.value, key, file)
      toast.success(`Uploaded ${file.name}`)
    } catch {
      toast.error(`Failed to upload ${file.name}`)
    }
  }
  uploading.value = false
  await loadObjects()
}

async function handleFileSelected(event: Event) {
  const target = event.target as HTMLInputElement
  const file = target.files?.[0]
  if (!file || !selectedService.value || !selectedBucket.value) return

  uploading.value = true
  const key = prefix.value + file.name
  try {
    await uploadFile(selectedService.value, selectedNamespace.value, selectedBucket.value, key, file)
    toast.success(`Uploaded ${file.name}`)
    await loadObjects()
  } catch {
    toast.error(`Failed to upload ${file.name}`)
  } finally {
    uploading.value = false
    target.value = ''
  }
}

async function handleDownload(key: string) {
  try {
    await downloadFile(selectedService.value, selectedNamespace.value, selectedBucket.value, key)
  } catch {
    toast.error('Failed to download file')
  }
}

async function openInNewTab(obj: StorageObject) {
  if (!selectedService.value || !selectedBucket.value) return
  try {
    if (obj.is_public) {
      // Public files have a direct URL
      const base = window.location.origin
      window.open(`${base}/api/v1/storage/${selectedService.value}/public/${selectedBucket.value}/${obj.key}?namespace=${encodeURIComponent(selectedNamespace.value)}`, '_blank')
    } else {
      // Private files: generate a short share link
      const link = await shareObject(selectedService.value, selectedNamespace.value, selectedBucket.value, obj.key, '1h')
      window.open(link.url, '_blank')
    }
  } catch {
    toast.error('Failed to open file')
  }
}

async function handleDelete(key: string) {
  try {
    await deleteObject(selectedService.value, selectedNamespace.value, selectedBucket.value, key)
    toast.success(`Deleted ${objectName(key)}`)
    confirmDelete.value = null
    await loadObjects()
  } catch {
    toast.error('Failed to delete file')
  }
}

async function togglePublic(obj: StorageObject) {
  if (!selectedService.value || !selectedBucket.value) return
  try {
    if (obj.is_public) {
      await makePrivate(selectedService.value, selectedNamespace.value, selectedBucket.value, obj.key)
      toast.success('Made private')
    } else {
      const resp = await makePublic(selectedService.value, selectedNamespace.value, selectedBucket.value, obj.key)
      await navigator.clipboard.writeText(resp.url)
      toast.success('Made public: URL copied to clipboard')
    }
    await loadObjects()
  } catch {
    toast.error('Failed to change access')
  }
}

const shareExpiry = ref('24h')
const shareGenerating = ref(false)

function openShare(key: string) {
  shareKey.value = key
  shareLink.value = null
  shareExpiry.value = '24h'
  shareModalOpen.value = true
}

const shareModalOpen = ref(false)

async function generateShareLink() {
  if (!selectedService.value || !selectedBucket.value || !shareKey.value) return
  shareGenerating.value = true
  try {
    shareLink.value = await shareObject(selectedService.value, selectedNamespace.value, selectedBucket.value, shareKey.value, shareExpiry.value)
  } catch {
    toast.error('Failed to generate share link')
  } finally {
    shareGenerating.value = false
  }
}

async function copyShareLink() {
  if (!shareLink.value) return
  try {
    await navigator.clipboard.writeText(shareLink.value.url)
    toast.success('Link copied to clipboard')
  } catch {
    toast.error('Failed to copy link')
  }
}
</script>

<template>
  <div class="animate-fade-in">
    <!-- Header -->
    <div class="mb-8 flex items-center justify-between">
      <div>
        <h1 class="text-2xl font-semibold tracking-tight text-slate-900 dark:text-slate-50">Storage</h1>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Browse and manage files in MinIO object storage</p>
      </div>
      <div class="flex items-center gap-2">
        <button
          v-if="selectedBucket && canInNamespace(selectedNamespace, 'storage.write')"
          @click="triggerUpload"
          :disabled="uploading"
          class="inline-flex items-center gap-1.5 rounded-lg bg-kipper-600 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-kipper-700 disabled:opacity-50"
        >
          <Upload class="h-4 w-4" />
          {{ uploading ? 'Uploading...' : 'Upload' }}
        </button>
        <button
          v-if="selectedBucket"
          @click="handleRefresh"
          :disabled="refreshing"
          class="rounded-lg border border-slate-300 p-2.5 text-slate-600 transition-colors hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
        >
          <RefreshCw class="h-4 w-4" :class="refreshing ? 'animate-spin' : ''" :stroke-width="1.75" />
        </button>
      </div>
    </div>

    <input ref="fileInput" type="file" class="hidden" @change="handleFileSelected" />

    <!-- Loading -->
    <div v-if="loading" class="py-12 text-center text-sm text-slate-500 dark:text-slate-400">Loading...</div>

    <!-- No MinIO services -->
    <div v-else-if="!minioServices.length" class="rounded-xl border border-dashed border-slate-300 py-16 text-center dark:border-slate-700">
      <FolderOpen class="mx-auto mb-3 h-10 w-10 text-slate-400 dark:text-slate-500" :stroke-width="1.5" />
      <p class="text-sm font-medium text-slate-900 dark:text-slate-50">No MinIO services</p>
      <p class="mt-1 max-w-sm mx-auto text-sm text-slate-500 dark:text-slate-400">
        Create a MinIO service with <code class="rounded bg-slate-100 px-1.5 py-0.5 font-mono text-xs dark:bg-slate-800">kip service add minio --name storage</code>
      </p>
    </div>

    <!-- Main content -->
    <div v-else class="space-y-4">
      <!-- Service and bucket selector -->
      <div class="flex flex-wrap items-center gap-4">
        <div class="w-48">
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Service</label>
          <select
            v-model="selectedKey"
            class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
          >
            <option v-for="svc in minioServices" :key="`${svc.namespace}/${svc.name}`" :value="`${svc.namespace}/${svc.name}`">
              {{ svc.name }}<template v-if="duplicateServiceNames.has(svc.name)"> ({{ svc.namespace }})</template>
            </option>
          </select>
        </div>

        <div class="w-48">
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Bucket</label>
          <select
            v-model="selectedBucket"
            class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
          >
            <option value="" disabled>Select bucket...</option>
            <option v-for="b in buckets" :key="b.name" :value="b.name">{{ b.name }}</option>
          </select>
        </div>

        <div class="flex items-end gap-2 pt-5">
          <button
            v-if="canInNamespace(selectedNamespace, 'storage.write')"
            @click="showCreateBucket = !showCreateBucket"
            class="inline-flex items-center gap-1.5 rounded-lg border border-slate-300 px-3 py-2 text-sm font-medium text-slate-600 transition-colors hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
          >
            <FolderPlus class="h-4 w-4" />
            New bucket
          </button>
          <button
            v-if="selectedBucket && canInNamespace(selectedNamespace, 'storage.write')"
            @click="showCreateFolder = !showCreateFolder"
            class="inline-flex items-center gap-1.5 rounded-lg border border-slate-300 px-3 py-2 text-sm font-medium text-slate-600 transition-colors hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
          >
            <FolderPlus class="h-4 w-4" />
            New folder
          </button>
        </div>
      </div>

      <!-- Create bucket form -->
      <div v-if="showCreateBucket" class="rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
        <h3 class="mb-4 text-sm font-semibold text-slate-900 dark:text-slate-50">Create bucket</h3>
        <div class="flex items-end gap-4">
          <div class="flex-1">
            <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Bucket name</label>
            <input
              v-model="newBucketName"
              type="text"
              placeholder="uploads"
              class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
              @keyup.enter="handleCreateBucket"
            />
          </div>
          <button
            @click="showCreateBucket = false"
            class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
          >
            Cancel
          </button>
          <button
            @click="handleCreateBucket"
            :disabled="!newBucketName || creatingBucket"
            class="rounded-lg bg-kipper-600 px-4 py-2 text-sm font-medium text-white hover:bg-kipper-700 disabled:opacity-50"
          >
            {{ creatingBucket ? 'Creating...' : 'Create' }}
          </button>
        </div>
      </div>

      <!-- Create folder form -->
      <div v-if="showCreateFolder" class="rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
        <h3 class="mb-4 text-sm font-semibold text-slate-900 dark:text-slate-50">Create folder</h3>
        <div class="flex items-end gap-4">
          <div class="flex-1">
            <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Folder name</label>
            <input
              v-model="newFolderName"
              type="text"
              placeholder="images"
              class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
              @keyup.enter="handleCreateFolder"
            />
          </div>
          <button
            @click="showCreateFolder = false"
            class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
          >
            Cancel
          </button>
          <button
            @click="handleCreateFolder"
            :disabled="!newFolderName || creatingFolder"
            class="rounded-lg bg-kipper-600 px-4 py-2 text-sm font-medium text-white hover:bg-kipper-700 disabled:opacity-50"
          >
            {{ creatingFolder ? 'Creating...' : 'Create' }}
          </button>
        </div>
      </div>

      <!-- Breadcrumbs -->
      <div v-if="selectedBucket" class="flex items-center gap-1 text-sm">
        <button
          @click="navigateToFolder('')"
          class="font-medium text-kipper-600 hover:text-kipper-700 dark:text-kipper-400 dark:hover:text-kipper-300"
        >
          {{ selectedBucket }}
        </button>
        <template v-for="crumb in breadcrumbs" :key="crumb.prefix">
          <ChevronRight class="h-4 w-4 text-slate-400" />
          <button
            @click="navigateToFolder(crumb.prefix)"
            class="font-medium text-kipper-600 hover:text-kipper-700 dark:text-kipper-400 dark:hover:text-kipper-300"
          >
            {{ crumb.label }}
          </button>
        </template>
      </div>

      <!-- Drop zone wrapper -->
      <div
        v-if="selectedBucket"
        @dragover="onDragOver"
        @dragleave="onDragLeave"
        @drop="onDrop"
        class="relative rounded-xl transition-colors"
        :class="dragging ? 'ring-2 ring-kipper-500 bg-kipper-50/50 dark:bg-kipper-950/50' : ''"
      >
        <div v-if="dragging" class="pointer-events-none absolute inset-0 z-10 flex items-center justify-center rounded-xl bg-kipper-600/10">
          <div class="text-center">
            <Upload class="mx-auto mb-2 h-8 w-8 text-kipper-500" />
            <p class="text-sm font-medium text-kipper-600 dark:text-kipper-400">Drop files to upload</p>
          </div>
        </div>

      <!-- Loading objects -->
      <div v-if="loadingObjects" class="py-12 text-center text-sm text-slate-500 dark:text-slate-400">Loading...</div>

      <!-- Object list -->
      <div v-else-if="sortedObjects.length" class="space-y-2">
        <!-- Select all header -->
        <div v-if="sortedObjects.some(o => !o.is_dir)" class="flex items-center gap-3 px-5 py-1">
          <input
            type="checkbox"
            :checked="allFilesSelected"
            @change="toggleSelectAll"
            class="h-4 w-4 rounded border-slate-300 text-kipper-600 focus:ring-kipper-500 dark:border-slate-600"
          />
          <span class="text-xs text-slate-500 dark:text-slate-400">Select all</span>
        </div>

        <!-- Parent directory -->
        <button
          v-if="prefix"
          @click="navigateUp"
          class="flex w-full items-center gap-3 rounded-xl border border-slate-200 bg-white px-5 py-3 text-left transition-colors hover:border-kipper-300 dark:border-slate-800 dark:bg-slate-900 dark:hover:border-kipper-700"
        >
          <span class="ml-7 inline-flex h-9 w-9 items-center justify-center rounded-lg bg-slate-100 dark:bg-slate-800">
            <Folder class="h-4 w-4 text-slate-400" :stroke-width="1.75" />
          </span>
          <span class="text-sm font-medium text-slate-500 dark:text-slate-400">..</span>
        </button>

        <div
          v-for="obj in sortedObjects"
          :key="obj.key"
          class="group flex items-center gap-3 rounded-xl border bg-white px-5 py-3 transition-colors dark:bg-slate-900"
          :class="selectedKeys.has(obj.key)
            ? 'border-kipper-300 bg-kipper-50/50 dark:border-kipper-700 dark:bg-kipper-950/30'
            : 'border-slate-200 hover:border-kipper-300 dark:border-slate-800 dark:hover:border-kipper-700'"
        >
          <!-- Checkbox (files only) -->
          <input
            v-if="!obj.is_dir"
            type="checkbox"
            :checked="selectedKeys.has(obj.key)"
            @change="toggleSelect(obj.key)"
            class="h-4 w-4 flex-shrink-0 rounded border-slate-300 text-kipper-600 focus:ring-kipper-500 dark:border-slate-600"
          />
          <div v-else class="w-4 flex-shrink-0" />

          <button
            class="flex flex-1 items-center gap-3 text-left"
            @click="obj.is_dir ? navigateToFolder(obj.key) : handleDownload(obj.key)"
          >
            <span class="inline-flex h-9 w-9 items-center justify-center rounded-lg bg-slate-100 dark:bg-slate-800">
              <Folder v-if="obj.is_dir" class="h-4 w-4 text-amber-500" :stroke-width="1.75" />
              <File v-else class="h-4 w-4 text-kipper-500" :stroke-width="1.75" />
            </span>
            <div class="min-w-0 flex-1">
              <p class="flex items-center gap-1.5 truncate text-sm font-medium text-slate-900 dark:text-slate-50">
                {{ objectName(obj.key) }}
                <Globe v-if="obj.is_public" class="h-3 w-3 flex-shrink-0 text-emerald-500" />
              </p>
              <p v-if="!obj.is_dir" class="mt-0.5 text-xs text-slate-500 dark:text-slate-400">
                {{ formatSize(obj.size) }} &middot; {{ formatDate(obj.last_modified) }}
              </p>
            </div>
          </button>

          <!-- Actions (files only) -->
          <div v-if="!obj.is_dir" class="flex items-center gap-1 md:opacity-0 transition-opacity group-hover:opacity-100">
            <button
              @click="openInNewTab(obj)"
              class="rounded-lg p-1.5 text-slate-400 hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800 dark:hover:text-slate-300"
              title="Open in new tab"
            >
              <ExternalLink class="h-4 w-4" />
            </button>
            <button
              @click="handleDownload(obj.key)"
              class="rounded-lg p-1.5 text-slate-400 hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800 dark:hover:text-slate-300"
              title="Download"
            >
              <Download class="h-4 w-4" />
            </button>
            <button
              v-if="canInNamespace(selectedNamespace, 'storage.write')"
              @click="togglePublic(obj)"
              class="rounded-lg p-1.5 transition-colors"
              :class="obj.is_public
                ? 'text-emerald-500 hover:bg-emerald-50 hover:text-emerald-600 dark:hover:bg-emerald-950'
                : 'text-slate-400 hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800 dark:hover:text-slate-300'"
              :title="obj.is_public ? 'Make private' : 'Make public'"
            >
              <Globe v-if="obj.is_public" class="h-4 w-4" />
              <Lock v-else class="h-4 w-4" />
            </button>
            <button
              v-if="canInNamespace(selectedNamespace, 'storage.write')"
              @click="openShare(obj.key)"
              class="rounded-lg p-1.5 text-slate-400 hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800 dark:hover:text-slate-300"
              title="Share link"
            >
              <Share2 class="h-4 w-4" />
            </button>
            <button
              v-if="canInNamespace(selectedNamespace, 'storage.write') && confirmDelete !== obj.key"
              @click="confirmDelete = obj.key"
              class="rounded-lg p-1.5 text-slate-400 hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-950 dark:hover:text-red-400"
              title="Delete"
            >
              <Trash2 class="h-4 w-4" />
            </button>
            <button
              v-else-if="canInNamespace(selectedNamespace, 'storage.write')"
              @click="handleDelete(obj.key)"
              class="rounded-lg bg-red-600 px-2.5 py-1 text-xs font-medium text-white hover:bg-red-700"
            >
              Confirm
            </button>
          </div>
        </div>
      </div>

      <!-- Empty bucket -->
      <div v-else-if="!loadingObjects" class="rounded-xl border border-dashed border-slate-300 py-16 text-center dark:border-slate-700">
        <FolderOpen class="mx-auto mb-3 h-10 w-10 text-slate-400 dark:text-slate-500" :stroke-width="1.5" />
        <p class="text-sm font-medium text-slate-900 dark:text-slate-50">This bucket is empty</p>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Drag files here or click Upload</p>
        <button
          v-if="canInNamespace(selectedNamespace, 'storage.write')"
          @click="triggerUpload"
          class="mt-4 inline-flex items-center gap-1.5 rounded-lg bg-kipper-600 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-kipper-700"
        >
          <Upload class="h-4 w-4" />
          Upload file
        </button>
      </div>

      </div><!-- end drop zone -->
    </div>

    <!-- Share link modal -->
    <div
      v-if="shareModalOpen"
      class="fixed inset-0 z-50 flex items-center justify-center bg-black/50"
      @click.self="shareModalOpen = false"
    >
      <div class="w-full max-w-lg rounded-xl border border-slate-200 bg-white p-6 shadow-xl dark:border-slate-700 dark:bg-slate-900">
        <div class="flex items-center justify-between mb-4">
          <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Share link</h3>
          <button @click="shareModalOpen = false" class="text-slate-400 hover:text-slate-600 dark:hover:text-slate-300">
            <X class="h-4 w-4" />
          </button>
        </div>
        <p class="mb-3 text-xs text-slate-500 dark:text-slate-400">
          Generate a public link for <span class="font-medium text-slate-700 dark:text-slate-300">{{ objectName(shareKey) }}</span>.
        </p>

        <!-- Expiry selector -->
        <div class="mb-4">
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Expires in</label>
          <select
            v-model="shareExpiry"
            class="rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
          >
            <option value="1h">1 hour</option>
            <option value="6h">6 hours</option>
            <option value="24h">24 hours</option>
            <option value="72h">3 days</option>
            <option value="168h">7 days</option>
          </select>
        </div>

        <!-- Generate button (before link is created) -->
        <button
          v-if="!shareLink"
          @click="generateShareLink"
          :disabled="shareGenerating"
          class="rounded-lg bg-kipper-600 px-4 py-2 text-sm font-medium text-white hover:bg-kipper-700 disabled:opacity-50"
        >
          {{ shareGenerating ? 'Generating...' : 'Generate link' }}
        </button>

        <!-- Link display (after generation) -->
        <div v-if="shareLink" class="space-y-2">
          <p class="text-xs text-slate-500 dark:text-slate-400">
            Anyone with this link can download the file. Expires in {{ shareLink.expires }}.
          </p>
          <div class="flex items-center gap-2">
            <input
              :value="shareLink.url"
              readonly
              class="block flex-1 rounded-md border border-slate-300 bg-slate-50 px-3 py-2 text-xs font-mono text-slate-700 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-300"
              @focus="($event.target as HTMLInputElement).select()"
            />
            <button
              @click="copyShareLink"
              class="inline-flex items-center gap-1.5 rounded-lg bg-kipper-600 px-3 py-2 text-sm font-medium text-white hover:bg-kipper-700"
            >
              <Copy class="h-4 w-4" />
              Copy
            </button>
          </div>
        </div>
      </div>
    </div>

    <!-- Bulk action bar -->
    <Teleport to="body">
      <Transition
        enter-active-class="transition-transform duration-200 ease-out"
        leave-active-class="transition-transform duration-150 ease-in"
        enter-from-class="translate-y-full"
        leave-to-class="translate-y-full"
      >
        <div
          v-if="selectedKeys.size > 0 || bulkRunning"
          class="fixed bottom-6 left-1/2 z-50 max-w-full -translate-x-1/2 rounded-xl border border-slate-200 bg-white px-5 py-3 shadow-xl dark:border-slate-700 dark:bg-slate-900"
        >
          <!-- Progress bar during bulk operation -->
          <div v-if="bulkRunning" class="w-80 max-w-full">
            <div class="mb-2 flex items-center justify-between text-xs text-slate-600 dark:text-slate-400">
              <span>{{ bulkAction }} {{ bulkProcessed }}/{{ bulkTotal }}</span>
              <span>{{ Math.round((bulkProcessed / bulkTotal) * 100) }}%</span>
            </div>
            <div class="h-2 w-full overflow-hidden rounded-full bg-slate-200 dark:bg-slate-700">
              <div
                class="h-full rounded-full bg-kipper-500 transition-all duration-300"
                :style="{ width: `${(bulkProcessed / bulkTotal) * 100}%` }"
              />
            </div>
            <p class="mt-1.5 truncate text-[10px] text-slate-400 dark:text-slate-500">{{ bulkCurrentFile }}</p>
          </div>

          <!-- Action buttons when not running -->
          <div v-else class="flex flex-wrap items-center gap-3">
            <span class="text-sm font-medium text-slate-700 dark:text-slate-300">
              {{ selectedKeys.size }} {{ selectedKeys.size === 1 ? 'file' : 'files' }} selected
            </span>
            <div class="h-5 w-px bg-slate-200 dark:bg-slate-700" />
            <button
              @click="bulkDownload"
              class="inline-flex items-center gap-1.5 rounded-lg px-3 py-1.5 text-xs font-medium text-slate-600 hover:bg-slate-100 dark:text-slate-400 dark:hover:bg-slate-800"
            >
              <Download class="h-3.5 w-3.5" />
              Download
            </button>
            <button
              v-if="canInNamespace(selectedNamespace, 'storage.write')"
              @click="bulkMakePublic"
              class="inline-flex items-center gap-1.5 rounded-lg px-3 py-1.5 text-xs font-medium text-slate-600 hover:bg-slate-100 dark:text-slate-400 dark:hover:bg-slate-800"
            >
              <Globe class="h-3.5 w-3.5" />
              Public
            </button>
            <button
              v-if="canInNamespace(selectedNamespace, 'storage.write')"
              @click="bulkMakePrivate"
              class="inline-flex items-center gap-1.5 rounded-lg px-3 py-1.5 text-xs font-medium text-slate-600 hover:bg-slate-100 dark:text-slate-400 dark:hover:bg-slate-800"
            >
              <Lock class="h-3.5 w-3.5" />
              Private
            </button>
            <button
              v-if="canInNamespace(selectedNamespace, 'storage.write')"
              @click="confirmBulkDelete"
              class="inline-flex items-center gap-1.5 rounded-lg px-3 py-1.5 text-xs font-medium text-red-600 hover:bg-red-50 dark:text-red-400 dark:hover:bg-red-950"
            >
              <Trash2 class="h-3.5 w-3.5" />
              Delete
            </button>
            <div class="h-5 w-px bg-slate-200 dark:bg-slate-700" />
            <button
              @click="clearSelection"
              class="rounded-lg p-1 text-slate-400 hover:text-slate-600 dark:hover:text-slate-300"
              title="Clear selection"
            >
              <X class="h-4 w-4" />
            </button>
          </div>
        </div>
      </Transition>
    </Teleport>
  </div>
</template>
