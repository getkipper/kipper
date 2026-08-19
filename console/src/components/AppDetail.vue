<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted, watch, nextTick, type ComponentPublicInstance } from 'vue'
import { Eye, EyeOff, Plus, Minus, Trash2, Terminal, RotateCw, Pencil, Save, AlertTriangle, Package, Undo2, Shield, X, Sparkles, Link, GitBranch, File, Folder, Download, ChevronRight, ChevronDown, Upload, Copy, Check, Globe, CheckCircle2, RefreshCw } from 'lucide-vue-next'
import SidePanel from '@/components/SidePanel.vue'
import SaveButton from '@/components/SaveButton.vue'
import LogAnalysis from '@/components/LogAnalysis.vue'
import DiagnoseModal from '@/components/DiagnoseModal.vue'
import ContainerErrorsModal from '@/components/ContainerErrorsModal.vue'
import ContainerFailureEntry from '@/components/ContainerFailureEntry.vue'
import OptimiseModal from '@/components/OptimiseModal.vue'
import FileViewerModal from '@/components/FileViewerModal.vue'
import WebTerminal from '@/components/WebTerminal.vue'
import ResourceControl from '@/components/ResourceControl.vue'
import MetricSparkline from '@/components/MetricSparkline.vue'
import TabBar, { type Tab } from '@/components/TabBar.vue'
import RevealDialog from '@/components/RevealDialog.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import NoticeCallout from '@/components/NoticeCallout.vue'
import { useModal } from '@/composables/useModal'
import { useProjectsStore } from '@/stores/projects'
import { useLogStream } from '@/composables/useLogStream'
import { useToast } from '@/composables/useToast'
import { useResourceUsage } from '@/composables/useResourceUsage'
import {
  parseCpuQuantity,
  parseMemoryQuantity,
  toKubernetesCpuQuantity,
  toKubernetesMemoryQuantity,
} from '@/utils/resources'
import * as api from '@/api/apps'
import type { AppLink } from '@/api/apps'
import { gitCardState as deriveGitCardState, imageCardState as deriveImageCardState } from '@/utils/deployMethods'
import { formatDateTime } from '@/utils/datetime'
import { isSensitiveEnvVar } from '@/utils/sensitiveEnv'
import EnvVariableValue from './EnvVariableValue.vue'
import { claimantsInNamespace, roleInNamespace } from '@/utils/projectRole'
import EnvAvailableVariables from './EnvAvailableVariables.vue'
import type { EnvPreview, EnvPreviewSnippet, EnvPreviewVariable } from '@/api/apps'
import * as filesApi from '@/api/files'
import { fetchServices, fetchServiceInfo, fetchRabbitMQVhosts, bindService, unbindService, type ServiceStatus } from '@/api/services'
import { fetchDBSchema } from '@/api/database'
import { getMode } from '@/api/mode'
import { Info } from 'lucide-vue-next'

const toast = useToast()

const isAutoMode = ref(false)

async function loadMode() {
  try {
    const resp = await getMode()
    isAutoMode.value = resp.mode === 'auto'
  } catch {
    isAutoMode.value = false
  }
}

const props = defineProps<{
  appName: string
  namespace?: string
}>()

const emit = defineEmits<{
  close: []
}>()

const projects = useProjectsStore()
const project = computed(() => props.namespace || projects.currentProject || 'default')

import { useAuthStore } from '@/stores/auth'
const authStore = useAuthStore()

type TabKey = 'logs' | 'deploys' | 'scale' | 'resources' | 'env' | 'files' | 'connect' | 'secrets' | 'settings'

const activeTab = ref<TabKey>('logs')

const TAB_LABELS: Record<TabKey, string> = {
  logs: 'Logs',
  deploys: 'Deploys',
  scale: 'Scale',
  resources: 'Resources',
  env: 'Env',
  files: 'Files',
  connect: 'Connect',
  secrets: 'Secrets',
  settings: 'Settings',
}

// Reading the Env tab and writing to it are different permissions, because the
// API treats them as different: a namespace-scoped read needs membership of the
// project owning the namespace, a mutation needs the deployer role there.
// A cluster deployer is not a deployer of every project. ProjectAccessResolver
// hands the cluster role an override to an admin alone, whom it resolves as
// owner, and evaluates project membership for everyone else. So the project
// role is the whole answer here, and where it is unknown — an unclaimed or
// contested namespace, or a store that has not loaded — there is no role to act
// on and nothing is offered. Falling back to the cluster role there showed a
// cluster deployer every control for a moment on someone else's project, each
// of which answers 403.
const namespaceRole = computed(() => roleInNamespace(projects.projects, project.value))
const canReadEnv = computed(() => authStore.isAdmin || namespaceRole.value !== null)
const canWriteEnv = computed(() =>
  authStore.isAdmin || namespaceRole.value === 'owner' || namespaceRole.value === 'deployer',
)

/**
 * Drops everything the Env tab read for one app.
 *
 * A load guard stops a late response publishing; it does not remove what already
 * published. Whatever is on screen was read under one app and one role, and both
 * can change under a panel that is reused rather than remounted.
 */
function clearEnvState() {
  // Retire every read already in flight. Clearing what is on screen achieves
  // nothing if a read issued under the old role publishes its answer a moment
  // later, and each loader holds its own generation, so this epoch is the one
  // thing that retires all of them at once.
  //
  // Reads only. A write in flight is still the write in flight, and the queue
  // behind it is still the thing that keeps two whole-map replacements from
  // overlapping. A momentary loss of the role — an ownership lookup that failed
  // and came back — must not release the next writer to run alongside it.
  // Changing app is different, and resetEnvWriteTracking covers that.
  readEpoch++
  envVars.value = {}
  envMapStale.value = true
  envPreview.value = null
  envConflicts.value = []
  injectedVars.value = []
  links.value = []
  linkableApps.value = []
  envRestartPending.value = false
  editingEnv.value = null
  editEnvValue.value = ''
}

// An editor opened while the role allowed it must not survive losing it. The
// panel is reused across apps, and a role can also change under a panel that
// never moves, which no prop watcher would see.
watch(canWriteEnv, (mayWrite) => {
  if (mayWrite) return
  editingEnv.value = null
  editEnvValue.value = ''
})

// Losing read access is not the same as being shown no tab to open. The panel is
// gated on activeTab alone, so a membership removed while it is open leaves
// every value this app has on screen for as long as it stays open.
//
// It closes rather than moving to another tab. Someone whose project role has
// gone has no more claim on Logs or Deploys than on Env, and picking one of them
// would swap a tab this cannot serve for a tab it also cannot serve, still
// showing what the previous role loaded.
//
// There is no cluster-role exemption here because canReadEnv already carries the
// only one the server honours. ProjectAccessResolver gives project-independent
// access to an admin and to nobody else, so a cluster deployer whose membership
// is removed loses this namespace exactly as a viewer does.
//
// Closing needs more than a false, though. A namespace no loaded project claims
// is a membership that has gone; a namespace two of them claim is a response
// that could not say who owns it, which happens when the server's ownership
// lookup fails and every declaration keeps its optimistic owned flag. Shutting
// the panel on that would throw an entitled caller out over a failed list, so
// the state goes either way and the panel closes only on the first.
watch(canReadEnv, (mayRead) => {
  if (mayRead) {
    // A contested refresh empties the tab; the next one that settles ownership
    // has to fill it again, or it stays blank until something else reloads it.
    if (activeTab.value === 'env') {
      loadEnv()
      loadLinks()
      loadAvailableServices()
    }
    return
  }
  clearEnvState()
  if (claimantsInNamespace(projects.projects, project.value) === 0) {
    emit('close')
  }
})

const visibleTabs = computed<Tab[]>(() => {
  const keys: TabKey[] = ['logs']
  if (authStore.isDeployer) {
    keys.push('deploys', 'scale', 'resources')
  }
  // Env answers to its own rule wherever it appears. The env routes gate on the
  // caller's role in the project owning this namespace rather than on the
  // cluster-wide role, so a project member is authorised by the API and was
  // being shown no tab to use it from — while a cluster deployer with no role
  // here was shown one whose reads the API refuses. A viewer gets it read-only;
  // every control that writes is behind canWriteEnv.
  //
  // Only Env. The other seven are gated by their own handlers and answering for
  // them here would be guessing at rules this does not know.
  if (canReadEnv.value) {
    keys.push('env')
  }
  if (authStore.isDeployer) {
    keys.push('files', 'connect', 'secrets', 'settings')
  }
  return keys.map((k) => ({ key: k, label: TAB_LABELS[k] }))
})

function setActiveTab(key: string) {
  activeTab.value = key as TabKey
}

// Service binding
const availableServices = ref<ServiceStatus[]>([])
// Restricted to the app's own namespace. Two services in different
// namespaces can share a name (one `db` per project), and the native
// <select> matches v-model by value alone — so duplicate names would
// visually flip the selection between matching options. The bind API
// hints with the app namespace first anyway, so cross-namespace
// binding was never reliable from this dialog.
const bindableServices = computed(() =>
  availableServices.value.filter(s => s.namespace === project.value),
)
const bindingService = ref('')
const bindingPrefix = ref('')
// Two-mode database picker. Pick existing (the default) or Create
// new. The service's own default DB is pre-selected in the dropdown
// and tagged "(service default)" so the common case is one click.
const bindingDbMode = ref<'existing' | 'new'>('existing')
const bindingDatabaseExisting = ref('')
const bindingDatabaseNew = ref('')
const bindingDatabases = ref<string[]>([])
const bindingDatabasesLoading = ref(false)
const bindingDefaultDatabase = ref('')
const binding = ref(false)

// Service types that carve out a per-binding logical namespace inside
// the service: postgres/mysql get a database, rabbitmq gets a vhost.
function hasNamespacePicker(type_: string): boolean {
  return ['postgres', 'mysql', 'rabbitmq'].includes(type_)
}

// Whether the service type can enumerate existing namespaces in the
// bind form. Postgres/mysql go through fetchDBSchema; rabbitmq goes
// through fetchRabbitMQVhosts (rabbitmqctl list_vhosts in the pod).
function canListNamespaces(type_: string): boolean {
  return ['postgres', 'mysql', 'rabbitmq'].includes(type_)
}

function namespaceLabel(type_: string): string {
  return type_ === 'rabbitmq' ? 'vhost' : 'database'
}

const selectedServiceType = computed(() => {
  const svc = bindableServices.value.find(s => s.name === bindingService.value)
  return svc?.type || ''
})

const selectedServiceNamespace = computed(() => {
  const svc = bindableServices.value.find(s => s.name === bindingService.value)
  return svc?.namespace || ''
})

const resolvedBindingDatabase = computed(() => {
  if (!hasNamespacePicker(selectedServiceType.value)) return ''
  switch (bindingDbMode.value) {
    case 'existing': return bindingDatabaseExisting.value || ''
    case 'new': return bindingDatabaseNew.value.trim()
    default: return ''
  }
})

const beginBindingDatabasesLoad = loadGuard()
async function loadBindingDatabases() {
  // Only postgres/mysql can list existing namespaces today; rabbitmq
  // is create-new-only until a vhost-list endpoint ships.
  if (!bindingService.value || !canListNamespaces(selectedServiceType.value) || !selectedServiceNamespace.value) {
    bindingDatabases.value = []
    bindingDefaultDatabase.value = ''
    // A request for the service just deselected will refuse to clear this, so
    // the selection that invalidated it owns the spinner.
    bindingDatabasesLoading.value = false
    return
  }
  const current = beginBindingDatabasesLoad()
  const requestedService = bindingService.value
  bindingDatabasesLoading.value = true
  // The answer describes one service on one panel, and both can change while it
  // is in flight — offering another service's databases in the form is a bind
  // against the wrong one.
  const stillCurrent = () => current() && requestedService === bindingService.value
  try {
    if (selectedServiceType.value === 'rabbitmq') {
      // rabbitmqctl list_vhosts in the service pod. The server
      // tags "/" with default:true so we can pre-select it the
      // same way postgres tags its default database.
      const vhosts = await fetchRabbitMQVhosts(bindingService.value, selectedServiceNamespace.value)
      if (!stillCurrent()) return
      bindingDatabases.value = vhosts.map(v => v.name).sort()
      const def = vhosts.find(v => v.default)
      bindingDefaultDatabase.value = def?.name || ''
    } else {
      // Postgres/MySQL: schema list + ServiceInfo for the default-DB tag.
      const [schema, info] = await Promise.all([
        fetchDBSchema(bindingService.value, selectedServiceNamespace.value),
        fetchServiceInfo(bindingService.value, selectedServiceNamespace.value).catch(() => null),
      ])
      if (!stillCurrent()) return
      bindingDatabases.value = (schema.databases || []).map(d => d.name).sort()
      bindingDefaultDatabase.value = info?.database || ''
    }
    if (bindingDefaultDatabase.value && bindingDatabases.value.includes(bindingDefaultDatabase.value)) {
      bindingDatabaseExisting.value = bindingDefaultDatabase.value
    } else if (bindingDatabases.value.length > 0) {
      bindingDatabaseExisting.value = bindingDatabases.value[0]
    }
  } catch {
    if (!stillCurrent()) return
    bindingDatabases.value = []
    bindingDefaultDatabase.value = ''
  } finally {
    if (stillCurrent()) bindingDatabasesLoading.value = false
  }
}

watch(bindingService, () => {
  // For services that can't list existing namespaces (rabbitmq today),
  // default the mode to "new" — there's no dropdown to "pick" from
  // anyway.
  bindingDbMode.value = canListNamespaces(selectedServiceType.value) ? 'existing' : 'new'
  bindingDatabaseExisting.value = ''
  bindingDatabaseNew.value = ''
  bindingDatabases.value = []
  bindingDefaultDatabase.value = ''
  loadBindingDatabases()
})

/**
 * Builds a guard for a loader that runs without being awaited.
 *
 * The Env tab starts its loads from two watchers — entering the tab, and the
 * panel switching to another app — so the responses can land in any order. The
 * panel is reused across apps and across namespaces, and an app name repeats
 * between environments, so what identifies a request is the app, the namespace
 * and a generation: a request that left while the panel showed A must not
 * publish once the panel has moved to B, and an older A must not overwrite a
 * newer A.
 *
 * The display is the smaller half. updateEnv sends the whole variable map, so a
 * stale map on screen plus a single edit writes another app's configuration
 * over the one now selected.
 *
 * Each loader gets its own generation, so a link reload cannot invalidate an env
 * reload that is still in flight.
 */
/**
 * True while the panel still shows what it showed when this was called.
 *
 * An in-flight flag belongs to the panel rather than to one operation, so it
 * must not be tied to a generation: a handler that starts a follow-up load
 * advances the generation itself, and testing it in the handler's own finally
 * would leave the flag set for a panel that never moved.
 */
function samePanel() {
  const app = props.appName
  const namespace = props.namespace
  return () => app === props.appName && namespace === props.namespace
}

function loadGuard() {
  let generation = 0
  return () => {
    const mine = ++generation
    const app = props.appName
    const namespace = props.namespace
    const epoch = readEpoch
    return () =>
      mine === generation &&
      app === props.appName &&
      namespace === props.namespace &&
      epoch === readEpoch
  }
}

const beginAvailableServicesLoad = loadGuard()
async function loadAvailableServices() {
  const current = beginAvailableServicesLoad()
  try {
    const result = await fetchServices()
    if (!current()) return
    availableServices.value = result
  } catch {
    if (!current()) return
    availableServices.value = []
  }
}

const beginBind = loadGuard()
async function handleBind() {
  if (!bindingService.value) return
  // The namespace picker only renders for postgres/mysql/rabbitmq;
  // for other services bindingDbMode is just a stale default from the
  // last reset and must not block the bind. A blank value is the
  // explicit "use the service default" path (same as the CLI).
  if (hasNamespacePicker(selectedServiceType.value)) {
    const label = namespaceLabel(selectedServiceType.value)
    // For services with a "Pick existing" mode (postgres/mysql),
    // typing nothing in "Create new" is an error. For services
    // without listing (rabbitmq today), a blank input means "share
    // the service default" — that path is what the help text below
    // advertises and the backend handles as a no-op bind.
    if (bindingDbMode.value === 'new' && !bindingDatabaseNew.value.trim() && canListNamespaces(selectedServiceType.value)) {
      toast.error(`Type a ${label} name, or switch to "Pick existing"`)
      return
    }
    if (bindingDbMode.value === 'existing' && canListNamespaces(selectedServiceType.value) && !bindingDatabaseExisting.value) {
      toast.error(`Pick an existing ${label} from the dropdown`)
      return
    }
  }
  const op = envMutation()
  const onPanel = op.onPanel
  // The button's own order. The env-write generation is the wrong one to test:
  // any unrelated variable edit advances it and would strand the button, while
  // the panel identity alone would let the first of two binds re-enable it
  // while the second is still running.
  const newestBind = beginBind()
  // Snapshot what was on screen at the click. The form stays usable while this
  // waits its turn, so reading the fields afterwards would send whatever the
  // operator had changed them to and report the service they first chose.
  const request = {
    service: bindingService.value,
    app: props.appName,
    namespace: project.value,
    prefix: bindingPrefix.value || undefined,
    database: resolvedBindingDatabase.value || undefined,
  }
  const bound = request.service
  binding.value = true
  try {
    await op.ready()
    if (!op.stillHere()) return
    await bindService(request)
    toast.success(`Bound ${bound}: app restarting`)
    if (!onPanel()) return
    bindingService.value = ''
    bindingPrefix.value = ''
    bindingDbMode.value = 'existing'
    bindingDatabaseExisting.value = ''
    bindingDatabaseNew.value = ''
    bindingDatabases.value = []
    bindingDefaultDatabase.value = ''
    if (!(await reloadEnvHoldingSlot(op)) && op.stillHere()) {
      toast.error('That worked, but the panel could not be refreshed. Reload before editing variables.')
    }
    void loadLinks()
  } catch {
    toast.error('Failed to bind service')
  } finally {
    op.done()
    if (newestBind() && onPanel()) binding.value = false
  }
}

async function handleUnbind(serviceName: string) {
  const op = envMutation()
  try {
    await op.ready()
    if (!op.stillHere()) return
    await unbindService({
      service: serviceName,
      app: props.appName,
      namespace: project.value,
    })
    toast.success(`Unbound ${serviceName}: app restarting`)
    if (!op.onPanel()) return
    if (!(await reloadEnvHoldingSlot(op)) && op.stillHere()) {
      toast.error('That worked, but the panel could not be refreshed. Reload before editing variables.')
    }
    void loadLinks()
  } catch {
    toast.error('Failed to unbind service')
  } finally {
    op.done()
  }
}

const boundServices = computed(() => {
  return [...new Set(injectedVars.value.map(v => v.service))]
})

// App linking
const linkableApps = ref<string[]>([])
const linkingTarget = ref('')
const linkingPublic = ref(false)
const linking = ref(false)

const beginLinkableAppsLoad = loadGuard()
async function loadLinkableApps() {
  const current = beginLinkableAppsLoad()
  try {
    const apps = await api.fetchApps(project.value)
    if (!current()) return
    linkableApps.value = apps
      .map(a => a.name)
      .filter(name => name !== props.appName)
  } catch {
    if (!current()) return
    linkableApps.value = []
  }
}

const beginLink = loadGuard()
async function handleLink() {
  if (!linkingTarget.value) return
  const op = envMutation()
  const onPanel = op.onPanel
  const newestLink = beginLink()
  const target = linkingTarget.value
  const asPublic = linkingPublic.value
  linking.value = true
  try {
    await op.ready()
    if (!op.stillHere()) return
    const resp = await api.linkApp(target, props.appName, project.value, asPublic)
    toast.success(`Linked ${resp.target}: ${resp.envVar} set`)
    if (!onPanel()) return
    linkingTarget.value = ''
    linkingPublic.value = false
    if (!(await reloadEnvHoldingSlot(op)) && op.stillHere()) {
      toast.error('That worked, but the panel could not be refreshed. Reload before editing variables.')
    }
    void loadLinks()
  } catch {
    toast.error('Failed to link app')
  } finally {
    op.done()
    if (newestLink() && onPanel()) linking.value = false
  }
}

async function handleUnlink(target: string) {
  const op = envMutation()
  try {
    await op.ready()
    if (!op.stillHere()) return
    await api.unlinkApp(target, props.appName, project.value)
    toast.success(`Unlinked ${target}`)
    if (!op.onPanel()) return
    if (!(await reloadEnvHoldingSlot(op)) && op.stillHere()) {
      toast.error('That worked, but the panel could not be refreshed. Reload before editing variables.')
    }
    void loadLinks()
  } catch {
    toast.error('Failed to unlink app')
  } finally {
    op.done()
  }
}

// Links come from the server, which reads what this app declares and what its
// pods were actually given. Guessing at *_URL variables cannot tell a link from
// a hand-typed internal address, and there is nothing left in the env map to
// guess at: a link's address is derived each reconcile, not stored.
const links = ref<AppLink[]>([])

const beginLinksLoad = loadGuard()
async function loadLinks() {
  // handleUnlink pairs the row it is given with the app the panel is currently
  // showing, so a stale list here unlinks against the wrong app.
  const current = beginLinksLoad()
  try {
    const result = await api.fetchLinks(project.value, props.appName)
    if (!current()) return
    links.value = result
  } catch {
    if (!current()) return
    links.value = []
  }
}

// Every variable in the env map is the operator's own now, so none of it is
// filtered out of the editor.
const regularEnvVars = computed(() => envVars.value)

// Logs — dual mode: Loki (searchable history) and Live (WebSocket stream)
const logMode = ref<'loki' | 'live'>('loki')
const { lines, connected, connect, disconnect, clear } = useLogStream()

// Live log pod selection and filtering
const livePods = ref<string[]>([])
const liveSelectedPod = ref('')
const liveTailLines = ref(100)
const liveFilter = ref('')

const filteredLines = computed(() => {
  if (!liveFilter.value) return lines.value
  const filter = liveFilter.value.toLowerCase()
  return lines.value.filter(line => line.toLowerCase().includes(filter))
})

async function loadLivePods() {
  try {
    livePods.value = await api.fetchPods(project.value, props.appName)
  } catch {
    livePods.value = []
  }
}

const appHealth = ref<api.PodHealth[]>([])
const healthError = ref<string | null>(null)

// A pod's phase reads Running as long as any container in it is up, so an app
// whose own container is dead and looping beside a healthy sidecar still shows
// as running. Judging each container is what makes that visible.
// Container state is not a failure predicate on its own. An init container
// that did its job is "terminated" with exit 0, and flagging it red would call
// every successful build a failure. A container caught in the moment between
// restarts is "running" and not ready, with a failed previous termination
// already recorded, and treating that as healthy hides the crash loop that is
// the whole reason this panel exists.
function hasFailed(container: api.ContainerHealth): boolean {
  if (container.state === 'terminated') return container.exit_code !== 0
  if (container.state === 'running') return !container.ready && container.restarts > 0
  return true
}

const failingContainers = computed(() => {
  const out: { pod: string; container: api.ContainerHealth }[] = []
  for (const pod of appHealth.value) {
    for (const container of [...pod.init_containers, ...pod.containers]) {
      if (hasFailed(container)) out.push({ pod: pod.name, container })
    }
  }
  return out
})

function openContainerErrors() {
  modal.open(ContainerErrorsModal, {
    title: `Container errors — ${props.appName}`,
    failures: failingContainers.value,
  })
}

async function loadHealth() {
  try {
    healthError.value = null
    // The declared type says a list, and a 204 or a null body says otherwise.
    // Everything downstream iterates this, so the coercion belongs here rather
    // than at each reader.
    const pods = await api.fetchAppHealth(project.value, props.appName)
    appHealth.value = Array.isArray(pods) ? pods : []
  } catch (e) {
    appHealth.value = []
    healthError.value = e instanceof Error ? e.message : 'container status could not be read'
  }
}

let healthPollTimer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  loadHealth()
  // Only while something is wrong: a healthy app has nothing to re-read, and a
  // failing one changes state as it restarts.
  healthPollTimer = setInterval(() => {
    // Also while something has restarted but currently looks fine: a crash
    // loop sampled during its running window would otherwise stop the polling
    // that would have caught the next crash.
    const restarting = appHealth.value.some(p =>
      [...p.init_containers, ...p.containers].some(c => c.restarts > 0))
    if (failingContainers.value.length > 0 || restarting) loadHealth()
  }, 5000)
})
onUnmounted(() => {
  if (healthPollTimer) clearInterval(healthPollTimer)
})

function onLivePodChange() {
  clear()
  connectLive()
}

function onLiveTailChange() {
  clear()
  connectLive()
}

function connectLive() {
  const pod = liveSelectedPod.value || undefined
  connect(project.value, props.appName, pod, liveTailLines.value)
}

// Loki logs
const lokiLogs = ref<api.LogEntry[]>([])
const lokiLoading = ref(false)

const currentLogsText = computed(() => {
  if (logMode.value === 'loki') {
    return lokiLogs.value.map(e => `${e.pod} ${e.line}`).join('\n')
  }
  return lines.value.join('\n')
})
const logSearch = ref('')
const logSince = ref('1h')

async function loadLokiLogs() {
  lokiLoading.value = true
  try {
    lokiLogs.value = await api.fetchLogs(project.value, props.appName, {
      search: logSearch.value || undefined,
      since: logSince.value,
      limit: 500,
    })
  } catch {
    lokiLogs.value = []
  } finally {
    lokiLoading.value = false
  }
}

function switchLogMode(mode: 'loki' | 'live') {
  logMode.value = mode
  if (mode === 'live') {
    loadLivePods()
    connectLive()
  } else {
    disconnect()
    loadLokiLogs()
  }
}

function formatLogTimestamp(nsTimestamp: string): string {
  const ms = Number(nsTimestamp) / 1_000_000
  if (isNaN(ms)) return ''
  return new Date(ms).toLocaleTimeString('en-GB', { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

// Env vars
const envVars = ref<Record<string, string>>({})
const newEnvKey = ref('')
const newEnvValue = ref('')
const envLoading = ref(false)

// A restart is pending when the running pods started before the last env
// change, since env is only read at pod startup. The console shows a banner
// rather than restarting automatically — a live service is not cycled without
// the user asking.
const envRestartPending = ref(false)

// What each value resolves to, with secret-derived parts masked by the server.
// Deployer-only, so a viewer gets nothing and the tab renders the templates
// alone — which is the whole of what it showed before this existed.
const envPreview = ref<EnvPreview | null>(null)

const previewByKey = computed(() => {
  const byKey = new Map<string, EnvPreviewVariable>()
  for (const v of envPreview.value?.variables ?? []) byKey.set(v.key, v)
  return byKey
})

const beginEnvPreviewLoad = loadGuard()
async function loadEnvPreview() {
  const current = beginEnvPreviewLoad()
  try {
    const result = await api.fetchEnvPreview(project.value, props.appName)
    if (!current()) return
    envPreview.value = result
  } catch {
    if (!current()) return
    // A viewer is refused this endpoint by design, and a failure to resolve is
    // not a reason to fail the tab. The values still render as written.
    envPreview.value = null
  }
}

const beginEnvRestartLoad = loadGuard()
async function refreshEnvRestartPending() {
  const current = beginEnvRestartLoad()
  try {
    const result = await api.fetchEnvRestartPending(project.value, props.appName)
    if (!current()) return
    envRestartPending.value = result
  } catch {
    if (!current()) return
    envRestartPending.value = false
  }
}

// Env conflicts (direct env: entries on the deployment that override envFrom)
const envConflicts = ref<string[]>([])
const fixingConflicts = ref(false)

const beginEnvConflictsLoad = loadGuard()
async function loadEnvConflicts() {
  const current = beginEnvConflictsLoad()
  try {
    const result = (await api.fetchEnvConflicts(project.value, props.appName)) || []
    if (!current()) return
    envConflicts.value = result
  } catch {
    if (!current()) return
    envConflicts.value = []
  }
}

const beginConflictFix = loadGuard()
async function fixEnvConflicts() {
  const current = beginConflictFix()
  fixingConflicts.value = true
  try {
    await api.removeEnvConflicts(project.value, props.appName)
    if (!current()) return
    envConflicts.value = []
    toast.success('Direct env entries removed: envFrom will now take effect')
  } catch {
    toast.error('Failed to remove direct env entries')
  } finally {
    if (current()) fixingConflicts.value = false
  }
}

// Injected env vars (from service bindings via EnvFrom)
const injectedVars = ref<api.InjectedVar[]>([])

const beginInjectedEnvLoad = loadGuard()
async function loadInjectedEnv() {
  const current = beginInjectedEnvLoad()
  try {
    const result = await api.fetchInjectedEnv(project.value, props.appName)
    if (!current()) return
    injectedVars.value = result
  } catch {
    if (!current()) return
    injectedVars.value = []
  }
}

// Secrets
const secretKeys = ref<string[]>([])
const revealedSecrets = ref<Record<string, string>>({})
const newSecretKey = ref('')
const newSecretValue = ref('')
const secretsLoading = ref(false)

/**
 * The variable map has two kinds of publisher, and which one the server honours
 * is not decided by which the client started last.
 *
 * updateEnv replaces the whole map and returns what was stored, so its response
 * is authoritative. Two of them overlapping cannot be ordered from here at all:
 * the handler retries on conflict, so the request sent first can be the one
 * committed last, carrying the older body. They are therefore serialised — one
 * in flight at a time — which removes the question rather than guessing at it.
 *
 * A read issued while a write is in flight may be answered from either side of
 * the commit, and nothing on the client can tell which, so it stands aside.
 * That covers both interleavings: a read overtaken by a write, and a read that
 * started later but was served the earlier state.
 *
 * Getting this wrong loses data rather than merely showing something stale,
 * because the next edit sends whatever map is displayed.
 *
 * panelEpoch is what makes the bookkeeping survive a panel that leaves and
 * comes back. Comparing the app and namespace cannot tell the first visit from
 * the second, so a write abandoned on the way out would return during the
 * second visit and decrement a count that was reset without it.
 */
let panelEpoch = 0
// Reads answer to their own epoch: retiring them must not disturb the write
// queue, which serialises whole-map replacements and is what stops two of them
// overlapping.
let readEpoch = 0
let envWritesPending = 0
let envWriteEpoch = 0
let envWriteChain: Promise<unknown> = Promise.resolve()

function resetEnvWriteTracking() {
  panelEpoch++
  envWritesPending = 0
  envWriteEpoch++
  // A write for the app being left has no ordering relationship with one for the
  // app arriving, so the new panel must not queue behind it — a request that
  // never settles would otherwise block every write from here on.
  envWriteChain = Promise.resolve()
}


/**
 * Runs one whole-map write, queued behind any other, and reports whether its
 * answer may still be published.
 */
function envMutation() {
  const myPanel = panelEpoch
  const myRead = readEpoch
  const onPanel = samePanel()
  envWritesPending++
  let settled = false

  // The place in the queue is taken now, while nothing can interleave. Taking
  // it after the await let two mutations created in the same turn — a
  // double-clicked button, a save and a delete — capture the same predecessor
  // and both run against it, which is the overlap this queue exists to prevent.
  const predecessor = envWriteChain
  let release: () => void = () => {}
  const mine = new Promise<void>(resolve => {
    release = resolve
  })
  envWriteChain = predecessor.then(() => mine, () => mine)

  return {
    onPanel,
    // Wait for the write ahead of this one, whether it succeeded or not.
    ready: () => predecessor.then(() => undefined, () => undefined),
    // Whether this write should still be sent at all once its turn comes. A
    // write queued behind another is not superseded by it — it carries the next
    // edit, built on what that one returned — so only the panel decides.
    //
    // The panel decides, and only the panel. A role that went and came back
    // while this waited its turn changed nothing about the edit the operator
    // asked for, and the server authorises the request when it arrives —
    // dropping it here would discard an acknowledged action for a permission
    // that is present again.
    stillHere: () => onPanel() && myPanel === panelEpoch,
    // The slot is held, so this is the only write that can be talking to the
    // server and its answer is the newest there is. Asking whether a later
    // write had booked would refuse it, and that later write would then build
    // its body from the optimistic map rather than from what the server stored.
    mayPublish: () => onPanel() && myPanel === panelEpoch && myRead === readEpoch,
    done: () => {
      if (settled) return
      settled = true
      release()
      // A write abandoned by a panel switch belongs to an epoch whose counts
      // were already reset, so it must not touch the current ones.
      if (myPanel === panelEpoch) {
        envWritesPending--
        envWriteEpoch++
      }
      // A write retired by a role that went cannot publish and cannot run its
      // own hand-off, so nothing else would refill the tab it was cleared from.
      // Once the count is down and the role is back, read it again — after the
      // decrement, or the read stands aside for the write that just finished.
      if (myRead !== readEpoch && canReadEnv.value && activeTab.value === 'env') {
        void loadEnv()
      }
    },
  }
}

const beginEnvRead = loadGuard()

function envRead() {
  const current = beginEnvRead()
  const epoch = envWriteEpoch
  const pendingAtStart = envWritesPending
  return {
    // The newest read for this panel owns the spinner, whether or not it is
    // allowed to publish. Tying the two together left the tab reading
    // "Loading..." for good whenever a read stood aside for a write.
    owns: current,
    mayPublish: () =>
      current() && envWriteEpoch === epoch && envWritesPending === 0 && pendingAtStart === 0,
  }
}

/**
 * True when a hand-off read failed, so what is displayed is no longer known to
 * match the server. A whole-map write built from it would post an old map back.
 */
const envMapStale = ref(false)

/**
 * Re-reads the map for a mutation that is still holding its queue slot, and
 * reports whether the panel now matches the server.
 *
 * A bind or a link changes spec.env on the server, so the panel has to adopt
 * the result before the next queued write builds its body from what is shown —
 * a public link is a plain env value, and a stale map would drop it. loadEnv
 * cannot do that job: it stands aside whenever a write is pending, and one
 * always is here, either this mutation or the one already queued behind it.
 *
 * Publishing from inside the slot needs no permission from the ordinary read
 * order. Nothing else can be sending, and any ordinary read — whenever it
 * started — saw this mutation pending and has already disqualified itself. It
 * still takes the read generation, so an older read cannot land on top later.
 */
async function reloadEnvHoldingSlot(op: ReturnType<typeof envMutation>): Promise<boolean> {
  beginEnvRead()
  try {
    const result = await api.fetchEnv(project.value, props.appName)
    // The panel having moved is not the same as the map being current, and a
    // caller that reads this as "safe to send" would post the app it left to
    // the app it arrived at.
    if (!op.stillHere()) return false
    envVars.value = result
    envMapStale.value = false
    // These describe the panel rather than the map, so the next writer does not
    // need them and must not queue behind them. Awaiting them here held the
    // slot across five requests that nothing bounds.
    void Promise.all([loadEnvConflicts(), loadInjectedEnv(), loadLinkableApps(), refreshEnvRestartPending(), loadEnvPreview()])
    return true
  } catch {
    if (op.stillHere()) envMapStale.value = true
    return false
  }
}

/**
 * Makes sure a whole-map write is built on what the server holds.
 *
 * The mutation before this one may have changed the environment and then failed
 * to read it back. Posting the displayed map at that point would delete
 * whatever it had added, so this re-reads first and refuses the write if it
 * cannot.
 */
async function envMapIsSafeToSend(op: ReturnType<typeof envMutation>): Promise<boolean> {
  if (!envMapStale.value) return true
  if (await reloadEnvHoldingSlot(op)) return true
  // A panel that moved has nothing to be told and nothing to send.
  if (op.stillHere()) {
    toast.error('Could not read the current variables. Reload the tab before editing.')
  }
  return false
}

async function loadEnv() {
  const read = envRead()
  envLoading.value = true
  try {
    const result = await api.fetchEnv(project.value, props.appName)
    if (!read.mayPublish()) return
    envVars.value = result
    envMapStale.value = false
    await Promise.all([loadEnvConflicts(), loadInjectedEnv(), loadLinkableApps(), refreshEnvRestartPending(), loadEnvPreview()])
  } catch {
    if (!read.mayPublish()) return
    // An empty map is what a read that failed has, not what the app has. Saying
    // so is what stops the next edit posting {} plus one key over a spec.env
    // that was never read — updateEnv replaces the whole map.
    envVars.value = {}
    envMapStale.value = true
  } finally {
    if (read.owns()) envLoading.value = false
  }
}

async function addEnvVar() {
  if (!canWriteEnv.value) return
  const key = newEnvKey.value.trim()
  const value = newEnvValue.value.trim()
  if (!key) return
  const op = envMutation()
  let restore: Record<string, string> | null = null
  try {
    await op.ready()
    if (!op.stillHere()) return
    if (!(await envMapIsSafeToSend(op))) return
    // The optimistic value is displayed before it is stored, so a failure has
    // to take it back: the next queued write builds its body from this map and
    // would store a value the server rejected.
    restore = { ...envVars.value }
    envVars.value[key] = value
    const result = await api.updateEnv(project.value, props.appName, envVars.value)
    if (!op.mayPublish()) return
    envVars.value = result
    newEnvKey.value = ''
    newEnvValue.value = ''
    // The reconciler stamps the change asynchronously, so show the banner now
    // rather than racing a status check; a load or restart reconciles it.
    envRestartPending.value = true
    // Started rather than awaited: it has its own guard, and the slot must not
    // be held across a request the next writer does not depend on.
    void loadEnvPreview()
  } catch {
    if (restore && op.stillHere()) {
      envVars.value = restore
      // The restore is the best guess available, not a fact: a request that
      // timed out may have been applied. Marking it stale makes the next write
      // read the map back rather than post this guess as the new truth.
      envMapStale.value = true
      toast.error('Failed to save the change')
    }
  } finally {
    op.done()
  }
}

async function deleteEnvVar(key: string) {
  if (!canWriteEnv.value) return
  const op = envMutation()
  let restore: Record<string, string> | null = null
  try {
    await op.ready()
    if (!op.stillHere()) return
    if (!(await envMapIsSafeToSend(op))) return
    // The optimistic value is displayed before it is stored, so a failure has
    // to take it back: the next queued write builds its body from this map and
    // would store a value the server rejected.
    restore = { ...envVars.value }
    delete envVars.value[key]
    const result = await api.updateEnv(project.value, props.appName, envVars.value)
    if (!op.mayPublish()) return
    envVars.value = result
    envRestartPending.value = true
    void loadEnvPreview()
  } catch {
    if (restore && op.stillHere()) {
      envVars.value = restore
      // The restore is the best guess available, not a fact: a request that
      // timed out may have been applied. Marking it stale makes the next write
      // read the map back rather than post this guess as the new truth.
      envMapStale.value = true
      toast.error('Failed to save the change')
    }
  } finally {
    op.done()
  }
}

const editingEnv = ref<string | null>(null)
const editEnvValue = ref('')

function startEditEnv(key: string) {
  editingEnv.value = key
  editEnvValue.value = envVars.value[key] || ''
}

async function saveEditEnv(key: string) {
  if (!canWriteEnv.value) return
  const newValue = editEnvValue.value.trim()
  const changed = envVars.value[key] !== newValue
  const op = envMutation()
  let restore: Record<string, string> | null = null
  try {
    await op.ready()
    if (!op.stillHere()) return
    if (!(await envMapIsSafeToSend(op))) return
    // The optimistic value is displayed before it is stored, so a failure has
    // to take it back: the next queued write builds its body from this map and
    // would store a value the server rejected.
    restore = { ...envVars.value }
    envVars.value[key] = newValue
    const result = await api.updateEnv(project.value, props.appName, envVars.value)
    if (!op.mayPublish()) return
    envVars.value = result
    toast.success(`${key} updated`)
    editingEnv.value = null
    editEnvValue.value = ''
    // Only flag a restart when the value actually changed, so re-saving the same
    // value doesn't raise a banner the reconciler will never confirm.
    if (changed) envRestartPending.value = true
    void loadEnvPreview()
  } catch {
    if (restore && op.stillHere()) {
      envVars.value = restore
      // The restore is the best guess available, not a fact: a request that
      // timed out may have been applied. Marking it stale makes the next write
      // read the map back rather than post this guess as the new truth.
      envMapStale.value = true
      toast.error('Failed to save the change')
    }
  } finally {
    op.done()
  }
}

function useEnvSnippet(snippet: EnvPreviewSnippet) {
  newEnvKey.value = snippet.key
  newEnvValue.value = snippet.value
}

// Appends to whichever field is in play: the edit box when a row is open, the
// add box otherwise.
function insertEnvReference(reference: string) {
  if (editingEnv.value !== null) editEnvValue.value += reference
  else newEnvValue.value += reference
}

function cancelEditEnv() {
  editingEnv.value = null
  editEnvValue.value = ''
}

async function loadSecrets() {
  secretsLoading.value = true
  try {
    const raw = await api.fetchSecretKeys(project.value, props.appName)
    secretKeys.value = raw.map((item) => item.key)
  } catch {
    secretKeys.value = []
  } finally {
    secretsLoading.value = false
  }
  revealedSecrets.value = {}
}

async function revealSecret(key: string) {
  if (revealedSecrets.value[key]) {
    delete revealedSecrets.value[key]
    return
  }
  try {
    const value = await api.revealSecret(project.value, props.appName, key)
    revealedSecrets.value[key] = value
  } catch {
    // ignore
  }
}

async function addSecret() {
  if (!newSecretKey.value || !newSecretValue.value) return
  await api.setSecrets(project.value, props.appName, { [newSecretKey.value]: newSecretValue.value })
  newSecretKey.value = ''
  newSecretValue.value = ''
  // A secret is read via envFrom at startup too, so the same restart applies.
  envRestartPending.value = true
  await loadSecrets()
}

function deleteSecretKey(key: string) {
  modal.open(ConfirmDialog, {
    title: `Delete secret ${key}?`,
    message: 'This permanently removes the secret from the app. Pods using it keep the old value until they restart. This cannot be undone.',
    confirmLabel: 'Delete secret',
    onConfirm: async () => {
      modal.close()
      try {
        await api.deleteSecret(project.value, props.appName, key)
        toast.success(`Secret ${key} deleted`)
        envRestartPending.value = true
        await loadSecrets()
      } catch {
        toast.error(`Failed to delete ${key}`)
      }
    },
  })
}

// Secret editing
const editingSecret = ref<string | null>(null)
const editSecretValue = ref('')

function startEditSecret(key: string) {
  const currentValue = revealedSecrets.value[key]
  if (currentValue) {
    editingSecret.value = key
    editSecretValue.value = currentValue
  } else {
    // Reveal first, then enable edit
    revealSecret(key).then(() => {
      editingSecret.value = key
      editSecretValue.value = revealedSecrets.value[key] || ''
    })
  }
}

async function saveEditSecret(key: string) {
  if (!editSecretValue.value) return
  try {
    const changed = editSecretValue.value !== revealedSecrets.value[key]
    await api.setSecrets(project.value, props.appName, { [key]: editSecretValue.value })
    toast.success(`Secret ${key} updated`)
    editingSecret.value = null
    editSecretValue.value = ''
    delete revealedSecrets.value[key]
    if (changed) envRestartPending.value = true
  } catch {
    toast.error(`Failed to update ${key}`)
  }
}

function cancelEditSecret() {
  editingSecret.value = null
  editSecretValue.value = ''
}

// JSON view for secrets
const showJsonView = ref(false)

const secretsAsJson = computed(() => {
  const obj: Record<string, string> = {}
  for (const key of secretKeys.value) {
    obj[key] = revealedSecrets.value[key] || '••••••••'
  }
  return JSON.stringify(obj, null, 2)
})

async function revealAllSecrets() {
  for (const key of secretKeys.value) {
    if (!revealedSecrets.value[key]) {
      try {
        const value = await api.revealSecret(project.value, props.appName, key)
        revealedSecrets.value[key] = value
      } catch {
        // ignore
      }
    }
  }
}

async function toggleJsonView() {
  if (!showJsonView.value) {
    await revealAllSecrets()
  }
  showJsonView.value = !showJsonView.value
}

// Scale
const replicaCount = ref(1)
const scaling = ref(false)

async function loadScale() {
  try {
    const apps = await api.fetchApps(project.value)
    const app = apps.find(a => a.name === props.appName)
    if (app) {
      replicaCount.value = app.replicas
    }
  } catch {
    // ignore
  }
  await loadAutoscale()
  loadRecommendation()
}

async function setScale(count: number) {
  if (count < 0) return
  scaling.value = true
  try {
    await api.scaleApp(project.value, props.appName, count)
    replicaCount.value = count
    toast.success(`Scaled ${props.appName} to ${count} replicas`)
  } catch {
    toast.error(`Failed to scale ${props.appName}`)
  } finally {
    scaling.value = false
  }
}

// Autoscaling
const autoscaleEnabled = ref(false)
const autoscaleMin = ref(1)
const autoscaleMax = ref(5)
const autoscaleCpu = ref(70)
const autoscaleMemory = ref(0)
const autoscaleCurrentCpu = ref('')
const autoscaleCurrentMemory = ref('')
const autoscaleSaving = ref(false)

async function loadAutoscale() {
  try {
    const config = await api.fetchAutoscale(project.value, props.appName)
    autoscaleEnabled.value = config.enabled
    if (config.enabled) {
      autoscaleMin.value = config.min_replicas
      autoscaleMax.value = config.max_replicas
      autoscaleCpu.value = config.cpu_target || 70
      autoscaleMemory.value = config.memory_target || 0
      autoscaleCurrentCpu.value = config.current_cpu
      autoscaleCurrentMemory.value = config.current_memory
    }
  } catch {
    autoscaleEnabled.value = false
  }
}

async function saveAutoscale() {
  autoscaleSaving.value = true
  try {
    if (autoscaleEnabled.value) {
      await api.setAutoscale(project.value, props.appName, {
        min_replicas: autoscaleMin.value,
        max_replicas: autoscaleMax.value,
        cpu_target: autoscaleCpu.value,
        memory_target: autoscaleMemory.value,
      })
      toast.success('Autoscaling enabled')
    } else {
      await api.disableAutoscale(project.value, props.appName)
      toast.success('Autoscaling disabled')
    }
  } catch {
    toast.error('Failed to update autoscaling')
  } finally {
    autoscaleSaving.value = false
  }
}

// Resource recommendation
const recommendation = ref<api.ResourceRecommendation | null>(null)

async function loadRecommendation() {
  try {
    recommendation.value = await api.fetchRecommendation(project.value, props.appName)
  } catch {
    recommendation.value = null
  }
}

async function handleApplyRecommendation() {
  try {
    await api.applyRecommendation(project.value, props.appName)
    recommendation.value = null
    toast.success('Resource profile applied')
    loadScale()
  } catch {
    toast.error('Failed to apply recommendation')
  }
}

async function handleDismissRecommendation() {
  try {
    await api.dismissRecommendation(project.value, props.appName)
    recommendation.value = null
  } catch {
    toast.error('Failed to dismiss recommendation')
  }
}

// Source / Build
const buildStatus = ref<api.BuildStatus | null>(null)
const buildLoading = ref(false)
const sourceDetailsOpen = ref(false)
const rebuilding = ref(false)

// Edit form state for the Git source panel. Opens inline below the
// existing repo/branch read-out. Token is write-only — the API never
// returns it, so the form starts empty and only sends `token` when the
// operator actually types one in.
//
// gitFormInitial captures the pre-fill values so saveGitEdit can do a
// real diff and only send fields the operator actually changed. Without
// the diff, a branch-only edit would also overwrite `url` with whatever
// the form was pre-populated with — which is the SANITIZED URL the API
// returned. Re-saving a sanitized URL would strip inline credentials
// the operator deliberately kept in the CR.
const gitEditing = ref(false)
const revealGitTokenOpen = ref(false)
const gitSaving = ref(false)
const gitForm = ref({ url: '', branch: '', dockerfile_path: '', context: '', token: '' })
const gitFormInitial = ref({ url: '', branch: '', dockerfile_path: '', context: '' })

function openGitEdit() {
  const initial = {
    url: buildStatus.value?.git_url ?? '',
    branch: buildStatus.value?.git_branch ?? '',
    dockerfile_path: '',
    context: '',
  }
  gitFormInitial.value = { ...initial }
  gitForm.value = { ...initial, token: '' }
  gitEditing.value = true
}

async function saveGitEdit() {
  gitSaving.value = true
  try {
    const payload: api.UpdateGitSourcePayload = {}
    // Diff against the pre-fill so unchanged fields stay server-side.
    // Token has no pre-fill (write-only), so any non-empty value is by
    // definition a deliberate rotation.
    if (gitForm.value.url && gitForm.value.url !== gitFormInitial.value.url) {
      payload.url = gitForm.value.url
    }
    if (gitForm.value.branch && gitForm.value.branch !== gitFormInitial.value.branch) {
      payload.branch = gitForm.value.branch
    }
    if (gitForm.value.dockerfile_path && gitForm.value.dockerfile_path !== gitFormInitial.value.dockerfile_path) {
      payload.dockerfile_path = gitForm.value.dockerfile_path
    }
    if (gitForm.value.context && gitForm.value.context !== gitFormInitial.value.context) {
      payload.context = gitForm.value.context
    }
    if (gitForm.value.token) payload.token = gitForm.value.token

    await api.updateGitSource(project.value, props.appName, payload)
    toast.success('Git source updated')
    gitEditing.value = false
    // The credentials Secret / branch / URL changes don't trigger a
    // rebuild on their own. Refresh the status so the new branch
    // shows; operator can click Rebuild when they want a new build.
    loadBuildStatus()
  } catch {
    toast.error('Failed to update git source')
  } finally {
    gitSaving.value = false
  }
}

async function loadBuildStatus(quiet = false) {
  if (!quiet) buildLoading.value = true
  try {
    buildStatus.value = await api.fetchBuildStatus(project.value, props.appName)
    // A discarded build is the case where someone pushed, the pipeline went
    // green and nothing appeared. Leaving the explanation behind a collapsed
    // section is the same dead end as not writing it at all.
    if (buildStatus.value?.phase === 'Discarded') deployMethodsOpen.value = true
  } catch {
    if (!quiet) buildStatus.value = null
  } finally {
    if (!quiet) buildLoading.value = false
  }
}

// A build still running will change state on its own, so poll while it does and
// stop once it settles — the Source panel updates from Building to Succeeded
// without the user leaving and reopening the tab.
const buildInProgress = computed(() => {
  const phase = buildStatus.value?.phase?.toLowerCase()
  return phase === 'building' || phase === 'pending'
})

let buildPollTimer: ReturnType<typeof setInterval> | undefined
let buildPolling = false

onMounted(() => {
  buildPollTimer = setInterval(async () => {
    // Skip if a previous poll is still in flight, so slow responses can't pile up.
    if (buildPolling || activeTab.value !== 'deploys' || !buildInProgress.value) return
    buildPolling = true
    try {
      await loadBuildStatus(true)
    } finally {
      buildPolling = false
    }
  }, 3000)
})

onUnmounted(() => {
  if (buildPollTimer) clearInterval(buildPollTimer)
})

const buildLogLines = ref<string[]>([])
const buildLogsVisible = ref(false)
let buildLogAbort: AbortController | null = null

function removeGitSource() {
  modal.open(ConfirmDialog, {
    title: 'Stop building from git?',
    message: `${props.appName} will deploy prebuilt images instead. It keeps running the image it has now. The stored access token and the last build's status are removed with the source.`,
    confirmLabel: 'Remove git source',
    onConfirm: async () => {
      modal.close()
      try {
        await api.deleteGitSource(project.value, props.appName)
        toast.success('Git source removed')
        gitEditing.value = false
        await loadBuildStatus()
      } catch {
        toast.error('Failed to remove the git source')
      }
    },
  })
}

async function handleRebuild() {
  rebuilding.value = true
  try {
    await api.triggerRebuild(project.value, props.appName)
    toast.success('Build triggered')
    loadBuildStatus()
    streamBuildLogs()
  } catch {
    toast.error('Failed to trigger build')
  } finally {
    rebuilding.value = false
  }
}

async function streamBuildLogs() {
  disconnectBuildLogs()
  buildLogLines.value = []
  buildLogsVisible.value = true

  const controller = new AbortController()
  buildLogAbort = controller

  try {
    await api.streamBuildLogs(project.value, props.appName, (line) => {
      buildLogLines.value.push(line)
      if (buildLogLines.value.length > 2000) {
        buildLogLines.value = buildLogLines.value.slice(-1000)
      }
    }, controller.signal)
  } catch (err) {
    if (controller.signal.aborted) return
    const message = err instanceof Error ? err.message : 'failed to stream build logs'
    buildLogLines.value.push(`--- ${message} ---`)
  } finally {
    if (buildLogAbort === controller) buildLogAbort = null
    loadBuildStatus()
  }
}

function disconnectBuildLogs() {
  if (buildLogAbort) {
    buildLogAbort.abort()
    buildLogAbort = null
  }
}

async function handleCancelBuild() {
  try {
    await api.cancelBuild(project.value, props.appName)
    toast.success('Build cancelled')
    loadBuildStatus()
  } catch {
    toast.error('Failed to cancel build')
  }
}

// Restart
const restarting = ref(false)

const beginRestart = loadGuard()
async function handleRestart() {
  const current = beginRestart()
  const restarted = props.appName
  restarting.value = true
  try {
    await api.restartApp(project.value, props.appName)
    toast.success(`${restarted} is restarting: pods will be replaced with zero downtime`)
    if (!current()) return
    // The restart is rolling; clear the banner rather than re-checking, since
    // the old pods still linger mid-rollout and would keep it up. A later env
    // load re-checks against the settled pods.
    envRestartPending.value = false
  } catch {
    toast.error(`Failed to restart ${restarted}`)
  } finally {
    if (current()) restarting.value = false
  }
}

// Update image
const showImageForm = ref(false)
const newImage = ref('')
const updatingImage = ref(false)

async function handleUpdateImage() {
  if (!newImage.value) return
  updatingImage.value = true
  try {
    await api.updateImage(project.value, props.appName, newImage.value)
    toast.success(`${props.appName} image updated, rollout in progress`)
    showImageForm.value = false
    newImage.value = ''
  } catch {
    toast.error(`Failed to update image for ${props.appName}`)
  } finally {
    updatingImage.value = false
  }
}

// Deploy history
const deployHistory = ref<api.DeployEntry[]>([])
const historyLoading = ref(false)
const rollingBack = ref(false)

// Deploy methods section is collapsed by default — image/git/webhook
// config is set up once per app; the history list below is the more
// frequent visit.
const deployMethodsOpen = ref(false)

async function loadHistory() {
  historyLoading.value = true
  try {
    deployHistory.value = (await api.fetchDeployHistory(project.value, props.appName)) || []
  } catch {
    deployHistory.value = []
  } finally {
    historyLoading.value = false
  }
}

async function handleRollback(revision: number) {
  rollingBack.value = true
  try {
    await api.rollbackApp(project.value, props.appName, revision)
    toast.success(`${props.appName} rolled back to revision #${revision}`)
    await loadHistory()
  } catch {
    toast.error(`Failed to rollback ${props.appName}`)
  } finally {
    rollingBack.value = false
  }
}

function formatTimeAgo(timestamp: string): string {
  const d = Date.now() - new Date(timestamp).getTime()
  const seconds = Math.floor(d / 1000)
  if (seconds < 60) return `${seconds}s ago`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  return `${Math.floor(hours / 24)}d ago`
}


const imageCardState = computed(() => deriveImageCardState(buildStatus.value ?? null))
const gitCardState = computed(() => deriveGitCardState(buildStatus.value ?? null))

// Route
const routeEnabled = ref(false)
const routeHost = ref('')
const routeRedirectFrom = ref<string[]>([])
const routePath = ref('/')
const routeURL = ref('')
const routeSaving = ref(false)
const routeHealth = ref<api.AppRouteHealth | null>(null)
const routeDnsStatus = ref<api.RouteDnsStatusResponse | null>(null)
const routeDnsChecking = ref(false)
const routeIPCopied = ref(false)

async function loadRoute() {
  try {
    const r = await api.fetchRoute(project.value, props.appName)
    routeEnabled.value = r.enabled
    routeHost.value = r.host
    routeRedirectFrom.value = r.redirect_from ? [...r.redirect_from] : []
    routePath.value = r.path || '/'
    routeURL.value = r.url
    routeHealth.value = r.enabled ? r.health : null
    if (r.enabled && r.host) {
      // Quietly load the cached check on open so the badge is there
      // immediately. Errors are non-fatal — the panel works without it.
      checkRouteDns({ silent: true })
    } else {
      routeDnsStatus.value = null
    }
  } catch {
    routeEnabled.value = false
    routeHealth.value = null
  }
}

async function checkRouteDns(opts: { silent?: boolean; verify?: boolean } = {}) {
  if (!routeEnabled.value || !routeHost.value) return
  routeDnsChecking.value = true
  try {
    routeDnsStatus.value = await api.fetchRouteDnsStatus(project.value, props.appName, { verify: opts.verify })
  } catch {
    if (!opts.silent) toast.error('Failed to check DNS')
  } finally {
    routeDnsChecking.value = false
  }
}

function addRouteRedirectFrom() {
  routeRedirectFrom.value.push('')
}

function removeRouteRedirectFrom(i: number) {
  routeRedirectFrom.value.splice(i, 1)
}

async function copyRouteIP() {
  const ip = routeDnsStatus.value?.expected_ips?.[0]
  if (!ip) return
  try {
    await navigator.clipboard.writeText(ip)
    routeIPCopied.value = true
    setTimeout(() => { routeIPCopied.value = false }, 1500)
  } catch {
    toast.error('Could not copy to clipboard')
  }
}

async function saveRoute() {
  routeSaving.value = true
  try {
    if (!routeEnabled.value) {
      await api.deleteRoute(project.value, props.appName)
      routeURL.value = ''
      routeDnsStatus.value = null
      routeHealth.value = null
      toast.success('Route removed')
    } else {
      const resp = await api.setRoute(project.value, props.appName, {
        host: routeHost.value || undefined,
        path: routePath.value || undefined,
        redirect_from: routeRedirectFrom.value.map((h) => h.trim()).filter((h) => h),
      })
      routeHost.value = resp.host
      routeRedirectFrom.value = resp.redirect_from ? [...resp.redirect_from] : []
      routeURL.value = resp.url
      routeHealth.value = resp.health
      toast.success('Route updated')
      // Verify DNS right after save so the user sees the result of their change.
      await checkRouteDns()
    }
  } catch {
    toast.error('Failed to update route')
  } finally {
    routeSaving.value = false
  }
}

// Settings
const settingsLoading = ref(false)
const settingsSaving = ref(false)
const securityHeaders = ref(true)
const instanceHeader = ref(true)
const rateLimit = ref(0)
const requireApiKey = ref(false)
const apiKeyGatePending = ref(false)
const cspAllowlist = ref('')
const redirects = ref<Array<{ source: string; target: string; permanent: boolean }>>([])
const basicAuthEnabled = ref(false)
const basicAuthUsers = ref<string[]>([])
const basicAuthUsername = ref('')
const basicAuthPassword = ref('')
const basicAuthSaving = ref(false)
const memoryLimit = ref('')
const memoryRequest = ref('')
const cpuLimit = ref('')
const cpuRequest = ref('')
const resourcesAdvanced = ref(false)
const resourcesSaving = ref(false)

const resourcesLoading = ref(false)

async function loadSettings() {
  settingsLoading.value = true
  try {
    const [s] = await Promise.all([
      api.fetchSettings(project.value, props.appName),
      loadRoute(),
    ])
    securityHeaders.value = s.security_headers
    instanceHeader.value = s.instance_header
    rateLimit.value = s.rate_limit
    requireApiKey.value = s.require_api_key || false
    apiKeyGatePending.value = s.api_key_gate_pending || false
    cspAllowlist.value = (s.csp_allowlist || []).join(', ')
    redirects.value = s.redirects || []
    basicAuthEnabled.value = s.basic_auth || false
  } catch {
    securityHeaders.value = true
    instanceHeader.value = true
    rateLimit.value = 0
    requireApiKey.value = false
    apiKeyGatePending.value = false
    redirects.value = []
    basicAuthEnabled.value = false
  } finally {
    settingsLoading.value = false
  }
  loadWebhookConfig()
}

async function loadResources() {
  resourcesLoading.value = true
  try {
    const r = await api.fetchResources(project.value, props.appName)
    memoryLimit.value = r.memory_limit || ''
    memoryRequest.value = r.memory_request || ''
    cpuLimit.value = r.cpu_limit || ''
    cpuRequest.value = r.cpu_request || ''
    // If request and limit differ, the user is already using burstable mode —
    // open the advanced controls automatically so they can see and edit both.
    resourcesAdvanced.value =
      (r.cpu_request !== r.cpu_limit && r.cpu_request !== '') ||
      (r.memory_request !== r.memory_limit && r.memory_request !== '')
  } catch {
    // leave values as-is
  } finally {
    resourcesLoading.value = false
  }
}

async function saveResources() {
  resourcesSaving.value = true
  try {
    // In simple mode, the limit doubles as the request (Guaranteed QoS).
    // In advanced mode, both fields are sent independently — request can be
    // lower than limit for burstable workloads (e.g. JVM cold start).
    const payload = resourcesAdvanced.value
      ? {
          memory_request: memoryRequest.value,
          memory_limit: memoryLimit.value,
          cpu_request: cpuRequest.value,
          cpu_limit: cpuLimit.value,
        }
      : {
          memory_limit: memoryLimit.value,
          cpu_limit: cpuLimit.value,
        }
    await api.updateResources(project.value, props.appName, payload)
    toast.success('Resources updated: pod will restart')
  } catch {
    toast.error('Failed to update resources')
  } finally {
    resourcesSaving.value = false
  }
}

// Live usage scoped to this app's pods. Selector matches the convention
// the app controller writes (app=<name>). includePrometheus pulls the
// 1h sparklines + CPU throttling badge that render under the main gauges.
const usageScope = computed(() => ({
  namespace: project.value,
  selector: `app=${props.appName}`,
  includePrometheus: true,
}))
const usage = useResourceUsage(usageScope)

const memorySparkline = computed(() => usage.data.value?.memory_sparkline ?? [])
const cpuSparkline = computed(() => usage.data.value?.cpu_sparkline ?? [])
const cpuThrottlingPct = computed(() => usage.data.value?.cpu_throttling_pct ?? null)

// Parse the text inputs that ride alongside the gauges into numeric form
// for ResourceControl. Empty / unparseable values fall through to 0 so
// the gauge renders with an honest "no limit set" denominator instead of
// silently rounding.
const memoryLimitBytes = computed(() => {
  if (!memoryLimit.value) return 0
  try {
    return parseMemoryQuantity(memoryLimit.value)
  } catch {
    return 0
  }
})
const cpuLimitMillis = computed(() => {
  if (!cpuLimit.value) return 0
  try {
    return parseCpuQuantity(cpuLimit.value)
  } catch {
    return 0
  }
})

const memoryUsageBytes = computed(() => usage.data.value?.totals.memory_bytes ?? 0)
const cpuUsageMillis = computed(() => usage.data.value?.totals.cpu_millis ?? 0)
const usagePodCount = computed(() => usage.data.value?.totals.pod_count ?? 0)
const perPodMemoryBytes = computed(() => {
  const pods = usagePodCount.value
  return pods > 0 ? Math.round(memoryUsageBytes.value / pods) : 0
})
const perPodCpuMillis = computed(() => {
  const pods = usagePodCount.value
  return pods > 0 ? Math.round(cpuUsageMillis.value / pods) : 0
})

// Group raw container usage rows by pod. The per-replica grid sums
// every container in each pod (matching the drill-down route's
// reading) so the gauge here lines up with "kubectl top pod".
const perPodUsage = computed(() => {
  const rows = usage.data.value?.containers ?? []
  const byPod = new Map<string, { memory: number; cpu: number; present: boolean }>()
  for (const c of rows) {
    const existing = byPod.get(c.pod) ?? { memory: 0, cpu: 0, present: false }
    existing.memory += c.metrics_present ? c.memory_bytes : 0
    existing.cpu += c.metrics_present ? c.cpu_millis : 0
    existing.present = existing.present || c.metrics_present
    byPod.set(c.pod, existing)
  }
  return Array.from(byPod.entries()).map(([pod, v]) => ({ pod, ...v }))
})

// applyResourceLimit writes one side (memory or CPU) without disturbing
// the other. The gauges emit bytes/millicores; the API expects K8s
// quantity strings.
async function applyMemoryLimit(bytes: number) {
  resourcesSaving.value = true
  try {
    const quantity = toKubernetesMemoryQuantity(bytes)
    await api.updateResources(project.value, props.appName, {
      memory_limit: quantity,
      memory_request: resourcesAdvanced.value ? memoryRequest.value : quantity,
      cpu_limit: cpuLimit.value,
      cpu_request: resourcesAdvanced.value ? cpuRequest.value : cpuLimit.value,
    })
    memoryLimit.value = quantity
    if (!resourcesAdvanced.value) memoryRequest.value = quantity
    toast.success(`Memory limit set to ${quantity}: pod will restart`)
    usage.refresh()
  } catch {
    toast.error('Failed to update memory limit')
  } finally {
    resourcesSaving.value = false
  }
}

async function applyCpuLimit(millis: number) {
  resourcesSaving.value = true
  try {
    const quantity = toKubernetesCpuQuantity(millis)
    await api.updateResources(project.value, props.appName, {
      cpu_limit: quantity,
      cpu_request: resourcesAdvanced.value ? cpuRequest.value : quantity,
      memory_limit: memoryLimit.value,
      memory_request: resourcesAdvanced.value ? memoryRequest.value : memoryLimit.value,
    })
    cpuLimit.value = quantity
    if (!resourcesAdvanced.value) cpuRequest.value = quantity
    toast.success(`CPU limit set to ${quantity}: pod will restart`)
    usage.refresh()
  } catch {
    toast.error('Failed to update CPU limit')
  } finally {
    resourcesSaving.value = false
  }
}

async function saveSettings() {
  settingsSaving.value = true
  try {
    await api.updateSettings(project.value, props.appName, {
      security_headers: securityHeaders.value,
      instance_header: instanceHeader.value,
      rate_limit: rateLimit.value,
      require_api_key: requireApiKey.value,
      csp_allowlist: cspAllowlist.value
        ? cspAllowlist.value.split(',').map((s: string) => s.trim()).filter(Boolean)
        : [],
      redirects: redirects.value,
      basic_auth: basicAuthEnabled.value,
    })
    toast.success('Settings updated')
  } catch {
    toast.error('Failed to update settings')
  } finally {
    settingsSaving.value = false
  }
}

function addRedirect() {
  redirects.value.push({ source: '', target: '', permanent: true })
}

function removeRedirect(index: number) {
  redirects.value.splice(index, 1)
}

async function loadBasicAuth() {
  try {
    const status = await api.fetchBasicAuth(project.value, props.appName)
    basicAuthUsers.value = status.users || []
    basicAuthEnabled.value = status.enabled
  } catch {
    basicAuthUsers.value = []
  }
}

async function handleAddBasicAuthUser() {
  if (!basicAuthUsername.value || !basicAuthPassword.value) return
  basicAuthSaving.value = true
  try {
    await api.setBasicAuthUser(project.value, props.appName, basicAuthUsername.value, basicAuthPassword.value)
    basicAuthUsername.value = ''
    basicAuthPassword.value = ''
    toast.success('User added')
    await loadBasicAuth()
  } catch {
    toast.error('Failed to add user')
  } finally {
    basicAuthSaving.value = false
  }
}

async function handleDeleteBasicAuth() {
  try {
    await api.deleteBasicAuth(project.value, props.appName)
    basicAuthUsers.value = []
    basicAuthEnabled.value = false
    toast.success('Basic auth removed')
  } catch {
    toast.error('Failed to remove basic auth')
  }
}

async function handleDeleteBasicAuthUser(username: string) {
  try {
    await api.deleteBasicAuthUser(project.value, props.appName, username)
    await loadBasicAuth()
    toast.success(`User ${username} removed`)
  } catch {
    toast.error('Failed to remove user')
  }
}

// Webhook
const webhookEnabled = ref(false)
const webhookToken = ref('')
const webhookLoading = ref(false)
const webhookTokenVisible = ref(false)
const webhookCopied = ref<'url' | 'token' | null>(null)

const webhookUrl = computed(() => {
  return `${window.location.origin}/api/v1/webhook/${project.value}/${props.appName}`
})

async function loadWebhookConfig() {
  webhookLoading.value = true
  try {
    const config = await api.fetchWebhookConfig(project.value, props.appName)
    webhookEnabled.value = config.enabled
    webhookToken.value = config.token || ''
  } catch {
    webhookEnabled.value = false
    webhookToken.value = ''
  } finally {
    webhookLoading.value = false
  }
}

async function generateWebhook() {
  try {
    const result = await api.generateWebhookToken(project.value, props.appName)
    webhookEnabled.value = true
    webhookToken.value = result.token
    webhookTokenVisible.value = true
    toast.success('Webhook token generated')
  } catch {
    toast.error('Failed to generate webhook token')
  }
}

async function removeWebhook() {
  try {
    await api.deleteWebhook(project.value, props.appName)
    webhookEnabled.value = false
    webhookToken.value = ''
    webhookTokenVisible.value = false
    toast.success('Webhook removed')
  } catch {
    toast.error('Failed to remove webhook')
  }
}

function copyWebhook(text: string, type: 'url' | 'token') {
  navigator.clipboard.writeText(text)
  webhookCopied.value = type
  setTimeout(() => { webhookCopied.value = null }, 2000)
}

watch(activeTab, (tab) => {
  if (tab === 'logs') {
    if (logMode.value === 'live') {
      loadLivePods()
      connectLive()
    } else {
      loadLokiLogs()
    }
  } else {
    disconnect()
  }
  if (tab === 'env') { loadEnv(); loadLinks(); loadAvailableServices() }
  if (tab === 'secrets') loadSecrets()
  if (tab === 'scale') { loadScale(); loadMode() }
  if (tab === 'deploys') { loadHistory(); loadBuildStatus(); loadWebhookConfig() }
  if (tab === 'resources') loadResources()
  if (tab === 'settings') { loadSettings(); loadBasicAuth() }
  if (tab === 'files') loadFiles(currentPath.value)
  if (tab === 'connect') {
    loadPods()
    nextTick(() => {
      if (selectedPod.value && terminalRefs.value[selectedPod.value]) {
        terminalRefs.value[selectedPod.value].focus()
      }
    })
  }
})

onMounted(() => {
  loadLokiLogs()
})

// Both props identify what the panel shows. The same app name exists in every
// environment of a project, so switching to another environment's copy changes
// only the namespace, and watching the name alone left the panel showing the
// environment it had been asked to leave.
watch(() => [props.appName, props.namespace], () => {
  clear()
  // An operation in flight belongs to the panel that started it, so its result
  // will not clear these. Reset them here, or a switch away mid-restart leaves
  // the button disabled for every app this panel goes on to show.
  // Everything read or typed under the app being left goes with it. The write
  // bookkeeping goes too, but only here: a write in flight belongs to the app
  // being left and must not hold back this one's first read, while the same
  // reset on a momentary role blip would free the next writer to overlap a write
  // that is still running against the app both belong to.
  clearEnvState()
  resetEnvWriteTracking()
  // A half-filled form belongs to the app it was filled in for. Submitting it
  // here would act on the app that arrived, with what was chosen for the one
  // that left.
  newEnvKey.value = ''
  newEnvValue.value = ''
  bindingService.value = ''
  bindingPrefix.value = ''
  bindingDatabaseExisting.value = ''
  bindingDatabaseNew.value = ''
  bindingDatabases.value = []
  bindingDefaultDatabase.value = ''
  linkingTarget.value = ''
  linkingPublic.value = false
  restarting.value = false
  fixingConflicts.value = false
  binding.value = false
  linking.value = false
  bindingDatabasesLoading.value = false
  envLoading.value = false
  lokiLogs.value = []
  livePods.value = []
  liveSelectedPod.value = ''
  selectedPod.value = ''
  terminalPods.value = []
  terminalRefs.value = {}
  currentPath.value = '/'
  fileEntries.value = []
  if (activeTab.value === 'logs') {
    if (logMode.value === 'live') {
      loadLivePods()
      connectLive()
    } else {
      loadLokiLogs()
    }
  }
  if (activeTab.value === 'env') { loadEnv(); loadLinks(); loadAvailableServices() }
  if (activeTab.value === 'secrets') loadSecrets()
  if (activeTab.value === 'scale') { loadScale(); loadMode() }
  if (activeTab.value === 'deploys') { loadHistory(); loadBuildStatus(); loadWebhookConfig() }
  if (activeTab.value === 'resources') loadResources()
  if (activeTab.value === 'settings') { loadSettings(); loadBasicAuth() }
  if (activeTab.value === 'files') loadFiles('/')
  if (activeTab.value === 'connect') loadPods()
})

// Files browser
const filesLoading = ref(false)
const filesError = ref<string | null>(null)
const currentPath = ref('/')
const fileEntries = ref<filesApi.FileEntry[]>([])
const fileUploadRef = ref<HTMLInputElement | null>(null)
const uploading = ref(false)

// Connect tab
const copiedCmd = ref('')
function copyCommand(cmd: string, id: string) {
  navigator.clipboard.writeText(cmd)
  copiedCmd.value = id
  setTimeout(() => { copiedCmd.value = '' }, 2000)
}
const currentPod = ref('')
const podCount = ref(0)

// Web terminal
const terminalPods = ref<string[]>([])
const selectedPod = ref('')
const terminalRefs = ref<Record<string, InstanceType<typeof WebTerminal>>>({})
const terminalLoading = ref(false)

function setTerminalRef(pod: string, el: Element | ComponentPublicInstance | null) {
  if (el) terminalRefs.value[pod] = el as InstanceType<typeof WebTerminal>
}

async function loadPods() {
  terminalLoading.value = true
  try {
    terminalPods.value = await api.fetchPods(project.value, props.appName)
    if (terminalPods.value.length > 0 && !selectedPod.value) {
      selectedPod.value = terminalPods.value[0]
    }
  } catch {
    terminalPods.value = []
  } finally {
    terminalLoading.value = false
  }
}

function reconnectTerminal() {
  if (selectedPod.value && terminalRefs.value[selectedPod.value]) {
    terminalRefs.value[selectedPod.value].reconnect()
  }
}

const breadcrumbSegments = computed(() => {
  const segments = currentPath.value.split('/').filter(Boolean)
  return segments.map((seg, i) => ({
    name: seg,
    path: '/' + segments.slice(0, i + 1).join('/'),
  }))
})

async function loadFiles(dirPath: string = '/') {
  filesLoading.value = true
  filesError.value = null
  try {
    const result = await filesApi.listFiles(project.value, props.appName, dirPath)
    // Hide directories the API refuses, so the browser does not offer a folder
    // that answers 403. Only at the paths where they are actually restricted:
    // 'secrets' is the ServiceAccount projection under /run and /var/run, and an
    // app's own /app/secrets is an ordinary directory.
    const atRoot = result.path === '/'
    const atRun = result.path === '/run' || result.path === '/var/run'
    fileEntries.value = result.entries.filter(e => {
      if (!e.is_dir) return true
      if (atRoot && ['proc', 'sys', 'dev'].includes(e.name)) return false
      if (atRun && e.name === 'secrets') return false
      return true
    })
    currentPath.value = result.path
    currentPod.value = result.pod || ''
    podCount.value = result.pod_count || 1
  } catch (e) {
    filesError.value = e instanceof Error ? e.message : 'failed to load files'
    fileEntries.value = []
  } finally {
    filesLoading.value = false
  }
}

function navigateToDir(dirPath: string) {
  loadFiles(dirPath)
}

function handleFileClick(entry: filesApi.FileEntry) {
  if (entry.is_dir) {
    const newPath = currentPath.value === '/'
      ? '/' + entry.name
      : currentPath.value + '/' + entry.name
    loadFiles(newPath)
  } else {
    viewFile(entry)
  }
}

function viewFile(entry: filesApi.FileEntry) {
  const filePath = currentPath.value === '/'
    ? '/' + entry.name
    : currentPath.value + '/' + entry.name
  modal.open(FileViewerModal, {
    project: project.value,
    appName: props.appName,
    filePath: filePath,
    fileName: entry.name,
    fileSize: entry.size,
  })
}

async function handleFileUpload(event: Event) {
  const input = event.target as HTMLInputElement
  if (!input.files?.length) return
  const file = input.files[0]
  uploading.value = true
  try {
    await filesApi.uploadFile(project.value, props.appName, currentPath.value, file)
    toast.success(`${file.name} uploaded`)
    await loadFiles(currentPath.value)
  } catch {
    toast.error('Failed to upload file')
  } finally {
    uploading.value = false
    input.value = ''
  }
}

async function handleDownload(entry: filesApi.FileEntry) {
  const filePath = currentPath.value === '/'
    ? '/' + entry.name
    : currentPath.value + '/' + entry.name
  try {
    await filesApi.downloadFile(project.value, props.appName, filePath)
  } catch {
    toast.error('Failed to download file')
  }
}

const modal = useModal()

function openDiagnose() {
  modal.open(DiagnoseModal, {
    project: project.value,
    appName: props.appName,
  })
}

function openOptimise() {
  modal.open(OptimiseModal, {
    project: project.value,
    appName: props.appName,
  })
}
</script>

<template>
  <SidePanel :open="true" :label="`App details for ${appName}`" @close="emit('close')">
    <template #header>
      <h2 class="text-sm font-semibold text-slate-900 dark:text-slate-50">{{ appName }}</h2>
    </template>
    <template #actions>
      <button
        @click="openDiagnose"
        class="rounded-lg p-1.5 text-slate-400 hover:bg-slate-100 hover:text-slate-700 dark:hover:bg-slate-800 dark:hover:text-slate-200"
        title="AI Diagnose"
      >
        <Sparkles class="h-4 w-4" :stroke-width="1.75" />
      </button>
      <button
        @click="showImageForm = !showImageForm"
        class="rounded-lg p-1.5 text-slate-400 hover:bg-slate-100 hover:text-slate-700 dark:hover:bg-slate-800 dark:hover:text-slate-200"
        title="Update image"
      >
        <Package class="h-4 w-4" :stroke-width="1.75" />
      </button>
      <button
        @click="handleRestart"
        :disabled="restarting"
        class="rounded-lg p-1.5 text-slate-400 hover:bg-slate-100 hover:text-slate-700 dark:hover:bg-slate-800 dark:hover:text-slate-200"
        title="Rolling restart"
      >
        <RotateCw class="h-4 w-4" :class="restarting ? 'animate-spin' : ''" :stroke-width="1.75" />
      </button>
    </template>

    <!-- Why the app is not running. Above the tabs, because this is what the
         operator opened the app to find out. -->
    <div
      v-if="failingContainers.length > 0"
      data-testid="app-health-banner"
      class="border-b border-red-200 bg-red-50 px-5 py-3 dark:border-red-900 dark:bg-red-950/40"
    >
      <div class="mb-2 flex items-center gap-2">
        <AlertTriangle class="h-4 w-4 text-red-600 dark:text-red-400" :stroke-width="1.75" />
        <span class="text-sm font-medium text-red-800 dark:text-red-300">
          {{ failingContainers.length === 1 ? 'A container is not running' : `${failingContainers.length} containers are not running` }}
        </span>
      </div>
      <!-- Only the first failure inline. Replicas of one workload fail
           identically, so further excerpts add height rather than information;
           the full list is behind the button. -->
      <ContainerFailureEntry
        :pod="failingContainers[0].pod"
        :container="failingContainers[0].container"
      />
      <div class="mt-2 flex items-center gap-4">
        <button
          v-if="failingContainers.length > 1"
          data-testid="app-health-show-all"
          @click="openContainerErrors"
          class="text-xs font-medium text-red-700 underline underline-offset-2 hover:text-red-900 dark:text-red-400 dark:hover:text-red-200"
        >Show all {{ failingContainers.length }} errors</button>
        <button
          @click="activeTab = 'logs'"
          class="text-xs font-medium text-red-700 underline underline-offset-2 hover:text-red-900 dark:text-red-400 dark:hover:text-red-200"
        >Open logs</button>
      </div>
    </div>

    <div
      v-else-if="healthError"
      class="border-b border-amber-200 bg-amber-50 px-5 py-2.5 text-xs text-amber-800 dark:border-amber-900 dark:bg-amber-950/40 dark:text-amber-300"
    >
      Container status could not be read, so this app may be failing without saying so here. {{ healthError }}
    </div>

    <!-- Update image form -->
    <div v-if="showImageForm" class="flex items-center gap-2 border-b border-slate-200 px-5 py-3 dark:border-slate-800">
      <input
        v-model="newImage"
        type="text"
        placeholder="registry.example.com/app:v2"
        class="flex-1 rounded-md border border-slate-300 bg-white px-2.5 py-1.5 font-mono text-xs text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
        @keyup.enter="handleUpdateImage"
      />
      <button
        @click="handleUpdateImage"
        :disabled="!newImage || updatingImage"
        class="rounded-md bg-kipper-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-kipper-700 disabled:opacity-50"
      >
        {{ updatingImage ? 'Updating...' : 'Update' }}
      </button>
      <button @click="showImageForm = false" class="rounded-md p-1.5 text-slate-400 hover:bg-slate-100 dark:hover:bg-slate-800">
        <X class="h-3.5 w-3.5" />
      </button>
    </div>

    <!-- Tabs -->
    <TabBar
      :tabs="visibleTabs"
      :model-value="activeTab"
      density="compact"
      aria-label="App detail sections"
      @update:model-value="setActiveTab"
    />

    <!-- Content -->
    <div class="flex-1 overflow-y-auto">
      <!-- Logs tab -->
      <div v-if="activeTab === 'logs'" class="flex h-full flex-col">
        <!-- Log toolbar -->
        <div class="flex flex-wrap items-center gap-2 border-b border-slate-100 px-5 py-2 dark:border-slate-800">
          <!-- Mode toggle -->
          <div class="flex rounded-md border border-slate-200 dark:border-slate-700">
            <button
              @click="switchLogMode('loki')"
              class="px-2.5 py-1 text-xs font-medium transition-colors"
              :class="logMode === 'loki' ? 'bg-kipper-600 text-white' : 'text-slate-500 hover:text-slate-700 dark:text-slate-400'"
            >History</button>
            <button
              @click="switchLogMode('live')"
              class="px-2.5 py-1 text-xs font-medium transition-colors"
              :class="logMode === 'live' ? 'bg-kipper-600 text-white' : 'text-slate-500 hover:text-slate-700 dark:text-slate-400'"
              title="Real-time stream from a single pod"
            >Live</button>
          </div>

          <!-- Loki controls -->
          <template v-if="logMode === 'loki'">
            <input
              v-model="logSearch"
              type="text"
              placeholder="Search logs..."
              class="flex-1 rounded-md border border-slate-300 bg-white px-2.5 py-1 text-xs text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
              @keyup.enter="loadLokiLogs"
            />
            <select
              v-model="logSince"
              @change="loadLokiLogs"
              class="rounded-md border border-slate-300 bg-white px-2 py-1 text-xs text-slate-700 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-300"
            >
              <option value="15m">15 min</option>
              <option value="1h">1 hour</option>
              <option value="3h">3 hours</option>
              <option value="12h">12 hours</option>
              <option value="24h">24 hours</option>
              <option value="72h">3 days</option>
            </select>
            <button @click="loadLokiLogs" class="rounded-md bg-kipper-600 px-2.5 py-1 text-xs font-medium text-white hover:bg-kipper-700">
              Search
            </button>
          </template>

          <!-- Live controls -->
          <template v-if="logMode === 'live'">
            <span class="flex items-center gap-2 text-xs text-slate-500 dark:text-slate-400">
              <span class="inline-block h-2 w-2 rounded-full" :class="connected ? 'bg-emerald-500' : 'bg-slate-400'" />
              {{ connected ? 'Connected' : 'Disconnected' }}
            </span>
            <select
              v-if="livePods.length > 1"
              v-model="liveSelectedPod"
              @change="onLivePodChange"
              class="rounded-md border border-slate-300 bg-white px-2 py-1 text-xs text-slate-700 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-300"
            >
              <option value="">Auto</option>
              <option v-for="pod in livePods" :key="pod" :value="pod">{{ pod }}</option>
            </select>
            <span v-else-if="livePods.length === 1" class="font-mono text-xs text-slate-500 dark:text-slate-400">{{ livePods[0] }}</span>
            <select
              v-model.number="liveTailLines"
              @change="onLiveTailChange"
              class="rounded-md border border-slate-300 bg-white px-2 py-1 text-xs text-slate-700 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-300"
              title="Initial lines to load"
            >
              <option :value="100">100 lines</option>
              <option :value="250">250 lines</option>
              <option :value="500">500 lines</option>
              <option :value="1000">1000 lines</option>
            </select>
            <input
              v-model="liveFilter"
              type="text"
              placeholder="Filter..."
              class="w-32 rounded-md border border-slate-300 bg-white px-2 py-1 text-xs text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
            />
            <span class="flex-1" />
            <button @click="clear" class="text-xs text-slate-400 hover:text-slate-600 dark:hover:text-slate-300">Clear</button>
          </template>

          <LogAnalysis
            :logs="currentLogsText"
            :app-name="appName"
            :namespace="project"
          />
        </div>

        <!-- Loki log output -->
        <div v-if="logMode === 'loki'" class="flex-1 overflow-y-auto bg-slate-950 p-4 font-mono text-xs leading-relaxed text-slate-300">
          <div v-if="lokiLoading" class="text-slate-600">Loading logs...</div>
          <div v-else-if="lokiLogs.length">
            <div v-for="(entry, i) in lokiLogs" :key="i" class="flex gap-2">
              <span class="flex-shrink-0 text-slate-600">{{ formatLogTimestamp(entry.timestamp) }}</span>
              <span class="flex-shrink-0 text-kipper-500">{{ entry.pod }}</span>
              <span>{{ entry.line }}</span>
            </div>
          </div>
          <div v-else class="text-slate-600">
            <Terminal class="mb-2 h-5 w-5" />
            No logs found for the selected time range.
          </div>
        </div>

        <!-- Live log output -->
        <div v-if="logMode === 'live'" class="flex-1 overflow-y-auto bg-slate-950 p-4 font-mono text-xs leading-relaxed text-slate-300">
          <div v-for="(line, i) in filteredLines" :key="i">{{ line }}</div>
          <div v-if="!lines.length" class="text-slate-600">
            <Terminal class="mb-2 h-5 w-5" />
            Waiting for logs...
          </div>
        </div>
      </div>

      <!-- Env tab -->
      <div v-if="activeTab === 'env'" class="p-5">
        <!-- Bind service -->
        <div v-if="canWriteEnv && bindableServices.length" class="mb-4 space-y-2">
          <div class="flex flex-wrap items-center gap-2">
            <Link class="h-4 w-4 text-slate-400" />
            <select
              v-model="bindingService"
              class="rounded-md border border-slate-300 bg-white px-3 py-1.5 text-sm text-slate-900 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
            >
              <option value="">Bind a service...</option>
              <option v-for="svc in bindableServices" :key="svc.name" :value="svc.name">
                {{ svc.name }} ({{ svc.type }})
              </option>
            </select>
            <input
              v-model="bindingPrefix"
              type="text"
              placeholder="Prefix (e.g. DB_, ANALYTICS_)"
              class="w-40 rounded-md border border-slate-300 bg-white px-3 py-1.5 text-sm text-slate-900 placeholder-slate-400 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
            />
            <button
              @click="handleBind"
              :disabled="!bindingService || binding"
              class="rounded-md bg-kipper-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-kipper-700 disabled:opacity-50"
            >
              {{ binding ? 'Binding...' : 'Bind' }}
            </button>
          </div>

          <!-- Namespace picker — postgres/mysql (databases) and
               rabbitmq (vhost). Pick existing where listing works,
               or create new. RabbitMQ is create-new-only until a
               vhost-list endpoint ships. -->
          <div v-if="hasNamespacePicker(selectedServiceType)" class="ml-6 space-y-1.5 text-xs text-slate-600 dark:text-slate-400">
            <div class="flex items-center gap-3">
              <span class="capitalize">{{ namespaceLabel(selectedServiceType) }}:</span>
              <label v-if="canListNamespaces(selectedServiceType)" class="inline-flex items-center gap-1 cursor-pointer">
                <input type="radio" value="existing" v-model="bindingDbMode" :disabled="!bindingDatabases.length" />
                Pick existing
              </label>
              <label class="inline-flex items-center gap-1 cursor-pointer">
                <input type="radio" value="new" v-model="bindingDbMode" />
                Create new
              </label>
              <span v-if="bindingDatabasesLoading" class="text-slate-400">loading…</span>
            </div>
            <select
              v-if="bindingDbMode === 'existing' && canListNamespaces(selectedServiceType)"
              v-model="bindingDatabaseExisting"
              class="w-72 rounded-md border border-slate-300 bg-white px-3 py-1.5 text-sm text-slate-900 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
            >
              <option value="">Pick a {{ namespaceLabel(selectedServiceType) }}…</option>
              <option v-for="d in bindingDatabases" :key="d" :value="d">
                {{ d }}{{ d === bindingDefaultDatabase ? ' (service default)' : '' }}
              </option>
            </select>
            <div v-else class="space-y-1">
              <input
                v-model="bindingDatabaseNew"
                type="text"
                :placeholder="namespaceLabel(selectedServiceType) === 'vhost' ? 'new vhost name (e.g. orders)' : 'new database name'"
                class="w-72 rounded-md border border-amber-400 bg-white px-3 py-1.5 text-sm font-mono text-slate-900 dark:border-amber-700 dark:bg-slate-800 dark:text-slate-50"
              />
              <p class="text-amber-700 dark:text-amber-400">
                <span v-if="namespaceLabel(selectedServiceType) === 'vhost'">
                  Creates a new vhost on <span class="font-mono">{{ bindingService }}</span> and grants the kipper user full access. To share the default vhost (<span class="font-mono">/</span>), use "Pick existing".
                </span>
                <span v-else>
                  This creates a new empty database on <span class="font-mono">{{ bindingService }}</span>. Use "Pick existing" to attach to existing data instead.
                </span>
              </p>
            </div>
          </div>
        </div>

        <!-- Link an app -->
        <div v-if="canWriteEnv && linkableApps.length" class="mb-4 flex flex-wrap items-center gap-2">
          <Link class="h-4 w-4 text-slate-400" />
          <select
            v-model="linkingTarget"
            class="rounded-md border border-slate-300 bg-white px-3 py-1.5 text-sm text-slate-900 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
          >
            <option value="">Link an app...</option>
            <option v-for="name in linkableApps" :key="name" :value="name">
              {{ name }}
            </option>
          </select>
          <label v-if="linkingTarget" class="flex items-center gap-1.5 text-xs text-slate-500 dark:text-slate-400">
            <input type="checkbox" v-model="linkingPublic" class="rounded border-slate-300 text-kipper-600 focus:ring-kipper-500 dark:border-slate-600" />
            public
          </label>
          <button
            @click="handleLink"
            :disabled="!linkingTarget || linking"
            class="rounded-md bg-kipper-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-kipper-700 disabled:opacity-50"
          >
            {{ linking ? 'Linking...' : 'Link' }}
          </button>
        </div>

        <div v-if="envLoading" class="text-sm text-slate-500 dark:text-slate-400">Loading...</div>

        <div v-else class="space-y-3">
          <!-- Restart-to-apply banner: env changed but running pods still hold the old values -->
          <NoticeCallout v-if="envRestartPending" tone="warning" class="p-3">
            <div class="flex items-start gap-2">
              <RotateCw class="mt-0.5 h-4 w-4 flex-shrink-0 text-amber-600 dark:text-orange-300" />
              <div class="flex-1">
                <p class="text-xs font-medium text-amber-800 dark:text-orange-300">
                  Environment or secrets changed. The running pods keep the old values until you restart.
                </p>
                <button
                  v-if="canWriteEnv"
                  @click="handleRestart"
                  :disabled="restarting"
                  class="mt-2 rounded-md bg-amber-600 px-3 py-1 text-xs font-medium text-white hover:bg-amber-700 disabled:opacity-50"
                >
                  {{ restarting ? 'Restarting...' : 'Restart to apply' }}
                </button>
              </div>
            </div>
          </NoticeCallout>

          <!-- Direct env conflict warning -->
          <NoticeCallout v-if="envConflicts.length" tone="warning" class="p-3">
            <div class="flex items-start gap-2">
              <AlertTriangle class="mt-0.5 h-4 w-4 flex-shrink-0 text-amber-600 dark:text-orange-300" />
              <div class="flex-1">
                <p class="text-xs font-medium text-amber-800 dark:text-orange-300">
                  This deployment has direct env entries that override values set here
                </p>
                <div class="mt-1.5 flex flex-wrap gap-1">
                  <span
                    v-for="key in envConflicts"
                    :key="key"
                    class="rounded bg-amber-200 px-1.5 py-0.5 font-mono text-xs text-amber-800 dark:bg-amber-800 dark:text-amber-200"
                  >{{ key }}</span>
                </div>
                <button
                  v-if="canWriteEnv"
                  @click="fixEnvConflicts"
                  :disabled="fixingConflicts"
                  class="mt-2 rounded-md bg-amber-600 px-3 py-1 text-xs font-medium text-white hover:bg-amber-700 disabled:opacity-50"
                >
                  {{ fixingConflicts ? 'Removing...' : 'Remove direct entries' }}
                </button>
              </div>
            </div>
          </NoticeCallout>

          <!-- Injected vars from service bindings -->
          <div v-for="svcName in boundServices" :key="svcName" class="mb-3">
            <div class="mb-2 flex items-center gap-1.5">
              <Link class="h-3.5 w-3.5 text-slate-400" />
              <span class="text-xs font-medium text-slate-500 dark:text-slate-400">{{ svcName }}</span>
              <span class="text-[10px] text-slate-400 dark:text-slate-500">(via EnvFrom)</span>
              <button v-if="canWriteEnv" @click="handleUnbind(svcName)" class="ml-auto text-slate-400 hover:text-red-500" title="Unbind service">
                <X class="h-3.5 w-3.5" />
              </button>
            </div>
            <div class="space-y-1.5">
              <div
                v-for="v in injectedVars.filter(iv => iv.service === svcName)"
                :key="v.name"
                class="flex items-center gap-2 rounded-lg border border-slate-100 bg-slate-50 px-3 py-2 dark:border-slate-700 dark:bg-slate-800/50"
              >
                <span class="font-mono text-xs font-medium text-slate-500 dark:text-slate-400">{{ v.name }}</span>
                <span class="text-xs text-slate-400">=</span>
                <span class="flex-1 truncate font-mono text-xs text-slate-400 dark:text-slate-500">
                  {{ v.secret ? '••••••••' : (v.value || '') }}
                </span>
              </div>
            </div>
          </div>

          <!-- Linked apps -->
          <div v-if="links.length">
            <div class="mb-2 flex items-center gap-1.5">
              <Link class="h-3.5 w-3.5 text-slate-400" />
              <span class="text-xs font-medium text-slate-500 dark:text-slate-400">Linked apps</span>
            </div>
            <div class="space-y-1.5">
              <div
                v-for="link in links"
                :key="link.namespace + '/' + link.app"
                class="flex items-center gap-2 rounded-lg border border-slate-100 bg-slate-50 px-3 py-2 dark:border-slate-700 dark:bg-slate-800/50"
              >
                <span class="font-mono text-xs font-medium text-slate-500 dark:text-slate-400">{{ link.envVar }}</span>
                <span class="text-xs text-slate-400">=</span>
                <span v-if="link.open && link.injected" class="flex-1 truncate font-mono text-xs text-slate-400 dark:text-slate-500">{{ link.url }}</span>
                <span v-else-if="link.open" class="flex-1 truncate text-xs text-slate-400 dark:text-slate-500">{{ link.url }} — waiting for the app to pick it up</span>
                <span v-else class="flex-1 truncate text-xs text-amber-600 dark:text-amber-500">{{ link.reason || 'no traffic allowed yet' }}</span>
                <span class="rounded-full bg-slate-100 px-1.5 py-0.5 text-[10px] text-slate-400 dark:bg-slate-700 dark:text-slate-500">{{ link.app }}</span>
                <button v-if="canWriteEnv" @click="handleUnlink(link.app)" class="text-slate-400 hover:text-red-500" title="Unlink">
                  <X class="h-3.5 w-3.5" />
                </button>
              </div>
            </div>
          </div>

          <!-- Existing vars -->
          <div v-if="Object.keys(regularEnvVars).length" class="mb-2 flex items-center gap-1.5">
            <span class="text-xs font-medium text-slate-500 dark:text-slate-400">Environment variables</span>
          </div>
          <div
            v-for="(value, key) in regularEnvVars"
            :key="key"
            class="rounded-lg border border-slate-200 bg-slate-50 px-3 py-2 dark:border-slate-700 dark:bg-slate-800"
          >
            <!-- Editing mode -->
            <div v-if="canWriteEnv && editingEnv === key" class="space-y-2">
              <span class="font-mono text-xs font-medium text-slate-700 dark:text-slate-300">{{ key }}</span>
              <div class="flex items-center gap-2">
                <input
                  v-model="editEnvValue"
                  type="text"
                  class="flex-1 rounded-md border border-slate-300 bg-white px-2.5 py-1.5 font-mono text-xs text-slate-900 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50"
                />
                <button @click="saveEditEnv(key as string)" class="rounded-md bg-kipper-600 p-1.5 text-white hover:bg-kipper-700">
                  <Save class="h-3.5 w-3.5" />
                </button>
                <button @click="cancelEditEnv" class="rounded-md p-1.5 text-slate-400 hover:bg-slate-200 dark:hover:bg-slate-700">
                  <X class="h-3.5 w-3.5" />
                </button>
              </div>
            </div>

            <!-- Display mode -->
            <div v-else class="flex items-start gap-2">
              <span
                v-if="isSensitiveEnvVar(key as string, value)"
                title="This looks like a credential. Environment variables are stored in plain text and are copied by kip export. If it holds a sensitive value, move it to the Secrets tab."
                class="flex shrink-0 items-center"
              >
                <AlertTriangle class="h-3.5 w-3.5 text-amber-500" />
              </span>
              <span class="font-mono text-xs font-medium text-slate-700 dark:text-slate-300">{{ key }}</span>
              <span class="text-xs text-slate-400">=</span>
              <EnvVariableValue :template="value" :preview="previewByKey.get(key as string)" />
              <button v-if="canWriteEnv" @click="startEditEnv(key as string)" class="text-slate-400 hover:text-kipper-600 dark:hover:text-kipper-400" title="Edit">
                <Pencil class="h-3.5 w-3.5" />
              </button>
              <button v-if="canWriteEnv" @click="deleteEnvVar(key as string)" class="text-slate-400 hover:text-red-500" title="Delete">
                <Trash2 class="h-3.5 w-3.5" />
              </button>
            </div>
          </div>

          <div v-if="!Object.keys(envVars).length && !injectedVars.length && !envLoading" class="text-sm text-slate-500 dark:text-slate-400">
            No environment variables configured
          </div>

          <NoticeCallout v-if="envPreview?.refused?.length" tone="warning" class="mt-3 p-3">
            <div class="text-xs">
              <p class="font-medium text-amber-900 dark:text-amber-200">
                Kipper cannot read the credentials of
                {{ envPreview.refused.join(', ') }}
              </p>
              <p class="mt-0.5 text-amber-800 dark:text-amber-300">
                Variables from that binding are missing below, so references to them show as
                unresolved. Running pods keep the environment they were last given until this is
                fixed.
              </p>
            </div>
          </NoticeCallout>

          <EnvAvailableVariables
            v-if="canWriteEnv && envPreview?.available.length"
            :available="envPreview.available"
            :snippets="envPreview.snippets"
            class="mt-3"
            @insert="insertEnvReference"
            @use-snippet="useEnvSnippet"
          />

          <p v-if="!canWriteEnv" class="pt-2 text-xs text-slate-500 dark:text-slate-400">
            You have read access to this project. Changing environment variables, bindings and links
            needs the deployer role.
          </p>

          <!-- Add new -->
          <div v-if="canWriteEnv" class="flex items-end gap-2 pt-2">
            <div class="min-w-0 flex-1">
              <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Key</label>
              <input v-model="newEnvKey" type="text" placeholder="LOG_LEVEL" class="block w-full rounded-md border border-slate-300 bg-white px-2.5 py-1.5 font-mono text-xs text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
            </div>
            <div class="min-w-0 flex-1">
              <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Value</label>
              <input v-model="newEnvValue" type="text" placeholder="debug" class="block w-full rounded-md border border-slate-300 bg-white px-2.5 py-1.5 font-mono text-xs text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
            </div>
            <button @click="addEnvVar" :disabled="!newEnvKey" class="rounded-md bg-kipper-600 p-1.5 text-white hover:bg-kipper-700 disabled:opacity-50">
              <Plus class="h-4 w-4" />
            </button>
          </div>
        </div>
      </div>

      <!-- Secrets tab -->
      <div v-if="activeTab === 'secrets'" class="p-5">
        <div v-if="secretsLoading" class="text-sm text-slate-500 dark:text-slate-400">Loading...</div>

        <div v-else class="space-y-3">
          <!-- JSON toggle -->
          <div v-if="secretKeys.length" class="flex justify-end">
            <button @click="toggleJsonView" class="text-xs text-kipper-600 hover:text-kipper-700 dark:text-kipper-400">
              {{ showJsonView ? 'Key-value view' : 'JSON view' }}
            </button>
          </div>

          <!-- JSON view -->
          <div v-if="showJsonView" class="rounded-lg border border-slate-200 bg-slate-950 p-4 dark:border-slate-700">
            <pre class="font-mono text-xs text-slate-300 whitespace-pre-wrap">{{ secretsAsJson }}</pre>
          </div>

          <!-- Key-value view -->
          <template v-else>
            <div
              v-for="key in secretKeys"
              :key="key"
              class="rounded-lg border border-slate-200 bg-slate-50 px-3 py-2 dark:border-slate-700 dark:bg-slate-800"
            >
              <!-- Editing mode -->
              <div v-if="editingSecret === key" class="space-y-2">
                <span class="font-mono text-xs font-medium text-slate-700 dark:text-slate-300">{{ key }}</span>
                <div class="flex items-center gap-2">
                  <input
                    v-model="editSecretValue"
                    type="text"
                    class="flex-1 rounded-md border border-slate-300 bg-white px-2.5 py-1.5 font-mono text-xs text-slate-900 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50"
                  />
                  <button @click="saveEditSecret(key)" class="rounded-md bg-kipper-600 p-1.5 text-white hover:bg-kipper-700">
                    <Save class="h-3.5 w-3.5" />
                  </button>
                  <button @click="cancelEditSecret" class="rounded-md p-1.5 text-slate-400 hover:bg-slate-200 dark:hover:bg-slate-700">
                    <X class="h-3.5 w-3.5" />
                  </button>
                </div>
              </div>

              <!-- Display mode -->
              <div v-else class="flex items-center gap-2">
                <span class="font-mono text-xs font-medium text-slate-700 dark:text-slate-300">{{ key }}</span>
                <span class="text-xs text-slate-400">=</span>
                <span class="flex-1 truncate font-mono text-xs text-slate-600 dark:text-slate-400">
                  {{ revealedSecrets[key] ?? '••••••••' }}
                </span>
                <button @click="revealSecret(key)" class="text-slate-400 hover:text-slate-600 dark:hover:text-slate-300" title="Reveal">
                  <EyeOff v-if="revealedSecrets[key]" class="h-3.5 w-3.5" />
                  <Eye v-else class="h-3.5 w-3.5" />
                </button>
                <button @click="startEditSecret(key)" class="text-slate-400 hover:text-kipper-600 dark:hover:text-kipper-400" title="Edit">
                  <Pencil class="h-3.5 w-3.5" />
                </button>
                <button @click="deleteSecretKey(key)" class="text-slate-400 hover:text-red-500" title="Delete">
                  <Trash2 class="h-3.5 w-3.5" />
                </button>
              </div>
            </div>
          </template>

          <div v-if="!secretKeys.length && !secretsLoading" class="text-sm text-slate-500 dark:text-slate-400">
            No secrets configured
          </div>

          <!-- Add new -->
          <div class="flex items-end gap-2 pt-2">
            <div class="min-w-0 flex-1">
              <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Key</label>
              <input v-model="newSecretKey" type="text" placeholder="DATABASE_URL" class="block w-full rounded-md border border-slate-300 bg-white px-2.5 py-1.5 font-mono text-xs text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
            </div>
            <div class="min-w-0 flex-1">
              <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Value</label>
              <input v-model="newSecretValue" type="password" placeholder="••••••••" class="block w-full rounded-md border border-slate-300 bg-white px-2.5 py-1.5 font-mono text-xs text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
            </div>
            <button @click="addSecret" :disabled="!newSecretKey || !newSecretValue" class="rounded-md bg-kipper-600 p-1.5 text-white hover:bg-kipper-700 disabled:opacity-50">
              <Plus class="h-4 w-4" />
            </button>
          </div>
        </div>
      </div>

      <!-- Deploys tab — Deploy methods + Deploy history -->
      <div v-if="activeTab === 'deploys'" class="p-5 space-y-8">
        <!-- Deploy methods section (collapsed by default) -->
        <section>
          <button
            type="button"
            class="mb-3 flex items-center gap-1.5 text-xs font-semibold uppercase tracking-wide text-slate-500 hover:text-slate-700 dark:text-slate-400 dark:hover:text-slate-200"
            :aria-expanded="deployMethodsOpen"
            @click="deployMethodsOpen = !deployMethodsOpen"
          >
            <ChevronDown
              class="h-3.5 w-3.5 transition-transform"
              :class="{ 'rotate-180': deployMethodsOpen }"
              :stroke-width="2"
            />
            Deploy methods
          </button>

          <div v-if="deployMethodsOpen && (buildLoading || webhookLoading)" class="text-sm text-slate-500 dark:text-slate-400">Loading…</div>

          <div v-else-if="deployMethodsOpen" class="space-y-3">
            <!-- Image card -->
            <div
              class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800"
              data-testid="deploy-method-image"
            >
              <div class="mb-2 flex items-center justify-between gap-3">
                <div class="flex items-center gap-2">
                  <Package class="h-4 w-4 text-slate-500 dark:text-slate-400" :stroke-width="1.75" />
                  <span class="text-sm font-medium text-slate-900 dark:text-slate-50">Image</span>
                  <span
                    class="rounded-full px-2 py-0.5 text-[10px] font-medium"
                    :class="imageCardState === 'active'
                      ? 'bg-kipper-100 text-kipper-700 dark:bg-kipper-900 dark:text-kipper-300'
                      : 'bg-slate-100 text-slate-600 dark:bg-slate-700 dark:text-slate-400'"
                  >{{ imageCardState === 'active' ? 'active' : 'not configured' }}</span>
                </div>
              </div>

              <!-- Image-mode app (always active for !git_configured) -->
              <div v-if="imageCardState === 'active'" class="text-xs text-slate-500 dark:text-slate-400">
                <p>This app deploys pre-built images. Push a new version with <code class="rounded bg-slate-100 px-1 dark:bg-slate-800">kip deploy {{ appName }} --image &lt;ref&gt;</code>.</p>
                <p class="mt-1">See Deploy history below for what's been rolled out.</p>
              </div>

              <!-- Git-mode app — image is built automatically -->
              <div v-else class="text-xs text-slate-500 dark:text-slate-400">
                Images are built automatically from the git source. See the Git card below.
              </div>
            </div>

            <!-- Git card -->
            <div
              class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800"
              data-testid="deploy-method-git"
            >
              <div class="mb-2 flex items-center justify-between gap-3">
                <div class="flex items-center gap-2">
                  <GitBranch class="h-4 w-4 text-slate-500 dark:text-slate-400" :stroke-width="1.75" />
                  <span class="text-sm font-medium text-slate-900 dark:text-slate-50">Git source</span>
                  <span
                    class="rounded-full px-2 py-0.5 text-[10px] font-medium"
                    :class="gitCardState === 'active' ? 'bg-kipper-100 text-kipper-700 dark:bg-kipper-900 dark:text-kipper-300'
                      : gitCardState === 'error' ? 'bg-red-100 text-red-700 dark:bg-red-900 dark:text-red-300'
                      : 'bg-slate-100 text-slate-600 dark:bg-slate-700 dark:text-slate-400'"
                  >{{ gitCardState === 'active' ? 'active' : gitCardState === 'error' ? 'build failed' : 'not configured' }}</span>
                </div>
                <div v-if="buildStatus?.git_configured" class="flex shrink-0 gap-2">
                  <button
                    v-if="!gitEditing"
                    @click="openGitEdit"
                    class="rounded-md border border-slate-300 px-3 py-1 text-xs font-medium text-slate-700 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-700"
                  >Edit</button>
                  <button
                    v-if="!gitEditing"
                    @click="removeGitSource"
                    data-testid="remove-git-source"
                    class="rounded-md border border-slate-300 px-3 py-1 text-xs font-medium text-slate-700 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-700"
                  >Remove</button>
                  <button
                    v-if="buildStatus.phase === 'Building' || buildStatus.phase === 'Pending'"
                    @click="handleCancelBuild"
                    class="rounded-md border border-red-300 px-3 py-1 text-xs font-medium text-red-600 hover:bg-red-50 dark:border-red-700 dark:text-red-400 dark:hover:bg-red-950"
                  >Cancel</button>
                  <button
                    @click="handleRebuild"
                    :disabled="rebuilding"
                    class="rounded-md bg-kipper-600 px-3 py-1 text-xs font-medium text-white hover:bg-kipper-700 disabled:opacity-50"
                  >{{ rebuilding ? 'Building…' : 'Rebuild' }}</button>
                </div>
              </div>

              <div v-if="gitEditing" class="mb-3 space-y-2 rounded-md border border-slate-200 bg-white p-3 text-xs dark:border-slate-700 dark:bg-slate-900">
                <div class="font-medium text-slate-900 dark:text-slate-50">Edit git source</div>
                <label class="block">
                  <span class="text-slate-500 dark:text-slate-400">Repo URL</span>
                  <input v-model="gitForm.url" type="text" placeholder="https://github.com/org/repo.git"
                    class="mt-0.5 w-full rounded border border-slate-300 px-2 py-1 font-mono text-xs dark:border-slate-600 dark:bg-slate-800 dark:text-slate-200" />
                </label>
                <div class="grid grid-cols-1 gap-2 sm:grid-cols-2">
                  <label class="block">
                    <span class="text-slate-500 dark:text-slate-400">Branch</span>
                    <input v-model="gitForm.branch" type="text" placeholder="main"
                      class="mt-0.5 w-full rounded border border-slate-300 px-2 py-1 font-mono text-xs dark:border-slate-600 dark:bg-slate-800 dark:text-slate-200" />
                  </label>
                  <label class="block">
                    <span class="text-slate-500 dark:text-slate-400">Dockerfile path</span>
                    <input v-model="gitForm.dockerfile_path" type="text" placeholder="Dockerfile"
                      class="mt-0.5 w-full rounded border border-slate-300 px-2 py-1 font-mono text-xs dark:border-slate-600 dark:bg-slate-800 dark:text-slate-200" />
                  </label>
                </div>
                <label class="block">
                  <span class="text-slate-500 dark:text-slate-400">Build context</span>
                  <input v-model="gitForm.context" type="text" placeholder="."
                    class="mt-0.5 w-full rounded border border-slate-300 px-2 py-1 font-mono text-xs dark:border-slate-600 dark:bg-slate-800 dark:text-slate-200" />
                </label>
                <label class="block">
                  <span class="text-slate-500 dark:text-slate-400">Git token (only set when rotating; leave empty to keep the existing one)</span>
                  <input v-model="gitForm.token" type="password" autocomplete="new-password" placeholder="ghp_… or glpat-… or github_pat_…"
                    class="mt-0.5 w-full rounded border border-slate-300 px-2 py-1 font-mono text-xs dark:border-slate-600 dark:bg-slate-800 dark:text-slate-200" />
                </label>
                <div class="flex items-center justify-end gap-2 pt-1">
                  <button @click="gitEditing = false"
                    class="rounded-md border border-slate-300 px-3 py-1 text-xs font-medium text-slate-700 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-700"
                  >Cancel</button>
                  <button @click="saveGitEdit" :disabled="gitSaving"
                    class="rounded-md bg-kipper-600 px-3 py-1 text-xs font-medium text-white hover:bg-kipper-700 disabled:opacity-50"
                  >{{ gitSaving ? 'Saving…' : 'Save' }}</button>
                </div>
                <p class="text-[11px] text-slate-500 dark:text-slate-400">
                  Saving does not trigger a build. Click Rebuild after saving to start a build with the new settings.
                </p>
              </div>

              <div v-if="buildStatus?.git_configured" class="space-y-2 text-xs">
                <div class="flex items-center gap-2">
                  <span class="text-slate-500 dark:text-slate-400">Repo</span>
                  <span class="truncate font-mono text-slate-700 dark:text-slate-300">{{ buildStatus.git_url }}</span>
                  <span class="text-slate-400 dark:text-slate-500">@</span>
                  <span class="font-mono text-slate-700 dark:text-slate-300">{{ buildStatus.git_branch }}</span>
                </div>
                <div v-if="buildStatus.phase !== 'none'" class="flex items-center gap-1.5">
                  <span
                    class="inline-block h-2 w-2 rounded-full"
                    :class="{
                      'bg-emerald-500': buildStatus.phase === 'Succeeded',
                      'bg-red-500': buildStatus.phase === 'Failed',
                      'bg-amber-500 animate-pulse': buildStatus.phase === 'Building' || buildStatus.phase === 'Pending',
                      'bg-slate-400': buildStatus.phase === 'none' || buildStatus.phase === 'Discarded',
                    }"
                  />
                  <span class="font-medium text-slate-700 dark:text-slate-300">{{ buildStatus.phase }}</span>
                  <span v-if="buildStatus.commit" class="font-mono text-slate-500 dark:text-slate-400">{{ buildStatus.commit.slice(0, 8) }}</span>
                  <span v-if="buildStatus.completedAt" class="text-slate-500 dark:text-slate-400">· {{ formatTimeAgo(buildStatus.completedAt) }}</span>
                </div>

                <NoticeCallout
                  v-if="buildStatus.message"
                  :tone="buildStatus.phase === 'Discarded' ? 'warning' : 'danger'"
                  class="p-2 text-xs"
                  :class="buildStatus.phase === 'Discarded' ? 'text-amber-800 dark:text-slate-300' : 'text-red-700 dark:text-slate-300'"
                >
                  {{ buildStatus.message }}
                </NoticeCallout>

                <div class="flex items-center justify-between">
                  <button
                    type="button"
                    class="font-medium text-kipper-600 hover:text-kipper-700 dark:text-kipper-400 dark:hover:text-kipper-300"
                    @click="sourceDetailsOpen = !sourceDetailsOpen"
                  >{{ sourceDetailsOpen ? 'Hide build details' : 'Show build details' }}</button>
                  <span v-if="buildStatus.credentials_secret" class="flex items-center gap-1.5 font-mono text-slate-500 dark:text-slate-400">
                    auth: {{ buildStatus.credentials_secret }}
                    <button
                      type="button"
                      @click="revealGitTokenOpen = true"
                      title="Reveal git token"
                      class="rounded p-0.5 text-slate-400 hover:bg-slate-200 hover:text-slate-600 dark:hover:bg-slate-700 dark:hover:text-slate-300"
                    >
                      <Eye class="h-3.5 w-3.5" :stroke-width="1.75" />
                    </button>
                  </span>
                </div>

                <div v-if="sourceDetailsOpen" class="space-y-2 border-t border-slate-200 pt-3 dark:border-slate-700">
                  <div v-if="buildStatus.startedAt" class="flex items-center gap-2">
                    <span class="w-20 text-slate-500 dark:text-slate-400">Started</span>
                    <span class="text-slate-700 dark:text-slate-300">{{ formatDateTime(buildStatus.startedAt) }}</span>
                  </div>
                  <div v-if="buildStatus.completedAt" class="flex items-center gap-2">
                    <span class="w-20 text-slate-500 dark:text-slate-400">Finished</span>
                    <span class="text-slate-700 dark:text-slate-300">{{ formatDateTime(buildStatus.completedAt) }}</span>
                  </div>

                  <!-- Build logs -->
                  <div v-if="buildLogsVisible" class="rounded-md border border-slate-200 dark:border-slate-700">
                    <div class="flex items-center justify-between bg-slate-100 px-3 py-1.5 dark:bg-slate-900">
                      <span class="text-xs font-semibold text-slate-900 dark:text-slate-50">Build logs</span>
                      <button
                        @click="buildLogsVisible = false; disconnectBuildLogs()"
                        class="text-xs text-slate-400 hover:text-slate-600 dark:hover:text-slate-300"
                      >Close</button>
                    </div>
                    <div ref="buildLogContainer" class="max-h-80 overflow-auto bg-slate-900 p-3 font-mono text-[11px] leading-relaxed text-slate-300">
                      <div v-if="buildLogLines.length === 0" class="text-slate-500">Waiting for build logs…</div>
                      <div v-for="(line, i) in buildLogLines" :key="i">{{ line }}</div>
                    </div>
                  </div>
                  <button
                    v-else-if="buildStatus.phase === 'Building' || buildStatus.phase === 'Pending' || buildStatus.phase === 'Succeeded' || buildStatus.phase === 'Failed' || buildStatus.phase === 'Discarded'"
                    @click="streamBuildLogs"
                    class="text-xs font-medium text-kipper-600 hover:text-kipper-700 dark:text-kipper-400"
                  >Show build logs</button>
                </div>

                <!-- Credential health warnings -->
                <NoticeCallout
                  v-if="buildStatus.git_credential_valid === false"
                  tone="warning"
                  class="flex items-start gap-2 p-2"
                >
                  <AlertTriangle class="mt-0.5 h-3.5 w-3.5 flex-shrink-0 text-amber-600 dark:text-orange-300" :stroke-width="1.75" />
                  <span class="text-amber-800 dark:text-slate-300">
                    Git credentials
                    <span v-if="buildStatus.credentials_secret" class="font-mono">({{ buildStatus.credentials_secret }})</span>
                    may be expired. Builds will fail until the token is rotated.
                    <RouterLink to="/settings" class="ml-1 font-medium underline dark:text-orange-300">Settings → Git Credentials</RouterLink>
                  </span>
                </NoticeCallout>
              </div>

              <div v-else class="text-xs text-slate-500 dark:text-slate-400">
                Builds from a git repo on every push. Deploy with <code class="rounded bg-slate-100 px-1 dark:bg-slate-800">kip deploy {{ appName }} --git &lt;url&gt;</code>.
              </div>
            </div>

            <!-- Webhook card -->
            <div
              class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800"
              data-testid="deploy-method-webhook"
            >
              <div class="mb-2 flex items-center justify-between gap-3">
                <div class="flex items-center gap-2">
                  <Link class="h-4 w-4 text-slate-500 dark:text-slate-400" :stroke-width="1.75" />
                  <span class="text-sm font-medium text-slate-900 dark:text-slate-50">CI webhook</span>
                  <span
                    class="rounded-full px-2 py-0.5 text-[10px] font-medium"
                    :class="webhookEnabled ? 'bg-kipper-100 text-kipper-700 dark:bg-kipper-900 dark:text-kipper-300'
                      : 'bg-slate-100 text-slate-600 dark:bg-slate-700 dark:text-slate-400'"
                  >{{ webhookEnabled ? 'active' : 'not configured' }}</span>
                </div>
                <div v-if="webhookEnabled && authStore.isDeployer" class="flex shrink-0 gap-2">
                  <button
                    @click="removeWebhook"
                    class="rounded-md border border-red-300 px-3 py-1 text-xs font-medium text-red-600 hover:bg-red-50 dark:border-red-700 dark:text-red-400 dark:hover:bg-red-950"
                  >Remove</button>
                  <button
                    @click="generateWebhook"
                    class="rounded-md bg-kipper-600 px-3 py-1 text-xs font-medium text-white hover:bg-kipper-700"
                  >Regenerate</button>
                </div>
              </div>

              <div v-if="!webhookEnabled" class="flex items-center justify-between gap-3 text-xs">
                <span class="text-slate-500 dark:text-slate-400">Trigger deploys from your CI pipeline.</span>
                <button
                  v-if="authStore.isDeployer"
                  @click="generateWebhook"
                  class="shrink-0 rounded-md bg-kipper-600 px-3 py-1 text-xs font-medium text-white hover:bg-kipper-700"
                >Generate webhook URL</button>
              </div>

              <div v-else class="space-y-3 text-xs">
                <div>
                  <label class="mb-1 block font-medium text-slate-600 dark:text-slate-400">URL</label>
                  <div class="flex items-center gap-2">
                    <input
                      :value="webhookUrl"
                      readonly
                      class="flex-1 rounded-md border border-slate-300 bg-white px-2.5 py-1.5 font-mono text-slate-700 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-300"
                    />
                    <button @click="copyWebhook(webhookUrl, 'url')" class="rounded-md border border-slate-300 p-1.5 text-slate-500 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-700">
                      <Check v-if="webhookCopied === 'url'" class="h-3.5 w-3.5 text-green-500" />
                      <Copy v-else class="h-3.5 w-3.5" />
                    </button>
                  </div>
                </div>

                <div>
                  <label class="mb-1 block font-medium text-slate-600 dark:text-slate-400">Token</label>
                  <div class="flex items-center gap-2">
                    <input
                      :value="webhookTokenVisible ? webhookToken : '••••••••••••••••••••••••••••••••'"
                      readonly
                      class="flex-1 rounded-md border border-slate-300 bg-white px-2.5 py-1.5 font-mono text-slate-700 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-300"
                    />
                    <button @click="webhookTokenVisible = !webhookTokenVisible" class="rounded-md border border-slate-300 p-1.5 text-slate-500 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-700">
                      <EyeOff v-if="webhookTokenVisible" class="h-3.5 w-3.5" />
                      <Eye v-else class="h-3.5 w-3.5" />
                    </button>
                    <button @click="copyWebhook(webhookToken, 'token')" class="rounded-md border border-slate-300 p-1.5 text-slate-500 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-700">
                      <Check v-if="webhookCopied === 'token'" class="h-3.5 w-3.5 text-green-500" />
                      <Copy v-else class="h-3.5 w-3.5" />
                    </button>
                  </div>
                </div>

                <div class="rounded-md bg-slate-100 p-2.5 dark:bg-slate-900">
                  <p class="mb-1 font-medium text-slate-600 dark:text-slate-400">Usage</p>
                  <code class="block whitespace-pre-wrap break-all text-slate-600 dark:text-slate-400">curl -X POST "{{ webhookUrl }}" \
  -H "X-Kipper-Token: &lt;token&gt;" \
  -H "Content-Type: application/json" \
  -d '{"image": "your-image:tag"}'</code>
                </div>
              </div>
            </div>

            <!-- Registry credential warning — applies to both image and git apps -->
            <NoticeCallout
              v-if="buildStatus?.registry_valid === false"
              tone="warning"
              class="flex items-start gap-2 p-2 text-xs"
            >
              <AlertTriangle class="mt-0.5 h-3.5 w-3.5 flex-shrink-0 text-amber-600 dark:text-orange-300" :stroke-width="1.75" />
              <span class="text-amber-800 dark:text-slate-300">
                Registry pull secret credentials may be expired. New pods will fail to start until they are updated.
                <RouterLink to="/settings" class="ml-1 font-medium underline dark:text-orange-300">Settings → Container Registries</RouterLink>
              </span>
            </NoticeCallout>
          </div>
        </section>

        <!-- Deploy history section -->
        <section>
          <h3 class="mb-3 text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">Deploy history</h3>

          <div v-if="historyLoading" class="text-sm text-slate-500 dark:text-slate-400">Loading...</div>

          <div v-else-if="deployHistory.length" class="space-y-2">
            <div
              v-for="(entry, i) in deployHistory"
              :key="entry.revision"
              class="flex items-center justify-between rounded-lg border px-4 py-3"
              :class="i === 0
                ? 'border-kipper-200 bg-kipper-50 dark:border-kipper-800 dark:bg-kipper-950'
                : 'border-slate-100 bg-slate-50 dark:border-slate-700 dark:bg-slate-800'"
            >
              <div class="min-w-0">
                <div class="flex flex-wrap items-center gap-2">
                  <span class="font-mono text-xs font-semibold text-slate-700 dark:text-slate-300">#{{ entry.revision }}</span>
                  <span
                    class="rounded-full px-2 py-0.5 text-xs font-medium"
                    :class="{
                      'bg-kipper-100 text-kipper-700 dark:bg-kipper-900 dark:text-kipper-300': entry.trigger === 'webhook',
                      'bg-slate-100 text-slate-600 dark:bg-slate-700 dark:text-slate-400': entry.trigger === 'manual',
                      'bg-amber-100 text-amber-700 dark:bg-amber-900 dark:text-amber-300': entry.trigger === 'rollback',
                      'bg-purple-100 text-purple-700 dark:bg-purple-900 dark:text-purple-300': entry.trigger.startsWith('promote'),
                    }"
                  >{{ entry.trigger.startsWith('promote:') ? `promoted from ${entry.trigger.slice(8)}` : entry.trigger }}</span>
                  <span v-if="i === 0" class="text-xs font-medium text-kipper-600 dark:text-kipper-400">current</span>
                </div>
                <div class="mt-1 flex items-center gap-3 text-xs text-slate-500 dark:text-slate-400">
                  <span class="font-mono truncate max-w-xs">{{ entry.image }}</span>
                  <span v-if="entry.commit" class="font-mono">{{ entry.commit.slice(0, 8) }}</span>
                  <span>{{ formatTimeAgo(entry.timestamp) }}</span>
                </div>
              </div>
              <button
                v-if="i > 0"
                @click="handleRollback(entry.revision)"
                :disabled="rollingBack"
                class="inline-flex items-center gap-1 rounded-md border border-slate-300 px-2.5 py-1 text-xs font-medium text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-700"
                title="Rollback to this version"
              >
                <Undo2 class="h-3 w-3" />
                Rollback
              </button>
            </div>
          </div>

          <div v-else class="py-8 text-center text-sm text-slate-500 dark:text-slate-400">
            No deploy history yet
          </div>
        </section>
      </div>

      <!-- Resources tab -->
      <div v-if="activeTab === 'resources'" class="p-5">
        <div v-if="resourcesLoading" class="text-sm text-slate-500 dark:text-slate-400">Loading...</div>
        <div v-else class="space-y-6">
          <div class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800">
            <div class="mb-3 flex items-start justify-between gap-3">
              <div>
                <p class="text-sm font-medium text-slate-900 dark:text-slate-50">Resource limits</p>
                <p class="text-xs text-slate-500 dark:text-slate-400">
                  Drag a slider to preview a new limit; Apply restarts the pod with the new value.
                </p>
              </div>
              <button
                type="button"
                class="text-xs font-medium text-kipper-600 hover:text-kipper-700 dark:text-kipper-400 dark:hover:text-kipper-300"
                @click="resourcesAdvanced = !resourcesAdvanced"
              >{{ resourcesAdvanced ? 'Hide request controls' : 'Show request controls (advanced)' }}</button>
            </div>

            <div class="grid grid-cols-1 gap-6 md:grid-cols-2">
              <div>
                <h4 class="mb-2 text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">Memory</h4>
                <ResourceControl
                  kind="memory"
                  :usage="perPodMemoryBytes"
                  :limit="memoryLimitBytes"
                  :applying="resourcesSaving"
                  size="md"
                  @apply="applyMemoryLimit"
                />
                <div v-if="memorySparkline.length > 1" class="mt-2 flex items-center justify-center gap-2 text-[10px] text-slate-400 dark:text-slate-500">
                  <span class="uppercase tracking-wide">Last hour</span>
                  <MetricSparkline :data="memorySparkline" :width="180" :height="28" color="#0ea5e9" />
                </div>
              </div>
              <div>
                <h4 class="mb-2 text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">CPU</h4>
                <ResourceControl
                  kind="cpu"
                  :usage="perPodCpuMillis"
                  :limit="cpuLimitMillis"
                  :throttling-pct="cpuThrottlingPct"
                  :applying="resourcesSaving"
                  size="md"
                  @apply="applyCpuLimit"
                />
                <div v-if="cpuSparkline.length > 1" class="mt-2 flex items-center justify-center gap-2 text-[10px] text-slate-400 dark:text-slate-500">
                  <span class="uppercase tracking-wide">Last hour</span>
                  <MetricSparkline :data="cpuSparkline" :width="180" :height="28" color="#a855f7" />
                </div>
              </div>
            </div>

            <p v-if="usagePodCount > 1" class="mt-3 text-xs text-slate-500 dark:text-slate-400">
              Gauges show the average usage across {{ usagePodCount }} replicas. See per-pod breakdown below.
            </p>

            <div v-if="resourcesAdvanced" class="mt-4 border-t border-slate-200 pt-4 dark:border-slate-700">
              <p class="mb-3 text-xs text-slate-500 dark:text-slate-400">
                Request is reserved on the node. Limit is the cap. For JVM apps, set request low (e.g. 100m) and limit high (e.g. 1000m) so cold-start JIT can finish without reserving a full core. Saving here updates request and limit independently.
              </p>
              <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
                <div>
                  <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">CPU request</label>
                  <input
                    v-model="cpuRequest"
                    type="text"
                    placeholder="e.g. 100m"
                    class="w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50"
                  />
                </div>
                <div>
                  <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">CPU limit</label>
                  <input
                    v-model="cpuLimit"
                    type="text"
                    placeholder="e.g. 1000m"
                    class="w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50"
                  />
                </div>
                <div>
                  <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Memory request</label>
                  <input
                    v-model="memoryRequest"
                    type="text"
                    placeholder="e.g. 2Gi"
                    class="w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50"
                  />
                </div>
                <div>
                  <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Memory limit</label>
                  <input
                    v-model="memoryLimit"
                    type="text"
                    placeholder="e.g. 2Gi"
                    class="w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50"
                  />
                </div>
              </div>
              <div class="mt-3 flex justify-end">
                <SaveButton :saving="resourcesSaving" label="Save request &amp; limit" @click="saveResources" />
              </div>
            </div>

            <div v-if="usagePodCount > 1" class="mt-4 border-t border-slate-200 pt-4 dark:border-slate-700">
              <div class="mb-2 flex items-center justify-between">
                <h4 class="text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">Per replica</h4>
                <RouterLink
                  :to="{ name: 'app-pods', params: { name: appName }, query: { ns: project } }"
                  class="inline-flex items-center gap-0.5 text-xs font-medium text-slate-500 hover:text-slate-900 dark:text-slate-400 dark:hover:text-slate-100"
                >
                  Open full view
                  <ChevronRight class="h-3 w-3" />
                </RouterLink>
              </div>
              <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
                <div
                  v-for="row in perPodUsage"
                  :key="row.pod"
                  class="rounded-md border border-slate-200 bg-white p-3 dark:border-slate-700 dark:bg-slate-900"
                >
                  <p class="mb-2 truncate text-xs font-medium text-slate-700 dark:text-slate-300" :title="row.pod">{{ row.pod }}</p>
                  <div class="grid grid-cols-2 gap-2">
                    <ResourceControl
                      kind="memory"
                      :usage="row.memory"
                      :limit="memoryLimitBytes"
                      size="sm"
                      readonly
                    />
                    <ResourceControl
                      kind="cpu"
                      :usage="row.cpu"
                      :limit="cpuLimitMillis"
                      size="sm"
                      readonly
                    />
                  </div>
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- Settings tab -->
      <div v-if="activeTab === 'settings'" class="p-5">
        <div v-if="settingsLoading" class="text-sm text-slate-500 dark:text-slate-400">Loading...</div>

        <div v-else class="space-y-6">
          <!-- Route -->
          <div class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800">
            <div class="flex items-center justify-between mb-3">
              <div class="flex items-center gap-3">
                <Globe class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
                <div>
                  <div class="flex items-center gap-2">
                    <p class="text-sm font-medium text-slate-900 dark:text-slate-50">Public route</p>
                    <span
                      v-if="routeEnabled && routeHealth"
                      class="inline-flex items-center gap-1 rounded-full px-1.5 py-0.5 text-[10px] font-medium"
                      :class="routeHealth.tls_ready
                        ? 'bg-emerald-50 text-emerald-700 dark:bg-emerald-950 dark:text-emerald-400'
                        : 'bg-amber-50 text-amber-700 dark:bg-amber-950 dark:text-amber-400'"
                      :title="routeHealth.message"
                    >
                      <Shield class="h-3 w-3" :stroke-width="2" />
                      {{ routeHealth.tls_ready ? 'TLS active' : routeHealth.ingress_ready ? 'TLS pending' : 'Provisioning' }}
                    </span>
                  </div>
                  <p class="text-xs text-slate-500 dark:text-slate-400">Expose this app via HTTPS with automatic TLS</p>
                </div>
              </div>
              <button
                @click="routeEnabled = !routeEnabled"
                class="relative inline-flex h-6 w-11 items-center rounded-full transition-colors"
                :class="routeEnabled ? 'bg-kipper-600' : 'bg-slate-300 dark:bg-slate-600'"
              >
                <span
                  class="inline-block h-4 w-4 rounded-full bg-white transition-transform"
                  :class="routeEnabled ? 'translate-x-6' : 'translate-x-1'"
                />
              </button>
            </div>
            <div v-if="routeEnabled" class="space-y-3">
              <div>
                <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Hostname</label>
                <input
                  v-model="routeHost"
                  type="text"
                  :placeholder="props.appName + '-' + '(auto-generated).kipper.run'"
                  class="w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50 dark:placeholder-slate-500"
                />
                <p class="mt-1 text-[10px] text-slate-400 dark:text-slate-500">Leave empty for auto-generated kipper.run subdomain</p>
              </div>
              <div>
                <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Path</label>
                <input
                  v-model="routePath"
                  type="text"
                  placeholder="/"
                  class="w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50 dark:placeholder-slate-500"
                />
              </div>
              <div>
                <div class="mb-1 flex items-center justify-between">
                  <label class="block text-xs font-medium text-slate-600 dark:text-slate-400">Redirect domains</label>
                  <button
                    v-if="authStore.isDeployer"
                    @click="addRouteRedirectFrom"
                    :disabled="routeRedirectFrom.length >= 10"
                    title="Up to 10 redirect domains per route"
                    class="inline-flex items-center gap-1 rounded-md border border-slate-300 px-2 py-1 text-xs font-medium text-slate-600 hover:bg-slate-100 disabled:cursor-not-allowed disabled:opacity-50 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
                  >
                    <Plus class="h-3 w-3" /> Add
                  </button>
                </div>
                <div v-if="routeRedirectFrom.length" class="space-y-2">
                  <div v-for="(_, i) in routeRedirectFrom" :key="i" class="flex items-center gap-2">
                    <input
                      v-model="routeRedirectFrom[i]"
                      type="text"
                      placeholder="www.example.com"
                      class="min-w-0 flex-1 rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50 dark:placeholder-slate-500"
                    />
                    <button
                      v-if="authStore.isDeployer"
                      @click="removeRouteRedirectFrom(i)"
                      class="rounded p-1 text-slate-400 hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-950"
                    >
                      <Minus class="h-3 w-3" />
                    </button>
                  </div>
                </div>
                <p class="mt-1 text-[10px] text-slate-400 dark:text-slate-500">Each domain answers with a permanent redirect to the hostname above. Point its DNS record at this cluster first.</p>
              </div>
              <div v-if="routeURL" class="flex items-center gap-2">
                <a :href="routeURL" target="_blank" rel="noopener" class="text-sm text-kipper-600 hover:text-kipper-700 dark:text-kipper-400">
                  {{ routeURL }}
                </a>
              </div>

              <!-- DNS verification -->
              <NoticeCallout
                v-if="routeDnsStatus && routeDnsStatus.status !== 'disabled'"
                :tone="routeDnsStatus.status === 'mismatch' ? 'danger' : routeDnsStatus.status === 'unresolved' ? 'warning' : 'success'"
                class="p-3 text-xs"
                :class="{
                  'text-emerald-800 dark:text-emerald-300': routeDnsStatus.status === 'ok' || routeDnsStatus.status === 'gateway' || routeDnsStatus.status === 'wildcard',
                  'text-amber-900 dark:text-orange-300': routeDnsStatus.status === 'unresolved',
                  'text-red-900 dark:text-rose-300': routeDnsStatus.status === 'mismatch',
                }"
              >
                <div class="flex items-start justify-between gap-3">
                  <div class="flex items-start gap-2">
                    <CheckCircle2
                      v-if="routeDnsStatus.status === 'ok' || routeDnsStatus.status === 'gateway' || routeDnsStatus.status === 'wildcard'"
                      class="mt-0.5 h-4 w-4 shrink-0"
                      :stroke-width="2"
                    />
                    <AlertTriangle
                      v-else
                      class="mt-0.5 h-4 w-4 shrink-0"
                      :stroke-width="2"
                    />
                    <div class="min-w-0">
                      <p class="font-medium">{{ routeDnsStatus.message }}</p>
                      <div
                        v-if="routeDnsStatus.status === 'mismatch' || routeDnsStatus.status === 'unresolved'"
                        class="mt-2 space-y-1.5"
                      >
                        <div v-if="routeDnsStatus.expected_ips && routeDnsStatus.expected_ips.length" class="flex flex-wrap items-center gap-2">
                          <span class="opacity-75">Point your A record to:</span>
                          <code class="rounded bg-white/60 px-1.5 py-0.5 font-mono text-[11px] dark:bg-black/30">{{ routeDnsStatus.expected_ips.join(', ') }}</code>
                          <button
                            @click="copyRouteIP"
                            class="inline-flex items-center gap-1 rounded px-1.5 py-0.5 text-[11px] hover:bg-white/60 dark:hover:bg-black/30"
                            type="button"
                            title="Copy IP"
                          >
                            <Check v-if="routeIPCopied" class="h-3 w-3" :stroke-width="2.5" />
                            <Copy v-else class="h-3 w-3" :stroke-width="2" />
                          </button>
                        </div>
                        <div v-if="routeDnsStatus.status === 'mismatch' && routeDnsStatus.resolved_ips && routeDnsStatus.resolved_ips.length" class="flex flex-wrap items-center gap-2 opacity-75">
                          <span>Currently resolves to:</span>
                          <code class="rounded bg-white/60 px-1.5 py-0.5 font-mono text-[11px] dark:bg-black/30">{{ routeDnsStatus.resolved_ips.join(', ') }}</code>
                        </div>
                      </div>
                      <button
                        v-if="routeDnsStatus.status === 'wildcard'"
                        @click="checkRouteDns({ verify: true })"
                        :disabled="routeDnsChecking"
                        type="button"
                        class="mt-2 inline-flex items-center gap-1 text-[11px] underline-offset-2 hover:underline disabled:opacity-50"
                      >
                        Verify wildcard anyway
                      </button>
                    </div>
                  </div>
                  <button
                    v-if="routeDnsStatus.status !== 'gateway'"
                    @click="checkRouteDns()"
                    :disabled="routeDnsChecking"
                    type="button"
                    class="shrink-0 rounded p-1 hover:bg-white/60 disabled:opacity-50 dark:hover:bg-black/30"
                    title="Check again"
                  >
                    <RefreshCw class="h-3.5 w-3.5" :stroke-width="2" :class="{ 'animate-spin': routeDnsChecking }" />
                  </button>
                </div>
              </NoticeCallout>

              <SaveButton :saving="routeSaving" label="Save route" @click="saveRoute" />
            </div>
            <div v-else-if="routeURL">
              <SaveButton :saving="routeSaving" label="Remove route" @click="saveRoute" />
            </div>
          </div>

          <!-- Security headers toggle -->
          <div class="flex items-center justify-between rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800">
            <div class="flex items-center gap-3">
              <Shield class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
              <div>
                <p class="text-sm font-medium text-slate-900 dark:text-slate-50">Security headers</p>
                <p class="text-xs text-slate-500 dark:text-slate-400">HSTS, X-Frame-Options, CSP, XSS protection, referrer policy</p>
              </div>
            </div>
            <button
              @click="securityHeaders = !securityHeaders"
              class="relative inline-flex h-6 w-11 items-center rounded-full transition-colors"
              :class="securityHeaders ? 'bg-kipper-600' : 'bg-slate-300 dark:bg-slate-600'"
            >
              <span
                class="inline-block h-4 w-4 rounded-full bg-white transition-transform"
                :class="securityHeaders ? 'translate-x-6' : 'translate-x-1'"
              />
            </button>
          </div>

          <!-- Instance ID header toggle -->
          <div class="flex items-center justify-between rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800">
            <div class="flex items-center gap-3">
              <Globe class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
              <div>
                <p class="text-sm font-medium text-slate-900 dark:text-slate-50">Instance ID header</p>
                <p class="text-xs text-slate-500 dark:text-slate-400">Adds X-Instance-ID response header identifying which pod served the request</p>
              </div>
            </div>
            <button
              @click="instanceHeader = !instanceHeader"
              class="relative inline-flex h-6 w-11 items-center rounded-full transition-colors"
              :class="instanceHeader ? 'bg-kipper-600' : 'bg-slate-300 dark:bg-slate-600'"
            >
              <span
                class="inline-block h-4 w-4 rounded-full bg-white transition-transform"
                :class="instanceHeader ? 'translate-x-6' : 'translate-x-1'"
              />
            </button>
          </div>

          <!-- CSP allowlist -->
          <div v-if="securityHeaders" class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800">
            <div class="flex items-center gap-3 mb-3">
              <Shield class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
              <div>
                <p class="text-sm font-medium text-slate-900 dark:text-slate-50">CSP allowlist</p>
                <p class="text-xs text-slate-500 dark:text-slate-400">External domains allowed in the Content Security Policy</p>
              </div>
            </div>
            <input
              v-model="cspAllowlist"
              type="text"
              placeholder="fonts.googleapis.com, cdn.example.com"
              class="w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50 dark:placeholder-slate-500"
            />
            <p class="mt-1.5 text-[10px] text-slate-400 dark:text-slate-500">Comma-separated. Added to style-src, font-src, script-src, and connect-src.</p>
          </div>

          <!-- Rate limit -->
          <div class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800">
            <div class="flex items-center gap-3 mb-3">
              <Shield class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
              <div>
                <p class="text-sm font-medium text-slate-900 dark:text-slate-50">Rate limiting</p>
                <p class="text-xs text-slate-500 dark:text-slate-400">Maximum requests per second per IP address</p>
              </div>
            </div>
            <div class="flex items-center gap-3">
              <input
                v-model.number="rateLimit"
                type="number"
                min="0"
                placeholder="0"
                class="w-32 rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50"
              />
              <span class="text-xs text-slate-500 dark:text-slate-400">req/s (0 = cluster default of 100)</span>
            </div>
          </div>

          <!-- API key gate -->
          <div class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800">
            <div class="flex items-center justify-between">
              <div class="flex items-center gap-3">
                <Shield class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
                <div>
                  <p class="text-sm font-medium text-slate-900 dark:text-slate-50">Require API key</p>
                  <p class="text-xs text-slate-500 dark:text-slate-400">Only requests with a valid X-API-Key are served. If key checking is ever unavailable, the route fails closed. Issue keys in the project's API keys panel.</p>
                </div>
              </div>
              <button
                @click="requireApiKey = !requireApiKey"
                class="relative inline-flex h-6 w-11 flex-shrink-0 items-center rounded-full transition-colors"
                :class="requireApiKey ? 'bg-kipper-600' : 'bg-slate-300 dark:bg-slate-600'"
              >
                <span
                  class="inline-block h-4 w-4 rounded-full bg-white transition-transform"
                  :class="requireApiKey ? 'translate-x-6' : 'translate-x-1'"
                />
              </button>
            </div>
            <NoticeCallout
              v-if="apiKeyGatePending"
              tone="warning"
              class="mt-3 flex items-start gap-2 p-3"
            >
              <AlertTriangle class="mt-0.5 h-4 w-4 shrink-0 text-amber-600 dark:text-orange-300" :stroke-width="1.75" />
              <p class="text-xs text-amber-800 dark:text-slate-300">
                The key gate isn't in place yet. Kipper is still applying it, so the route may still be reachable without a key until this clears. It usually resolves within a minute; if it persists, check the app's events.
              </p>
            </NoticeCallout>
          </div>

          <!-- URL Redirects -->
          <div class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800">
            <div class="flex items-center justify-between mb-3">
              <div class="flex items-center gap-3">
                <Globe class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
                <div>
                  <p class="text-sm font-medium text-slate-900 dark:text-slate-50">URL Redirects</p>
                  <p class="text-xs text-slate-500 dark:text-slate-400">Redirect incoming paths to other URLs</p>
                </div>
              </div>
              <button
                v-if="authStore.isDeployer"
                @click="addRedirect"
                class="inline-flex items-center gap-1 rounded-md border border-slate-300 px-2 py-1 text-xs font-medium text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
              >
                <Plus class="h-3 w-3" /> Add
              </button>
            </div>
            <div v-if="redirects.length" class="space-y-2">
              <div v-for="(redirect, i) in redirects" :key="i" class="flex items-center gap-2">
                <input
                  v-model="redirect.source"
                  type="text"
                  placeholder="Source regex (e.g. ^/$)"
                  class="min-w-0 flex-1 rounded-md border border-slate-300 bg-white px-2 py-1.5 text-xs text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50"
                />
                <span class="text-xs text-slate-400">→</span>
                <input
                  v-model="redirect.target"
                  type="text"
                  placeholder="Target (e.g. /en/)"
                  class="min-w-0 flex-1 rounded-md border border-slate-300 bg-white px-2 py-1.5 text-xs text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50"
                />
                <label class="flex items-center gap-1 text-xs text-slate-500 dark:text-slate-400">
                  <input type="checkbox" v-model="redirect.permanent" class="rounded" />
                  301
                </label>
                <button
                  v-if="authStore.isDeployer"
                  @click="removeRedirect(i)"
                  class="rounded p-1 text-slate-400 hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-950"
                >
                  <Minus class="h-3 w-3" />
                </button>
              </div>
            </div>
            <p v-else class="text-xs text-slate-400 dark:text-slate-500">No redirects configured.</p>
          </div>

          <!-- Basic Auth -->
          <div class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800">
            <div class="flex items-center justify-between mb-3">
              <div class="flex items-center gap-3">
                <Shield class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
                <div>
                  <p class="text-sm font-medium text-slate-900 dark:text-slate-50">Basic authentication</p>
                  <p class="text-xs text-slate-500 dark:text-slate-400">Password-protect this app with HTTP basic auth</p>
                </div>
              </div>
              <button
                v-if="authStore.isDeployer && basicAuthEnabled"
                @click="handleDeleteBasicAuth"
                class="text-xs text-red-500 hover:text-red-700"
              >Remove all</button>
            </div>

            <div v-if="basicAuthUsers.length" class="mb-3 space-y-1">
              <div v-for="user in basicAuthUsers" :key="user" class="flex items-center justify-between rounded-md bg-white px-3 py-1.5 text-xs dark:bg-slate-900">
                <span class="font-mono text-slate-700 dark:text-slate-300">{{ user }}</span>
                <button
                  v-if="authStore.isDeployer"
                  @click="handleDeleteBasicAuthUser(user)"
                  class="rounded p-0.5 text-slate-400 transition-colors hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-950 dark:hover:text-red-400"
                  title="Remove user"
                >
                  <X class="h-3.5 w-3.5" />
                </button>
              </div>
            </div>

            <div v-if="authStore.isDeployer" class="flex items-center gap-2">
              <input
                v-model="basicAuthUsername"
                type="text"
                placeholder="Username"
                class="min-w-0 flex-1 rounded-md border border-slate-300 bg-white px-2 py-1.5 text-xs text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50"
              />
              <input
                v-model="basicAuthPassword"
                type="password"
                placeholder="Password"
                class="min-w-0 flex-1 rounded-md border border-slate-300 bg-white px-2 py-1.5 text-xs text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50"
              />
              <button
                @click="handleAddBasicAuthUser"
                :disabled="!basicAuthUsername || !basicAuthPassword || basicAuthSaving"
                class="rounded-md bg-kipper-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-kipper-700 disabled:opacity-50"
              >
                {{ basicAuthSaving ? 'Adding...' : 'Add user' }}
              </button>
            </div>
          </div>

          <!-- Save button -->
          <div class="flex justify-end">
            <SaveButton :saving="settingsSaving" label="Save settings" @click="saveSettings" />
          </div>

          <!-- Info box -->
          <div class="rounded-lg border border-slate-200 bg-white p-4 dark:border-slate-700 dark:bg-slate-900">
            <p class="text-xs text-slate-600 dark:text-slate-400">
              Security headers and rate limiting are applied at the ingress level by Traefik. They protect your app without requiring code changes.
              Disabling security headers is useful for MVPs that embed content from other domains or need a custom Content Security Policy.
              Leave resource limits empty to use Kubernetes defaults (no limits).
            </p>
          </div>
        </div>
      </div>

      <!-- Files tab -->
      <div v-show="activeTab === 'files'" class="flex h-full flex-col">
        <!-- Warnings -->
        <div class="space-y-0">
          <div v-if="podCount > 1" class="flex items-center gap-2 border-b border-amber-200 bg-amber-50 px-5 py-2 text-xs text-amber-700 dark:border-slate-800 dark:bg-slate-900 dark:text-slate-300 dark:shadow-[inset_3px_0_0_theme(colors.orange.300)]">
            <AlertTriangle class="h-3.5 w-3.5 flex-shrink-0 dark:text-orange-300" />
            Browsing pod <span class="font-mono font-medium">{{ currentPod }}</span> ({{ podCount }} pods running). Edits are saved to all pods.
          </div>
          <div class="flex items-center gap-2 border-b border-slate-100 bg-slate-50 px-5 py-1.5 text-[10px] text-slate-400 dark:border-slate-800 dark:bg-slate-900">
            Manual file changes are lost when the app is redeployed. For permanent changes, update the Docker image.
          </div>
        </div>

        <!-- Breadcrumb navigation -->
        <div class="flex flex-wrap items-center gap-1 border-b border-slate-100 px-5 py-2.5 text-xs dark:border-slate-800">
          <button
            @click="navigateToDir('/')"
            class="text-kipper-600 hover:text-kipper-700 dark:text-kipper-400 dark:hover:text-kipper-300"
          >/</button>
          <template v-for="seg in breadcrumbSegments" :key="seg.path">
            <ChevronRight class="h-3 w-3 text-slate-400" />
            <button
              @click="navigateToDir(seg.path)"
              class="text-kipper-600 hover:text-kipper-700 dark:text-kipper-400 dark:hover:text-kipper-300"
            >{{ seg.name }}</button>
          </template>
          <span class="flex-1" />
          <input ref="fileUploadRef" type="file" class="hidden" @change="handleFileUpload" />
          <button
            @click="fileUploadRef?.click()"
            :disabled="uploading"
            class="inline-flex items-center gap-1 rounded-md bg-kipper-600 px-2 py-1 text-xs font-medium text-white hover:bg-kipper-700 disabled:opacity-50"
          >
            <Upload class="h-3 w-3" />
            {{ uploading ? 'Uploading...' : 'Upload' }}
          </button>
        </div>

        <!-- File list -->
        <div class="flex-1 overflow-y-auto">
          <div v-if="filesLoading" class="p-5 text-sm text-slate-500 dark:text-slate-400">Loading...</div>
          <div v-else-if="filesError" class="p-5 text-sm text-red-500">{{ filesError }}</div>
          <div v-else-if="!fileEntries.length" class="p-5 text-sm text-slate-500 dark:text-slate-400">
            <Folder class="mb-2 h-5 w-5" />
            Directory is empty
          </div>
          <div v-else>
            <div
              v-for="entry in fileEntries"
              :key="entry.name"
              class="flex cursor-pointer items-center gap-3 border-b border-slate-100 px-5 py-2.5 transition-colors hover:bg-slate-50 dark:border-slate-800 dark:hover:bg-slate-800/50"
              @click="handleFileClick(entry)"
            >
              <Folder v-if="entry.is_dir" class="h-4 w-4 flex-shrink-0 text-kipper-500" />
              <File v-else class="h-4 w-4 flex-shrink-0 text-slate-400" />
              <span class="flex-1 truncate text-sm text-slate-900 dark:text-slate-50">{{ entry.name }}</span>
              <span class="text-xs text-slate-400 tabular-nums">{{ entry.is_dir ? '' : filesApi.formatFileSize(entry.size) }}</span>
              <span class="hidden text-xs text-slate-400 sm:inline">{{ entry.permissions }}</span>
              <span class="hidden text-xs text-slate-400 sm:inline">{{ entry.modified }}</span>
              <button
                v-if="!entry.is_dir"
                @click.stop="handleDownload(entry)"
                class="rounded p-1 text-slate-400 hover:bg-slate-200 hover:text-slate-600 dark:hover:bg-slate-700 dark:hover:text-slate-300"
                title="Download"
              >
                <Download class="h-3.5 w-3.5" />
              </button>
            </div>
          </div>
        </div>
      </div>

      <!-- File viewer uses the global modal system (ModalContainer in App.vue) -->

      <!-- Connect tab -->
      <div v-show="activeTab === 'connect'" class="flex-1 overflow-y-auto p-5">
        <div class="space-y-6">
          <!-- Web Terminal -->
          <div>
            <div class="mb-3 flex flex-wrap items-center justify-between gap-y-2">
              <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Web Terminal</h3>
              <div class="flex items-center gap-2">
                <select
                  v-if="terminalPods.length > 1"
                  v-model="selectedPod"
                  @change="nextTick(() => { if (selectedPod && terminalRefs[selectedPod]) terminalRefs[selectedPod].focus() })"
                  class="rounded-md border border-slate-300 bg-white px-2 py-1 text-xs text-slate-900 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
                >
                  <option v-for="pod in terminalPods" :key="pod" :value="pod">{{ pod }}</option>
                </select>
                <span v-else-if="selectedPod" class="font-mono text-xs text-slate-500 dark:text-slate-400">{{ selectedPod }}</span>
                <button
                  @click="reconnectTerminal"
                  class="inline-flex items-center gap-1 rounded-md border border-slate-300 px-2 py-1 text-xs text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
                >
                  <RotateCw class="h-3 w-3" />
                  Reconnect
                </button>
              </div>
            </div>
            <WebTerminal
              v-for="pod in terminalPods"
              :key="pod"
              v-show="pod === selectedPod"
              :ref="(el) => setTerminalRef(pod, el)"
              :namespace="project"
              :app-name="appName"
              :pod="pod"
            />
            <div v-if="terminalLoading && !terminalPods.length" class="flex h-80 items-center justify-center rounded-lg border border-slate-200 bg-[#020617] dark:border-slate-700">
              <span class="text-sm text-slate-400">Loading pods...</span>
            </div>
            <div v-else-if="!terminalPods.length && !terminalLoading" class="flex h-80 items-center justify-center rounded-lg border border-slate-200 bg-[#020617] dark:border-slate-700">
              <span class="text-sm text-slate-400">No running pods found</span>
            </div>
          </div>

          <!-- CLI Commands -->
          <div>
            <h3 class="mb-3 text-sm font-semibold text-slate-900 dark:text-slate-50">CLI Commands</h3>

            <!-- Pod Connect -->
            <div class="space-y-3">
              <div>
                <p class="mb-1.5 text-xs text-slate-500 dark:text-slate-400">
                  Open a shell inside the running container via the CLI.
                </p>
                <div class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800">
                  <div class="flex items-center justify-between">
                    <code class="break-all font-mono text-sm text-slate-900 dark:text-slate-50">kip exec {{ appName }}</code>
                    <button
                      @click="copyCommand(`kip exec ${appName}`, 'exec')"
                      class="rounded-md p-1.5 text-slate-400 hover:bg-slate-200 hover:text-slate-600 dark:hover:bg-slate-700"
                    >
                      <Check v-if="copiedCmd === 'exec'" class="h-4 w-4 text-emerald-500" />
                      <Copy v-else class="h-4 w-4" />
                    </button>
                  </div>
                </div>
              </div>

              <!-- Port forwarding / Tunnel -->
              <div>
                <p class="mb-1.5 text-xs text-slate-500 dark:text-slate-400">
                  Create a tunnel to access the app on your local machine.
                </p>
                <div class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800">
                  <div class="flex items-center justify-between">
                    <code class="break-all font-mono text-sm text-slate-900 dark:text-slate-50">kip tunnel {{ appName }}</code>
                    <button
                      @click="copyCommand(`kip tunnel ${appName}`, 'tunnel')"
                      class="rounded-md p-1.5 text-slate-400 hover:bg-slate-200 hover:text-slate-600 dark:hover:bg-slate-700"
                    >
                      <Check v-if="copiedCmd === 'tunnel'" class="h-4 w-4 text-emerald-500" />
                      <Copy v-else class="h-4 w-4" />
                    </button>
                  </div>
                  <p class="mt-2 text-xs text-slate-500 dark:text-slate-400">
                    Then access the app at <code class="rounded bg-slate-200 px-1 py-0.5 font-mono text-xs dark:bg-slate-700">http://localhost:&lt;port&gt;</code>
                  </p>
                </div>
              </div>

              <!-- Run a command -->
              <div>
                <p class="mb-1.5 text-xs text-slate-500 dark:text-slate-400">
                  Execute a one-off command inside the container.
                </p>
                <div class="space-y-2">
                  <div class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800">
                    <p class="mb-1 text-[10px] font-medium text-slate-400">View a file</p>
                    <div class="flex items-center justify-between">
                      <code class="break-all font-mono text-sm text-slate-900 dark:text-slate-50">kip exec {{ appName }} -- cat /app/config.php</code>
                      <button
                        @click="copyCommand(`kip exec ${appName} -- cat /app/config.php`, 'cat')"
                        class="rounded-md p-1.5 text-slate-400 hover:bg-slate-200 hover:text-slate-600 dark:hover:bg-slate-700"
                      >
                        <Check v-if="copiedCmd === 'cat'" class="h-4 w-4 text-emerald-500" />
                        <Copy v-else class="h-4 w-4" />
                      </button>
                    </div>
                  </div>
                  <div class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800">
                    <p class="mb-1 text-[10px] font-medium text-slate-400">List directory</p>
                    <div class="flex items-center justify-between">
                      <code class="break-all font-mono text-sm text-slate-900 dark:text-slate-50">kip exec {{ appName }} -- ls -la /app</code>
                      <button
                        @click="copyCommand(`kip exec ${appName} -- ls -la /app`, 'ls')"
                        class="rounded-md p-1.5 text-slate-400 hover:bg-slate-200 hover:text-slate-600 dark:hover:bg-slate-700"
                      >
                        <Check v-if="copiedCmd === 'ls'" class="h-4 w-4 text-emerald-500" />
                        <Copy v-else class="h-4 w-4" />
                      </button>
                    </div>
                  </div>
                  <div class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800">
                    <p class="mb-1 text-[10px] font-medium text-slate-400">Check disk usage</p>
                    <div class="flex items-center justify-between">
                      <code class="break-all font-mono text-sm text-slate-900 dark:text-slate-50">kip exec {{ appName }} -- df -h</code>
                      <button
                        @click="copyCommand(`kip exec ${appName} -- df -h`, 'df')"
                        class="rounded-md p-1.5 text-slate-400 hover:bg-slate-200 hover:text-slate-600 dark:hover:bg-slate-700"
                      >
                        <Check v-if="copiedCmd === 'df'" class="h-4 w-4 text-emerald-500" />
                        <Copy v-else class="h-4 w-4" />
                      </button>
                    </div>
                  </div>
                </div>
              </div>
            </div>
          </div>

          <!-- Prerequisites -->
          <div class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800">
            <p class="text-xs font-medium text-slate-700 dark:text-slate-300">Prerequisites (CLI only)</p>
            <ul class="mt-2 space-y-1 text-xs text-slate-500 dark:text-slate-400">
              <li>Install the kip CLI: <code class="rounded bg-slate-200 px-1 py-0.5 font-mono dark:bg-slate-700">curl -fsSL https://getkipper.com/install | sh</code></li>
              <li>Import cluster credentials: <code class="rounded bg-slate-200 px-1 py-0.5 font-mono dark:bg-slate-700">kip cluster add cluster.kip</code></li>
            </ul>
          </div>
        </div>
      </div>

      <div v-if="activeTab === 'scale'" class="p-5">
        <!-- Auto mode banner -->
        <div v-if="isAutoMode" class="mb-4 flex items-start gap-2 rounded-lg border border-kipper-200 bg-kipper-50 p-3 dark:border-kipper-800 dark:bg-kipper-950">
          <Info class="mt-0.5 h-4 w-4 flex-shrink-0 text-kipper-600 dark:text-kipper-400" />
          <div class="text-xs text-kipper-700 dark:text-kipper-300">
            <p>Resources are managed automatically. CPU and memory are adjusted based on usage.</p>
            <p v-if="replicaCount < 2" class="mt-1 font-medium">Scale-down is paused because this app has a single replica. With 2+ replicas, resource adjustments use rolling updates with zero downtime.</p>
          </div>
        </div>
        <!-- Resource recommendation banner -->
        <NoticeCallout v-if="recommendation?.active" tone="warning" class="mb-4 flex items-start gap-3 p-4">
          <AlertTriangle class="mt-0.5 h-5 w-5 flex-shrink-0 text-amber-600 dark:text-orange-300" />
          <div class="flex-1">
            <p class="text-sm font-medium text-amber-800 dark:text-orange-300">Resource recommendation</p>
            <p class="mt-1 text-xs text-amber-700 dark:text-slate-400">{{ recommendation.message }}</p>
            <div class="mt-3 flex gap-2">
              <button
                @click="handleApplyRecommendation"
                class="rounded-md bg-amber-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-amber-700"
              >
                Apply {{ recommendation.recommended_profile }} profile
              </button>
              <button
                @click="handleDismissRecommendation"
                class="rounded-md border border-amber-300 px-3 py-1.5 text-xs font-medium text-amber-700 hover:bg-amber-100 dark:border-amber-600 dark:text-amber-300 dark:hover:bg-amber-900"
              >
                Dismiss
              </button>
            </div>
          </div>
        </NoticeCallout>

        <div class="mb-4 flex justify-end">
          <button
            @click="openOptimise"
            class="inline-flex items-center gap-1.5 rounded-lg border border-emerald-600/30 bg-emerald-600/10 px-3 py-1.5 text-xs font-medium text-emerald-500 transition-colors hover:bg-emerald-600/20"
          >
            <Sparkles class="h-3.5 w-3.5" />
            Optimise resources
          </button>
        </div>
        <div class="space-y-6">
          <!-- Manual scaling -->
          <div>
            <label class="mb-2 block text-sm font-medium text-slate-700 dark:text-slate-300">Manual replicas</label>
            <div class="flex items-center gap-4">
              <button
                @click="setScale(replicaCount - 1)"
                :disabled="scaling || replicaCount <= 0 || autoscaleEnabled"
                class="rounded-lg border border-slate-300 p-2 text-slate-600 hover:bg-slate-100 disabled:opacity-50 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
              >
                <Minus class="h-4 w-4" />
              </button>
              <span class="font-mono text-3xl font-bold text-slate-900 dark:text-slate-50">
                {{ replicaCount }}
              </span>
              <button
                @click="setScale(replicaCount + 1)"
                :disabled="scaling || autoscaleEnabled"
                class="rounded-lg border border-slate-300 p-2 text-slate-600 hover:bg-slate-100 disabled:opacity-50 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
              >
                <Plus class="h-4 w-4" />
              </button>
            </div>
            <p v-if="scaling" class="mt-2 text-xs text-slate-500 dark:text-slate-400">Scaling...</p>
            <p v-if="autoscaleEnabled" class="mt-2 text-xs text-amber-600 dark:text-amber-400">Manual scaling disabled, autoscaling is active</p>
          </div>

          <!-- Autoscaling -->
          <div class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800">
            <div class="flex items-center justify-between mb-4">
              <div>
                <p class="text-sm font-medium text-slate-900 dark:text-slate-50">Autoscaling</p>
                <p class="text-xs text-slate-500 dark:text-slate-400">Automatically scale based on CPU and memory usage</p>
              </div>
              <button
                @click="autoscaleEnabled = !autoscaleEnabled"
                class="relative inline-flex h-6 w-11 items-center rounded-full transition-colors"
                :class="autoscaleEnabled ? 'bg-kipper-600' : 'bg-slate-300 dark:bg-slate-600'"
              >
                <span
                  class="inline-block h-4 w-4 rounded-full bg-white transition-transform"
                  :class="autoscaleEnabled ? 'translate-x-6' : 'translate-x-1'"
                />
              </button>
            </div>

            <div v-if="autoscaleEnabled" class="space-y-4">
              <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
                <div>
                  <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Min replicas</label>
                  <input v-model.number="autoscaleMin" type="number" min="1" class="w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50" />
                </div>
                <div>
                  <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Max replicas</label>
                  <input v-model.number="autoscaleMax" type="number" min="1" class="w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50" />
                </div>
              </div>

              <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
                <div>
                  <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">CPU target (%)</label>
                  <input v-model.number="autoscaleCpu" type="number" min="0" max="100" class="w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50" />
                  <p v-if="autoscaleCurrentCpu" class="mt-1 text-xs text-slate-500">Current: {{ autoscaleCurrentCpu }}</p>
                </div>
                <div>
                  <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Memory target (%)</label>
                  <input v-model.number="autoscaleMemory" type="number" min="0" max="100" placeholder="0 = disabled" class="w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm dark:border-slate-600 dark:bg-slate-900 dark:text-slate-50" />
                  <p v-if="autoscaleCurrentMemory" class="mt-1 text-xs text-slate-500">Current: {{ autoscaleCurrentMemory }}</p>
                </div>
              </div>

              <div class="flex justify-end">
                <SaveButton :saving="autoscaleSaving" label="Save autoscaling" @click="saveAutoscale" />
              </div>
            </div>

            <div v-if="!autoscaleEnabled" class="text-xs text-slate-500 dark:text-slate-400">
              Enable to automatically scale between a minimum and maximum number of replicas based on resource usage.
            </div>
          </div>
        </div>
      </div>
    </div>

    <RevealDialog
      v-if="revealGitTokenOpen"
      type="app-git"
      :name="props.appName"
      :server="buildStatus?.git_url || props.appName"
      :project="project"
      :app="props.appName"
      @close="revealGitTokenOpen = false"
    />
  </SidePanel>
</template>

<style scoped>
.tabs-scroll::-webkit-scrollbar {
  display: none;
}
</style>
