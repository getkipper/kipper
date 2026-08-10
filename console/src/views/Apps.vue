<script setup lang="ts">
import { onMounted, ref, computed, watch } from 'vue'
import { Plus, Trash2, Rocket, RefreshCw } from 'lucide-vue-next'
import AppDetail from '@/components/AppDetail.vue'
import NoticeCallout from '@/components/NoticeCallout.vue'
import { useAppsStore } from '@/stores/apps'
import { useProjectsStore } from '@/stores/projects'
import { useAuthStore } from '@/stores/auth'
import { useToast } from '@/composables/useToast'
import { useModal } from '@/composables/useModal'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import type { CreateAppPayload } from '@/api/types'

const apps = useAppsStore()
const projects = useProjectsStore()
const authStore = useAuthStore()
const toast = useToast()
const modal = useModal()

const showDeploy = ref(false)
const deployMode = ref<'image' | 'git'>('git')
const gitUrl = ref('')
const gitBranch = ref('main')
const gitToken = ref('')
const form = ref<CreateAppPayload>({
  name: '',
  image: '',
  port: 0,
  replicas: 1,
  env: {},
  resource_profile: 'lightweight',
})

const resourceProfiles = [
  { value: 'lightweight', label: 'Lightweight (50m CPU, 64Mi memory)' },
  { value: 'standard', label: 'Standard (100m CPU, 128Mi memory)' },
  { value: 'compute-heavy', label: 'Compute-heavy (500m CPU, 256Mi memory)' },
  { value: 'memory-heavy', label: 'Memory-heavy (100m CPU, 512Mi memory)' },
  { value: 'jvm', label: 'JVM / Heavy (100m–1000m CPU burstable, 2Gi memory)' },
  { value: 'custom', label: 'Custom...' },
]

const showCustomResources = computed(() => form.value.resource_profile === 'custom')
const envText = ref('')
const deploying = ref(false)
const selectedApp = ref<string | null>(null)
const selectedAppNamespace = ref<string | null>(null)
const refreshing = ref(false)

const isAllProjects = computed(() => !projects.globalNamespace)
const currentProject = computed(() => projects.globalNamespace || 'default')

onMounted(() => {
  if (isAllProjects.value) {
    apps.loadAllApps()
  } else {
    apps.loadApps(currentProject.value)
  }
})

watch(() => projects.globalNamespace, (ns) => {
  if (!ns) {
    apps.loadAllApps()
  } else {
    apps.loadApps(ns)
  }
})

function statusColor(status: string): string {
  switch (status) {
    case 'running': return 'bg-emerald-500'
    case 'pending': return 'bg-amber-500'
    case 'failed': return 'bg-red-500'
    case 'stopped': return 'bg-slate-400'
    default: return 'bg-slate-400'
  }
}

function statusTextColor(status: string): string {
  switch (status) {
    case 'running': return 'text-emerald-700 dark:text-emerald-400'
    case 'pending': return 'text-amber-700 dark:text-amber-400'
    case 'failed': return 'text-red-700 dark:text-red-400'
    case 'stopped': return 'text-slate-500 dark:text-slate-400'
    default: return 'text-slate-500'
  }
}

async function handleDeploy() {
  if (!currentProject.value || !form.value.name) return

  if (deployMode.value === 'image' && !form.value.image) return
  if (deployMode.value === 'git' && !gitUrl.value) return

  deploying.value = true

  const env: Record<string, string> = {}
  for (const line of envText.value.split('\n')) {
    const trimmed = line.trim()
    if (!trimmed || trimmed.startsWith('#')) continue
    const idx = trimmed.indexOf('=')
    if (idx > 0) {
      env[trimmed.substring(0, idx)] = trimmed.substring(idx + 1)
    }
  }
  form.value.env = env

  if (deployMode.value === 'git') {
    form.value.git = {
      url: gitUrl.value,
      branch: gitBranch.value || 'main',
      token: gitToken.value || undefined,
    }
    form.value.image = ''
  }

  try {
    await apps.deployApp(currentProject.value, form.value)
    toast.success(`${form.value.name} deployed successfully`)
    form.value = { name: '', image: '', port: 0, replicas: 1, env: {}, resource_profile: 'lightweight' }
    gitUrl.value = ''
    gitBranch.value = 'main'
    gitToken.value = ''
    envText.value = ''
    showDeploy.value = false
  } catch {
    toast.error('Failed to deploy app')
  } finally {
    deploying.value = false
  }
}

function handleDelete(appName: string, namespace?: string) {
  const ns = namespace || currentProject.value
  if (!ns) return
  modal.open(ConfirmDialog, {
    title: `Delete app ${appName}?`,
    message: 'This removes the deployment, service, ingress, and all secrets. This cannot be undone.',
    confirmLabel: 'Delete app',
    confirmPhrase: appName,
    onConfirm: async () => {
      modal.close()
      try {
        await apps.removeApp(ns, appName)
        toast.success(`${appName} deleted`)
      } catch {
        toast.error(`Failed to delete ${appName}`)
      }
    },
  })
}

async function refreshApps() {
  refreshing.value = true
  try {
    if (isAllProjects.value) {
      await apps.loadAllApps()
    } else {
      await apps.loadApps(currentProject.value)
    }
    toast.success('App list refreshed')
  } catch {
    toast.error('Failed to refresh apps')
  } finally {
    refreshing.value = false
  }
}

function openApp(appName: string, namespace?: string) {
  selectedApp.value = appName
  selectedAppNamespace.value = namespace || currentProject.value
}
</script>

<template>
  <div class="animate-fade-in">
    <!-- Header -->
    <div class="mb-8 flex items-center justify-between">
      <div>
        <h1 class="text-2xl font-semibold tracking-tight text-slate-900 dark:text-slate-50">Apps</h1>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">
          <template v-if="isAllProjects">All projects</template>
          <template v-else>
            <span class="font-mono font-medium text-kipper-600 dark:text-kipper-400">{{ currentProject }}</span>
          </template>
        </p>
      </div>
      <div class="flex items-center gap-2">
        <button
          @click="refreshApps"
          :disabled="refreshing"
          class="rounded-lg border border-slate-300 p-2.5 text-slate-600 transition-colors hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
          title="Refresh app list"
        >
          <RefreshCw class="h-4 w-4" :class="refreshing ? 'animate-spin' : ''" :stroke-width="1.75" />
        </button>
        <button
          v-if="!isAllProjects && authStore.isDeployer"
          @click="showDeploy = !showDeploy"
          class="inline-flex items-center gap-2 rounded-lg bg-kipper-600 px-4 py-2.5 text-sm font-semibold text-white shadow-sm transition-colors hover:bg-kipper-700 dark:bg-kipper-500 dark:hover:bg-kipper-600"
        >
          <Plus class="h-4 w-4" :stroke-width="2" />
          Deploy app
        </button>
      </div>
    </div>

    <!-- Deploy form -->
    <div v-if="showDeploy" class="mb-6 animate-slide-up rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
      <!-- Source tabs -->
      <div class="mb-4 flex gap-1 rounded-lg bg-slate-100 p-1 dark:bg-slate-800">
        <button
          @click="deployMode = 'git'"
          :class="deployMode === 'git' ? 'bg-white shadow-sm dark:bg-slate-700 dark:text-slate-50' : 'text-slate-500 dark:text-slate-400'"
          class="flex-1 rounded-md px-3 py-1.5 text-sm font-medium transition-colors"
        >
          From Git repository
        </button>
        <button
          @click="deployMode = 'image'"
          :class="deployMode === 'image' ? 'bg-white shadow-sm dark:bg-slate-700 dark:text-slate-50' : 'text-slate-500 dark:text-slate-400'"
          class="flex-1 rounded-md px-3 py-1.5 text-sm font-medium transition-colors"
        >
          From container image
        </button>
      </div>

      <form @submit.prevent="handleDeploy" class="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <!-- Git fields -->
        <template v-if="deployMode === 'git'">
          <div class="sm:col-span-2">
            <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Git URL</label>
            <input
              v-model="gitUrl"
              type="text"
              placeholder="https://github.com/org/repo.git"
              class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
            />
          </div>
          <div>
            <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Branch</label>
            <input
              v-model="gitBranch"
              type="text"
              placeholder="main"
              class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
            />
          </div>
          <div>
            <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Git token <span class="font-normal text-slate-400">(for private repos)</span></label>
            <input
              v-model="gitToken"
              type="password"
              placeholder="ghp_... or glpat-..."
              class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
            />
          </div>
        </template>

        <!-- Image field (image mode only) -->
        <template v-if="deployMode === 'image'">
          <div class="sm:col-span-2">
            <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Image</label>
            <input
              v-model="form.image"
              type="text"
              placeholder="ghcr.io/acme/api:latest"
              class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
            />
          </div>
        </template>

        <!-- Common fields -->
        <div>
          <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">App name</label>
          <input
            v-model="form.name"
            type="text"
            placeholder="api"
            class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
          />
        </div>
        <div>
          <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Port</label>
          <input
            v-model.number="form.port"
            type="number"
            placeholder="8080"
            class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
          />
        </div>
        <div>
          <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Replicas</label>
          <input
            v-model.number="form.replicas"
            type="number"
            min="1"
            class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
          />
        </div>
        <div>
          <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Resource profile</label>
          <select
            v-model="form.resource_profile"
            class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
          >
            <option v-for="p in resourceProfiles" :key="p.value" :value="p.value">{{ p.label }}</option>
          </select>
        </div>
        <div v-if="showCustomResources">
          <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Memory</label>
          <input
            v-model="form.memory_limit"
            type="text"
            placeholder="e.g. 512Mi, 2Gi, 4Gi"
            class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
          />
        </div>
        <div v-if="showCustomResources">
          <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">CPU</label>
          <input
            v-model="form.cpu_limit"
            type="text"
            placeholder="e.g. 250m, 500m, 1, 2"
            class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
          />
        </div>
        <div class="sm:col-span-2">
          <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Environment variables</label>
          <textarea
            v-model="envText"
            rows="3"
            placeholder="KEY=value&#10;API_KEY=sk-...&#10;# Lines starting with # are ignored"
            class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 font-mono text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
          />
          <p class="mt-1 text-xs text-slate-500 dark:text-slate-400">One per line, KEY=value format. For sensitive values, use the Secrets tab after deploying.</p>
        </div>
        <div class="sm:col-span-2">
          <button
            type="submit"
            :disabled="deploying || !form.name || (deployMode === 'image' && !form.image) || (deployMode === 'git' && !gitUrl)"
            class="rounded-lg bg-kipper-600 px-4 py-2.5 text-sm font-semibold text-white shadow-sm transition-colors hover:bg-kipper-700 disabled:opacity-50 dark:bg-kipper-500 dark:hover:bg-kipper-600"
          >
            {{ deploying ? 'Deploying...' : 'Deploy' }}
          </button>
        </div>
      </form>
    </div>

    <!-- Error -->
    <NoticeCallout v-if="apps.error" tone="danger" class="mb-4 px-4 py-3 text-sm text-red-700 dark:text-slate-300">
      {{ apps.error }}
    </NoticeCallout>

    <!-- Loading -->
    <div v-if="apps.loading" class="py-12 text-center text-sm text-slate-500 dark:text-slate-400">Loading...</div>

    <!-- App list -->
    <div v-else-if="apps.apps.length" class="space-y-3">
      <div
        v-for="app in apps.apps"
        :key="`${app.namespace || currentProject}-${app.name}`"
        class="group flex items-center justify-between rounded-xl border border-slate-200 bg-white p-5 transition-colors hover:border-kipper-300 dark:border-slate-800 dark:bg-slate-900 dark:hover:border-kipper-700 cursor-pointer"
        @click="openApp(app.name, app.namespace)"
      >
        <div class="flex min-w-0 items-center gap-4">
          <span class="inline-flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-slate-100 dark:bg-slate-800">
            <Rocket class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
          </span>
          <div class="min-w-0">
            <div class="text-sm font-semibold text-slate-900 dark:text-slate-50">{{ app.name }}</div>
            <div class="mt-0.5 flex items-center gap-2">
              <span v-if="isAllProjects && app.project" class="rounded bg-kipper-50 px-1.5 py-0.5 text-xs font-medium text-kipper-700 dark:bg-kipper-950 dark:text-kipper-400">
                {{ app.project }}
              </span>
              <span class="truncate font-mono text-xs text-slate-500 dark:text-slate-400">{{ app.image }}</span>
            </div>
          </div>
        </div>

        <div class="flex items-center gap-4">
          <!-- Status -->
          <span class="inline-flex items-center gap-1.5 text-sm">
            <span class="inline-block h-2 w-2 rounded-full" :class="statusColor(app.status)" />
            <span :class="statusTextColor(app.status)">{{ app.status }}</span>
          </span>

          <!-- Replicas -->
          <span class="font-mono text-xs text-slate-500 dark:text-slate-400">
            {{ app.ready }}/{{ app.replicas }}
          </span>

          <!-- Delete -->
          <button
            v-if="authStore.isDeployer"
            @click.stop="handleDelete(app.name, app.namespace)"
            class="rounded-lg p-2 text-slate-400 md:opacity-0 transition-all hover:bg-red-50 hover:text-red-600 group-hover:opacity-100 dark:hover:bg-red-950 dark:hover:text-red-400"
            title="Delete app"
          >
            <Trash2 class="h-4 w-4" :stroke-width="1.75" />
          </button>
        </div>
      </div>
    </div>

    <!-- Empty state -->
    <div v-else class="rounded-xl border border-dashed border-slate-300 py-16 text-center dark:border-slate-700">
      <Rocket class="mx-auto mb-3 h-10 w-10 text-slate-400 dark:text-slate-500" :stroke-width="1.5" />
      <p class="text-sm font-medium text-slate-900 dark:text-slate-50">No apps deployed</p>
      <p v-if="isAllProjects" class="mt-1 text-sm text-slate-500 dark:text-slate-400">Select a project to deploy your first app</p>
      <p v-else class="mt-1 text-sm text-slate-500 dark:text-slate-400">Deploy your first app to get started</p>
    </div>

    <!-- App detail panel -->
    <AppDetail
      v-if="selectedApp"
      :app-name="selectedApp"
      :namespace="selectedAppNamespace || currentProject"
      @close="selectedApp = null; selectedAppNamespace = null"
    />
  </div>
</template>
