<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { Archive, Plus, RefreshCw, Clock, Undo2, CheckCircle, XCircle, Loader, ExternalLink, Trash2 } from 'lucide-vue-next'
import { useToast } from '@/composables/useToast'
import { useModal } from '@/composables/useModal'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import NoticeCallout from '@/components/NoticeCallout.vue'
import { useAuthStore } from '@/stores/auth'
import { fetchBackups, fetchSchedules, createBackup, restoreBackup, deleteBackup, toggleSchedule, type Backup, type BackupSchedule } from '@/api/backups'
import { fetchProjects, type Project } from '@/api/projects'

const toast = useToast()
const modal = useModal()
const authStore = useAuthStore()
const allProjects = ref<Project[]>([])

const namespaceOptions = computed(() => {
  const options: { label: string; value: string }[] = [
    { label: 'All namespaces', value: '' },
  ]
  for (const p of allProjects.value) {
    for (const env of p.environments) {
      options.push({
        label: `${p.display_name || p.name}, ${env.name}`,
        value: env.namespace,
      })
    }
  }
  return options
})

const backups = ref<Backup[]>([])
const schedules = ref<BackupSchedule[]>([])
// Match SubdomainFor: hyphen on kipper.run, dot on custom domains.
const grafanaLokiUrl = `https://${window.location.hostname.replace(/^console(--|\.)/, 'grafana$1')}/explore?orgId=1&left=%7B%22datasource%22:%22Loki%22,%22queries%22:%5B%7B%22expr%22:%22%7Bnamespace%3D%5C%22velero%5C%22%7D%22%7D%5D%7D`

const loading = ref(false)
const refreshing = ref(false)
const showCreate = ref(false)
const creating = ref(false)
const restoring = ref<string | null>(null)

const newName = ref('')
const newNamespaces = ref('')
const newTtl = ref('168h0m0s')

onMounted(async () => {
  await loadAll()
})

async function loadAll() {
  loading.value = true
  try {
    const [b, s, p] = await Promise.all([fetchBackups(), fetchSchedules(), fetchProjects()])
    backups.value = b
    schedules.value = s
    allProjects.value = p
  } catch {
    toast.error('Failed to load backups')
  } finally {
    loading.value = false
  }
}

async function refreshList() {
  refreshing.value = true
  try {
    const [b, s] = await Promise.all([fetchBackups(), fetchSchedules()])
    backups.value = b
    schedules.value = s
    toast.success('Backups refreshed')
  } catch {
    toast.error('Failed to refresh')
  } finally {
    refreshing.value = false
  }
}

async function handleCreate() {
  creating.value = true
  try {
    await createBackup({
      name: newName.value || undefined,
      namespaces: newNamespaces.value || undefined,
      ttl: newTtl.value,
    })
    toast.success('Backup started')
    newName.value = ''
    newNamespaces.value = ''
    showCreate.value = false
    await loadAll()
  } catch {
    toast.error('Failed to create backup')
  } finally {
    creating.value = false
  }
}

const showRestore = ref(false)
const restoreBackupName = ref('')
const restoreNamespace = ref('')
const restoreResources = ref('')

function openRestore(name: string) {
  restoreBackupName.value = name
  restoreNamespace.value = ''
  restoreResources.value = ''
  showRestore.value = true
}

async function handleRestore() {
  restoring.value = restoreBackupName.value
  try {
    await restoreBackup(restoreBackupName.value, {
      namespace: restoreNamespace.value || undefined,
      resources: restoreResources.value || undefined,
    })
    const scope = restoreNamespace.value || 'everything'
    toast.success(`Restoring ${scope} from ${restoreBackupName.value}`)
    showRestore.value = false
  } catch {
    toast.error(`Failed to restore from ${restoreBackupName.value}`)
  } finally {
    restoring.value = null
  }
}

function handleDeleteBackup(name: string) {
  modal.open(ConfirmDialog, {
    title: `Delete backup ${name}?`,
    message: 'This permanently removes the backup. Restoring from it will no longer be possible. This cannot be undone.',
    confirmLabel: 'Delete backup',
    onConfirm: async () => {
      modal.close()
      try {
        await deleteBackup(name)
        toast.success(`Backup ${name} deleted`)
        await loadAll()
      } catch {
        toast.error(`Failed to delete ${name}`)
      }
    },
  })
}

async function handleToggleSchedule(name: string, currentlyEnabled: boolean) {
  try {
    await toggleSchedule(name, currentlyEnabled)
    toast.success(`Schedule ${name} ${currentlyEnabled ? 'paused' : 'enabled'}`)
    await loadAll()
  } catch {
    toast.error(`Failed to update schedule ${name}`)
  }
}

function statusColor(status: string): string {
  switch (status) {
    case 'Completed': return 'text-emerald-700 dark:text-emerald-400'
    case 'Failed': case 'PartiallyFailed': return 'text-red-700 dark:text-red-400'
    case 'InProgress': return 'text-amber-700 dark:text-amber-400'
    default: return 'text-slate-500'
  }
}

function statusIcon(status: string) {
  switch (status) {
    case 'Completed': return CheckCircle
    case 'Failed': case 'PartiallyFailed': return XCircle
    case 'InProgress': return Loader
    default: return Clock
  }
}
</script>

<template>
  <div class="animate-fade-in">
    <!-- Header -->
    <div class="mb-8 flex items-center justify-between">
      <div>
        <h1 class="text-2xl font-semibold tracking-tight text-slate-900 dark:text-slate-50">Backups</h1>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Automatic and manual cluster backups with restore</p>
      </div>
      <div class="flex items-center gap-2">
        <button
          v-if="authStore.isAdmin"
          @click="showCreate = !showCreate"
          class="inline-flex items-center gap-1.5 rounded-lg bg-kipper-600 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-kipper-700"
        >
          <Plus class="h-4 w-4" />
          Create backup
        </button>
        <button
          @click="refreshList"
          :disabled="refreshing"
          class="rounded-lg border border-slate-300 p-2.5 text-slate-600 transition-colors hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
          title="Refresh"
        >
          <RefreshCw class="h-4 w-4" :class="refreshing ? 'animate-spin' : ''" :stroke-width="1.75" />
        </button>
      </div>
    </div>

    <!-- Create form -->
    <div v-if="showCreate" class="mb-6 rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
      <h3 class="mb-4 text-sm font-semibold text-slate-900 dark:text-slate-50">Create backup</h3>
      <div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
        <div>
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Name (optional)</label>
          <input v-model="newName" type="text" placeholder="auto-generated" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Namespaces (optional, comma-separated)</label>
          <input v-model="newNamespaces" type="text" placeholder="all namespaces" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Retention</label>
          <select v-model="newTtl" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50">
            <option value="24h0m0s">1 day</option>
            <option value="72h0m0s">3 days</option>
            <option value="168h0m0s">7 days</option>
            <option value="720h0m0s">30 days</option>
          </select>
        </div>
      </div>
      <div class="mt-4 flex justify-end gap-2">
        <button @click="showCreate = false" class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800">
          Cancel
        </button>
        <button
          @click="handleCreate"
          :disabled="creating"
          class="rounded-lg bg-kipper-600 px-4 py-2 text-sm font-medium text-white hover:bg-kipper-700 disabled:opacity-50"
        >
          {{ creating ? 'Creating...' : 'Start backup' }}
        </button>
      </div>
    </div>

    <!-- Loading -->
    <div v-if="loading" class="py-12 text-center text-sm text-slate-500 dark:text-slate-400">Loading...</div>

    <template v-else>
      <!-- Schedules -->
      <div v-if="schedules.length" class="mb-8">
        <h2 class="mb-3 text-sm font-semibold text-slate-700 dark:text-slate-300">Automatic schedules</h2>
        <div class="space-y-2">
          <div
            v-for="s in schedules"
            :key="s.name"
            class="flex flex-wrap items-center justify-between gap-y-2 rounded-lg border border-slate-200 bg-white px-4 py-3 dark:border-slate-800 dark:bg-slate-900"
          >
            <div class="flex items-center gap-3">
              <Clock class="h-4 w-4 text-kipper-500" />
              <div>
                <span class="text-sm font-medium text-slate-900 dark:text-slate-50">{{ s.name }}</span>
                <span class="ml-2 font-mono text-xs text-slate-500 dark:text-slate-400">{{ s.schedule }}</span>
              </div>
            </div>
            <div class="flex items-center gap-3 text-xs text-slate-500 dark:text-slate-400">
              <span>Last: {{ s.last_backup }}</span>
              <button
                v-if="authStore.isAdmin"
                @click="handleToggleSchedule(s.name, s.status === 'Enabled')"
                class="relative inline-flex h-5 w-9 items-center rounded-full transition-colors"
                :class="s.status === 'Enabled' ? 'bg-emerald-500' : 'bg-slate-300 dark:bg-slate-600'"
                :title="s.status === 'Enabled' ? 'Click to pause' : 'Click to enable'"
              >
                <span
                  class="inline-block h-3.5 w-3.5 rounded-full bg-white transition-transform"
                  :class="s.status === 'Enabled' ? 'translate-x-4' : 'translate-x-0.5'"
                />
              </button>
            </div>
          </div>
        </div>
      </div>

      <!-- Backup list -->
      <div v-if="backups.length">
        <h2 class="mb-3 text-sm font-semibold text-slate-700 dark:text-slate-300">Backup history</h2>
        <div class="space-y-2">
          <div
            v-for="b in backups"
            :key="b.name"
            class="group flex flex-wrap items-center justify-between gap-y-2 rounded-xl border border-slate-200 bg-white px-5 py-4 transition-colors hover:border-kipper-300 dark:border-slate-800 dark:bg-slate-900 dark:hover:border-kipper-700"
          >
            <div class="flex min-w-0 items-center gap-4">
              <span class="inline-flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-slate-100 dark:bg-slate-800">
                <Archive class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
              </span>
              <div class="min-w-0">
                <div class="flex flex-wrap items-center gap-2">
                  <span class="text-sm font-semibold text-slate-900 dark:text-slate-50">{{ b.name }}</span>
                  <span class="inline-flex items-center gap-1 text-xs" :class="statusColor(b.status)">
                    <component :is="statusIcon(b.status)" class="h-3.5 w-3.5" :class="b.status === 'InProgress' ? 'animate-spin' : ''" />
                    {{ b.status }}
                  </span>
                </div>
                <div class="mt-0.5 flex flex-wrap items-center gap-x-3 gap-y-0.5 text-xs text-slate-500 dark:text-slate-400">
                  <span>{{ b.namespaces }}</span>
                  <span>{{ b.created }}</span>
                  <span v-if="b.ttl">TTL: {{ b.ttl }}</span>
                </div>
                <div v-if="b.reason" class="mt-1 flex items-center gap-2 text-xs text-red-600 dark:text-red-400">
                  <span>{{ b.reason }}</span>
                  <a
                    :href="grafanaLokiUrl"
                    target="_blank"
                    rel="noopener"
                    class="inline-flex items-center gap-1 text-kipper-600 hover:text-kipper-700 dark:text-kipper-400"
                  >
                    Investigate in Grafana
                    <ExternalLink class="h-3 w-3" />
                  </a>
                </div>
              </div>
            </div>
            <div v-if="authStore.isAdmin" class="flex items-center gap-2 md:opacity-0 transition-opacity group-hover:opacity-100">
              <button
                v-if="b.status === 'Completed'"
                @click="openRestore(b.name)"
                class="inline-flex items-center gap-1 rounded-md border border-slate-300 px-3 py-1.5 text-xs font-medium text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-700"
              >
                <Undo2 class="h-3 w-3" />
                Restore
              </button>
              <button
                @click="handleDeleteBackup(b.name)"
                class="rounded-md p-1.5 text-slate-400 hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-950 dark:hover:text-red-400"
                title="Delete backup"
              >
                <Trash2 class="h-3.5 w-3.5" />
              </button>
            </div>
          </div>
        </div>
      </div>

      <!-- Restore dialog -->
      <div v-if="showRestore" class="fixed inset-0 z-50 flex items-center justify-center bg-black/50">
        <div class="w-full max-w-md rounded-xl border border-slate-200 bg-white p-6 shadow-xl dark:border-slate-700 dark:bg-slate-900">
          <h3 class="text-lg font-semibold text-slate-900 dark:text-slate-50">Restore from {{ restoreBackupName }}</h3>
          <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Choose what to restore. Leave fields empty to restore everything.</p>

          <div class="mt-4 space-y-4">
            <div>
              <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Namespace</label>
              <select
                v-model="restoreNamespace"
                class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
              >
                <option v-for="opt in namespaceOptions" :key="opt.value" :value="opt.value">{{ opt.label }}</option>
              </select>
            </div>
            <div>
              <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Resource types (optional)</label>
              <select
                v-model="restoreResources"
                class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
              >
                <option value="">All resources</option>
                <option value="deployments,services,ingresses">Apps only (Deployments, Services, Ingresses)</option>
                <option value="secrets">Secrets only</option>
                <option value="persistentvolumeclaims">Database volumes only</option>
                <option value="configmaps,secrets">Configuration only (ConfigMaps, Secrets)</option>
              </select>
            </div>
          </div>

          <NoticeCallout tone="warning" class="mt-4 p-3">
            <p class="text-xs text-amber-800 dark:text-slate-300">
              <strong class="dark:text-orange-300">Warning:</strong> Restoring will overwrite existing resources that match the backup.
              {{ restoreNamespace ? `Only resources in "${restoreNamespace}" will be affected.` : 'All namespaces in this backup will be affected.' }}
            </p>
          </NoticeCallout>

          <div class="mt-5 flex justify-end gap-2">
            <button @click="showRestore = false" class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800">
              Cancel
            </button>
            <button
              @click="handleRestore"
              :disabled="restoring !== null"
              class="rounded-lg bg-amber-600 px-4 py-2 text-sm font-medium text-white hover:bg-amber-700 disabled:opacity-50"
            >
              {{ restoring ? 'Restoring...' : 'Confirm restore' }}
            </button>
          </div>
        </div>
      </div>

      <!-- Empty state -->
      <div v-if="!backups.length && !schedules.length" class="rounded-xl border border-dashed border-slate-300 py-16 text-center dark:border-slate-700">
        <Archive class="mx-auto mb-3 h-10 w-10 text-slate-400 dark:text-slate-500" :stroke-width="1.5" />
        <p class="text-sm font-medium text-slate-900 dark:text-slate-50">No backups yet</p>
        <p class="mt-1 max-w-sm mx-auto text-sm text-slate-500 dark:text-slate-400">
          Create a backup with <code class="rounded bg-slate-100 px-1.5 py-0.5 font-mono text-xs dark:bg-slate-800">kip backup create</code> or use the button above.
        </p>
      </div>
    </template>
  </div>
</template>
