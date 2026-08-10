<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { Plus, Trash2, FolderKanban, ArrowRight, Rocket, RefreshCw, X, Settings, ExternalLink } from 'lucide-vue-next'
import AppDetail from '@/components/AppDetail.vue'
import ConfirmDeleteProject from '@/components/ConfirmDeleteProject.vue'
import ConfirmDeleteEnvironment from '@/components/ConfirmDeleteEnvironment.vue'
import CopyEnvironmentWizard from '@/components/CopyEnvironmentWizard.vue'
import ProjectSettingsPanel from '@/components/ProjectSettingsPanel.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import * as appsApi from '@/api/apps'
import { useToast } from '@/composables/useToast'
import { useModal } from '@/composables/useModal'
import { fetchProjects, createProject, deleteProject, promoteApp, promoteAll, updateProjectEnvironments, addEnvironment, type Project, type Environment } from '@/api/projects'
import { useAuthStore } from '@/stores/auth'

const modal = useModal()
const authStore = useAuthStore()

const toast = useToast()

const projects = ref<Project[]>([])
const loading = ref(false)
const showCreate = ref(false)
const newName = ref('')
const newDisplayName = ref('')
const newEnvs = ref('test,acc,prod')
const creating = ref(false)
const selectedProject = ref<Project | null>(null)
const activeEnv = ref<string>('')
const promoting = ref(false)
const selectedAppName = ref<string | null>(null)
const selectedAppNamespace = ref<string>('')
const addingEnvFor = ref<string | null>(null)
const newEnvName = ref('')
const newEnvCopyFrom = ref<string>('')
const savingEnv = ref(false)

const isWizardChoice = computed(() => newEnvCopyFrom.value.endsWith(':wizard'))

const addButtonLabel = computed(() => {
  if (savingEnv.value) return newEnvCopyFrom.value ? 'Copying…' : 'Adding…'
  if (isWizardChoice.value) return 'Customize…'
  return 'Add'
})

// "Add" / "Copy as-is" need a name before we can submit. The wizard
// collects it in its own first step, so "Customize…" stays clickable
// even with the inline name field empty.
const addButtonDisabled = computed(() =>
  savingEnv.value || (!isWizardChoice.value && !newEnvName.value.trim()),
)

const addButtonTitle = computed(() =>
  addButtonDisabled.value && !savingEnv.value ? 'Type an environment name first' : '',
)

// A project can hold up to env_limit environments (tier default or an admin
// override). At the limit the console stops offering Add, matching the API,
// which rejects an over-limit create.
function atEnvLimit(project: Project): boolean {
  return project.environments.length >= project.env_limit
}

function envLimitTitle(project: Project): string {
  return `Environment limit reached (${project.env_limit}). A cluster admin can raise it for this project once the cluster has capacity for more environments.`
}

// An app in one of these states is still settling (a build running, pods rolling
// out), so its status will change on its own. While any app is transient the
// page polls so a finishing build updates in place instead of only on tab switch.
const transientStatuses = new Set(['pending', 'building', 'rebuilding', 'deploying'])
const hasTransientApp = computed(() =>
  projects.value.some(p => p.environments.some(e => e.apps.some(a => transientStatuses.has(a.status))))
)

let pollTimer: ReturnType<typeof setInterval> | undefined

onMounted(async () => {
  await loadProjects()
  pollTimer = setInterval(() => {
    if (hasTransientApp.value) void refreshProjects()
  }, 5000)
})

onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
})

async function loadProjects() {
  loading.value = true
  try {
    projects.value = await fetchProjects()
  } catch {
    toast.error('Failed to load projects')
  } finally {
    loading.value = false
  }
}

// refreshProjects reloads without the loading spinner so an in-progress build
// updates in place. It keeps the last good list if a poll fails.
async function refreshProjects() {
  try {
    projects.value = await fetchProjects()
  } catch {
    // Ignore a transient poll failure; the page keeps the last good data.
  }
}

async function handleCreate() {
  if (!newName.value.trim()) return
  creating.value = true
  try {
    const envs = newEnvs.value.trim() ? newEnvs.value.split(',').map(e => e.trim()) : undefined
    const display = newDisplayName.value.trim() || undefined
    await createProject(newName.value.trim(), envs, display)
    toast.success(`Project ${newName.value} created`)
    const createdName = newName.value.trim()
    newName.value = ''
    newDisplayName.value = ''
    newEnvs.value = 'test,acc,prod'
    showCreate.value = false
    // The reconciler creates namespaces asynchronously — poll until the project appears
    for (let i = 0; i < 10; i++) {
      await loadProjects()
      if (projects.value.some(p => p.name === createdName)) break
      await new Promise(r => setTimeout(r, 500))
    }
  } catch {
    toast.error('Failed to create project')
  } finally {
    creating.value = false
  }
}

function handleDelete(project: Project) {
  modal.open(ConfirmDeleteProject, {
    projectName: project.name,
    displayName: project.display_name,
    environments: project.environments,
    onConfirm: async () => {
      modal.close()
      try {
        await deleteProject(project.name)
        toast.success(`Project ${project.name} deleted`)
        if (selectedProject.value?.name === project.name) {
          selectedProject.value = null
        }
        if (settingsProjectName.value === project.name) {
          closeSettings()
        }
        await loadProjects()
      } catch {
        toast.error(`Failed to delete ${project.name}`)
      }
    },
  })
}

function selectProject(project: Project) {
  if (selectedProject.value?.name === project.name) {
    // Toggle closed
    selectedProject.value = null
    return
  }
  selectedProject.value = project
  if (project.environments.length > 0) {
    activeEnv.value = project.environments[0].name
  }
}

async function refreshAfterPromotion() {
  // First refresh — deployment exists but pod may not be ready
  await loadProjects()
  selectedProject.value = projects.value.find(p => p.name === selectedProject.value?.name) || null

  // Second refresh after pods have had time to pull and start
  setTimeout(async () => {
    await loadProjects()
    selectedProject.value = projects.value.find(p => p.name === selectedProject.value?.name) || null
  }, 5000)
}

function handlePromote(appName: string, from: string, to: string) {
  if (!selectedProject.value) return
  modal.open(ConfirmDialog, {
    title: `Promote ${appName} to ${to}?`,
    message: `This deploys ${appName} from ${from} into ${to}.`,
    confirmLabel: 'Promote',
    danger: false,
    onConfirm: async () => {
      modal.close()
      promoting.value = true
      try {
        await promoteApp(selectedProject.value!.name, appName, from, to)
        toast.success(`${appName} promoted to ${to}: initialising...`)
        activeEnv.value = to
        // Refresh immediately and again after pods have time to start
        await refreshAfterPromotion()
      } catch {
        toast.error(`Failed to promote ${appName}`)
      } finally {
        promoting.value = false
      }
    },
  })
}

function handlePromoteAll(from: string, to: string) {
  if (!selectedProject.value) return
  modal.open(ConfirmDialog, {
    title: `Promote all apps to ${to}?`,
    message: `This deploys every app from ${from} into ${to}.`,
    confirmLabel: 'Promote all',
    danger: false,
    onConfirm: async () => {
      modal.close()
      promoting.value = true
      try {
        await promoteAll(selectedProject.value!.name, from, to)
        toast.success(`All apps promoted to ${to}: initialising...`)
        activeEnv.value = to
        await refreshAfterPromotion()
      } catch {
        toast.error('Failed to promote')
      } finally {
        promoting.value = false
      }
    },
  })
}

// The settings panel resolves the live project by name, so a reload replacing
// the projects array can never leave it stale props. When any reload drops the
// project (deleted here or by another user), the watch below funnels the
// disappearance through closeSettings so focus restoration is never skipped.
const settingsProjectName = ref<string | null>(null)
const settingsProject = computed(
  () => projects.value.find(p => p.name === settingsProjectName.value) ?? null
)
let settingsTrigger: HTMLElement | null = null

watch(settingsProject, (val) => {
  if (!val && settingsProjectName.value) {
    closeSettings()
  }
})

function openSettings(project: Project, event: MouseEvent) {
  settingsTrigger = event.currentTarget as HTMLElement
  settingsProjectName.value = project.name
  // Only one slide-over at a time, so Esc and click-outside stay unambiguous
  selectedAppName.value = null
}

function closeSettings() {
  settingsProjectName.value = null
  settingsTrigger?.focus()
  settingsTrigger = null
}

function hostOf(url: string) {
  return url.replace(/^https:\/\//, '')
}

function openAppDetail(appName: string, namespace: string) {
  selectedAppName.value = appName
  selectedAppNamespace.value = namespace
  closeSettings()
}

function closeAppDetail() {
  selectedAppName.value = null
}

function handleDeleteEnvApp(appName: string, namespace: string) {
  modal.open(ConfirmDialog, {
    title: `Delete app ${appName}?`,
    message: 'This removes the deployment, service, ingress, and all secrets from this environment. This cannot be undone.',
    confirmLabel: 'Delete app',
    confirmPhrase: appName,
    onConfirm: async () => {
      modal.close()
      try {
        await appsApi.deleteApp(namespace, appName)
        toast.success(`${appName} deleted`)
        await loadProjects()
        selectedProject.value = projects.value.find(p => p.name === selectedProject.value?.name) || null
      } catch {
        toast.error(`Failed to delete ${appName}`)
      }
    },
  })
}

function startAddEnv(project: Project) {
  addingEnvFor.value = project.name
  newEnvName.value = ''
  newEnvCopyFrom.value = ''
}

function openCopyWizard(project: Project, opts: { initialName?: string; initialSource?: string } = {}) {
  // Close the inline form so we don't leave its dropdown open behind the
  // modal. The wizard takes over from here, pre-filled with whatever
  // values the user already typed.
  cancelAddEnv()
  modal.open(CopyEnvironmentWizard, {
    projectName: project.name,
    sourceEnvs: project.environments.map(e => e.name),
    initialName: opts.initialName,
    initialSource: opts.initialSource,
    onCreated: async ({ name }: { name: string }) => {
      await loadProjects()
      const updated = projects.value.find(p => p.name === project.name)
      selectedProject.value = updated ?? null
      if (updated?.environments.some(e => e.name === name)) {
        activeEnv.value = name
      }
    },
  })
}

function cancelAddEnv() {
  addingEnvFor.value = null
  newEnvName.value = ''
  newEnvCopyFrom.value = ''
}

// parseEnvOption decodes the dropdown value into a (source, wizard) pair.
// Plain "test" means copy-as-is; "test:wizard" means open the override
// wizard pre-filled with that source. Empty string means blank.
function parseEnvOption(value: string): { source: string; wizard: boolean } {
  if (!value) return { source: '', wizard: false }
  const [source, mode] = value.split(':')
  return { source, wizard: mode === 'wizard' }
}

async function handleAddEnv(project: Project) {
  const envName = newEnvName.value.trim()
  const choice = parseEnvOption(newEnvCopyFrom.value)
  if (choice.wizard) {
    // Wizard collects the env name in its own first step, so we don't
    // require it here. envName may be empty.
    openCopyWizard(project, { initialName: envName, initialSource: choice.source })
    return
  }
  if (!envName) return
  if (project.environments.some(e => e.name === envName)) {
    toast.error(`Environment ${envName} already exists`)
    return
  }
  savingEnv.value = true
  try {
    const resp = await addEnvironment(project.name, {
      name: envName,
      copy_from: choice.source || undefined,
    })

    if (resp.copy) {
      const c = resp.copy
      const parts = []
      if (c.apps) parts.push(`${c.apps} app${c.apps === 1 ? '' : 's'}`)
      if (c.services) parts.push(`${c.services} service${c.services === 1 ? '' : 's'}`)
      if (c.volumes) parts.push(`${c.volumes} volume${c.volumes === 1 ? '' : 's'}`)
      if (c.functions) parts.push(`${c.functions} function${c.functions === 1 ? '' : 's'}`)
      if (c.jobs) parts.push(`${c.jobs} job${c.jobs === 1 ? '' : 's'}`)
      if (c.secrets) parts.push(`${c.secrets} secret${c.secrets === 1 ? '' : 's'}`)
      const summary = parts.length ? `: copied ${parts.join(', ')} from ${choice.source}` : ''
      toast.success(`Environment ${envName} added${summary}`)
      if (c.warnings) {
        for (const w of c.warnings) toast.info(w)
      }
    } else {
      toast.success(`Environment ${envName} added`)
    }

    addingEnvFor.value = null
    newEnvName.value = ''
    newEnvCopyFrom.value = ''
    for (let i = 0; i < 10; i++) {
      await loadProjects()
      const updated = projects.value.find(p => p.name === project.name)
      selectedProject.value = updated ?? null
      if (updated?.environments.some(e => e.name === envName)) {
        activeEnv.value = envName
        break
      }
      await new Promise(r => setTimeout(r, 500))
    }
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    toast.error(`Failed to add ${envName}: ${msg}`)
  } finally {
    savingEnv.value = false
  }
}

function handleRemoveEnv(project: Project, env: Environment) {
  modal.open(ConfirmDeleteEnvironment, {
    projectName: project.name,
    displayName: project.display_name,
    environment: env,
    onConfirm: async () => {
      modal.close()
      try {
        const next = project.environments.filter(e => e.name !== env.name).map(e => e.name)
        if (next.length === 0) {
          toast.error('A project must keep at least one environment. Delete the project instead')
          return
        }
        await updateProjectEnvironments(project.name, next)
        toast.success(`Environment ${env.name} removed`)
        if (activeEnv.value === env.name) {
          activeEnv.value = next[0]
        }
        await loadProjects()
        selectedProject.value = projects.value.find(p => p.name === project.name) || null
      } catch {
        toast.error(`Failed to remove ${env.name}`)
      }
    },
  })
}

function getNextEnv(currentEnv: string): string | null {
  if (!selectedProject.value) return null
  const envs = selectedProject.value.environments
  const idx = envs.findIndex(e => e.name === currentEnv)
  if (idx < 0 || idx >= envs.length - 1) return null
  return envs[idx + 1].name
}

function statusColor(status: string): string {
  switch (status) {
    case 'running': return 'bg-emerald-500'
    case 'building': return 'bg-amber-500'
    case 'pending': return 'bg-amber-500'
    case 'failed': return 'bg-red-500'
    case 'stopped': return 'bg-slate-400'
    default: return 'bg-slate-400'
  }
}
</script>

<template>
  <div class="animate-fade-in">
    <!-- Header -->
    <div class="mb-8 flex items-center justify-between">
      <div>
        <h1 class="text-2xl font-semibold tracking-tight text-slate-900 dark:text-slate-50">Projects</h1>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Projects, environments, and promotion pipelines</p>
      </div>
      <div class="flex items-center gap-2">
        <button
          @click="loadProjects"
          class="rounded-lg border border-slate-300 p-2.5 text-slate-600 transition-colors hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
          title="Refresh"
        >
          <RefreshCw class="h-4 w-4" :stroke-width="1.75" />
        </button>
        <button
          v-if="authStore.isDeployer"
          @click="showCreate = !showCreate"
          class="inline-flex items-center gap-2 rounded-lg bg-kipper-600 px-4 py-2.5 text-sm font-semibold text-white shadow-sm transition-colors hover:bg-kipper-700 dark:bg-kipper-500 dark:hover:bg-kipper-600"
        >
          <Plus class="h-4 w-4" :stroke-width="2" />
          New project
        </button>
      </div>
    </div>

    <!-- Create form -->
    <div v-if="showCreate" class="mb-6 animate-slide-up rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
      <form @submit.prevent="handleCreate" class="space-y-4">
        <div>
          <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Project name</label>
          <input
            v-model="newName"
            type="text"
            placeholder="blog"
            class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
          />
        </div>
        <div>
          <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Display name</label>
          <input
            v-model="newDisplayName"
            type="text"
            placeholder="example.com Domain Platform"
            class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
          />
          <p class="mt-1 text-xs text-slate-500 dark:text-slate-400">Human-readable name shown in the console</p>
        </div>
        <div>
          <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Environments (comma-separated, in promotion order)</label>
          <input
            v-model="newEnvs"
            type="text"
            placeholder="test,acc,prod"
            class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
          />
          <p class="mt-1 text-xs text-slate-500 dark:text-slate-400">Leave empty for a single default environment</p>
        </div>
        <button
          type="submit"
          :disabled="creating || !newName.trim()"
          class="rounded-lg bg-kipper-600 px-4 py-2.5 text-sm font-semibold text-white shadow-sm transition-colors hover:bg-kipper-700 disabled:opacity-50 dark:bg-kipper-500 dark:hover:bg-kipper-600"
        >
          {{ creating ? 'Creating...' : 'Create project' }}
        </button>
      </form>
    </div>

    <!-- Loading -->
    <div v-if="loading" class="py-12 text-center text-sm text-slate-500 dark:text-slate-400">Loading...</div>

    <!-- Project list -->
    <div v-else-if="projects.length" class="space-y-3">
      <div
        v-for="project in projects"
        :key="project.name"
        class="rounded-xl border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900 transition-colors"
        :class="selectedProject?.name === project.name ? 'border-kipper-400 dark:border-kipper-600' : 'hover:border-kipper-300 dark:hover:border-kipper-700'"
      >
        <div class="group flex items-center justify-between px-5 py-4 cursor-pointer" @click="selectProject(project)">
          <div class="flex items-center gap-3">
            <FolderKanban class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
            <div>
              <div class="text-sm font-semibold text-slate-900 dark:text-slate-50">{{ project.display_name || project.name }}</div>
              <div class="mt-0.5 flex items-center gap-2 text-xs text-slate-500 dark:text-slate-400">
                <span v-if="project.display_name" class="font-mono">{{ project.name }}</span>
                <span v-if="project.org" class="rounded bg-slate-100 px-1 py-0.5 font-mono dark:bg-slate-800">{{ project.org }}</span>
                <span>{{ project.environments.map(e => e.name).join(' → ') }}</span>
              </div>
            </div>
          </div>
          <div class="flex items-center gap-1">
            <button
              @click.stop="openSettings(project, $event)"
              class="rounded-lg p-2 text-slate-400 transition-all hover:bg-slate-100 hover:text-slate-700 dark:hover:bg-slate-800 dark:hover:text-slate-300"
              title="Project settings"
              aria-label="Project settings"
            >
              <Settings class="h-4 w-4" :stroke-width="1.75" />
            </button>
            <button
              v-if="authStore.isDeployer"
              @click.stop="handleDelete(project)"
              class="rounded-lg p-2 text-slate-400 md:opacity-0 transition-all hover:bg-red-50 hover:text-red-600 group-hover:opacity-100 dark:hover:bg-red-950 dark:hover:text-red-400"
              title="Delete project"
            >
              <Trash2 class="h-4 w-4" :stroke-width="1.75" />
            </button>
          </div>
        </div>

        <!-- Environment tabs and apps (expanded when selected) -->
        <div v-if="selectedProject?.name === project.name" class="border-t border-slate-200 dark:border-slate-800">
          <!-- Environment tabs -->
          <div class="flex flex-wrap items-center border-b border-slate-200 dark:border-slate-800">
            <div
              v-for="env in project.environments"
              :key="env.name"
              class="group/tab relative flex items-center"
            >
              <button
                @click.stop="activeEnv = env.name"
                class="pl-5 pr-2 py-2.5 text-sm font-medium capitalize transition-colors"
                :class="activeEnv === env.name
                  ? 'border-b-2 border-kipper-500 text-kipper-600 dark:text-kipper-400'
                  : 'text-slate-500 hover:text-slate-700 dark:text-slate-400 dark:hover:text-slate-200'"
              >
                {{ env.name }}
                <span class="ml-1 text-xs text-slate-400">{{ env.apps.length }}</span>
              </button>
              <button
                v-if="authStore.isDeployer && project.environments.length > 1"
                @click.stop="handleRemoveEnv(project, env)"
                class="mr-2 rounded p-0.5 text-slate-300 md:opacity-0 transition-all hover:bg-red-50 hover:text-red-600 group-hover/tab:opacity-100 dark:text-slate-600 dark:hover:bg-red-950 dark:hover:text-red-400"
                :title="`Remove ${env.name}`"
              >
                <X class="h-3.5 w-3.5" :stroke-width="2" />
              </button>
            </div>

            <!-- Add environment -->
            <div v-if="authStore.isDeployer" class="ml-2">
              <div v-if="addingEnvFor === project.name" class="px-3 py-1.5">
                <form
                  @submit.prevent="handleAddEnv(project)"
                  class="flex items-center gap-1.5"
                >
                  <input
                    v-model="newEnvName"
                    type="text"
                    placeholder="env name"
                    autofocus
                    :disabled="savingEnv"
                    class="w-28 rounded-md border border-slate-300 bg-white px-2 py-1 text-sm text-slate-900 focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
                    @keyup.escape="cancelAddEnv"
                  />
                  <select
                    v-model="newEnvCopyFrom"
                    :disabled="savingEnv || project.environments.length === 0"
                    class="rounded-md border border-slate-300 bg-white px-2 py-1 text-sm text-slate-900 focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
                  >
                    <option value="">Blank. Start empty</option>
                    <optgroup v-if="project.environments.length" label="Copy as-is">
                      <option v-for="env in project.environments" :key="`asis-${env.name}`" :value="env.name">
                        from {{ env.name }}
                      </option>
                    </optgroup>
                    <optgroup v-if="project.environments.length" label="Copy and customize">
                      <option v-for="env in project.environments" :key="`wiz-${env.name}`" :value="`${env.name}:wizard`">
                        from {{ env.name }}…
                      </option>
                    </optgroup>
                  </select>
                  <button
                    type="submit"
                    :disabled="addButtonDisabled"
                    :title="addButtonTitle"
                    class="rounded-md bg-kipper-600 px-2 py-1 text-xs font-medium text-white hover:bg-kipper-700 disabled:opacity-50"
                  >
                    {{ addButtonLabel }}
                  </button>
                  <button
                    type="button"
                    @click.stop="cancelAddEnv"
                    class="rounded-md px-2 py-1 text-xs text-slate-500 hover:bg-slate-100 dark:text-slate-400 dark:hover:bg-slate-800"
                  >
                    Cancel
                  </button>
                </form>
              </div>
              <button
                v-else-if="!atEnvLimit(project)"
                @click.stop="startAddEnv(project)"
                class="inline-flex items-center gap-1 rounded-md px-3 py-1.5 text-xs font-medium text-slate-500 transition-colors hover:bg-slate-100 hover:text-kipper-600 dark:text-slate-400 dark:hover:bg-slate-800 dark:hover:text-kipper-400"
                title="Add environment"
              >
                <Plus class="h-3.5 w-3.5" :stroke-width="2" />
                Add environment
                <span class="ml-1 text-slate-400 dark:text-slate-500">{{ project.environments.length }}/{{ project.env_limit }}</span>
              </button>
              <span
                v-else
                class="inline-flex items-center gap-1 rounded-md px-3 py-1.5 text-xs font-medium"
                :class="project.environments.length > project.env_limit ? 'text-amber-600 dark:text-amber-400' : 'text-slate-400 dark:text-slate-500'"
                :title="envLimitTitle(project)"
              >
                {{ project.environments.length }}/{{ project.env_limit }} environments
              </span>
            </div>
          </div>

          <!-- Active environment apps -->
          <div v-for="env in project.environments" :key="env.name">
            <div v-if="activeEnv === env.name" class="p-5">
              <!-- Promote all button -->
              <div v-if="authStore.isDeployer && getNextEnv(env.name) && env.apps.length" class="mb-4 flex justify-end">
                <button
                  @click.stop="handlePromoteAll(env.name, getNextEnv(env.name)!)"
                  :disabled="promoting"
                  class="inline-flex items-center gap-1.5 rounded-lg border border-kipper-300 px-3 py-1.5 text-xs font-medium text-kipper-600 transition-colors hover:bg-kipper-50 dark:border-kipper-700 dark:text-kipper-400 dark:hover:bg-kipper-950"
                >
                  <ArrowRight class="h-3 w-3" />
                  Promote all to {{ getNextEnv(env.name) }}
                </button>
              </div>

              <!-- App list -->
              <div v-if="env.apps.length" class="space-y-2">
                <div
                  v-for="app in env.apps"
                  :key="app.name"
                  class="group flex flex-wrap items-center justify-between gap-y-2 rounded-lg border border-slate-100 bg-slate-50 px-4 py-3 cursor-pointer transition-colors hover:border-kipper-300 dark:border-slate-700 dark:bg-slate-800 dark:hover:border-kipper-700"
                  @click.stop="openAppDetail(app.name, env.namespace)"
                >
                  <div class="flex items-center gap-3">
                    <Rocket class="h-4 w-4 text-kipper-500" :stroke-width="1.75" />
                    <div>
                      <span class="text-sm font-medium text-slate-900 dark:text-slate-50">{{ app.name }}</span>
                      <span class="ml-2 font-mono text-xs text-slate-500 dark:text-slate-400">{{ app.image.split(':').pop() || 'latest' }}</span>
                    </div>
                  </div>

                  <div class="flex items-center gap-3">
                    <a
                      v-if="app.url"
                      :href="app.url"
                      target="_blank"
                      rel="noopener noreferrer"
                      @click.stop
                      :title="app.url"
                      :aria-label="`Open ${app.url}`"
                      class="inline-flex items-center gap-1.5 rounded-md p-1.5 text-slate-400 transition-colors hover:bg-kipper-50 hover:text-kipper-600 dark:hover:bg-kipper-950 dark:hover:text-kipper-400"
                    >
                      <span class="hidden lg:inline-block max-w-[14rem] truncate font-mono text-xs">{{ hostOf(app.url) }}</span>
                      <ExternalLink class="h-4 w-4" :stroke-width="1.75" />
                    </a>
                    <span class="inline-flex items-center gap-1.5 text-xs">
                      <span class="inline-block h-2 w-2 rounded-full" :class="statusColor(app.status)" />
                      {{ app.status }}
                    </span>
                    <span class="font-mono text-xs text-slate-400">{{ app.ready }}</span>

                    <!-- Promote button -->
                    <button
                      v-if="authStore.isDeployer && getNextEnv(env.name)"
                      @click.stop="handlePromote(app.name, env.name, getNextEnv(env.name)!)"
                      :disabled="promoting"
                      class="inline-flex items-center gap-1 rounded-md bg-kipper-600 px-2 py-1 text-xs font-medium text-white transition-colors hover:bg-kipper-700 disabled:opacity-50"
                    >
                      <ArrowRight class="h-3 w-3" />
                      {{ getNextEnv(env.name) }}
                    </button>

                    <!-- Delete button -->
                    <button
                      v-if="authStore.isDeployer"
                      @click.stop="handleDeleteEnvApp(app.name, env.namespace)"
                      class="rounded-md p-1 text-slate-400 md:opacity-0 transition-all hover:bg-red-50 hover:text-red-600 group-hover:opacity-100 dark:hover:bg-red-950 dark:hover:text-red-400"
                      title="Delete app"
                    >
                      <Trash2 class="h-3.5 w-3.5" :stroke-width="1.75" />
                    </button>
                  </div>
                </div>
              </div>

              <div v-else class="py-8 text-center text-sm text-slate-500 dark:text-slate-400">
                No apps deployed in {{ env.name }}
              </div>
            </div>
          </div>

        </div>
      </div>
    </div>

    <!-- Empty state -->
    <div v-else class="rounded-xl border border-dashed border-slate-300 py-16 text-center dark:border-slate-700">
      <FolderKanban class="mx-auto mb-3 h-10 w-10 text-slate-400 dark:text-slate-500" :stroke-width="1.5" />
      <p class="text-sm font-medium text-slate-900 dark:text-slate-50">No projects yet</p>
      <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Create one to start deploying apps</p>
    </div>

    <!-- App detail panel -->
    <AppDetail
      v-if="selectedAppName"
      :app-name="selectedAppName"
      :namespace="selectedAppNamespace"
      @close="closeAppDetail"
    />

    <!-- Project settings panel -->
    <ProjectSettingsPanel
      v-if="settingsProject"
      :project="settingsProject"
      @close="closeSettings"
    />
  </div>
</template>
