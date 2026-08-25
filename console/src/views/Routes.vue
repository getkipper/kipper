<script setup lang="ts">
import { ref, computed, nextTick, onMounted, watch } from 'vue'
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
  // While the editor is open its own `formNamespace` is authoritative for
  // the apps dropdown, so a top-selector change must not overwrite the list
  // the operator is choosing from mid-edit.
  if (showForm.value) return
  loadApps(globalNs.value)
})

async function loadRoutes() {
  loading.value = true
  error.value = null
  try {
    routes.value = await fetchRoutes()
    // While the editor is open, `formNamespace` governs the dropdown — a
    // Refresh that also refetched apps from the top selector would replace
    // the edited group's choices with unrelated ones.
    if (!showForm.value) await loadApps(globalNs.value)
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'unknown error'
  } finally {
    loading.value = false
  }
}

const appsError = ref<string | null>(null)
// The empty-project callout is only true once the request has come back with
// no rows. Without this an in-flight request would render "no apps yet" and
// then flip to a populated list, telling the operator something the code did
// not yet know.
const appsLoading = ref(false)
// Two openEdit clicks in quick succession issue two overlapping fetches.
// Whichever settles last would win, and if it is the older one it silently
// replaces the current namespace's apps with a stale set. A monotonic token
// lets a superseded fetch drop its response.
let appsRequestId = 0

async function loadApps(ns: string) {
  const requestId = ++appsRequestId
  appsError.value = null
  // Clear before every fetch so the previous namespace's apps do not remain
  // selectable while the new fetch is in flight. `optionsFor` still keeps any
  // existing mapping's app visible on its own row.
  apps.value = []
  if (!ns) {
    appsLoading.value = false
    return
  }
  appsLoading.value = true
  try {
    const result = await fetchApps(ns)
    if (requestId !== appsRequestId) return
    apps.value = result
  } catch (e) {
    if (requestId !== appsRequestId) return
    // Not the same as "no apps". Collapsing a permission or network failure
    // into an empty list is what made an empty dropdown indistinguishable from
    // a denied one, and sent an operator looking for the wrong problem.
    appsError.value = e instanceof Error ? e.message : 'the app list could not be read'
  } finally {
    if (requestId === appsRequestId) appsLoading.value = false
  }
}

// An app a mapping already points at, even when the list did not load. A
// <select> whose value has no matching <option> renders blank, so a route group
// with five working mappings looked unconfigured — which reads as data already
// lost rather than as a list that failed to arrive.
function optionsFor(current: string): { name: string }[] {
  if (!current || apps.value.some(a => a.name === current)) return apps.value
  return [...apps.value, { name: current }]
}

// Create / edit form
const showForm = ref(false)
const editingHost = ref<string | null>(null)
const formHost = ref('')
const formMappings = ref<PathMapping[]>([{ path: '/', app: '' }])
const formSaving = ref(false)
// The namespace the form writes to. On create this follows the top selector,
// because there is no other project to infer from. On edit it follows the
// group being edited, so the editor works while "All projects" is selected
// and does not force the operator to change the top selector just to fix a
// path they already know belongs to a specific project.
const formNamespace = ref<string>('')
const formRef = ref<HTMLElement | null>(null)

async function openCreate() {
  editingHost.value = null
  formHost.value = ''
  formMappings.value = [{ path: '/', app: '' }]
  formNamespace.value = globalNs.value
  showForm.value = true
  // Kick the app list off in the background. Blocking the reveal on it lets
  // a slow backend swallow bug 3 all over again.
  void loadApps(formNamespace.value)
  await revealForm()
}

async function openEdit(group: RouteGroup) {
  editingHost.value = group.host
  formHost.value = group.host
  formMappings.value = group.routes.map(r => ({ path: r.path, app: r.service }))
  formNamespace.value = group.namespace
  showForm.value = true
  void loadApps(formNamespace.value)
  await revealForm()
}

// Clicking Edit on a group far down the page used to open the form at the top
// of the page with no visible change, so the click looked like it had done
// nothing. Scroll it into view and land focus in the first field the operator
// can actually type into — the Domain input is disabled during an edit.
async function revealForm() {
  await nextTick()
  formRef.value?.scrollIntoView({ behavior: 'smooth', block: 'center' })
  formRef.value
    ?.querySelector<HTMLInputElement | HTMLSelectElement>('input:not([disabled]), select:not([disabled])')
    ?.focus()
}

function addMapping() {
  formMappings.value.push({ path: '', app: '' })
}

function removeMapping(index: number) {
  formMappings.value.splice(index, 1)
}

async function saveForm() {
  const ns = formNamespace.value
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

async function handleDelete(group: RouteGroup) {
  try {
    await deleteRouteGroup(group.namespace, group.host)
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
    <div v-if="showForm" ref="formRef" class="mb-6 rounded-xl border border-kipper-200 bg-kipper-50 p-5 dark:border-kipper-800 dark:bg-kipper-950">
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

        <!-- Why there is nothing to choose. Said here, where the empty dropdown
             is met, rather than only on save. The empty callout waits for the
             request to actually return with no rows, so an operator does not
             see "no apps yet" during the wait. -->
        <p
          v-if="appsError"
          data-testid="routes-apps-error"
          class="mb-2 rounded-md bg-red-50 px-3 py-2 text-xs text-red-800 dark:bg-red-950/40 dark:text-red-300"
        >The apps in this project could not be listed, so this form may be incomplete. {{ appsError }}</p>
        <p
          v-else-if="appsLoading"
          data-testid="routes-apps-loading"
          class="mb-2 rounded-md bg-slate-100 px-3 py-2 text-xs text-slate-600 dark:bg-slate-800 dark:text-slate-400"
        >Loading apps...</p>
        <p
          v-else-if="apps.length === 0"
          data-testid="routes-no-apps"
          class="mb-2 rounded-md bg-slate-100 px-3 py-2 text-xs text-slate-600 dark:bg-slate-800 dark:text-slate-400"
        >This project has no apps yet, so there is nothing to route to.</p>

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
              <option v-for="app in optionsFor(mapping.app)" :key="app.name" :value="app.name">{{ app.name }}</option>
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
                    @click="handleDelete(group)"
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
