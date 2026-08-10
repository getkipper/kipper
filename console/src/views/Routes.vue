<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { Globe, ShieldCheck, ArrowRight, ExternalLink, RefreshCw, Plus, Trash2, X, Pencil } from 'lucide-vue-next'
import SaveButton from '@/components/SaveButton.vue'
import NoticeCallout from '@/components/NoticeCallout.vue'
import { useToast } from '@/composables/useToast'
import { useProjectsStore } from '@/stores/projects'
import { useAuthStore } from '@/stores/auth'
import { fetchRoutes, createRouteGroup, updateRouteGroup, deleteRouteGroup, type RouteGroup, type PathMapping } from '@/api/routes'
import { fetchApps } from '@/api/apps'
import type { App } from '@/api/types'

const toast = useToast()
const projectsStore = useProjectsStore()
const authStore = useAuthStore()
const globalNs = computed(() => projectsStore.globalNamespace)
const routes = ref<RouteGroup[]>([])
const loading = ref(false)
const error = ref<string | null>(null)
const apps = ref<App[]>([])

onMounted(async () => {
  await loadRoutes()
})

watch(globalNs, () => {
  loadApps()
})

async function loadRoutes() {
  loading.value = true
  error.value = null
  try {
    routes.value = await fetchRoutes()
    await loadApps()
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'unknown error'
  } finally {
    loading.value = false
  }
}

async function loadApps() {
  const ns = globalNs.value
  if (!ns) {
    apps.value = []
    return
  }
  try {
    apps.value = await fetchApps(ns)
  } catch {
    apps.value = []
  }
}

// Create / edit form
const showForm = ref(false)
const editingHost = ref<string | null>(null)
const formHost = ref('')
const formMappings = ref<PathMapping[]>([{ path: '/', app: '' }])
const formSaving = ref(false)

function openCreate() {
  editingHost.value = null
  formHost.value = ''
  formMappings.value = [{ path: '/', app: '' }]
  showForm.value = true
}

function openEdit(group: RouteGroup) {
  editingHost.value = group.host
  formHost.value = group.host
  formMappings.value = group.routes.map(r => ({ path: r.path, app: r.service }))
  showForm.value = true
}

function addMapping() {
  formMappings.value.push({ path: '', app: '' })
}

function removeMapping(index: number) {
  formMappings.value.splice(index, 1)
}

async function saveForm() {
  const ns = globalNs.value
  if (!ns) {
    toast.error('Select a project first')
    return
  }

  const validMappings = formMappings.value.filter(m => m.app && m.path)
  if (validMappings.length === 0) {
    toast.error('Add at least one path mapping')
    return
  }

  formSaving.value = true
  try {
    if (editingHost.value) {
      await updateRouteGroup(ns, {
        host: editingHost.value,
        mappings: validMappings,
      })
      toast.success('Route group updated')
    } else {
      const resp = await createRouteGroup(ns, {
        host: formHost.value || undefined,
        mappings: validMappings,
      })
      toast.success(`Route group created, ${resp.url}`)
    }
    showForm.value = false
    await loadRoutes()
  } catch {
    toast.error('Failed to save route group')
  } finally {
    formSaving.value = false
  }
}

async function handleDelete(host: string) {
  const ns = globalNs.value
  if (!ns) return
  try {
    await deleteRouteGroup(ns, host)
    toast.success('Route group deleted')
    await loadRoutes()
  } catch {
    toast.error('Failed to delete route group')
  }
}

interface ProjectRoutes {
  name: string
  environments: { name: string; namespace: string; routes: RouteGroup[] }[]
}

const groupedByProject = computed(() => {
  const projectMap = new Map<string, Map<string, { namespace: string; routes: RouteGroup[] }>>()

  for (const route of routes.value) {
    if (projectsStore.isSystemNamespace(route.namespace)) continue
    if (globalNs.value && route.namespace !== globalNs.value) continue

    const project = route.project || route.namespace
    const env = route.environment || 'default'

    if (!projectMap.has(project)) {
      projectMap.set(project, new Map())
    }
    const envMap = projectMap.get(project)!
    if (!envMap.has(env)) {
      envMap.set(env, { namespace: route.namespace, routes: [] })
    }
    envMap.get(env)!.routes.push(route)
  }

  const result: ProjectRoutes[] = []
  for (const [name, envMap] of projectMap) {
    const environments: { name: string; namespace: string; routes: RouteGroup[] }[] = []
    for (const [envName, envData] of envMap) {
      environments.push({ name: envName, namespace: envData.namespace, routes: envData.routes })
    }
    environments.sort((a, b) => a.name.localeCompare(b.name))
    result.push({ name, environments })
  }
  result.sort((a, b) => a.name.localeCompare(b.name))
  return result
})

function envColor(env: string): string {
  switch (env) {
    case 'test': return 'bg-blue-100 text-blue-700 dark:bg-blue-950 dark:text-blue-400'
    case 'acc': return 'bg-amber-100 text-amber-700 dark:bg-amber-950 dark:text-amber-400'
    case 'prod': return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-950 dark:text-emerald-400'
    default: return 'bg-slate-100 text-slate-700 dark:bg-slate-800 dark:text-slate-400'
  }
}
</script>

<template>
  <div class="animate-fade-in">
    <!-- Header -->
    <div class="mb-8 flex items-center justify-between">
      <div>
        <h1 class="text-2xl font-semibold tracking-tight text-slate-900 dark:text-slate-50">Routes</h1>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Domains, paths, and SSL certificates</p>
      </div>
      <div class="flex items-center gap-2">
        <button
          v-if="globalNs && authStore.isDeployer"
          @click="openCreate"
          class="flex items-center gap-1.5 rounded-lg bg-kipper-600 px-3.5 py-2 text-sm font-medium text-white hover:bg-kipper-700"
        >
          <Plus class="h-4 w-4" />
          Create route
        </button>
        <button
          @click="loadRoutes"
          class="rounded-lg border border-slate-300 p-2.5 text-slate-600 transition-colors hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
          title="Refresh"
        >
          <RefreshCw class="h-4 w-4" :stroke-width="1.75" />
        </button>
      </div>
    </div>

    <!-- Create / Edit form -->
    <div v-if="showForm" class="mb-6 rounded-xl border border-kipper-200 bg-kipper-50 p-5 dark:border-kipper-800 dark:bg-kipper-950">
      <div class="mb-4 flex items-center justify-between">
        <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-50">
          {{ editingHost ? 'Edit route group' : 'Create route group' }}
        </h3>
        <button @click="showForm = false" class="text-slate-400 hover:text-slate-600 dark:hover:text-slate-300">
          <X class="h-4 w-4" />
        </button>
      </div>

      <!-- Hostname -->
      <div class="mb-4">
        <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Domain</label>
        <input
          v-model="formHost"
          type="text"
          :disabled="!!editingHost"
          placeholder="Leave empty for auto-generated kipper.run subdomain"
          class="w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none disabled:opacity-50 dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50 dark:placeholder-slate-500"
        />
      </div>

      <!-- Path mappings -->
      <div class="mb-4">
        <label class="mb-2 block text-xs font-medium text-slate-600 dark:text-slate-400">Path mappings</label>
        <div class="space-y-2">
          <div v-for="(mapping, index) in formMappings" :key="index" class="flex items-center gap-2">
            <input
              v-model="mapping.path"
              type="text"
              placeholder="/"
              class="w-40 rounded-md border border-slate-300 bg-white px-3 py-2 text-sm font-mono text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50"
            />
            <ArrowRight class="h-3.5 w-3.5 flex-shrink-0 text-slate-400" />
            <select
              v-model="mapping.app"
              class="flex-1 rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50"
            >
              <option value="">Select app...</option>
              <option v-for="app in apps" :key="app.name" :value="app.name">{{ app.name }}</option>
            </select>
            <button
              v-if="formMappings.length > 1"
              @click="removeMapping(index)"
              class="text-slate-400 hover:text-red-500"
            >
              <Trash2 class="h-4 w-4" />
            </button>
          </div>
        </div>
        <button
          @click="addMapping"
          class="mt-2 flex items-center gap-1 text-xs font-medium text-kipper-600 hover:text-kipper-700 dark:text-kipper-400"
        >
          <Plus class="h-3.5 w-3.5" />
          Add path
        </button>
      </div>

      <SaveButton :saving="formSaving" :label="editingHost ? 'Update route group' : 'Create route group'" @click="saveForm" />
    </div>

    <!-- Loading -->
    <div v-if="loading" class="py-12 text-center text-sm text-slate-500 dark:text-slate-400">Loading...</div>

    <!-- Error -->
    <NoticeCallout v-else-if="error" tone="danger" class="px-4 py-3 text-sm text-red-700 dark:text-slate-300">
      {{ error }}
    </NoticeCallout>

    <!-- Grouped routes -->
    <div v-else-if="groupedByProject.length" class="space-y-6">
      <div v-for="project in groupedByProject" :key="project.name">
        <!-- Project heading -->
        <h2 class="mb-3 text-sm font-semibold text-slate-900 dark:text-slate-50">{{ project.name }}</h2>

        <div class="space-y-3">
          <div v-for="env in project.environments" :key="env.name">
            <div v-for="group in env.routes" :key="group.name"
              class="rounded-xl border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900"
            >
              <!-- Route group header -->
              <div class="flex flex-wrap items-center justify-between gap-y-2 border-b border-slate-200 px-5 py-3 dark:border-slate-800">
                <div class="flex flex-wrap items-center gap-3">
                  <Globe class="h-4 w-4 text-kipper-500" :stroke-width="1.75" />
                  <span class="text-sm font-medium text-slate-900 dark:text-slate-50">{{ group.host }}</span>
                  <span
                    v-if="group.tls"
                    class="inline-flex items-center gap-1 rounded-full px-1.5 py-0.5 text-xs font-medium"
                    :class="group.health?.tls_ready
                      ? 'bg-emerald-50 text-emerald-700 dark:bg-emerald-950 dark:text-emerald-400'
                      : 'bg-amber-50 text-amber-700 dark:bg-amber-950 dark:text-amber-400'"
                    :title="group.health?.message"
                  >
                    <ShieldCheck class="h-3 w-3" />
                    {{ group.health?.tls_ready ? 'TLS' : group.health?.ingress_ready ? 'TLS pending' : 'Provisioning' }}
                  </span>
                  <span
                    class="rounded-full px-2 py-0.5 text-xs font-medium"
                    :class="envColor(env.name)"
                  >
                    {{ env.name }}
                  </span>
                </div>
                <div class="flex items-center gap-1">
                  <button
                    v-if="authStore.isDeployer"
                    @click="openEdit(group)"
                    class="rounded-lg p-1.5 text-slate-400 hover:bg-slate-100 hover:text-slate-700 dark:hover:bg-slate-800 dark:hover:text-slate-200"
                    title="Edit"
                  >
                    <Pencil class="h-4 w-4" :stroke-width="1.75" />
                  </button>
                  <button
                    v-if="authStore.isDeployer"
                    @click="handleDelete(group.host)"
                    class="rounded-lg p-1.5 text-slate-400 hover:bg-red-50 hover:text-red-500 dark:hover:bg-red-950"
                    title="Delete route group"
                  >
                    <Trash2 class="h-4 w-4" :stroke-width="1.75" />
                  </button>
                  <a
                    :href="'https://' + group.host"
                    target="_blank"
                    class="rounded-lg p-1.5 text-slate-400 hover:bg-slate-100 hover:text-slate-700 dark:hover:bg-slate-800 dark:hover:text-slate-200"
                    title="Open in browser"
                  >
                    <ExternalLink class="h-4 w-4" :stroke-width="1.75" />
                  </a>
                </div>
              </div>

              <!-- Path rules -->
              <div class="divide-y divide-slate-50 dark:divide-slate-800/50">
                <div
                  v-for="route in group.routes"
                  :key="route.path"
                  class="flex items-center gap-4 px-5 py-2.5"
                >
                  <span class="w-40 font-mono text-xs text-slate-900 dark:text-slate-50">{{ route.path }}</span>
                  <ArrowRight class="h-3 w-3 text-slate-400" :stroke-width="1.75" />
                  <span class="font-mono text-xs text-slate-600 dark:text-slate-400">{{ route.service }}</span>
                  <span class="text-xs text-slate-400 dark:text-slate-500">:{{ route.port }}</span>
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- Empty state -->
    <div v-else class="rounded-xl border border-dashed border-slate-300 py-16 text-center dark:border-slate-700">
      <Globe class="mx-auto mb-3 h-10 w-10 text-slate-400 dark:text-slate-500" :stroke-width="1.5" />
      <p class="text-sm font-medium text-slate-900 dark:text-slate-50">No routes configured</p>
      <p class="mt-1 max-w-sm mx-auto text-sm text-slate-500 dark:text-slate-400">
        Create a route group to expose your apps via HTTPS with automatic TLS.
      </p>
      <button
        v-if="globalNs && authStore.isDeployer"
        @click="openCreate"
        class="mt-4 inline-flex items-center gap-1.5 rounded-lg bg-kipper-600 px-4 py-2 text-sm font-medium text-white hover:bg-kipper-700"
      >
        <Plus class="h-4 w-4" />
        Create route
      </button>
    </div>
  </div>
</template>
