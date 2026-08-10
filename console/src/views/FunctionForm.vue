<script setup lang="ts">
import { ref, computed, onMounted, watch, nextTick } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ArrowLeft, Plus, Trash2, RefreshCw, Save, Code as CodeIcon, Wand2, Eye, EyeOff, ScanLine, Loader2, Play, Sparkles } from 'lucide-vue-next'
import { Codemirror } from 'vue-codemirror'
import { javascript } from '@codemirror/lang-javascript'
import { python } from '@codemirror/lang-python'
import { oneDark } from '@codemirror/theme-one-dark'
import { EditorView } from '@codemirror/view'

import AIChat from '@/components/AIChat.vue'
import { useToast } from '@/composables/useToast'
import { useModal } from '@/composables/useModal'
import DiagnoseModal from '@/components/DiagnoseModal.vue'
import NoticeCallout from '@/components/NoticeCallout.vue'
import { useProjectsStore } from '@/stores/projects'
import {
  fetchFunctions,
  fetchFunctionResources,
  updateFunctionResources,
  fetchFunctionEnv,
  updateFunctionEnv,
  fetchFunctionSecretKeys,
  setFunctionSecrets,
  deleteFunctionSecret,
  fetchFunctionDependencies,
  updateFunctionDependencies,
  fetchFunctionLogs,
  runFunctionTest,
  type FunctionInfo,
  type FunctionLogEntry,
} from '@/api/functions'
import { createInlineFunction, getInlineFunctionCode, updateInlineFunctionCode, type CreateInlineBinding } from '@/api/inline-functions'
import type { SecretKeyInfo } from '@/api/types'
import { fetchServices, fetchServiceInfo, fetchFunctionBindings, fetchRabbitMQVhosts, bindService, unbindService, type ServiceStatus, type FunctionBinding } from '@/api/services'
import { fetchDBSchema } from '@/api/database'
import { describeCron, commonCronPresets } from '@/utils/cron'
import {
  scanPythonImports,
  scanNodeImports,
  detectSiblingConflicts,
  PYTHON_SIBLING_PAIRS,
} from '@/utils/depsScan'

const route = useRoute()
const router = useRouter()
const toast = useToast()
const modal = useModal()
const projectsStore = useProjectsStore()

const namespace = computed(() => projectsStore.globalNamespace || 'default')

// Mode: edit when route has :fn param, create when it doesn't.
const fnName = computed(() => (route.params.fn as string) || '')
const isEdit = computed(() => fnName.value !== '')

// Form state — populated on mount in edit mode, empty in create mode.
const name = ref('')
const runtime = ref<'node' | 'python'>('node')
const code = ref('')
const trigger = ref<'http' | 'cron' | 'postgres' | 'mysql' | 'redis' | 'minio'>('http')
const schedule = ref('')
const eventSource = ref('')
const eventQuery = ref('')
const eventMarkDone = ref('')
const eventList = ref('')
const eventBucket = ref('')

const env = ref<Array<{ key: string; value: string }>>([])
// New secret values the user entered in this session, to be PUT on save.
const secretsToSet = ref<Array<{ key: string; value: string }>>([])
// Existing secret keys (server-known). Values never round-trip.
const secretKeys = ref<SecretKeyInfo[]>([])
const showSecretValueIdx = ref<number | null>(null)

const deps = ref<Array<{ name: string; version: string }>>([])

const bindings = ref<FunctionBinding[]>([])
const services = ref<ServiceStatus[]>([])

const memoryLimit = ref('')
const cpuLimit = ref('')
const memoryRequest = ref('')
const cpuRequest = ref('')
// Advanced toggle exposes the four request/limit fields. Off by
// default — most users want one number per dimension. On automatically
// when the function is already running with request != limit (e.g.
// a JVM-style burstable config someone set via kubectl).
const resourcesAdvanced = ref(false)

// UI state
const loading = ref(false)
const saving = ref(false)
const showAI = ref(false)
const aiSplit = ref(60)
const isDraggingSplit = ref(false)
const fnInfo = ref<FunctionInfo | null>(null)

// Section open state — sensible defaults for the first-build flow.
const sectionOpen = ref<Record<string, boolean>>({
  code: true,
  trigger: true,
  bindings: true,
  env: true,
  secrets: true,
  deps: true,
  resources: false,
  logs: false,
})

// --- Test run (cron triggers only) ---
// Lets the user kick off a one-off run of the function with the cron
// pod template. Useful for cron schedules that fire infrequently
// (e.g. nightly at 02:00 UTC) where you want to confirm the function
// works without waiting hours for the next scheduled run. The
// resulting pod logs are visible in the Logs section like any other
// run, since they share the same `app=<fn>` Loki label.
const testRunning = ref(false)
const lastTestJob = ref<string>('')

async function runTest() {
  if (!isEdit.value || testRunning.value) return
  testRunning.value = true
  try {
    const res = await runFunctionTest(namespace.value, fnName.value)
    lastTestJob.value = res.job_name
    toast.success(`Test run started: job ${res.job_name}`)
    // Open the Logs section if it isn't already, and refresh so the
    // user sees the new pod's output as soon as it boots.
    sectionOpen.value.logs = true
    setTimeout(() => { loadLogs() }, 1500)
  } catch (e: unknown) {
    const err = e as { response?: { data?: { error?: string } }; message?: string }
    toast.error(err.response?.data?.error || err.message || 'Failed to start test run')
  } finally {
    testRunning.value = false
  }
}

// openDiagnose pops the AI diagnosis modal for this function. The
// modal queries pod status, events, and logs server-side, then asks
// the configured AI provider for a one-shot diagnosis. Useful when a
// cron run failed silently or a pod is stuck without an obvious
// reason in the log tail.
function openDiagnose() {
  if (!isEdit.value) return
  modal.open(DiagnoseModal, {
    project: namespace.value,
    appName: fnName.value,
    kind: 'function',
  })
}

// --- Logs ---
const logs = ref<FunctionLogEntry[]>([])
const logSearch = ref('')
const logSince = ref('1h')
const logsLoading = ref(false)
const logsLoaded = ref(false)

async function loadLogs() {
  if (!isEdit.value) return
  logsLoading.value = true
  try {
    logs.value = await fetchFunctionLogs(namespace.value, fnName.value, {
      search: logSearch.value || undefined,
      since: logSince.value,
    })
    logsLoaded.value = true
  } catch {
    logs.value = []
    toast.error('Failed to load logs')
  } finally {
    logsLoading.value = false
  }
}

// Auto-load logs the first time the section is opened.
function applySchedulePreset(e: Event) {
  const select = e.target as HTMLSelectElement
  if (select.value) {
    schedule.value = select.value
    select.value = ''
  }
}

function onLogsToggle(open: boolean) {
  sectionOpen.value.logs = open
  if (open && !logsLoaded.value && isEdit.value) {
    loadLogs()
  }
}

function formatLogTime(nsTimestamp: string): string {
  const ms = Number(nsTimestamp) / 1_000_000
  if (Number.isNaN(ms)) return ''
  return new Date(ms).toLocaleTimeString('en-GB', {
    hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit',
  })
}

// Templates per runtime, used to pre-fill code in create mode.
const NODE_TEMPLATE = `module.exports = async (event) => {
  console.log('Got event:', event)
  return { processed: true }
}
`
const PYTHON_TEMPLATE = `def handler(event, context=None):
    print('Got event:', event)
    return {'processed': True}
`

const codeExtensions = computed(() => {
  const lang = runtime.value === 'python' ? python() : javascript()
  return [lang, oneDark, EditorView.theme({ '&': { height: '100%' }, '.cm-scroller': { overflow: 'auto' } })]
})

const cronInfo = computed(() => describeCron(schedule.value))
const nextRuns = computed(() => cronInfo.value.nextRuns.slice(0, 5))

const cronPresets = commonCronPresets()

// Services that can be used as event-trigger sources. Same
// namespace-restriction as bindableServices below: duplicate names
// across projects would otherwise let the native select swap the
// visible option after the user clicks one.
const eventTriggerCapableServices = computed(() =>
  services.value
    .filter((s) => s.namespace === namespace.value)
    .filter((s) => ['postgres', 'mysql', 'redis', 'minio'].includes(s.type)),
)

// Services that can be bound (database/cache/queue/storage).
// Restricted to the function's own namespace: two services in
// different namespaces can share a name (e.g. one `db` in each
// project), and the native <select> matches v-model by value alone,
// so duplicate names would visually flip the selection between the
// matching options. The server's bind handler already hints with the
// app/function namespace first, so cross-namespace binding never
// worked reliably from this form anyway.
const bindableServices = computed(() =>
  services.value
    .filter((s) => s.namespace === namespace.value)
    .filter((s) => !bindings.value.some((b) => b.service === s.name)),
)

function applyTemplateOnRuntimeChange() {
  if (!isEdit.value && (!code.value || code.value === NODE_TEMPLATE || code.value === PYTHON_TEMPLATE)) {
    code.value = runtime.value === 'python' ? PYTHON_TEMPLATE : NODE_TEMPLATE
  }
}

watch(runtime, applyTemplateOnRuntimeChange)

onMounted(async () => {
  loading.value = true
  try {
    services.value = (await fetchServices()) || []
    if (isEdit.value) {
      await loadExisting()
    } else {
      code.value = NODE_TEMPLATE
    }
  } catch {
    toast.error('Failed to load function')
  } finally {
    loading.value = false
  }
})

async function loadExisting() {
  // We pull the bits we need in parallel to keep the page snappy.
  const [list, codeResp, envMap, secretsList, depsMap, fnBindings, resources] = await Promise.all([
    fetchFunctions(namespace.value).catch(() => [] as FunctionInfo[]),
    getInlineFunctionCode(namespace.value, fnName.value).catch(() => ({ name: fnName.value, runtime: 'node', code: '' } as Awaited<ReturnType<typeof getInlineFunctionCode>>)),
    fetchFunctionEnv(namespace.value, fnName.value).catch(() => ({})),
    fetchFunctionSecretKeys(namespace.value, fnName.value).catch(() => []),
    fetchFunctionDependencies(namespace.value, fnName.value).catch(() => ({})),
    fetchFunctionBindings(namespace.value, fnName.value).catch(() => []),
    fetchFunctionResources(namespace.value, fnName.value).catch(() => ({ memory_limit: '', memory_request: '', cpu_limit: '', cpu_request: '' })),
  ])

  name.value = fnName.value
  runtime.value = (codeResp.runtime === 'python' ? 'python' : 'node')
  code.value = codeResp.code

  fnInfo.value = list.find((f) => f.name === fnName.value) || null
  // Prefer the trigger from the inline-function payload (it carries
  // the schedule + event config too). Fall back to the list view's
  // trigger field if the inline payload didn't carry one (older CRs).
  const triggerType = codeResp.trigger || fnInfo.value?.trigger
  if (triggerType) {
    trigger.value = triggerType as typeof trigger.value
  }
  schedule.value = codeResp.schedule || ''
  eventSource.value = codeResp.source || ''
  eventQuery.value = codeResp.query || ''
  eventMarkDone.value = codeResp.mark_done || ''
  eventList.value = codeResp.redis_list || ''
  eventBucket.value = codeResp.bucket || ''

  env.value = Object.entries(envMap).map(([key, value]) => ({ key, value }))
  secretKeys.value = secretsList
  deps.value = Object.entries(depsMap).map(([n, v]) => ({ name: n, version: v }))
  bindings.value = fnBindings
  memoryLimit.value = resources.memory_limit || ''
  cpuLimit.value = resources.cpu_limit || ''
  memoryRequest.value = resources.memory_request || ''
  cpuRequest.value = resources.cpu_request || ''
  resourcesAdvanced.value =
    (resources.cpu_request !== resources.cpu_limit && resources.cpu_request !== '') ||
    (resources.memory_request !== resources.memory_limit && resources.memory_request !== '')
}

// --- Env table actions ---
function addEnvRow() {
  env.value.push({ key: '', value: '' })
}
function removeEnvRow(idx: number) {
  env.value.splice(idx, 1)
}

// --- Secrets table actions ---
function addSecretRow() {
  secretsToSet.value.push({ key: '', value: '' })
}
function removeNewSecretRow(idx: number) {
  secretsToSet.value.splice(idx, 1)
}
async function deleteExistingSecret(key: string) {
  if (!isEdit.value) return
  try {
    await deleteFunctionSecret(namespace.value, fnName.value, key)
    secretKeys.value = secretKeys.value.filter((s) => s.key !== key)
    toast.success(`Secret ${key} deleted`)
  } catch {
    toast.error('Failed to delete secret')
  }
}

// --- Dependencies actions ---
function addDepRow() {
  deps.value.push({ name: '', version: '' })
}
function removeDepRow(idx: number) {
  deps.value.splice(idx, 1)
}

// depConflicts surfaces sibling-package mistakes (e.g. psycopg2 +
// psycopg2-binary, mysqlclient + pymysql) so the user can clear the
// redundant entry before clicking Save.
const depConflicts = computed(() =>
  detectSiblingConflicts(deps.value.map((d) => d.name).filter((n) => n)),
)

function applyConflictFix(drop: string) {
  deps.value = deps.value.filter((d) => d.name !== drop)
}

// reloadDeps re-fetches the function's dependencies from the server.
// Useful when the CR was changed externally (kubectl patch, AI
// diagnose suggestion, another browser tab) so saving doesn't
// silently revert those changes.
async function reloadDeps() {
  if (!isEdit.value) return
  try {
    const map = await fetchFunctionDependencies(namespace.value, fnName.value)
    deps.value = Object.entries(map).map(([name, version]) => ({ name, version }))
    toast.success('Dependencies refreshed from server')
  } catch {
    toast.error('Failed to reload dependencies')
  }
}
function scanCodeForImports() {
  const scanned = runtime.value === 'python'
    ? scanPythonImports(code.value)
    : scanNodeImports(code.value)

  if (scanned.length === 0) {
    toast.info('No third-party imports found')
    return
  }

  // Existing entries are matched both by their listed name AND by
  // their PyPI sibling (psycopg2 vs psycopg2-binary count as the
  // same thing) so the scan doesn't append a near-duplicate.
  const existing = new Set(deps.value.map((d) => d.name))
  const supersededBy = new Map<string, string>()
  for (const [keep, drop] of PYTHON_SIBLING_PAIRS) {
    supersededBy.set(drop, keep)
    supersededBy.set(keep, drop)
  }

  let added = 0
  let skippedAsSibling = 0
  for (const dep of scanned) {
    if (existing.has(dep.name)) continue
    const sibling = supersededBy.get(dep.name)
    if (sibling && existing.has(sibling)) {
      skippedAsSibling++
      continue
    }
    deps.value.push({ name: dep.name, version: dep.version })
    existing.add(dep.name)
    added++
  }

  if (added === 0 && skippedAsSibling > 0) {
    toast.info('All scanned imports already covered by existing entries')
  } else if (added === 0) {
    toast.info('No new dependencies: code only uses already-listed packages')
  } else {
    toast.success(`Added ${added} dependenc${added === 1 ? 'y' : 'ies'} from code scan`)
  }
}

// --- Binding actions ---
const bindServiceName = ref('')
const bindPrefix = ref('')
// bindDbMode picks one of two paths for the database the binding
// connects to:
//   existing — pick from the dropdown of databases that already
//             exist on the service. The service's own default DB is
//             pre-selected and tagged "(service default)" so the
//             common case (just attach to the data) is one click.
//   new — explicitly create a new empty database with this name.
//             Carries an amber warning so it's hard to do by accident.
const bindDbMode = ref<'existing' | 'new'>('existing')
const bindDatabaseExisting = ref('')
const bindDatabaseNew = ref('')
// Databases that already exist on the picked service. Populated when
// bindServiceName changes for postgres/mysql; empty for other types.
const bindDatabases = ref<string[]>([])
const bindDatabasesLoading = ref(false)
// Name of the service's default DB (the NAME field on its
// credentials secret). Used to tag the dropdown entry and pre-select.
const bindDefaultDatabase = ref('')

// Look up the picked service inside bindableServices (already
// namespace-scoped) — never the raw services list, because a service
// with the same name in another namespace can shadow ours and feed
// the wrong type/default-database into the bind form.
const bindPickerSvc = computed(() => bindableServices.value.find((s) => s.name === bindServiceName.value))

// Service types that carve out a per-binding logical namespace inside
// the service: postgres/mysql get a database, rabbitmq gets a vhost.
// mongodb is omitted from the UI for now because the bind form lacks
// the schema discovery to "Pick existing" for it.
const bindPickerSupportsNamespace = computed(() => {
  const t = bindPickerSvc.value?.type
  return t === 'postgres' || t === 'mysql' || t === 'rabbitmq'
})

// Whether the service type can enumerate existing namespaces in the
// form. Postgres/mysql go through fetchDBSchema; rabbitmq goes
// through fetchRabbitMQVhosts (rabbitmqctl list_vhosts in the pod).
const bindPickerCanListNamespaces = computed(() => {
  const t = bindPickerSvc.value?.type
  return t === 'postgres' || t === 'mysql' || t === 'rabbitmq'
})

// Per-service-type label and placeholder text for the namespace
// input — "database" for postgres/mysql, "vhost" for rabbitmq.
const bindNamespaceLabel = computed(() => (bindPickerSvc.value?.type === 'rabbitmq' ? 'vhost' : 'database'))

// resolvedBindDatabase produces the actual string sent to the backend.
// The wire field is called "database" but it carries either a database
// name or a vhost name depending on the service type — the bind
// handler interprets it based on the service.
const resolvedBindDatabase = computed(() => {
  if (!bindPickerSupportsNamespace.value) return ''
  switch (bindDbMode.value) {
    case 'existing': return bindDatabaseExisting.value || ''
    case 'new': return bindDatabaseNew.value.trim()
    default: return ''
  }
})

async function loadBindDatabases() {
  if (!bindServiceName.value || !bindPickerCanListNamespaces.value || !bindPickerSvc.value) {
    bindDatabases.value = []
    bindDefaultDatabase.value = ''
    return
  }
  bindDatabasesLoading.value = true
  try {
    const svcNs = bindPickerSvc.value.namespace || namespace.value
    if (bindPickerSvc.value.type === 'rabbitmq') {
      // RabbitMQ: rabbitmqctl list_vhosts in the pod. The default
      // "/" is flagged on the server response so we can tag it the
      // same way postgres tags its default database.
      const vhosts = await fetchRabbitMQVhosts(bindServiceName.value, svcNs)
      bindDatabases.value = vhosts.map((v) => v.name).sort()
      const def = vhosts.find((v) => v.default)
      bindDefaultDatabase.value = def?.name || ''
    } else {
      // Postgres/MySQL: schema endpoint for the list, ServiceInfo
      // for the default-DB tag (the NAME field on the credentials
      // secret). Two concurrent calls so the dropdown lands fast.
      const [schema, info] = await Promise.all([
        fetchDBSchema(bindServiceName.value, svcNs),
        fetchServiceInfo(bindServiceName.value, svcNs).catch(() => null),
      ])
      bindDatabases.value = (schema.databases || []).map((d) => d.name).sort()
      bindDefaultDatabase.value = info?.database || ''
    }
    // Pre-select the service default if it appears in the list.
    if (bindDefaultDatabase.value && bindDatabases.value.includes(bindDefaultDatabase.value)) {
      bindDatabaseExisting.value = bindDefaultDatabase.value
    } else if (bindDatabases.value.length > 0) {
      bindDatabaseExisting.value = bindDatabases.value[0]
    }
  } catch {
    bindDatabases.value = []
    bindDefaultDatabase.value = ''
  } finally {
    bindDatabasesLoading.value = false
  }
}

watch(bindServiceName, (next) => {
  // Same reason as bindPickerSvc: stay on the namespace-filtered set
  // so a same-named service in another project can't slip its type
  // (and thus prefix) into our binding.
  const svc = bindableServices.value.find((s) => s.name === next)
  if (svc) {
    // Provide a sensible default prefix based on the service type.
    const prefixMap: Record<string, string> = { postgres: 'DB_', mysql: 'DB_', mongodb: 'DB_', redis: 'REDIS_', minio: 'S3_', rabbitmq: 'AMQP_' }
    bindPrefix.value = prefixMap[svc.type] || svc.type.toUpperCase() + '_'
  } else {
    bindPrefix.value = ''
  }
  // Reset DB picker on service change so we don't carry stale state
  // between two different services. For services that can't list
  // existing namespaces (rabbitmq today), default the mode to "new"
  // so the input field is shown immediately — the user has no
  // dropdown to pick from anyway.
  bindDbMode.value = bindPickerCanListNamespaces.value ? 'existing' : 'new'
  bindDatabaseExisting.value = ''
  bindDatabaseNew.value = ''
  bindDatabases.value = []
  bindDefaultDatabase.value = ''
  loadBindDatabases()
})

// Mirror of kipperv1.CredentialKeys (console-api/api/v1alpha1/service_bindings.go).
// Keep these aligned — the Go side is the source of truth for what
// the credentials Secret actually contains, and this preview is what
// the user (and our AI code-suggestion path) reads to know which
// env vars will exist on the function. Drift here would advertise
// env vars the runtime never gets.
function credentialKeysFor(svcType: string): string[] {
  switch (svcType) {
    case 'postgres':
    case 'mysql':
    case 'mongodb':
      return ['HOST', 'PORT', 'USERNAME', 'PASSWORD', 'NAME']
    case 'rabbitmq':
      return ['HOST', 'PORT', 'USERNAME', 'PASSWORD', 'VHOST']
    case 'minio':
      // S3 rather than the host/port/user/pass baseline. This said the
      // baseline and advertised four variables a MinIO binding never injects.
      return ['ENDPOINT', 'ACCESS_KEY', 'SECRET_KEY']
    default:
      // redis, opensearch and mailhog all start with authentication off, so
      // their binding carries an address and nothing else.
      return ['HOST', 'PORT']
  }
}

const previewInjectedFor = computed(() => {
  const svc = bindPickerSvc.value
  if (!svc) return [] as string[]
  const prefix = bindPrefix.value || ''
  return credentialKeysFor(svc.type).map((k) => prefix + k)
})

function resetBindForm() {
  bindServiceName.value = ''
  bindPrefix.value = ''
  bindDbMode.value = 'existing'
  bindDatabaseExisting.value = ''
  bindDatabaseNew.value = ''
  bindDatabases.value = []
  bindDefaultDatabase.value = ''
}

async function applyBinding() {
  if (!bindServiceName.value) return
  // The namespace picker only renders for services that have a
  // logical-namespace concept (postgres/mysql databases, rabbitmq
  // vhost). For redis/mongodb/minio the picker is hidden and
  // bindDbMode is just a stale default from the last reset — must
  // not block the bind. Validation is also opt-in: a binding with
  // an empty namespace value is the explicit "use the service
  // default" path, the same as omitting --database on the CLI.
  if (bindPickerSupportsNamespace.value) {
    const label = bindNamespaceLabel.value
    // For services with a "Pick existing" mode (postgres/mysql),
    // typing nothing in "Create new" is an error — the user has the
    // dropdown if they want the service default. For services
    // without listing (rabbitmq today), a blank input is the
    // explicit "share the service default" path, which the help
    // text advertises and the backend treats as a no-op bind.
    if (bindDbMode.value === 'new' && !bindDatabaseNew.value.trim() && bindPickerCanListNamespaces.value) {
      toast.error(`Type a ${label} name, or switch to "Pick existing"`)
      return
    }
    if (bindDbMode.value === 'existing' && bindPickerCanListNamespaces.value && !bindDatabaseExisting.value) {
      toast.error(`Pick an existing ${label} from the dropdown`)
      return
    }
  }

  const databasePayload = resolvedBindDatabase.value || undefined

  if (!isEdit.value) {
    // In create mode, queue locally — we'll write bindings as part of POST.
    bindings.value.push({
      service: bindServiceName.value,
      type: bindPickerSvc.value?.type || '',
      prefix: bindPrefix.value,
      database: databasePayload,
      injected_env: previewInjectedFor.value,
    })
    resetBindForm()
    return
  }
  // Edit mode — POST /bind so the controller picks up the per-binding secret.
  try {
    await bindService({
      service: bindServiceName.value,
      app: fnName.value,
      namespace: namespace.value,
      prefix: bindPrefix.value || undefined,
      database: databasePayload,
      target: 'function',
    })
    bindings.value = await fetchFunctionBindings(namespace.value, fnName.value)
    resetBindForm()
    toast.success('Service bound')
  } catch {
    toast.error('Failed to bind service')
  }
}

async function removeBinding(serviceName: string) {
  if (!isEdit.value) {
    bindings.value = bindings.value.filter((b) => b.service !== serviceName)
    return
  }
  try {
    await unbindService({
      service: serviceName,
      app: fnName.value,
      namespace: namespace.value,
      target: 'function',
    })
    bindings.value = bindings.value.filter((b) => b.service !== serviceName)
    toast.success('Service unbound')
  } catch {
    toast.error('Failed to unbind service')
  }
}

// --- Save ---
async function save() {
  if (!name.value || !code.value) {
    toast.error('Function name and code are required')
    return
  }
  if (trigger.value === 'cron' && !schedule.value.trim()) {
    toast.error('Cron schedule is required')
    return
  }
  saving.value = true
  try {
    const envObj: Record<string, string> = {}
    for (const row of env.value) if (row.key) envObj[row.key] = row.value
    const secretsObj: Record<string, string> = {}
    for (const row of secretsToSet.value) if (row.key) secretsObj[row.key] = row.value
    const depsObj: Record<string, string> = {}
    for (const row of deps.value) if (row.name) depsObj[row.name] = row.version || '*'

    if (!isEdit.value) {
      // Create flow — single POST creates the CR + bindings + secrets.
      const bindingsPayload: CreateInlineBinding[] = bindings.value.map((b) => ({
        service: b.service,
        prefix: b.prefix || undefined,
        database: b.database || undefined,
      }))
      await createInlineFunction(namespace.value, {
        name: name.value,
        runtime: runtime.value,
        code: code.value,
        trigger: trigger.value,
        schedule: trigger.value === 'cron' ? schedule.value.trim() : undefined,
        source: eventSource.value || undefined,
        query: eventQuery.value || undefined,
        mark_done: eventMarkDone.value || undefined,
        redis_list: eventList.value || undefined,
        bucket: eventBucket.value || undefined,
        env: Object.keys(envObj).length ? envObj : undefined,
        secrets: Object.keys(secretsObj).length ? secretsObj : undefined,
        dependencies: Object.keys(depsObj).length ? depsObj : undefined,
        bindings: bindingsPayload.length ? bindingsPayload : undefined,
      })
      // InlineFunctions.Create runs per-binding provisioning server-side
      // now (postgres CREATE DATABASE, rabbitmqctl add_vhost, per-binding
      // credentials Secret), so the frontend doesn't have to fan out
      // /bind calls after CR creation.
      toast.success(`Function ${name.value} created`)
      // Route to the edit URL so the form picks up server-side state.
      await router.replace({ name: 'function-edit', params: { fn: name.value } })
      return
    }

    // Edit flow — fan out PUTs in parallel; the controller picks each up.
    // The inline function PUT carries code, runtime, and trigger config
    // so changes to any of them survive a page refresh.
    await Promise.all([
      updateInlineFunctionCode(namespace.value, fnName.value, {
        code: code.value,
        runtime: runtime.value,
        trigger: trigger.value,
        schedule: trigger.value === 'cron' ? schedule.value.trim() : undefined,
        source: eventSource.value || undefined,
        query: eventQuery.value || undefined,
        mark_done: eventMarkDone.value || undefined,
        redis_list: eventList.value || undefined,
        bucket: eventBucket.value || undefined,
      }),
      updateFunctionEnv(namespace.value, fnName.value, envObj),
      updateFunctionDependencies(namespace.value, fnName.value, depsObj),
      Object.keys(secretsObj).length
        ? setFunctionSecrets(namespace.value, fnName.value, secretsObj)
        : Promise.resolve(),
      (memoryLimit.value || cpuLimit.value || memoryRequest.value || cpuRequest.value)
        ? updateFunctionResources(
            namespace.value,
            fnName.value,
            resourcesAdvanced.value
              ? {
                  memory_request: memoryRequest.value,
                  memory_limit: memoryLimit.value,
                  cpu_request: cpuRequest.value,
                  cpu_limit: cpuLimit.value,
                }
              : {
                  memory_limit: memoryLimit.value,
                  cpu_limit: cpuLimit.value,
                },
          )
        : Promise.resolve(),
    ])
    secretsToSet.value = []
    secretKeys.value = await fetchFunctionSecretKeys(namespace.value, fnName.value)
    toast.success('Function saved: controller is rolling out')
  } catch {
    toast.error('Failed to save function')
  } finally {
    saving.value = false
  }
}

// --- AI panel split drag ---
function startAISplit(e: MouseEvent) {
  e.preventDefault()
  const overlay = document.createElement('div')
  overlay.style.cssText = 'position:fixed;inset:0;z-index:9999;cursor:col-resize;'
  document.body.appendChild(overlay)
  isDraggingSplit.value = true
  const handle = (e.target as HTMLElement).closest('[data-split-handle]') as HTMLElement
  const container = handle?.parentElement
  if (!container) {
    overlay.remove()
    return
  }
  const onMove = (ev: MouseEvent) => {
    ev.preventDefault()
    const rect = container.getBoundingClientRect()
    const pct = ((ev.clientX - rect.left) / rect.width) * 100
    aiSplit.value = Math.min(80, Math.max(30, pct))
  }
  const onUp = () => {
    isDraggingSplit.value = false
    overlay.remove()
    document.removeEventListener('mousemove', onMove)
    document.removeEventListener('mouseup', onUp)
  }
  document.addEventListener('mousemove', onMove)
  document.addEventListener('mouseup', onUp)
}

function applyAICode(newCode: string) {
  code.value = newCode
}

function backToList() {
  router.push({ name: 'functions' })
}

function expandAll() {
  for (const k of Object.keys(sectionOpen.value)) sectionOpen.value[k] = true
}
function collapseAll() {
  for (const k of Object.keys(sectionOpen.value)) sectionOpen.value[k] = false
  nextTick(() => { sectionOpen.value.code = true })
}

// "Kipper knows" context for the AI panel — names only, never values.
const aiContext = computed(() => ({
  bindings: bindings.value.map((b) => ({ service: b.service, type: b.type, env: b.injected_env })),
  envKeys: env.value.map((r) => r.key).filter(Boolean),
  secretKeys: [...secretKeys.value.map((s) => s.key), ...secretsToSet.value.map((r) => r.key).filter(Boolean)],
  // For chat: full deps map keyed by package name. The display block above
  // formats them differently for human reading.
  dependencyMap: computedDependencyMap(),
  dependencies: deps.value.filter((d) => d.name).map((d) => `${d.name}@${d.version || '*'}`),
}))

function computedDependencyMap(): Record<string, string> {
  const out: Record<string, string> = {}
  for (const d of deps.value) {
    if (d.name) out[d.name] = d.version || '*'
  }
  return out
}

// Called from AIChat when the user clicks "Add <pkg>" on an AI suggestion.
// We append to the dependencies list so the user can edit the version.
function addDependencyFromAI(pkg: string) {
  if (!pkg) return
  if (deps.value.some((d) => d.name === pkg)) {
    toast.info(`${pkg} is already in dependencies`)
    return
  }
  deps.value.push({ name: pkg, version: '*' })
  sectionOpen.value.deps = true
  toast.success(`Added ${pkg}. Set a version then Save & deploy`)
}
</script>

<template>
  <div class="flex flex-col h-full">
    <!-- Sticky header -->
    <div class="sticky top-0 z-10 flex flex-wrap items-center justify-between gap-4 px-6 py-3 bg-white dark:bg-slate-900 border-b border-slate-200 dark:border-slate-800">
      <div class="flex items-center gap-3 min-w-0">
        <button
          class="p-2 -ml-2 rounded hover:bg-slate-100 dark:hover:bg-slate-800"
          aria-label="Back to functions"
          @click="backToList"
        >
          <ArrowLeft class="w-4 h-4" />
        </button>
        <div class="min-w-0">
          <div class="flex items-center gap-2">
            <input
              v-if="!isEdit"
              v-model="name"
              type="text"
              placeholder="function-name"
              class="text-lg font-semibold bg-transparent border-b border-slate-300 dark:border-slate-700 focus:border-kipper-500 focus:outline-none px-1"
            />
            <h1 v-else class="text-lg font-semibold truncate">{{ name }}</h1>
            <span v-if="fnInfo" class="inline-flex items-center gap-1 text-xs px-2 py-0.5 rounded-full bg-slate-100 dark:bg-slate-800 text-slate-700 dark:text-slate-300">
              {{ fnInfo.trigger }}
            </span>
            <span v-if="fnInfo?.status" class="inline-flex items-center gap-1 text-xs text-slate-500">
              {{ fnInfo.status }}
            </span>
          </div>
          <p v-if="fnInfo?.url" class="text-xs text-slate-500 truncate">{{ fnInfo.url }}</p>
        </div>
      </div>
      <div class="flex items-center gap-2">
        <button
          class="text-xs text-slate-500 hover:text-slate-700 dark:hover:text-slate-300"
          @click="expandAll"
        >Expand all</button>
        <button
          class="text-xs text-slate-500 hover:text-slate-700 dark:hover:text-slate-300"
          @click="collapseAll"
        >Collapse all</button>
        <button
          class="inline-flex items-center gap-2 px-4 py-2 rounded bg-kipper-600 text-white hover:bg-kipper-700 disabled:opacity-50"
          :disabled="saving"
          @click="save"
        >
          <Loader2 v-if="saving" class="w-4 h-4 animate-spin" />
          <Save v-else class="w-4 h-4" />
          {{ isEdit ? 'Save & deploy' : 'Create' }}
        </button>
      </div>
    </div>

    <div v-if="loading" class="flex items-center justify-center p-12 text-slate-500">
      <Loader2 class="w-5 h-5 animate-spin mr-2" /> Loading…
    </div>

    <div v-else class="flex-1 overflow-y-auto px-6 py-4 space-y-4">
      <!-- Section: Code -->
      <details :open="sectionOpen.code" class="group rounded-lg border border-slate-200 dark:border-slate-800 bg-white dark:bg-slate-900" @toggle="sectionOpen.code = ($event.target as HTMLDetailsElement).open">
        <summary class="flex items-center justify-between gap-3 px-4 py-3 cursor-pointer list-none">
          <span class="flex items-center gap-2 font-medium">
            <CodeIcon class="w-4 h-4 text-slate-500" />
            Code
            <select v-model="runtime" class="ml-2 text-xs border border-slate-300 dark:border-slate-700 rounded px-2 py-0.5 bg-transparent" @click.stop>
              <option value="node">Node 22</option>
              <option value="python">Python 3.12</option>
            </select>
          </span>
          <button
            class="text-xs inline-flex items-center gap-1 px-2 py-1 rounded hover:bg-slate-100 dark:hover:bg-slate-800"
            @click.stop.prevent="showAI = !showAI"
          >
            <Wand2 class="w-3.5 h-3.5" /> {{ showAI ? 'Hide AI' : 'AI Assistant' }}
          </button>
        </summary>
        <div class="border-t border-slate-200 dark:border-slate-800">
          <div class="relative flex" style="height: 480px;">
            <div :style="{ width: showAI ? `${aiSplit}%` : '100%' }" class="h-full">
              <Codemirror
                v-model="code"
                :extensions="codeExtensions"
                :tab-size="2"
                class="h-full"
              />
            </div>
            <div
              v-if="showAI"
              data-split-handle
              class="w-1 cursor-col-resize bg-slate-200 dark:bg-slate-800 hover:bg-kipper-500"
              @mousedown="startAISplit"
            ></div>
            <div v-if="showAI" :style="{ width: `${100 - aiSplit}%` }" class="h-full flex flex-col border-l border-slate-200 dark:border-slate-800">
              <div class="px-3 py-2 text-xs text-slate-500 border-b border-slate-200 dark:border-slate-800">
                <p class="font-medium text-slate-700 dark:text-slate-300 mb-1">Kipper knows</p>
                <ul class="space-y-0.5">
                  <li>Runtime: {{ runtime === 'node' ? 'Node 22' : 'Python 3.12' }}</li>
                  <li v-for="b in aiContext.bindings" :key="b.service">
                    Bound: {{ b.service }} ({{ b.type }}) → {{ b.env.join(', ') }}
                  </li>
                  <li v-if="aiContext.envKeys.length">Env: {{ aiContext.envKeys.join(', ') }}</li>
                  <li v-if="aiContext.secretKeys.length">Secrets: {{ aiContext.secretKeys.join(', ') }}</li>
                  <li v-if="aiContext.dependencies.length">Deps: {{ aiContext.dependencies.join(', ') }}</li>
                </ul>
              </div>
              <div class="flex-1 min-h-0">
                <AIChat
                  :code="code"
                  :runtime="runtime"
                  :bindings="aiContext.bindings"
                  :env-keys="aiContext.envKeys"
                  :secret-keys="aiContext.secretKeys"
                  :dependencies="aiContext.dependencyMap"
                  @apply="applyAICode"
                  @add-dependency="addDependencyFromAI"
                />
              </div>
            </div>
          </div>
        </div>
      </details>

      <!-- Section: Trigger -->
      <details :open="sectionOpen.trigger" class="rounded-lg border border-slate-200 dark:border-slate-800 bg-white dark:bg-slate-900" @toggle="sectionOpen.trigger = ($event.target as HTMLDetailsElement).open">
        <summary class="flex items-center justify-between px-4 py-3 cursor-pointer list-none">
          <span class="font-medium">Trigger</span>
          <span class="text-xs text-slate-500">{{ trigger === 'cron' ? schedule || 'cron' : trigger }}</span>
        </summary>
        <div class="px-4 py-3 border-t border-slate-200 dark:border-slate-800 space-y-3">
          <div class="flex flex-wrap gap-2">
            <label v-for="t in (['http', 'cron', 'postgres', 'mysql', 'redis', 'minio'] as const)" :key="t"
              class="inline-flex items-center gap-2 px-3 py-1.5 rounded border cursor-pointer text-sm"
              :class="trigger === t ? 'border-kipper-500 bg-kipper-50 dark:bg-kipper-900/20 text-kipper-700 dark:text-kipper-300' : 'border-slate-300 dark:border-slate-700 hover:bg-slate-50 dark:hover:bg-slate-800'"
            >
              <input type="radio" v-model="trigger" :value="t" class="sr-only" />
              {{ t.charAt(0).toUpperCase() + t.slice(1) }}
            </label>
          </div>

          <div v-if="trigger === 'cron'" class="space-y-2">
            <div class="flex flex-wrap items-center gap-2">
              <input
                v-model="schedule"
                type="text"
                placeholder="0 2 * * *"
                class="flex-1 px-3 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent font-mono text-sm"
              />
              <select
                class="text-sm border border-slate-300 dark:border-slate-700 rounded px-2 py-1.5 bg-transparent"
                @change="applySchedulePreset($event)"
              >
                <option value="">Common schedules…</option>
                <option v-for="p in cronPresets" :key="p.expr" :value="p.expr">{{ p.label }}</option>
              </select>
            </div>
            <p v-if="cronInfo.text" class="text-sm text-slate-600 dark:text-slate-400">{{ cronInfo.text }}</p>
            <div v-if="nextRuns.length" class="text-xs text-slate-500">
              <span class="font-medium">Next runs:</span>
              <ul class="ml-4 list-disc">
                <li v-for="iso in nextRuns" :key="iso">{{ new Date(iso).toUTCString() }}</li>
              </ul>
            </div>

            <!-- Test run button. Only available once the function is
                 deployed; pre-deploy there's no image to run yet. -->
            <div v-if="isEdit" class="flex items-center gap-3 pt-1">
              <button
                type="button"
                @click="runTest"
                :disabled="testRunning"
                class="inline-flex items-center gap-1.5 rounded-md border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-800 px-3 py-1.5 text-xs font-medium text-slate-700 dark:text-slate-200 transition-colors hover:bg-slate-50 dark:hover:bg-slate-700 disabled:opacity-60"
                :title="`Run the function once now with KIPPER_TRIGGER=test, using the deployed image. The cron schedule is unchanged.`"
              >
                <Loader2 v-if="testRunning" class="h-3.5 w-3.5 animate-spin" />
                <Play v-else class="h-3.5 w-3.5" />
                {{ testRunning ? 'Starting…' : 'Test run' }}
              </button>
              <span v-if="lastTestJob" class="text-xs text-slate-500">
                Last: <span class="font-mono">{{ lastTestJob }}</span>
              </span>
            </div>
          </div>

          <div v-if="['postgres', 'mysql'].includes(trigger)" class="space-y-2">
            <div>
              <label class="text-xs text-slate-500 block mb-1">Source service</label>
              <select v-model="eventSource" class="w-full px-3 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent">
                <option value="">Select…</option>
                <option v-for="s in eventTriggerCapableServices" :key="s.name" :value="s.name">{{ s.name }} ({{ s.type }})</option>
              </select>
            </div>
            <div>
              <label class="text-xs text-slate-500 block mb-1">Query (SELECT…)</label>
              <textarea v-model="eventQuery" rows="2" class="w-full px-3 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent font-mono text-xs"></textarea>
            </div>
            <div>
              <label class="text-xs text-slate-500 block mb-1">Mark-done query (optional)</label>
              <textarea v-model="eventMarkDone" rows="2" class="w-full px-3 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent font-mono text-xs"></textarea>
            </div>
          </div>

          <div v-if="trigger === 'redis'" class="space-y-2">
            <div>
              <label class="text-xs text-slate-500 block mb-1">Source service</label>
              <select v-model="eventSource" class="w-full px-3 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent">
                <option value="">Select…</option>
                <option v-for="s in eventTriggerCapableServices.filter(x => x.type === 'redis')" :key="s.name" :value="s.name">{{ s.name }}</option>
              </select>
            </div>
            <div>
              <label class="text-xs text-slate-500 block mb-1">List name</label>
              <input v-model="eventList" type="text" class="w-full px-3 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent" />
            </div>
          </div>

          <div v-if="trigger === 'minio'" class="space-y-2">
            <div>
              <label class="text-xs text-slate-500 block mb-1">Source service</label>
              <select v-model="eventSource" class="w-full px-3 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent">
                <option value="">Select…</option>
                <option v-for="s in eventTriggerCapableServices.filter(x => x.type === 'minio')" :key="s.name" :value="s.name">{{ s.name }}</option>
              </select>
            </div>
            <div>
              <label class="text-xs text-slate-500 block mb-1">Bucket</label>
              <input v-model="eventBucket" type="text" class="w-full px-3 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent" />
            </div>
          </div>
        </div>
      </details>

      <!-- Section: Bindings -->
      <details :open="sectionOpen.bindings" class="rounded-lg border border-slate-200 dark:border-slate-800 bg-white dark:bg-slate-900" @toggle="sectionOpen.bindings = ($event.target as HTMLDetailsElement).open">
        <summary class="flex items-center justify-between px-4 py-3 cursor-pointer list-none">
          <span class="font-medium">Service bindings</span>
          <span class="text-xs text-slate-500">{{ bindings.length }} bound</span>
        </summary>
        <div class="px-4 py-3 border-t border-slate-200 dark:border-slate-800 space-y-3">
          <div v-for="b in bindings" :key="b.service" class="rounded border border-slate-200 dark:border-slate-800 p-3">
            <div class="flex items-center justify-between">
              <div>
                <span class="font-medium">{{ b.service }}</span>
                <span class="ml-2 text-xs text-slate-500">{{ b.type }}{{ b.database ? ' · ' + b.database : '' }} · prefix {{ b.prefix }}</span>
              </div>
              <button class="text-xs text-rose-600 hover:underline" @click="removeBinding(b.service)">Unbind</button>
            </div>
            <p class="mt-2 text-xs text-slate-600 dark:text-slate-400">
              Injects: <code class="font-mono">{{ b.injected_env.join('  ') }}</code>
            </p>
          </div>

          <div class="rounded border border-dashed border-slate-300 dark:border-slate-700 p-3 space-y-2">
            <div class="grid grid-cols-1 gap-2 sm:grid-cols-2">
              <select v-model="bindServiceName" class="px-2 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent text-sm">
                <option value="">Pick service…</option>
                <option v-for="s in bindableServices" :key="s.name" :value="s.name">{{ s.name }} ({{ s.type }})</option>
              </select>
              <input v-model="bindPrefix" placeholder="Prefix (e.g. DB_)" class="px-2 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent text-sm font-mono" />
            </div>

            <!-- Namespace picker — meaningful for postgres/mysql
                 (databases) and rabbitmq (vhost). Pick existing
                 (where listing is available) or create new. The
                 service's own default is pre-selected and tagged so
                 the common case is one click. RabbitMQ is
                 create-new-only for now: a vhost-list endpoint
                 ships in a follow-up. -->
            <div v-if="bindPickerSupportsNamespace" class="space-y-2">
              <div class="flex items-center gap-2 text-xs">
                <span class="text-slate-500 capitalize">{{ bindNamespaceLabel }}:</span>
                <label v-if="bindPickerCanListNamespaces" class="inline-flex items-center gap-1 cursor-pointer">
                  <input type="radio" value="existing" v-model="bindDbMode" :disabled="!bindDatabases.length" />
                  Pick existing
                </label>
                <label class="inline-flex items-center gap-1 cursor-pointer">
                  <input type="radio" value="new" v-model="bindDbMode" />
                  Create new
                </label>
                <span v-if="bindDatabasesLoading" class="text-slate-400">loading…</span>
              </div>

              <select
                v-if="bindDbMode === 'existing' && bindPickerCanListNamespaces"
                v-model="bindDatabaseExisting"
                class="w-full px-2 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent text-sm"
              >
                <option value="">Pick a {{ bindNamespaceLabel }}…</option>
                <option v-for="d in bindDatabases" :key="d" :value="d">
                  {{ d }}{{ d === bindDefaultDatabase ? ' (service default)' : '' }}
                </option>
              </select>

              <div v-else-if="bindDbMode === 'new'">
                <input
                  v-model="bindDatabaseNew"
                  :placeholder="bindNamespaceLabel === 'vhost' ? 'new vhost name (e.g. orders)' : 'new database name (e.g. myapp_data)'"
                  class="w-full px-2 py-1.5 rounded border border-amber-400 dark:border-amber-700 bg-transparent text-sm font-mono"
                />
                <p class="mt-1 text-xs text-amber-700 dark:text-amber-400">
                  <span v-if="bindNamespaceLabel === 'vhost'">
                    Creates a new vhost on <span class="font-mono">{{ bindServiceName }}</span> and grants the kipper user full access. To share the default vhost (<span class="font-mono">/</span>), use "Pick existing" instead.
                  </span>
                  <span v-else>
                    This creates a new empty database on <span class="font-mono">{{ bindServiceName }}</span>. Only pick this if you want a fresh isolated DB. To attach to existing data, use "Pick existing".
                  </span>
                </p>
              </div>
            </div>

            <p v-if="previewInjectedFor.length" class="text-xs text-slate-600 dark:text-slate-400">
              Will inject: <code class="font-mono">{{ previewInjectedFor.join('  ') }}</code>
            </p>
            <button
              :disabled="!bindServiceName"
              class="text-sm inline-flex items-center gap-1 px-3 py-1.5 rounded border border-slate-300 dark:border-slate-700 hover:bg-slate-50 dark:hover:bg-slate-800 disabled:opacity-50"
              @click="applyBinding"
            >
              <Plus class="w-4 h-4" /> Bind
            </button>
          </div>
        </div>
      </details>

      <!-- Section: Environment variables -->
      <details :open="sectionOpen.env" class="rounded-lg border border-slate-200 dark:border-slate-800 bg-white dark:bg-slate-900" @toggle="sectionOpen.env = ($event.target as HTMLDetailsElement).open">
        <summary class="flex items-center justify-between px-4 py-3 cursor-pointer list-none">
          <span class="font-medium">Environment variables</span>
          <span class="text-xs text-slate-500">{{ env.length }}</span>
        </summary>
        <div class="px-4 py-3 border-t border-slate-200 dark:border-slate-800 space-y-2">
          <div v-for="(row, idx) in env" :key="idx" class="grid grid-cols-[1fr_2fr_auto] gap-2">
            <input v-model="row.key" placeholder="KEY" class="px-2 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent text-sm font-mono" />
            <input v-model="row.value" placeholder="value" class="px-2 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent text-sm" />
            <button class="p-1.5 text-rose-600 hover:bg-rose-50 dark:hover:bg-rose-950 rounded" @click="removeEnvRow(idx)" aria-label="Remove">
              <Trash2 class="w-4 h-4" />
            </button>
          </div>
          <button class="text-sm inline-flex items-center gap-1 px-3 py-1.5 rounded border border-slate-300 dark:border-slate-700 hover:bg-slate-50 dark:hover:bg-slate-800" @click="addEnvRow">
            <Plus class="w-4 h-4" /> Add variable
          </button>
        </div>
      </details>

      <!-- Section: Secrets -->
      <details :open="sectionOpen.secrets" class="rounded-lg border border-slate-200 dark:border-slate-800 bg-white dark:bg-slate-900" @toggle="sectionOpen.secrets = ($event.target as HTMLDetailsElement).open">
        <summary class="flex items-center justify-between px-4 py-3 cursor-pointer list-none">
          <span class="font-medium">Secrets</span>
          <span class="text-xs text-slate-500">{{ secretKeys.length + secretsToSet.length }}</span>
        </summary>
        <div class="px-4 py-3 border-t border-slate-200 dark:border-slate-800 space-y-2">
          <div v-for="s in secretKeys" :key="s.key" class="grid grid-cols-[1fr_2fr_auto] gap-2 items-center text-sm">
            <code class="font-mono">{{ s.key }}</code>
            <span class="text-slate-400">••••••••••••</span>
            <button class="p-1.5 text-rose-600 hover:bg-rose-50 dark:hover:bg-rose-950 rounded" @click="deleteExistingSecret(s.key)" aria-label="Delete">
              <Trash2 class="w-4 h-4" />
            </button>
          </div>
          <div v-for="(row, idx) in secretsToSet" :key="'new-' + idx" class="grid grid-cols-[1fr_2fr_auto] gap-2">
            <input v-model="row.key" placeholder="KEY" class="px-2 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent text-sm font-mono" />
            <div class="relative">
              <input
                v-model="row.value"
                :type="showSecretValueIdx === idx ? 'text' : 'password'"
                placeholder="value"
                class="w-full pr-8 px-2 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent text-sm font-mono"
              />
              <button
                class="absolute inset-y-0 right-1 text-slate-500 hover:text-slate-700 dark:hover:text-slate-300"
                @click="showSecretValueIdx = showSecretValueIdx === idx ? null : idx"
                :aria-label="showSecretValueIdx === idx ? 'Hide' : 'Show'"
              >
                <EyeOff v-if="showSecretValueIdx === idx" class="w-4 h-4" />
                <Eye v-else class="w-4 h-4" />
              </button>
            </div>
            <button class="p-1.5 text-rose-600 hover:bg-rose-50 dark:hover:bg-rose-950 rounded" @click="removeNewSecretRow(idx)" aria-label="Remove">
              <Trash2 class="w-4 h-4" />
            </button>
          </div>
          <button class="text-sm inline-flex items-center gap-1 px-3 py-1.5 rounded border border-slate-300 dark:border-slate-700 hover:bg-slate-50 dark:hover:bg-slate-800" @click="addSecretRow">
            <Plus class="w-4 h-4" /> Add secret
          </button>
          <p class="text-xs text-slate-500">Secret values are write-only. They never round-trip through this UI.</p>
        </div>
      </details>

      <!-- Section: Dependencies -->
      <details :open="sectionOpen.deps" class="rounded-lg border border-slate-200 dark:border-slate-800 bg-white dark:bg-slate-900" @toggle="sectionOpen.deps = ($event.target as HTMLDetailsElement).open">
        <summary class="flex items-center justify-between px-4 py-3 cursor-pointer list-none">
          <span class="font-medium">Dependencies</span>
          <span class="text-xs text-slate-500">{{ deps.length }}</span>
        </summary>
        <div class="px-4 py-3 border-t border-slate-200 dark:border-slate-800 space-y-2">
          <!-- Sibling-package conflict warnings — e.g. psycopg2 +
               psycopg2-binary in the same list. Pip happily installs
               both and the source build is the one that fails. -->
          <NoticeCallout
            v-for="conflict in depConflicts"
            :key="conflict.drop"
            tone="warning"
            class="flex items-start justify-between gap-3 px-3 py-2 text-xs text-amber-900 dark:text-slate-400"
          >
            <div>
              <span class="font-mono font-semibold dark:text-slate-300">{{ conflict.keep }}</span>
              and
              <span class="font-mono font-semibold dark:text-slate-300">{{ conflict.drop }}</span>
              install the same module. Keep one. The
              <span class="font-mono dark:text-slate-300">{{ conflict.keep }}</span> variant is the safer pick on the slim runtime image.
            </div>
            <button
              class="shrink-0 inline-flex items-center gap-1 rounded border border-amber-400 bg-white px-2 py-1 text-[11px] font-medium text-amber-700 hover:bg-amber-100 dark:bg-amber-950 dark:text-amber-200"
              @click="applyConflictFix(conflict.drop)"
            >Remove {{ conflict.drop }}</button>
          </NoticeCallout>

          <div v-for="(row, idx) in deps" :key="idx" class="grid grid-cols-[2fr_1fr_auto] gap-2">
            <input v-model="row.name" placeholder="package name" class="px-2 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent text-sm font-mono" />
            <input v-model="row.version" placeholder="version (e.g. 8.11.5)" class="px-2 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent text-sm font-mono" />
            <button class="p-1.5 text-rose-600 hover:bg-rose-50 dark:hover:bg-rose-950 rounded" @click="removeDepRow(idx)" aria-label="Remove">
              <Trash2 class="w-4 h-4" />
            </button>
          </div>
          <div class="flex items-center gap-2">
            <button class="text-sm inline-flex items-center gap-1 px-3 py-1.5 rounded border border-slate-300 dark:border-slate-700 hover:bg-slate-50 dark:hover:bg-slate-800" @click="addDepRow">
              <Plus class="w-4 h-4" /> Add dependency
            </button>
            <button class="text-sm inline-flex items-center gap-1 px-3 py-1.5 rounded border border-slate-300 dark:border-slate-700 hover:bg-slate-50 dark:hover:bg-slate-800" @click="scanCodeForImports">
              <ScanLine class="w-4 h-4" /> Scan code for imports
            </button>
            <button
              v-if="isEdit"
              class="text-sm inline-flex items-center gap-1 px-3 py-1.5 rounded border border-slate-300 dark:border-slate-700 hover:bg-slate-50 dark:hover:bg-slate-800"
              @click="reloadDeps"
              title="Re-fetch dependencies from the server. Useful if the CR was changed externally (kubectl, AI suggestion)."
            >
              <RefreshCw class="w-4 h-4" /> Reload
            </button>
          </div>
        </div>
      </details>

      <!-- Section: Resources (collapsed by default) -->
      <details :open="sectionOpen.resources" class="rounded-lg border border-slate-200 dark:border-slate-800 bg-white dark:bg-slate-900" @toggle="sectionOpen.resources = ($event.target as HTMLDetailsElement).open">
        <summary class="flex items-center justify-between px-4 py-3 cursor-pointer list-none">
          <span class="font-medium">Resources</span>
          <span class="text-xs text-slate-500">{{ memoryLimit || '—' }} / {{ cpuLimit || '—' }}</span>
        </summary>
        <div class="px-4 py-3 border-t border-slate-200 dark:border-slate-800 space-y-3">
          <div class="flex items-center justify-end">
            <button
              type="button"
              class="text-xs font-medium text-kipper-600 hover:text-kipper-700 dark:text-kipper-400 dark:hover:text-kipper-300"
              @click="resourcesAdvanced = !resourcesAdvanced"
            >{{ resourcesAdvanced ? 'Simple' : 'Advanced (request &amp; limit)' }}</button>
          </div>

          <div v-if="!resourcesAdvanced" class="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <div>
              <label class="text-xs text-slate-500 block mb-1">Memory</label>
              <input v-model="memoryLimit" placeholder="64Mi" class="w-full px-2 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent text-sm" />
              <p v-if="memoryRequest && memoryRequest !== memoryLimit" class="text-xs text-amber-600 dark:text-amber-400 mt-1">
                Saved request: {{ memoryRequest }} (overridden in advanced mode)
              </p>
            </div>
            <div>
              <label class="text-xs text-slate-500 block mb-1">CPU</label>
              <input v-model="cpuLimit" placeholder="50m" class="w-full px-2 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent text-sm" />
              <p v-if="cpuRequest && cpuRequest !== cpuLimit" class="text-xs text-amber-600 dark:text-amber-400 mt-1">
                Saved request: {{ cpuRequest }} (overridden in advanced mode)
              </p>
            </div>
          </div>

          <div v-else class="space-y-3">
            <p class="text-xs text-slate-500 dark:text-slate-400">
              Request is reserved on the node. Limit is the cap. Set request lower than limit for burstable workloads, useful when a function spikes during cold start (e.g. JVM JIT or a Python ML model loading) but idles afterwards.
            </p>
            <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div>
                <label class="text-xs text-slate-500 block mb-1">CPU request</label>
                <input v-model="cpuRequest" placeholder="50m" class="w-full px-2 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent text-sm" />
              </div>
              <div>
                <label class="text-xs text-slate-500 block mb-1">CPU limit</label>
                <input v-model="cpuLimit" placeholder="500m" class="w-full px-2 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent text-sm" />
              </div>
              <div>
                <label class="text-xs text-slate-500 block mb-1">Memory request</label>
                <input v-model="memoryRequest" placeholder="256Mi" class="w-full px-2 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent text-sm" />
              </div>
              <div>
                <label class="text-xs text-slate-500 block mb-1">Memory limit</label>
                <input v-model="memoryLimit" placeholder="1Gi" class="w-full px-2 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent text-sm" />
              </div>
            </div>
          </div>
        </div>
      </details>

      <!-- Section: Logs (edit-mode only; auto-loads on first open) -->
      <details
        v-if="isEdit"
        :open="sectionOpen.logs"
        class="rounded-lg border border-slate-200 dark:border-slate-800 bg-white dark:bg-slate-900"
        @toggle="onLogsToggle(($event.target as HTMLDetailsElement).open)"
      >
        <summary class="flex items-center justify-between px-4 py-3 cursor-pointer list-none">
          <span class="font-medium">Logs</span>
          <span class="text-xs text-slate-500">{{ logs.length }} {{ logs.length === 1 ? 'line' : 'lines' }}</span>
        </summary>
        <div class="px-4 py-3 border-t border-slate-200 dark:border-slate-800 space-y-3">
          <div class="flex items-center gap-2">
            <input
              v-model="logSearch"
              placeholder="filter logs (e.g. error)"
              class="flex-1 px-3 py-1.5 rounded border border-slate-300 dark:border-slate-700 bg-transparent text-sm"
              @keyup.enter="loadLogs"
            />
            <select v-model="logSince" class="text-sm border border-slate-300 dark:border-slate-700 rounded px-2 py-1.5 bg-transparent">
              <option value="5m">last 5 min</option>
              <option value="15m">last 15 min</option>
              <option value="1h">last hour</option>
              <option value="6h">last 6 hours</option>
              <option value="24h">last 24 hours</option>
              <option value="7d">last 7 days</option>
            </select>
            <button
              class="text-sm inline-flex items-center gap-1 px-3 py-1.5 rounded border border-slate-300 dark:border-slate-700 hover:bg-slate-50 dark:hover:bg-slate-800 disabled:opacity-50"
              :disabled="logsLoading"
              @click="loadLogs"
            >
              <Loader2 v-if="logsLoading" class="w-4 h-4 animate-spin" />
              <RefreshCw v-else class="w-4 h-4" />
              Refresh
            </button>
            <button
              class="text-sm inline-flex items-center gap-1 px-3 py-1.5 rounded border border-amber-400/40 text-amber-600 dark:text-amber-400 hover:bg-amber-50 dark:hover:bg-amber-950/40"
              @click="openDiagnose"
              title="Ask the AI to look at pod status, events, and logs and tell you what's wrong"
            >
              <Sparkles class="w-4 h-4" />
              AI diagnose
            </button>
          </div>

          <div v-if="logsLoading && !logs.length" class="py-8 text-center text-sm text-slate-500">Loading…</div>
          <div v-else-if="!logs.length && logsLoaded" class="py-8 text-center text-sm text-slate-500">
            No logs in the selected time range.
            <p class="mt-1 text-xs">Cron functions only emit logs while a run is in flight.</p>
          </div>
          <div
            v-else-if="logs.length"
            class="rounded bg-slate-950 text-slate-100 font-mono text-xs leading-relaxed overflow-auto"
            style="max-height: 480px;"
          >
            <div
              v-for="(entry, idx) in logs"
              :key="idx"
              class="px-3 py-0.5 hover:bg-slate-900 grid grid-cols-[auto_auto_1fr] gap-3 items-baseline"
            >
              <span class="text-slate-500 select-none">{{ formatLogTime(entry.timestamp) }}</span>
              <span class="text-slate-400 select-none truncate" :title="entry.pod">{{ entry.pod.slice(0, 12) }}</span>
              <span class="whitespace-pre-wrap break-words">{{ entry.line }}</span>
            </div>
          </div>
        </div>
      </details>
    </div>
  </div>
</template>
