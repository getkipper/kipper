<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { ArrowLeft, ArrowRight, Check, FolderKanban, Globe, Settings2, KeyRound, ListChecks, Sparkles } from 'lucide-vue-next'
import { useToast } from '@/composables/useToast'
import { fetchCopyPreview, addEnvironment, type CopyPreview, type AppOverride } from '@/api/projects'

const props = defineProps<{
  projectName: string
  sourceEnvs: string[]
  // Optional pre-fill from the inline Add environment form so the user
  // doesn't lose what they already typed when escalating from quick-add
  // to the wizard.
  initialName?: string
  initialSource?: string
}>()

const emit = defineEmits<{
  close: []
  created: [{ name: string }]
}>()

const toast = useToast()

type Step = 'source' | 'routes' | 'env' | 'resources' | 'review'
const stepOrder: Step[] = ['source', 'routes', 'env', 'resources', 'review']
const stepLabels: Record<Step, string> = {
  source: 'Source & target',
  routes: 'Routes',
  env: 'Env vars',
  resources: 'Resources',
  review: 'Review',
}
const stepIcons = {
  source: FolderKanban,
  routes: Globe,
  env: KeyRound,
  resources: Settings2,
  review: ListChecks,
}

const currentStep = ref<Step>('source')
const stepIndex = computed(() => stepOrder.indexOf(currentStep.value))

const newEnvName = ref('')
const sourceEnv = ref('')
const preview = ref<CopyPreview | null>(null)
const previewLoading = ref(false)
const submitting = ref(false)

// Per-app edited state. Keyed by app name.
const editedHosts = ref<Record<string, string>>({})
const editedEnv = ref<Record<string, Record<string, string>>>({})
const editedReplicas = ref<Record<string, number>>({})
const editedMemoryLimit = ref<Record<string, string>>({})
const editedCpuLimit = ref<Record<string, string>>({})

// Track env vars whose value the wizard auto-rewrote (because the
// source value contained the source namespace string). We keep the
// original around so the user can revert and so the table can show what
// changed and why. Key format: `<app>::<key>`.
type Rewrite = { from: string; to: string }
const autoRewrites = ref<Record<string, Rewrite>>({})

function rewriteKey(app: string, key: string): string {
  return `${app}::${key}`
}

const envFilter = ref<'all' | 'suspect'>('all')
const SUSPECT_PATTERNS = /STRIPE_|SENTRY_|_URL$|CALLBACK|_KEY$|SECRET|LIVE|TEST|PROD|STAGING|API_KEY|TOKEN/i

function isSuspect(key: string): boolean {
  return SUSPECT_PATTERNS.test(key)
}

const appsWithRoute = computed(() => preview.value?.apps.filter(a => a.route) ?? [])
const allEnvVars = computed(() => {
  if (!preview.value) return []
  const rows: { app: string; key: string; value: string }[] = []
  for (const app of preview.value.apps) {
    const env = editedEnv.value[app.name] ?? {}
    for (const key of Object.keys(env).sort()) {
      rows.push({ app: app.name, key, value: env[key] })
    }
  }
  return rows
})
const filteredEnvVars = computed(() =>
  envFilter.value === 'suspect' ? allEnvVars.value.filter(r => isSuspect(r.key)) : allEnvVars.value,
)

// Compute simple diffs for the review step.
const diffSummary = computed(() => {
  if (!preview.value) return { hostsChanged: 0, envsChanged: 0, resourcesChanged: 0, replicasChanged: 0 }
  let hostsChanged = 0
  let envsChanged = 0
  let resourcesChanged = 0
  let replicasChanged = 0
  for (const app of preview.value.apps) {
    const defaultHost = preview.value.default_hosts[app.name]
    if (defaultHost && editedHosts.value[app.name] !== defaultHost) hostsChanged++

    const original = app.env ?? {}
    const current = editedEnv.value[app.name] ?? {}
    for (const k of Object.keys(current)) {
      if (current[k] !== (original[k] ?? '')) {
        envsChanged++
        break
      }
    }

    if (editedReplicas.value[app.name] !== app.replicas) replicasChanged++
    const oMem = app.resources?.memoryLimit ?? ''
    const oCpu = app.resources?.cpuLimit ?? ''
    if ((editedMemoryLimit.value[app.name] ?? '') !== oMem || (editedCpuLimit.value[app.name] ?? '') !== oCpu) {
      resourcesChanged++
    }
  }
  return { hostsChanged, envsChanged, resourcesChanged, replicasChanged }
})

async function loadPreview() {
  if (!sourceEnv.value || !newEnvName.value.trim()) {
    preview.value = null
    return
  }
  previewLoading.value = true
  try {
    preview.value = await fetchCopyPreview(props.projectName, sourceEnv.value, newEnvName.value.trim())
    // Seed edit state from the preview. Env values that contain the
    // source namespace string get auto-rewritten to the target
    // namespace — covers the SPRING_DATASOURCE_URL=...hrportal-test...
    // case where a hardcoded ref would silently aim prod at test.
    editedHosts.value = { ...preview.value.default_hosts }
    editedEnv.value = {}
    editedReplicas.value = {}
    editedMemoryLimit.value = {}
    editedCpuLimit.value = {}
    autoRewrites.value = {}
    const srcNs = preview.value.source_namespace
    const tgtNs = preview.value.target_namespace
    for (const app of preview.value.apps) {
      const env: Record<string, string> = {}
      for (const [key, value] of Object.entries(app.env ?? {})) {
        if (srcNs && tgtNs && srcNs !== tgtNs && value.includes(srcNs)) {
          const newValue = value.split(srcNs).join(tgtNs)
          env[key] = newValue
          autoRewrites.value[rewriteKey(app.name, key)] = { from: value, to: newValue }
        } else {
          env[key] = value
        }
      }
      editedEnv.value[app.name] = env
      editedReplicas.value[app.name] = app.replicas
      editedMemoryLimit.value[app.name] = app.resources?.memoryLimit ?? ''
      editedCpuLimit.value[app.name] = app.resources?.cpuLimit ?? ''
    }
  } catch {
    toast.error('Failed to load source environment')
    preview.value = null
  } finally {
    previewLoading.value = false
  }
}

watch([sourceEnv, newEnvName], () => {
  // Lazy-load on every change; cheap call, keeps default_hosts accurate.
  if (currentStep.value === 'source' || preview.value) loadPreview()
})

function applyProdDefaults() {
  if (!preview.value) return
  for (const app of preview.value.apps) {
    const replicas = editedReplicas.value[app.name] ?? app.replicas
    editedReplicas.value[app.name] = Math.max(2, replicas)
    const mem = editedMemoryLimit.value[app.name] ?? app.resources?.memoryLimit ?? ''
    if (mem) editedMemoryLimit.value[app.name] = bumpMemory(mem)
  }
  toast.success('Prod defaults applied. Review and adjust if needed')
}

// bumpMemory multiplies a Kubernetes memory string (Mi, Gi) by ~1.5x,
// rounding to the nearest 64Mi step. Returns the original string if the
// suffix is unrecognised.
function bumpMemory(s: string): string {
  const m = /^(\d+)(Mi|Gi)$/.exec(s.trim())
  if (!m) return s
  const value = Number(m[1])
  const unit = m[2]
  const scaled = Math.ceil((value * 1.5) / 64) * 64
  return `${scaled}${unit}`
}

function setEnvVar(app: string, key: string, value: string) {
  if (!editedEnv.value[app]) editedEnv.value[app] = {}
  editedEnv.value[app][key] = value
  // Manual edits invalidate the auto-rewrite badge — the user has
  // taken ownership of the value.
  delete autoRewrites.value[rewriteKey(app, key)]
}

function rewriteFor(app: string, key: string): Rewrite | undefined {
  return autoRewrites.value[rewriteKey(app, key)]
}

function undoRewrite(app: string, key: string) {
  const r = rewriteFor(app, key)
  if (!r) return
  if (!editedEnv.value[app]) editedEnv.value[app] = {}
  editedEnv.value[app][key] = r.from
  delete autoRewrites.value[rewriteKey(app, key)]
}

function buildOverrides(): Record<string, AppOverride> {
  if (!preview.value) return {}
  const out: Record<string, AppOverride> = {}
  for (const app of preview.value.apps) {
    const o: AppOverride = {}
    let touched = false

    // Route override only if user changed it from the default OR cleared it.
    const defaultHost = preview.value.default_hosts[app.name]
    if (defaultHost !== undefined) {
      const desired = (editedHosts.value[app.name] ?? '').trim()
      if (desired !== defaultHost) {
        o.route = { host: desired, path: app.route?.path || '/' }
        touched = true
      }
    }

    // Env: send the full map any time we have edits, since the wizard
    // exposes every key — easier server-side to treat as authoritative.
    const original = app.env ?? {}
    const current = editedEnv.value[app.name] ?? {}
    let envChanged = false
    if (Object.keys(original).length !== Object.keys(current).length) envChanged = true
    else {
      for (const k of Object.keys(original)) {
        if (original[k] !== current[k]) {
          envChanged = true
          break
        }
      }
    }
    if (envChanged) {
      o.env = { ...current }
      touched = true
    }

    if (editedReplicas.value[app.name] !== app.replicas) {
      o.replicas = editedReplicas.value[app.name]
      touched = true
    }

    const oMem = app.resources?.memoryLimit ?? ''
    const oCpu = app.resources?.cpuLimit ?? ''
    const newMem = editedMemoryLimit.value[app.name] ?? ''
    const newCpu = editedCpuLimit.value[app.name] ?? ''
    if (newMem !== oMem || newCpu !== oCpu) {
      o.resources = {
        ...app.resources,
        memoryLimit: newMem,
        cpuLimit: newCpu,
      }
      touched = true
    }

    if (touched) out[app.name] = o
  }
  return out
}

const canAdvance = computed(() => {
  switch (currentStep.value) {
    case 'source':
      return Boolean(sourceEnv.value && newEnvName.value.trim() && preview.value)
    default:
      return true
  }
})

function next() {
  if (!canAdvance.value) return
  const i = stepOrder.indexOf(currentStep.value)
  if (i < stepOrder.length - 1) currentStep.value = stepOrder[i + 1]
}

function back() {
  const i = stepOrder.indexOf(currentStep.value)
  if (i > 0) currentStep.value = stepOrder[i - 1]
}

async function submit() {
  if (!preview.value) return
  submitting.value = true
  try {
    const overrides = buildOverrides()
    const resp = await addEnvironment(props.projectName, {
      name: newEnvName.value.trim(),
      copy_from: sourceEnv.value,
      apps: Object.keys(overrides).length ? overrides : undefined,
    })
    const c = resp.copy
    const parts = []
    if (c?.apps) parts.push(`${c.apps} app${c.apps === 1 ? '' : 's'}`)
    if (c?.services) parts.push(`${c.services} service${c.services === 1 ? '' : 's'}`)
    if (c?.secrets) parts.push(`${c.secrets} secret${c.secrets === 1 ? '' : 's'}`)
    toast.success(`Environment ${resp.name} created${parts.length ? `, copied ${parts.join(', ')}` : ''}`)
    emit('created', { name: resp.name })
    emit('close')
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    toast.error(`Failed to create environment: ${msg}`)
  } finally {
    submitting.value = false
  }
}

onMounted(() => {
  if (props.initialName) newEnvName.value = props.initialName
  if (props.initialSource && props.sourceEnvs.includes(props.initialSource)) {
    sourceEnv.value = props.initialSource
  } else if (props.sourceEnvs.length > 0) {
    sourceEnv.value = props.sourceEnvs[0]
  }
})
</script>

<template>
  <div
    class="flex h-[90vh] w-full max-w-5xl flex-col rounded-xl bg-white shadow-xl dark:bg-slate-900"
    @click.stop
  >
    <!-- Header -->
    <div class="border-b border-slate-200 px-6 py-4 dark:border-slate-800">
      <div class="flex items-center justify-between">
        <div class="flex items-center gap-2">
          <Sparkles class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
          <h2 class="text-lg font-semibold text-slate-900 dark:text-slate-50">Copy environment wizard</h2>
        </div>
        <button @click="emit('close')" class="text-sm text-slate-500 hover:text-slate-700 dark:text-slate-400">
          Close
        </button>
      </div>
      <!-- Step pills -->
      <div class="mt-4 flex flex-wrap items-center gap-2">
        <div
          v-for="(step, i) in stepOrder"
          :key="step"
          class="flex items-center gap-2"
        >
          <div
            class="flex h-7 items-center gap-1.5 rounded-full px-3 text-xs font-medium transition-colors"
            :class="i < stepIndex
              ? 'bg-emerald-100 text-emerald-800 dark:bg-emerald-950 dark:text-emerald-300'
              : i === stepIndex
                ? 'bg-kipper-600 text-white'
                : 'bg-slate-100 text-slate-500 dark:bg-slate-800 dark:text-slate-400'"
          >
            <Check v-if="i < stepIndex" class="h-3 w-3" :stroke-width="2.5" />
            <component :is="stepIcons[step]" v-else class="h-3 w-3" :stroke-width="2" />
            {{ stepLabels[step] }}
          </div>
          <ArrowRight v-if="i < stepOrder.length - 1" class="h-3 w-3 text-slate-300" />
        </div>
      </div>
    </div>

    <!-- Body -->
    <div class="flex-1 overflow-auto px-6 py-5">
      <!-- Step: Source & target -->
      <div v-if="currentStep === 'source'" class="space-y-5">
        <p class="text-sm text-slate-600 dark:text-slate-400">
          Pick a source environment to copy from, and name the new one.
          The wizard then walks you through any per-app overrides for
          hostnames, env vars and resources.
        </p>
        <div>
          <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Source environment</label>
          <select
            v-model="sourceEnv"
            class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
          >
            <option v-for="env in sourceEnvs" :key="env" :value="env">{{ env }}</option>
          </select>
        </div>
        <div>
          <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">New environment name</label>
          <input
            v-model="newEnvName"
            type="text"
            placeholder="prod"
            class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
          />
        </div>
        <div v-if="previewLoading" class="text-sm text-slate-500">Loading source environment…</div>
        <div v-else-if="preview" class="space-y-3">
          <p class="text-xs font-medium uppercase tracking-wide text-slate-500 dark:text-slate-400">
            Will be created in <span class="font-mono normal-case">{{ preview.target_namespace || preview.source_namespace + '-…' }}</span>
          </p>

          <!-- Apps -->
          <div class="rounded-lg border border-slate-200 dark:border-slate-700">
            <div class="flex items-center justify-between border-b border-slate-200 px-4 py-2 dark:border-slate-700">
              <span class="text-xs font-semibold text-slate-700 dark:text-slate-300">Apps</span>
              <span class="text-xs text-slate-500 dark:text-slate-400">{{ preview.apps.length }} ({{ appsWithRoute.length }} with route)</span>
            </div>
            <ul v-if="preview.apps.length" class="divide-y divide-slate-100 px-4 text-xs dark:divide-slate-800">
              <li v-for="app in preview.apps" :key="app.name" class="flex items-center justify-between py-1.5">
                <span class="font-mono text-slate-900 dark:text-slate-50">{{ app.name }}</span>
                <span class="break-all text-slate-500 dark:text-slate-400">{{ app.image }}</span>
              </li>
            </ul>
            <p v-else class="px-4 py-3 text-xs text-slate-500">No apps in source.</p>
          </div>

          <!-- Services — explicit because they're the most expensive misunderstanding -->
          <div class="rounded-lg border border-slate-200 dark:border-slate-700">
            <div class="flex items-center justify-between border-b border-slate-200 px-4 py-2 dark:border-slate-700">
              <span class="text-xs font-semibold text-slate-700 dark:text-slate-300">Services</span>
              <span class="text-xs text-slate-500 dark:text-slate-400">{{ preview.services.length }} — fresh credentials, empty data</span>
            </div>
            <ul v-if="preview.services.length" class="divide-y divide-slate-100 px-4 text-xs dark:divide-slate-800">
              <li v-for="svc in preview.services" :key="svc.name" class="flex items-center justify-between py-1.5">
                <span class="font-mono text-slate-900 dark:text-slate-50">{{ svc.name }}</span>
                <span class="text-slate-500 dark:text-slate-400">
                  {{ svc.type }}<span v-if="svc.version"> {{ svc.version }}</span><span v-if="svc.storage"> · {{ svc.storage }}</span>
                </span>
              </li>
            </ul>
            <p v-else class="px-4 py-3 text-xs text-slate-500">No services in source.</p>
          </div>

          <!-- Volumes -->
          <div v-if="preview.volumes.length" class="rounded-lg border border-slate-200 dark:border-slate-700">
            <div class="flex items-center justify-between border-b border-slate-200 px-4 py-2 dark:border-slate-700">
              <span class="text-xs font-semibold text-slate-700 dark:text-slate-300">Volumes</span>
              <span class="text-xs text-slate-500 dark:text-slate-400">{{ preview.volumes.length }} — empty PVCs</span>
            </div>
            <ul class="divide-y divide-slate-100 px-4 text-xs dark:divide-slate-800">
              <li v-for="vol in preview.volumes" :key="vol.name" class="flex items-center justify-between py-1.5">
                <span class="font-mono text-slate-900 dark:text-slate-50">{{ vol.name }}</span>
                <span class="text-slate-500 dark:text-slate-400">{{ vol.size }}</span>
              </li>
            </ul>
          </div>

          <!-- Functions / Jobs / Secrets — quieter line since they need no decisions -->
          <div class="flex flex-wrap gap-3 text-xs text-slate-500 dark:text-slate-400">
            <span v-if="preview.functions.length"><span class="font-semibold">{{ preview.functions.length }}</span> function{{ preview.functions.length === 1 ? '' : 's' }}</span>
            <span v-if="preview.jobs.length"><span class="font-semibold">{{ preview.jobs.length }}</span> job{{ preview.jobs.length === 1 ? '' : 's' }}</span>
            <span v-if="preview.secrets.length"><span class="font-semibold">{{ preview.secrets.length }}</span> user secret{{ preview.secrets.length === 1 ? '' : 's' }}</span>
          </div>
        </div>
      </div>

      <!-- Step: Routes -->
      <div v-else-if="currentStep === 'routes'" class="space-y-3">
        <p class="text-sm text-slate-600 dark:text-slate-400">
          Set the public hostname for each app on the new environment.
          Defaults use your cluster wildcard ({{ preview?.cluster_domain }}) so
          they work immediately. Edit if you want a custom domain. You can
          always change later via the route panel.
        </p>
        <div v-if="appsWithRoute.length === 0" class="rounded-lg border border-dashed border-slate-300 p-6 text-center text-sm text-slate-500 dark:border-slate-700">
          None of the source apps have a public route. Skip this step.
        </div>
        <div v-else class="overflow-x-auto rounded-lg border border-slate-200 dark:border-slate-700">
          <table class="w-full text-sm">
            <thead class="bg-slate-50 text-left text-xs font-medium text-slate-600 dark:bg-slate-800 dark:text-slate-400">
              <tr>
                <th class="px-4 py-2.5">App</th>
                <th class="px-4 py-2.5">Source host</th>
                <th class="px-4 py-2.5">New host</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-slate-100 dark:divide-slate-800">
              <tr v-for="app in appsWithRoute" :key="app.name">
                <td class="px-4 py-2 font-medium text-slate-900 dark:text-slate-50">{{ app.name }}</td>
                <td class="px-4 py-2 font-mono text-xs text-slate-500 dark:text-slate-400">{{ app.route?.host }}</td>
                <td class="px-4 py-2">
                  <input
                    v-model="editedHosts[app.name]"
                    type="text"
                    class="w-full rounded-md border border-slate-300 bg-white px-2 py-1 font-mono text-xs dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
                  />
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <!-- Step: Env vars -->
      <div v-else-if="currentStep === 'env'" class="space-y-3">
        <!-- Reminder of what's about to be created so the user has the
             context they need when reviewing in-cluster references like
             SPRING_DATASOURCE_URL. -->
        <div v-if="preview && preview.services.length" class="rounded-lg border border-kipper-200 bg-kipper-50 p-3 text-xs dark:border-kipper-900 dark:bg-kipper-950/40">
          <p class="font-medium text-kipper-800 dark:text-kipper-200">
            New services in <span class="font-mono">{{ preview.target_namespace }}</span>:
            <span v-for="(svc, i) in preview.services" :key="svc.name">
              <span class="font-mono">{{ svc.name }}</span> <span class="opacity-70">({{ svc.type }})</span><span v-if="i < preview.services.length - 1">, </span>
            </span>
          </p>
          <p class="mt-1 text-kipper-700 dark:text-kipper-300">
            Each gets its own pod, PVC and credentials. Cross-service
            references that use the source namespace literally
            (<span class="font-mono">{{ preview.source_namespace }}</span>) are
            auto-rewritten below. Review the rows tagged
            <span class="rounded bg-sky-100 px-1 py-0.5 text-[10px] font-medium text-sky-700 dark:bg-sky-900/40 dark:text-sky-300">auto-updated</span>.
          </p>
        </div>

        <div class="flex items-center justify-between">
          <p class="text-sm text-slate-600 dark:text-slate-400">
            Edit any env vars that need to differ in the new environment.
          </p>
          <div class="flex items-center gap-1 rounded-lg border border-slate-300 p-0.5 text-xs dark:border-slate-700">
            <button
              @click="envFilter = 'all'"
              :class="envFilter === 'all' ? 'bg-slate-900 text-white dark:bg-slate-50 dark:text-slate-900' : 'text-slate-500'"
              class="rounded px-2.5 py-1"
            >
              All ({{ allEnvVars.length }})
            </button>
            <button
              @click="envFilter = 'suspect'"
              :class="envFilter === 'suspect' ? 'bg-slate-900 text-white dark:bg-slate-50 dark:text-slate-900' : 'text-slate-500'"
              class="rounded px-2.5 py-1"
              title="Show only keys matching common per-env patterns (STRIPE_, SENTRY_, _URL, KEY, SECRET…)"
            >
              Suspect only
            </button>
          </div>
        </div>
        <div v-if="filteredEnvVars.length === 0" class="rounded-lg border border-dashed border-slate-300 p-6 text-center text-sm text-slate-500 dark:border-slate-700">
          {{ envFilter === 'suspect' ? 'No env vars look env-specific.' : 'No env vars on any app.' }}
        </div>
        <div v-else class="overflow-x-auto rounded-lg border border-slate-200 dark:border-slate-700">
          <table class="w-full text-sm">
            <thead class="bg-slate-50 text-left text-xs font-medium text-slate-600 dark:bg-slate-800 dark:text-slate-400">
              <tr>
                <th class="px-4 py-2.5">App</th>
                <th class="px-4 py-2.5">Key</th>
                <th class="px-4 py-2.5">Value</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-slate-100 dark:divide-slate-800">
              <tr v-for="row in filteredEnvVars" :key="`${row.app}-${row.key}`">
                <td class="px-4 py-2 font-medium text-slate-900 dark:text-slate-50">{{ row.app }}</td>
                <td class="px-4 py-2 font-mono text-xs text-slate-700 dark:text-slate-300">
                  {{ row.key }}
                  <span v-if="isSuspect(row.key)" class="ml-1 rounded bg-amber-100 px-1.5 py-0.5 text-[10px] font-medium text-amber-700 dark:bg-amber-900/40 dark:text-amber-300">
                    review
                  </span>
                  <span
                    v-if="rewriteFor(row.app, row.key)"
                    class="ml-1 rounded bg-sky-100 px-1.5 py-0.5 text-[10px] font-medium text-sky-700 dark:bg-sky-900/40 dark:text-sky-300"
                    :title="`Auto-updated to point at the new env's namespace. Source value: ${rewriteFor(row.app, row.key)?.from}`"
                  >
                    auto-updated
                  </span>
                </td>
                <td class="px-4 py-2">
                  <div class="space-y-1">
                    <input
                      :value="editedEnv[row.app]?.[row.key] ?? ''"
                      @input="setEnvVar(row.app, row.key, ($event.target as HTMLInputElement).value)"
                      type="text"
                      class="w-full rounded-md border border-slate-300 bg-white px-2 py-1 font-mono text-xs dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
                    />
                    <div v-if="rewriteFor(row.app, row.key)" class="flex items-center gap-2 text-[10px] text-slate-500 dark:text-slate-400">
                      <span>was <code class="rounded bg-slate-100 px-1 py-0.5 dark:bg-slate-800">{{ rewriteFor(row.app, row.key)?.from }}</code></span>
                      <button
                        @click="undoRewrite(row.app, row.key)"
                        type="button"
                        class="text-kipper-600 hover:underline dark:text-kipper-400"
                      >
                        revert
                      </button>
                    </div>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <!-- Step: Resources -->
      <div v-else-if="currentStep === 'resources'" class="space-y-3">
        <div class="flex items-center justify-between">
          <p class="text-sm text-slate-600 dark:text-slate-400">
            Adjust replicas and resource limits per app. Defaults match the source.
          </p>
          <button
            @click="applyProdDefaults"
            class="inline-flex items-center gap-1.5 rounded-md border border-kipper-300 bg-kipper-50 px-2.5 py-1 text-xs font-medium text-kipper-700 hover:bg-kipper-100 dark:border-kipper-700 dark:bg-kipper-950 dark:text-kipper-300"
            title="Bumps replicas to at least 2 and memory limit by 1.5x"
          >
            <Sparkles class="h-3 w-3" />
            Apply prod defaults
          </button>
        </div>
        <div class="overflow-x-auto rounded-lg border border-slate-200 dark:border-slate-700">
          <table class="w-full text-sm">
            <thead class="bg-slate-50 text-left text-xs font-medium text-slate-600 dark:bg-slate-800 dark:text-slate-400">
              <tr>
                <th class="px-4 py-2.5">App</th>
                <th class="px-4 py-2.5 w-24">Replicas</th>
                <th class="px-4 py-2.5 w-32">Memory limit</th>
                <th class="px-4 py-2.5 w-32">CPU limit</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-slate-100 dark:divide-slate-800">
              <tr v-for="app in preview?.apps ?? []" :key="app.name">
                <td class="px-4 py-2 font-medium text-slate-900 dark:text-slate-50">{{ app.name }}</td>
                <td class="px-4 py-2">
                  <input
                    v-model.number="editedReplicas[app.name]"
                    type="number"
                    min="0"
                    class="w-full rounded-md border border-slate-300 bg-white px-2 py-1 text-xs dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
                  />
                </td>
                <td class="px-4 py-2">
                  <input
                    v-model="editedMemoryLimit[app.name]"
                    type="text"
                    placeholder="512Mi"
                    class="w-full rounded-md border border-slate-300 bg-white px-2 py-1 font-mono text-xs dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
                  />
                </td>
                <td class="px-4 py-2">
                  <input
                    v-model="editedCpuLimit[app.name]"
                    type="text"
                    placeholder="500m"
                    class="w-full rounded-md border border-slate-300 bg-white px-2 py-1 font-mono text-xs dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
                  />
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <!-- Step: Review -->
      <div v-else-if="currentStep === 'review'" class="space-y-4">
        <p class="text-sm text-slate-600 dark:text-slate-400">
          Click Create to provision the new environment with these settings.
        </p>
        <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div class="rounded-lg border border-slate-200 bg-slate-50 p-4 text-sm dark:border-slate-700 dark:bg-slate-800">
            <p class="text-xs uppercase tracking-wide text-slate-500 dark:text-slate-400">From</p>
            <p class="mt-1 font-mono">{{ preview?.source_namespace }}</p>
          </div>
          <div class="rounded-lg border border-kipper-300 bg-kipper-50 p-4 text-sm dark:border-kipper-700 dark:bg-kipper-950/40">
            <p class="text-xs uppercase tracking-wide text-kipper-700 dark:text-kipper-300">To</p>
            <p class="mt-1 font-mono">{{ preview?.target_namespace }}</p>
          </div>
        </div>
        <div class="rounded-lg border border-slate-200 dark:border-slate-700">
          <div class="border-b border-slate-200 px-4 py-2 text-xs font-medium uppercase tracking-wide text-slate-500 dark:border-slate-700 dark:text-slate-400">
            Will be copied
          </div>
          <ul class="divide-y divide-slate-100 px-4 text-sm dark:divide-slate-800">
            <li class="py-2">{{ preview?.apps.length ?? 0 }} apps</li>
            <li class="py-2">{{ preview?.services.length ?? 0 }} services (fresh credentials, empty data)</li>
            <li class="py-2">{{ preview?.volumes.length ?? 0 }} volumes (empty PVCs)</li>
            <li class="py-2">{{ preview?.functions.length ?? 0 }} functions, {{ preview?.jobs.length ?? 0 }} jobs</li>
            <li class="py-2">{{ preview?.secrets.length ?? 0 }} user-managed secrets</li>
          </ul>
        </div>
        <div class="rounded-lg border border-slate-200 dark:border-slate-700">
          <div class="border-b border-slate-200 px-4 py-2 text-xs font-medium uppercase tracking-wide text-slate-500 dark:border-slate-700 dark:text-slate-400">
            Your overrides
          </div>
          <ul class="divide-y divide-slate-100 px-4 text-sm dark:divide-slate-800">
            <li class="py-2">{{ diffSummary.hostsChanged }} app{{ diffSummary.hostsChanged === 1 ? '' : 's' }} with custom hostnames</li>
            <li class="py-2">{{ diffSummary.envsChanged }} app{{ diffSummary.envsChanged === 1 ? '' : 's' }} with edited env vars</li>
            <li class="py-2">{{ diffSummary.replicasChanged }} app{{ diffSummary.replicasChanged === 1 ? '' : 's' }} with replica changes</li>
            <li class="py-2">{{ diffSummary.resourcesChanged }} app{{ diffSummary.resourcesChanged === 1 ? '' : 's' }} with resource changes</li>
          </ul>
        </div>
      </div>
    </div>

    <!-- Footer -->
    <div class="flex items-center justify-between border-t border-slate-200 px-6 py-4 dark:border-slate-800">
      <button
        v-if="stepIndex > 0"
        @click="back"
        :disabled="submitting"
        class="inline-flex items-center gap-1.5 rounded-md border border-slate-300 px-3 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50 dark:border-slate-700 dark:text-slate-300 dark:hover:bg-slate-800"
      >
        <ArrowLeft class="h-4 w-4" :stroke-width="2" />
        Back
      </button>
      <div v-else />
      <button
        v-if="currentStep !== 'review'"
        @click="next"
        :disabled="!canAdvance"
        class="inline-flex items-center gap-1.5 rounded-md bg-kipper-600 px-4 py-2 text-sm font-semibold text-white hover:bg-kipper-700 disabled:opacity-50"
      >
        Next
        <ArrowRight class="h-4 w-4" :stroke-width="2" />
      </button>
      <button
        v-else
        @click="submit"
        :disabled="submitting"
        class="inline-flex items-center gap-1.5 rounded-md bg-kipper-600 px-4 py-2 text-sm font-semibold text-white hover:bg-kipper-700 disabled:opacity-50"
      >
        {{ submitting ? 'Creating…' : 'Create environment' }}
      </button>
    </div>
  </div>
</template>
