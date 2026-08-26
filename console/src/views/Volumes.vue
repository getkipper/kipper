<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { HardDrive, Plus, Trash2, RefreshCw, Link, Unlink } from 'lucide-vue-next'
import { useToast } from '@/composables/useToast'
import { useModal } from '@/composables/useModal'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import { useProjectsStore } from '@/stores/projects'
import { useCapabilities } from '@/composables/useCapabilities'
import { fetchVolumes, createVolume, deleteVolume, mountVolume, unmountVolume, type Volume } from '@/api/volumes'
import { fetchApps } from '@/api/apps'
import type { App } from '@/api/types'

const toast = useToast()
const modal = useModal()
const projectsStore = useProjectsStore()
const { canInNamespace } = useCapabilities()

// Projects loaded globally via sidebar selector
const selectedNamespace = computed(() => projectsStore.globalNamespace || 'default')

const volumes = ref<Volume[]>([])
const apps = ref<App[]>([])
const loading = ref(false)
const refreshing = ref(false)
const showCreate = ref(false)
const creating = ref(false)
const showMount = ref(false)
const mounting = ref(false)

const newName = ref('')
const newSize = ref('5Gi')
const mountVolumeName = ref('')
const mountAppName = ref('')
const mountPath = ref('/data')

onMounted(async () => {
  await loadVolumes()
})

watch(selectedNamespace, () => {
  loadVolumes()
})

async function loadVolumes() {
  loading.value = true
  try {
    const [v, a] = await Promise.all([
      fetchVolumes(selectedNamespace.value),
      fetchApps(selectedNamespace.value),
    ])
    volumes.value = v
    apps.value = a
  } catch {
    toast.error('Failed to load volumes')
  } finally {
    loading.value = false
  }
}


async function refreshList() {
  refreshing.value = true
  try {
    volumes.value = await fetchVolumes(selectedNamespace.value)
    toast.success('Volumes refreshed')
  } catch {
    toast.error('Failed to refresh')
  } finally {
    refreshing.value = false
  }
}

async function handleCreate() {
  if (!newName.value || !newSize.value) return
  creating.value = true
  try {
    await createVolume(selectedNamespace.value, newName.value, newSize.value)
    toast.success(`Volume ${newName.value} created`)
    newName.value = ''
    newSize.value = '5Gi'
    showCreate.value = false
    await loadVolumes()
  } catch {
    toast.error('Failed to create volume')
  } finally {
    creating.value = false
  }
}

function handleDelete(name: string) {
  modal.open(ConfirmDialog, {
    title: `Delete volume ${name}?`,
    message: 'This permanently deletes the volume and everything stored on it. This cannot be undone.',
    confirmLabel: 'Delete volume',
    confirmPhrase: name,
    onConfirm: async () => {
      modal.close()
      try {
        await deleteVolume(selectedNamespace.value, name)
        toast.success(`Volume ${name} deleted`)
        await loadVolumes()
      } catch {
        toast.error(`Failed to delete ${name}`)
      }
    },
  })
}

async function handleMount() {
  if (!mountVolumeName.value || !mountAppName.value || !mountPath.value) return
  mounting.value = true
  try {
    await mountVolume(selectedNamespace.value, mountVolumeName.value, mountAppName.value, mountPath.value)
    toast.success(`Volume ${mountVolumeName.value} mounted at ${mountPath.value} in ${mountAppName.value}`)
    await loadVolumes()
    showMount.value = false
    mountVolumeName.value = ''
    mountAppName.value = ''
    mountPath.value = '/data'
  } catch {
    toast.error('Failed to mount volume')
  } finally {
    mounting.value = false
  }
}

function handleUnmount(volumeName: string, appName: string) {
  modal.open(ConfirmDialog, {
    title: `Unmount ${volumeName} from ${appName}?`,
    message: `${appName} loses access to this volume until it is mounted again. The data on the volume is kept.`,
    confirmLabel: 'Unmount',
    danger: false,
    onConfirm: async () => {
      modal.close()
      await doUnmount(volumeName, appName)
    },
  })
}

async function doUnmount(volumeName: string, appName: string) {
  try {
    await unmountVolume(selectedNamespace.value, volumeName, appName)
    toast.success(`Volume ${volumeName} unmounted from ${appName}`)
    await loadVolumes()
  } catch {
    toast.error('Failed to unmount volume')
  }
}

function statusColor(status: string): string {
  if (status === 'Bound') return 'text-emerald-700 dark:text-emerald-400'
  if (status === 'Pending') return 'text-amber-700 dark:text-amber-400'
  return 'text-slate-500'
}
</script>

<template>
  <div class="animate-fade-in">
    <!-- Header -->
    <div class="mb-8 flex items-center justify-between">
      <div>
        <h1 class="text-2xl font-semibold tracking-tight text-slate-900 dark:text-slate-50">Volumes</h1>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Shared persistent storage for multi-pod file access</p>
      </div>
      <div class="flex items-center gap-2">
        <button
          v-if="canInNamespace(selectedNamespace, 'kipper.write')"
          @click="showMount = !showMount"
          class="inline-flex items-center gap-1.5 rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-600 transition-colors hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
        >
          <Link class="h-4 w-4" />
          Mount
        </button>
        <button
          v-if="canInNamespace(selectedNamespace, 'kipper.write')"
          @click="showCreate = !showCreate"
          class="inline-flex items-center gap-1.5 rounded-lg bg-kipper-600 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-kipper-700"
        >
          <Plus class="h-4 w-4" />
          Create
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

    <!-- Create form -->
    <div v-if="showCreate" class="mb-6 rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
      <h3 class="mb-4 text-sm font-semibold text-slate-900 dark:text-slate-50">Create shared volume</h3>
      <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div>
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Name</label>
          <input v-model="newName" type="text" placeholder="uploads" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Size</label>
          <select v-model="newSize" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50">
            <option value="1Gi">1 Gi</option>
            <option value="5Gi">5 Gi</option>
            <option value="10Gi">10 Gi</option>
            <option value="20Gi">20 Gi</option>
            <option value="50Gi">50 Gi</option>
          </select>
        </div>
      </div>
      <div class="mt-4 flex justify-end gap-2">
        <button @click="showCreate = false" class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800">Cancel</button>
        <button @click="handleCreate" :disabled="!newName || creating" class="rounded-lg bg-kipper-600 px-4 py-2 text-sm font-medium text-white hover:bg-kipper-700 disabled:opacity-50">
          {{ creating ? 'Creating...' : 'Create volume' }}
        </button>
      </div>
    </div>

    <!-- Mount form -->
    <div v-if="showMount" class="mb-6 rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
      <h3 class="mb-4 text-sm font-semibold text-slate-900 dark:text-slate-50">Mount volume into app</h3>
      <div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
        <div>
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Volume</label>
          <select v-model="mountVolumeName" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50">
            <option value="" disabled>Select volume...</option>
            <option v-for="vol in volumes" :key="vol.name" :value="vol.name">{{ vol.name }} ({{ vol.size }})</option>
          </select>
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">App</label>
          <select v-model="mountAppName" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50">
            <option value="" disabled>Select app...</option>
            <option v-for="app in apps" :key="app.name" :value="app.name">{{ app.name }}</option>
          </select>
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Mount path</label>
          <input v-model="mountPath" type="text" placeholder="/data/uploads" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
        </div>
      </div>
      <div class="mt-4 flex justify-end gap-2">
        <button @click="showMount = false" class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800">Cancel</button>
        <button @click="handleMount" :disabled="!mountVolumeName || !mountAppName || mounting" class="rounded-lg bg-kipper-600 px-4 py-2 text-sm font-medium text-white hover:bg-kipper-700 disabled:opacity-50">
          {{ mounting ? 'Mounting...' : 'Mount volume' }}
        </button>
      </div>
    </div>

    <!-- Loading -->
    <div v-if="loading" class="py-12 text-center text-sm text-slate-500 dark:text-slate-400">Loading...</div>

    <!-- Volume list -->
    <div v-else-if="volumes.length" class="space-y-3">
      <div
        v-for="vol in volumes"
        :key="vol.name"
        class="rounded-xl border border-slate-200 bg-white p-5 transition-colors hover:border-kipper-300 dark:border-slate-800 dark:bg-slate-900 dark:hover:border-kipper-700"
      >
        <div class="flex items-center justify-between">
          <div class="flex items-center gap-4">
            <span class="inline-flex h-10 w-10 items-center justify-center rounded-lg bg-slate-100 dark:bg-slate-800">
              <HardDrive class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
            </span>
            <div>
              <div class="flex items-center gap-2">
                <span class="text-sm font-semibold text-slate-900 dark:text-slate-50">{{ vol.name }}</span>
                <span class="rounded-full bg-slate-100 px-2 py-0.5 text-xs font-medium text-slate-600 dark:bg-slate-800 dark:text-slate-400">{{ vol.size }}</span>
                <span class="text-xs" :class="statusColor(vol.status)">{{ vol.status }}</span>
              </div>
              <div class="mt-0.5 text-xs text-slate-500 dark:text-slate-400">{{ vol.access }}</div>
            </div>
          </div>

          <button
            v-if="canInNamespace(selectedNamespace, 'kipper.write')"
            @click="handleDelete(vol.name)"
            class="rounded-lg p-1.5 text-slate-400 hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-950 dark:hover:text-red-400"
            title="Delete volume"
          >
            <Trash2 class="h-4 w-4" />
          </button>
        </div>

        <!-- Mounts -->
        <div v-if="vol.mounts && vol.mounts.length" class="mt-3 space-y-1.5 border-t border-slate-100 pt-3 dark:border-slate-800">
          <p class="text-xs font-medium text-slate-500 dark:text-slate-400">Mounted in:</p>
          <div
            v-for="mount in vol.mounts"
            :key="mount.app + mount.path"
            class="flex items-center justify-between rounded-md bg-slate-50 px-3 py-2 dark:bg-slate-800"
          >
            <div class="flex items-center gap-2 text-xs">
              <span class="font-medium text-slate-700 dark:text-slate-300">{{ mount.app }}</span>
              <span class="text-slate-400">→</span>
              <span class="font-mono text-slate-500 dark:text-slate-400">{{ mount.path }}</span>
            </div>
            <button
              v-if="canInNamespace(selectedNamespace, 'kipper.write')"
              @click="handleUnmount(vol.name, mount.app)"
              class="inline-flex items-center gap-1 rounded-md px-2 py-1 text-xs text-slate-400 hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-950 dark:hover:text-red-400"
              title="Unmount"
            >
              <Unlink class="h-3 w-3" />
              Disconnect
            </button>
          </div>
        </div>

        <div v-else class="mt-3 border-t border-slate-100 pt-3 dark:border-slate-800">
          <p class="text-xs text-slate-400 dark:text-slate-500">Not mounted in any app</p>
        </div>
      </div>
    </div>

    <!-- Empty state -->
    <div v-else class="rounded-xl border border-dashed border-slate-300 py-16 text-center dark:border-slate-700">
      <HardDrive class="mx-auto mb-3 h-10 w-10 text-slate-400 dark:text-slate-500" :stroke-width="1.5" />
      <p class="text-sm font-medium text-slate-900 dark:text-slate-50">No shared volumes</p>
      <p class="mt-1 max-w-sm mx-auto text-sm text-slate-500 dark:text-slate-400">
        Create a shared volume with <code class="rounded bg-slate-100 px-1.5 py-0.5 font-mono text-xs dark:bg-slate-800">kip volume create uploads --size 5Gi</code>
      </p>
    </div>
  </div>
</template>
