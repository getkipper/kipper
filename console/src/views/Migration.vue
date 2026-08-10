<script setup lang="ts">
import { ref, computed, onUnmounted } from 'vue'
import { ArrowRightLeft, Copy, Check, AlertTriangle, ExternalLink, Globe, RefreshCw, ShieldCheck, OctagonAlert } from 'lucide-vue-next'
import NoticeCallout from '@/components/NoticeCallout.vue'
import { useProjectsStore } from '@/stores/projects'
import { useToast } from '@/composables/useToast'
import * as migrationApi from '@/api/migration'
import type { MigrationStep, AppVerification, DomainStatus, MigrationPlan, PlanItem } from '@/api/migration'

const projects = useProjectsStore()
const toast = useToast()

// State machine: idle → token | initiate → plan → migrating → verifying → cutover → complete.
// Every migration passes through the plan: Start lives on the report, and the
// server refuses a start without the receipt the plan issued.
type Phase = 'idle' | 'token' | 'initiate' | 'plan' | 'migrating' | 'verifying' | 'cutover' | 'complete'
const phase = ref<Phase>('idle')

// Token generation
const generatedToken = ref('')
const tokenCopied = ref(false)
const generatingToken = ref(false)

// Migration initiation
const migrationToken = ref('')
const selectedProjects = ref<string[]>([])
const starting = ref(false)
const targetClusterInfo = ref<{ endpoint: string; cluster: string; expires: string } | null>(null)

function decodeToken(token: string) {
  try {
    const json = atob(token)
    const parsed = JSON.parse(json)
    targetClusterInfo.value = {
      endpoint: parsed.endpoint || '',
      cluster: parsed.cluster || '',
      expires: parsed.expires || '',
    }
  } catch {
    targetClusterInfo.value = null
  }
}

function onTokenInput() {
  if (migrationToken.value.length > 20) {
    decodeToken(migrationToken.value)
  } else {
    targetClusterInfo.value = null
  }
}

// Plan report
const plan = ref<MigrationPlan | null>(null)
const planning = ref(false)
const totpCode = ref('')
const cutoverTotp = ref('')

// Conflict handling, on the plan: type each project name to confirm its
// overwrite, then the plan refreshes with the confirmations applied.
const confirmedOverwrites = ref<Set<string>>(new Set())
const overwriteInput = ref<Record<string, string>>({})

const unconfirmedConflicts = computed(() =>
  (plan.value?.conflicts ?? []).filter(name => !confirmedOverwrites.value.has(name)),
)

// Domain dispositions: custom domains the operator chose to keep on the source
// instead of moving to the target. Platform subdomains coexist and kipper.run
// hosts re-register, neither of which is a per-app choice.
const keepDomains = ref<Set<string>>(new Set())

const appDomains = computed(() =>
  (plan.value?.will_migrate ?? []).filter(i => i.kind === 'app' && i.host),
)

function domainKey(item: PlanItem): string {
  return `${item.namespace}/${item.name}`
}

function setKeepDomain(item: PlanItem, keep: boolean) {
  const key = domainKey(item)
  if (keepDomains.value.has(key) === keep) return
  if (keep) keepDomains.value.add(key)
  else keepDomains.value.delete(key)
  // A changed disposition changes the report, so rebuild the plan and receipt.
  loadPlan()
}

const startBlocked = computed(() =>
  !plan.value || plan.value.blockers.length > 0 || totpCode.value.length !== 6,
)

function formatCPU(millis: number): string {
  return `${millis}m`
}

function formatBytes(bytes: number): string {
  const gb = bytes / (1024 * 1024 * 1024)
  if (gb >= 1) return `${gb.toFixed(1)} GB`
  return `${Math.round(bytes / (1024 * 1024))} MB`
}

// Progress
const sessionId = ref('')
const steps = ref<MigrationStep[]>([])
const currentDetail = ref('')
const sessionStatus = ref('')
let ws: WebSocket | null = null

// Verification
const verificationApps = ref<AppVerification[]>([])
const loadingVerification = ref(false)
const buildsPending = computed(() =>
  verificationApps.value.some(a => a.build_phase && a.build_phase !== 'Succeeded' && a.build_phase !== 'Failed'),
)
let verifyPollTimer: ReturnType<typeof setTimeout> | null = null

// DNS
const domains = ref<DomainStatus[]>([])
const checkingDNS = ref(false)
const dnsWarning = ref('')

// Cutover build gate
const pendingBuilds = ref<string[]>([])

// Animated squares
const transferActive = computed(() => steps.value.some(s => s.status === 'running'))

async function handleGenerateToken() {
  generatingToken.value = true
  try {
    generatedToken.value = await migrationApi.generateToken()
    phase.value = 'token'
  } catch {
    toast.error('Failed to generate migration token')
  } finally {
    generatingToken.value = false
  }
}

async function copyToken() {
  await navigator.clipboard.writeText(generatedToken.value)
  tokenCopied.value = true
  setTimeout(() => { tokenCopied.value = false }, 2000)
}

async function loadPlan() {
  if (!migrationToken.value || selectedProjects.value.length === 0) return
  planning.value = true
  try {
    plan.value = await migrationApi.getPlan(
      migrationToken.value,
      selectedProjects.value,
      [...confirmedOverwrites.value],
      [...keepDomains.value],
    )
    phase.value = 'plan'
  } catch (e: unknown) {
    const axiosErr = e as { response?: { data?: { error?: string } } }
    toast.error(axiosErr.response?.data?.error || 'Failed to build the migration plan')
  } finally {
    planning.value = false
  }
}

function handleConfirmOverwrites() {
  for (const name of unconfirmedConflicts.value) {
    if (overwriteInput.value[name] !== name) {
      toast.error(`Type "${name}" to confirm overwrite`)
      return
    }
  }
  for (const name of unconfirmedConflicts.value) {
    confirmedOverwrites.value.add(name)
  }
  // The confirmations change the report, so the plan (and its receipt) is
  // rebuilt with them applied.
  loadPlan()
}

async function handleStartMigration() {
  if (!plan.value || startBlocked.value) return
  starting.value = true
  try {
    const result = await migrationApi.startMigration(
      migrationToken.value,
      selectedProjects.value,
      [...confirmedOverwrites.value],
      totpCode.value,
      plan.value.receipt,
      [...keepDomains.value],
    )

    sessionId.value = result.session_id
    totpCode.value = ''
    phase.value = 'migrating'
    connectWebSocket()
  } catch (e: unknown) {
    // A 409 means the target's state moved since the plan (e.g. a project
    // appeared there); anything else surfaces the server's reason. Both
    // paths go through a fresh plan — the old receipt is spent.
    const axiosErr = e as { response?: { status?: number; data?: { error?: string } } }
    toast.error(axiosErr.response?.data?.error || 'Failed to start migration')
    totpCode.value = ''
    loadPlan()
  } finally {
    starting.value = false
  }
}

function connectWebSocket() {
  ws = migrationApi.connectProgress(
    sessionId.value,
    (step) => {
      const idx = steps.value.findIndex(s => s.name === step.name)
      if (idx >= 0) {
        steps.value[idx] = step
      } else {
        steps.value.push(step)
      }
      if (step.detail) {
        currentDetail.value = step.detail
      }
      // Detect verification phase from the step
      if (step.name === 'Waiting for verification' && step.phase === 'verification') {
        phase.value = 'verifying'
        loadVerification()
      }
    },
    (status) => {
      sessionStatus.value = status
      if (status === 'verifying') {
        phase.value = 'verifying'
        loadVerification()
      } else if (status === 'failed') {
        toast.error('Migration failed')
      }
    },
  )
}

async function loadVerification() {
  loadingVerification.value = true
  try {
    const result = await migrationApi.getVerification(sessionId.value)
    verificationApps.value = result.apps || []
  } catch {
    toast.error('Failed to load verification data')
  } finally {
    loadingVerification.value = false
  }
  // A build is an automatic step the operator is waiting on, so keep it fresh
  // ourselves until every build resolves instead of leaving it stale.
  if (verifyPollTimer) {
    clearTimeout(verifyPollTimer)
    verifyPollTimer = null
  }
  if (phase.value === 'verifying' && buildsPending.value) {
    verifyPollTimer = setTimeout(loadVerification, 5000)
  }
}

async function handleCutover(force = false) {
  if (cutoverTotp.value.length !== 6) {
    toast.error('Enter the 6-digit code from your authenticator app')
    return
  }
  phase.value = 'cutover'
  pendingBuilds.value = []
  try {
    const result = await migrationApi.performCutover(sessionId.value, cutoverTotp.value, force)
    domains.value = result.domains || []
    dnsWarning.value = result.dns_warning || ''
    cutoverTotp.value = ''
    phase.value = 'complete'
  } catch (e: unknown) {
    // A failed cutover keeps the session in verifying on the server, so the
    // button stays available for a retry once the cause is fixed.
    const axiosErr = e as { response?: { status?: number; data?: { error?: string; builds_pending?: string[] } } }
    if (axiosErr.response?.status === 409 && axiosErr.response.data?.builds_pending) {
      pendingBuilds.value = axiosErr.response.data.builds_pending
    } else {
      toast.error(axiosErr.response?.data?.error || 'Failed to apply custom domains')
    }
    cutoverTotp.value = ''
    phase.value = 'verifying'
  }
}

// Every skip in the run, repeated at completion: "completed, one database
// left behind" must be impossible to miss.
const skippedSteps = computed(() => steps.value.filter(s => s.status === 'skipped'))

// DNS changes are a manual step the operator makes at their own provider and
// propagation can take a while, so there is no auto-poll countdown. The
// operator changes the records, then checks when ready.
async function checkDNS() {
  checkingDNS.value = true
  try {
    domains.value = await migrationApi.getDNSStatus(sessionId.value)
  } catch {
    toast.error('Failed to check DNS status')
  } finally {
    checkingDNS.value = false
  }
}

async function handleCancel() {
  if (sessionId.value) {
    await migrationApi.cancelMigration(sessionId.value)
  }
  phase.value = 'idle'
  steps.value = []
}

onUnmounted(() => {
  ws?.close()
  if (verifyPollTimer) clearTimeout(verifyPollTimer)
})
</script>

<template>
  <div class="animate-fade-in">
    <!-- Header -->
    <div class="mb-8">
      <h1 class="text-2xl font-semibold tracking-tight text-slate-900 dark:text-slate-50">Migration</h1>
      <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Move projects between Kipper clusters</p>
    </div>

    <!-- Idle: Choose action -->
    <div v-if="phase === 'idle'" class="space-y-4">
      <div class="grid gap-4 sm:grid-cols-2">
        <!-- Receive (this is the target cluster) -->
        <button
          @click="handleGenerateToken"
          :disabled="generatingToken"
          class="group rounded-xl border border-slate-200 bg-white p-6 text-left transition-all hover:border-kipper-300 hover:shadow-md dark:border-slate-800 dark:bg-slate-900 dark:hover:border-kipper-700"
        >
          <div class="mb-3 inline-flex rounded-lg bg-kipper-50 p-2.5 dark:bg-kipper-950">
            <ArrowRightLeft class="h-5 w-5 text-kipper-600 dark:text-kipper-400" :stroke-width="1.75" />
          </div>
          <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Receive from another cluster</h3>
          <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Generate a migration token for the source cluster to connect</p>
        </button>

        <!-- Send (this is the source cluster) -->
        <button
          @click="phase = 'initiate'"
          class="group rounded-xl border border-slate-200 bg-white p-6 text-left transition-all hover:border-kipper-300 hover:shadow-md dark:border-slate-800 dark:bg-slate-900 dark:hover:border-kipper-700"
        >
          <div class="mb-3 inline-flex rounded-lg bg-emerald-50 p-2.5 dark:bg-emerald-950">
            <ArrowRightLeft class="h-5 w-5 text-emerald-600 dark:text-emerald-400" :stroke-width="1.75" />
          </div>
          <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Migrate to another cluster</h3>
          <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Paste a migration token from the target cluster</p>
        </button>
      </div>
    </div>

    <!-- Token generated -->
    <div v-if="phase === 'token'" class="rounded-xl border border-slate-200 bg-white p-6 dark:border-slate-800 dark:bg-slate-900">
      <h3 class="mb-2 text-sm font-semibold text-slate-900 dark:text-slate-50">Migration token generated</h3>
      <p class="mb-4 text-sm text-slate-500 dark:text-slate-400">Copy this token and paste it on the source cluster's Migration page.</p>
      <div class="flex items-center gap-2">
        <input
          :value="generatedToken"
          readonly
          class="flex-1 rounded-lg border border-slate-300 bg-slate-50 px-3 py-2.5 font-mono text-xs text-slate-700 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-300"
        />
        <button
          @click="copyToken"
          class="inline-flex items-center gap-1.5 rounded-lg bg-kipper-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-kipper-700"
        >
          <Check v-if="tokenCopied" class="h-4 w-4" />
          <Copy v-else class="h-4 w-4" :stroke-width="1.75" />
          {{ tokenCopied ? 'Copied' : 'Copy' }}
        </button>
      </div>
      <p class="mt-3 text-xs text-slate-400 dark:text-slate-500">Token expires in 24 hours. Single use only.</p>
      <button @click="phase = 'idle'" class="mt-4 text-sm text-slate-500 hover:text-slate-700 dark:hover:text-slate-300">Back</button>
    </div>

    <!-- Initiate migration -->
    <div v-if="phase === 'initiate'" class="rounded-xl border border-slate-200 bg-white p-6 dark:border-slate-800 dark:bg-slate-900">
      <h3 class="mb-4 text-sm font-semibold text-slate-900 dark:text-slate-50">Migrate to another cluster</h3>
      <div class="space-y-4">
        <div>
          <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Migration token</label>
          <input
            v-model="migrationToken"
            @input="onTokenInput"
            type="text"
            placeholder="Paste the token from the target cluster"
            class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 font-mono text-xs text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
          />
          <NoticeCallout v-if="targetClusterInfo" tone="success" class="mt-2 px-4 py-3">
            <p class="text-sm font-medium text-emerald-800 dark:text-emerald-300">Target cluster identified</p>
            <p class="mt-1 text-xs text-emerald-700 dark:text-slate-400">
              <span class="font-mono dark:text-slate-300">{{ targetClusterInfo.cluster }}</span>
            </p>
            <p class="text-xs text-emerald-600 dark:text-slate-400">
              {{ targetClusterInfo.endpoint }}
            </p>
          </NoticeCallout>
          <p v-else-if="migrationToken.length > 20" class="mt-1 text-xs text-red-500">Invalid token format</p>
        </div>
        <div>
          <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Projects to migrate</label>
          <div class="space-y-2">
            <label
              v-for="p in projects.projects"
              :key="p.name"
              class="flex items-center gap-2 rounded-lg border border-slate-200 px-3 py-2.5 transition-colors hover:bg-slate-50 dark:border-slate-700 dark:hover:bg-slate-800"
              :class="selectedProjects.includes(p.name) ? 'border-kipper-300 bg-kipper-50 dark:border-kipper-700 dark:bg-kipper-950' : ''"
            >
              <input
                type="checkbox"
                :value="p.name"
                v-model="selectedProjects"
                class="rounded border-slate-300 text-kipper-600 focus:ring-kipper-500"
              />
              <span class="text-sm font-medium text-slate-900 dark:text-slate-50">{{ p.display_name || p.name }}</span>
              <span class="font-mono text-xs text-slate-400">{{ p.environments.map(e => e.name).join(', ') }}</span>
            </label>
          </div>
        </div>
        <NoticeCallout tone="warning" class="px-4 py-3">
          <p class="text-sm font-medium text-amber-800 dark:text-orange-300">Freeze writes before you start</p>
          <p class="mt-1 text-xs text-amber-700 dark:text-slate-400">
            Data is copied while the source apps keep running. Anything written after a database or volume has been copied stays behind on this cluster.
            For a production move, stop writes first (for example <span class="font-mono dark:text-slate-300">kip app scale &lt;app&gt; --replicas 0</span>) and keep them stopped until the domain cutover.
            Autoscaled apps keep running whatever the replica count says: disable autoscaling first (<span class="font-mono dark:text-slate-300">kip app autoscale &lt;app&gt; --off</span>), then scale to zero.
          </p>
        </NoticeCallout>
        <div class="flex gap-3">
          <button @click="phase = 'idle'" class="rounded-lg border border-slate-300 px-4 py-2.5 text-sm font-medium text-slate-700 hover:bg-slate-50 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-800">Cancel</button>
          <button
            @click="loadPlan"
            :disabled="planning || !migrationToken || selectedProjects.length === 0"
            class="rounded-lg bg-kipper-600 px-4 py-2.5 text-sm font-semibold text-white hover:bg-kipper-700 disabled:opacity-40"
          >
            {{ planning ? 'Building plan...' : 'Review migration plan' }}
          </button>
        </div>
      </div>
    </div>

    <!-- Migration plan: the mandatory report Start lives on -->
    <div v-if="phase === 'plan' && plan" class="relative">
      <!-- While the plan recomputes (e.g. after confirming an overwrite) the
           old report stays mounted, so dim it and overlay a notice — otherwise
           the previous blockers read as the new answer for a second or two. -->
      <div
        v-if="planning"
        class="absolute inset-0 z-10 flex items-start justify-center rounded-xl bg-white/60 pt-28 backdrop-blur-[1px] dark:bg-slate-950/60"
      >
        <div class="flex items-center gap-2 rounded-lg border border-slate-200 bg-white px-4 py-2 text-sm font-medium text-slate-600 shadow-sm dark:border-slate-700 dark:bg-slate-900 dark:text-slate-300">
          <RefreshCw class="h-4 w-4 animate-spin" :stroke-width="1.75" />
          Rebuilding plan with your confirmations…
        </div>
      </div>
      <div class="space-y-4 transition-opacity" :class="{ 'pointer-events-none opacity-50': planning }">
      <!-- Consent line -->
      <div class="rounded-xl border border-slate-200 bg-white p-6 dark:border-slate-800 dark:bg-slate-900">
        <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Migration plan</h3>
        <p class="mt-2 text-sm text-slate-600 dark:text-slate-400">
          You are sending <span class="font-semibold">{{ selectedProjects.join(', ') }}</span> — including databases, volumes, and secrets — to cluster
          <span class="font-mono font-semibold text-slate-900 dark:text-slate-50">{{ plan.target_cluster }}</span>
          at <span class="font-mono">{{ plan.target_endpoint }}</span><template v-if="plan.target_version"> (Kipper {{ plan.target_version }})</template>.
        </p>
        <p class="mt-2 text-xs text-slate-400 dark:text-slate-500">
          This cluster stays untouched: migration copies, and until you decommission the source yourself, pointing DNS back undoes everything written before the cutover.
        </p>
      </div>

      <!-- Blockers -->
      <NoticeCallout v-if="plan.blockers.length > 0" tone="danger" class="p-5">
        <div class="mb-2 flex items-center gap-2">
          <OctagonAlert class="h-4 w-4 text-red-600 dark:text-rose-300" :stroke-width="1.75" />
          <h4 class="text-sm font-semibold text-red-800 dark:text-rose-300">Blockers — the migration cannot start</h4>
        </div>
        <ul class="space-y-1.5">
          <li v-for="b in plan.blockers" :key="b" class="text-sm text-red-700 dark:text-slate-400">{{ b }}</li>
        </ul>
      </NoticeCallout>

      <!-- Overwrite confirmation, inline on the report -->
      <div v-if="unconfirmedConflicts.length > 0" class="rounded-xl border border-red-200 bg-white p-5 dark:border-red-900 dark:bg-slate-900">
        <h4 class="mb-1 text-sm font-semibold text-slate-900 dark:text-slate-50">Confirm overwrites</h4>
        <p class="mb-3 text-sm text-slate-500 dark:text-slate-400">These projects already exist on the target and would be overwritten. Type each name to confirm.</p>
        <div class="space-y-3">
          <div v-for="name in unconfirmedConflicts" :key="name">
            <label class="mb-1 block text-sm text-slate-700 dark:text-slate-300">
              Type <span class="font-mono font-semibold">{{ name }}</span> to confirm
            </label>
            <input
              v-model="overwriteInput[name]"
              :placeholder="name"
              class="block w-full rounded-lg border border-slate-300 bg-white px-3 py-2 text-sm dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
            />
          </div>
        </div>
        <button @click="handleConfirmOverwrites" :disabled="planning" class="mt-3 rounded-lg bg-red-600 px-4 py-2 text-sm font-semibold text-white hover:bg-red-700 disabled:opacity-40">
          Confirm overwrites and refresh the plan
        </button>
      </div>

      <!-- Warnings -->
      <NoticeCallout v-if="plan.warnings.length > 0" tone="warning" class="p-5">
        <div class="mb-2 flex items-center gap-2">
          <AlertTriangle class="h-4 w-4 text-amber-600 dark:text-orange-300" :stroke-width="1.75" />
          <h4 class="text-sm font-semibold text-amber-800 dark:text-orange-300">Warnings</h4>
        </div>
        <ul class="space-y-1.5">
          <li v-for="wmsg in plan.warnings" :key="wmsg" class="text-sm text-amber-700 dark:text-slate-400">{{ wmsg }}</li>
        </ul>
      </NoticeCallout>

      <!-- Capacity numbers -->
      <div v-if="plan.capacity" class="rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
        <h4 class="mb-3 text-sm font-semibold text-slate-900 dark:text-slate-50">Target capacity</h4>
        <div class="grid grid-cols-3 gap-4">
          <div>
            <p class="text-xs text-slate-500 dark:text-slate-400">CPU</p>
            <p class="mt-0.5 text-sm font-medium" :class="plan.capacity.need_cpu_millis > plan.capacity.free_cpu_millis ? 'text-red-600 dark:text-red-400' : 'text-slate-900 dark:text-slate-50'">
              {{ formatCPU(plan.capacity.need_cpu_millis) }} needed · {{ formatCPU(plan.capacity.free_cpu_millis) }} free
            </p>
          </div>
          <div>
            <p class="text-xs text-slate-500 dark:text-slate-400">Memory</p>
            <p class="mt-0.5 text-sm font-medium" :class="plan.capacity.need_memory_bytes > plan.capacity.free_memory_bytes ? 'text-red-600 dark:text-red-400' : 'text-slate-900 dark:text-slate-50'">
              {{ formatBytes(plan.capacity.need_memory_bytes) }} needed · {{ formatBytes(plan.capacity.free_memory_bytes) }} free
            </p>
          </div>
          <div>
            <p class="text-xs text-slate-500 dark:text-slate-400">Storage</p>
            <p v-if="plan.capacity.storage_known" class="mt-0.5 text-sm font-medium" :class="plan.capacity.need_storage_bytes > plan.capacity.free_storage_bytes ? 'text-red-600 dark:text-red-400' : 'text-slate-900 dark:text-slate-50'">
              {{ formatBytes(plan.capacity.need_storage_bytes) }} needed · {{ formatBytes(plan.capacity.free_storage_bytes) }} free
            </p>
            <p v-else class="mt-0.5 text-sm text-slate-400">Target could not measure its disk</p>
          </div>
        </div>
      </div>

      <!-- Skips -->
      <div v-if="plan.will_skip.length > 0" class="rounded-xl border border-amber-200 bg-white p-5 dark:border-amber-900 dark:bg-slate-900">
        <h4 class="mb-1 text-sm font-semibold text-slate-900 dark:text-slate-50">Data that will be skipped</h4>
        <p class="mb-3 text-sm text-slate-500 dark:text-slate-400">These stay on this cluster; the migration shows manual transfer steps for each.</p>
        <div class="space-y-1.5">
          <div v-for="item in plan.will_skip" :key="item.namespace + '/' + item.name" class="flex items-start gap-2">
            <AlertTriangle class="mt-0.5 h-3.5 w-3.5 shrink-0 text-amber-500" :stroke-width="2" />
            <p class="text-sm text-slate-700 dark:text-slate-300">
              <span class="font-medium">{{ item.name }}</span>
              <span class="font-mono text-xs text-slate-400"> {{ item.namespace }}</span>
              <span v-if="item.detail" class="text-slate-500 dark:text-slate-400"> — {{ item.detail }}</span>
            </p>
          </div>
        </div>
      </div>

      <!-- Will migrate -->
      <div class="rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
        <h4 class="mb-3 text-sm font-semibold text-slate-900 dark:text-slate-50">Will migrate ({{ plan.will_migrate.length }})</h4>
        <div class="space-y-1.5">
          <div v-for="item in plan.will_migrate" :key="item.kind + item.namespace + '/' + item.name" class="flex items-start gap-2">
            <component :is="item.status === 'warn' ? AlertTriangle : Check" class="mt-0.5 h-3.5 w-3.5 shrink-0" :class="item.status === 'warn' ? 'text-amber-500' : 'text-emerald-500'" :stroke-width="2" />
            <p class="text-sm text-slate-700 dark:text-slate-300">
              <span class="text-xs uppercase tracking-wide text-slate-400">{{ item.kind }}</span>
              <span class="ml-1.5 font-medium">{{ item.name }}</span>
              <span class="ml-1 font-mono text-xs text-slate-400">{{ item.namespace }}</span>
              <span v-if="item.detail" class="text-slate-500 dark:text-slate-400"> — {{ item.detail }}</span>
            </p>
          </div>
        </div>
      </div>

      <!-- Domains -->
      <div v-if="appDomains.length > 0" class="rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
        <h4 class="mb-1 text-sm font-semibold text-slate-900 dark:text-slate-50">Domains</h4>
        <p class="mb-3 text-sm text-slate-500 dark:text-slate-400">Custom domains move to the new cluster. Cluster subdomains keep working on both until you decommission the old one.</p>
        <div class="space-y-2">
          <div
            v-for="item in appDomains"
            :key="'dom-' + domainKey(item)"
            class="flex items-center justify-between gap-3 rounded-lg border border-slate-100 bg-slate-50 px-3 py-2 dark:border-slate-700 dark:bg-slate-800"
          >
            <div class="min-w-0">
              <span class="text-sm font-medium text-slate-900 dark:text-slate-50">{{ item.name }}</span>
              <span class="ml-1.5 font-mono text-xs text-slate-500 dark:text-slate-400">{{ item.host }}</span>
              <p v-if="item.domain_class === 'platform'" class="text-xs text-slate-400">Stays on both. New cluster serves {{ item.target_url }}. The old URL keeps working until you decommission the old cluster.</p>
              <p v-else-if="item.domain_class === 'gateway'" class="text-xs text-slate-400">Free kipper.run subdomain — the new cluster gets its own; re-register separately if you need the old one.</p>
              <p v-else-if="keepDomains.has(domainKey(item))" class="text-xs text-slate-400">Kept on the old cluster. The new cluster serves {{ item.target_url }}.</p>
              <p v-else class="text-xs text-slate-400">Moves to the new cluster. Live once you repoint its DNS; the new cluster issues the certificate then.</p>
            </div>
            <div v-if="item.domain_class === 'custom'" class="flex shrink-0 overflow-hidden rounded-md border border-slate-300 text-xs dark:border-slate-600">
              <button
                @click="setKeepDomain(item, false)"
                :class="keepDomains.has(domainKey(item)) ? 'text-slate-600 hover:bg-slate-100 dark:text-slate-300 dark:hover:bg-slate-700' : 'bg-kipper-600 text-white'"
                class="px-2.5 py-1 font-medium"
              >Move</button>
              <button
                @click="setKeepDomain(item, true)"
                :class="keepDomains.has(domainKey(item)) ? 'bg-kipper-600 text-white' : 'text-slate-600 hover:bg-slate-100 dark:text-slate-300 dark:hover:bg-slate-700'"
                class="px-2.5 py-1 font-medium"
              >Keep on old</button>
            </div>
            <span v-else class="shrink-0 rounded-md bg-slate-200 px-2 py-0.5 text-xs font-medium text-slate-600 dark:bg-slate-700 dark:text-slate-300">
              {{ item.domain_class === 'gateway' ? 'kipper.run' : 'stays on both' }}
            </span>
          </div>
        </div>
      </div>

      <!-- Never migrates -->
      <div class="rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
        <h4 class="mb-1 text-sm font-semibold text-slate-900 dark:text-slate-50">Never migrates</h4>
        <ul class="mt-2 space-y-1">
          <li v-for="n in plan.not_migrated" :key="n" class="text-sm text-slate-500 dark:text-slate-400">{{ n }}</li>
        </ul>
      </div>

      <!-- Start, on the report -->
      <div class="rounded-xl border border-slate-200 bg-white p-6 dark:border-slate-800 dark:bg-slate-900">
        <div class="mb-3 flex items-center gap-2">
          <ShieldCheck class="h-4 w-4 text-kipper-600 dark:text-kipper-400" :stroke-width="1.75" />
          <h4 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Confirm with your authenticator</h4>
        </div>
        <div class="flex flex-wrap items-center gap-3">
          <input
            v-model="totpCode"
            placeholder="6-digit code"
            inputmode="numeric"
            maxlength="6"
            class="w-36 rounded-lg border border-slate-300 bg-white px-3 py-2.5 font-mono text-sm dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
          />
          <button
            @click="handleStartMigration"
            :disabled="starting || startBlocked"
            class="rounded-lg bg-kipper-600 px-5 py-2.5 text-sm font-semibold text-white hover:bg-kipper-700 disabled:opacity-40"
          >
            {{ starting ? 'Starting...' : 'Start migration' }}
          </button>
          <button @click="phase = 'initiate'" class="rounded-lg border border-slate-300 px-4 py-2.5 text-sm font-medium text-slate-700 hover:bg-slate-50 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-800">Back</button>
        </div>
        <p v-if="plan.blockers.length > 0" class="mt-2 text-xs text-red-600 dark:text-red-400">Resolve the blockers above first.</p>
        <p v-else-if="plan.receipt_expires" class="mt-2 text-xs text-slate-400 dark:text-slate-500">This plan is valid until {{ new Date(plan.receipt_expires).toLocaleTimeString() }}; after that, review a fresh one.</p>
      </div>
      </div>
    </div>

    <!-- Migration progress -->
    <div v-if="phase === 'migrating' || phase === 'verifying' || phase === 'cutover' || phase === 'complete'" class="space-y-6">
      <!-- Cluster pair visual -->
      <div class="flex items-center justify-center gap-6 py-6">
        <div class="flex flex-col items-center gap-2">
          <div class="flex h-16 w-16 items-center justify-center rounded-xl bg-slate-100 dark:bg-slate-800">
            <svg class="h-8 w-8 text-slate-500" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5">
              <rect x="3" y="1" width="18" height="8" rx="2" />
              <rect x="3" y="11" width="18" height="8" rx="2" />
              <circle cx="7" cy="5" r="1" fill="currentColor" />
              <circle cx="7" cy="15" r="1" fill="currentColor" />
              <line x1="11" y1="5" x2="17" y2="5" />
              <line x1="11" y1="15" x2="17" y2="15" />
              <line x1="12" y1="22" x2="12" y2="19" />
              <line x1="9" y1="22" x2="15" y2="22" />
            </svg>
          </div>
          <span class="text-xs font-medium text-slate-500 dark:text-slate-400">Source</span>
        </div>

        <!-- Animated squares — sequential left-to-right flow -->
        <div class="flex items-center gap-1">
          <div
            v-for="i in 10"
            :key="i"
            class="h-2.5 w-2.5 rounded-sm"
            :class="transferActive
              ? 'bg-emerald-400 dark:bg-emerald-500'
              : 'bg-slate-200 dark:bg-slate-700'"
            :style="transferActive ? { animation: `migrateFlow 2s ease-in-out infinite`, animationDelay: `${(i - 1) * 0.18}s` } : {}"
          />
        </div>

        <div class="flex flex-col items-center gap-2">
          <div class="flex h-16 w-16 items-center justify-center rounded-xl bg-kipper-50 dark:bg-kipper-950">
            <svg class="h-8 w-8 text-kipper-500" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5">
              <rect x="3" y="1" width="18" height="8" rx="2" />
              <rect x="3" y="11" width="18" height="8" rx="2" />
              <circle cx="7" cy="5" r="1" fill="currentColor" />
              <circle cx="7" cy="15" r="1" fill="currentColor" />
              <line x1="11" y1="5" x2="17" y2="5" />
              <line x1="11" y1="15" x2="17" y2="15" />
              <line x1="12" y1="22" x2="12" y2="19" />
              <line x1="9" y1="22" x2="15" y2="22" />
            </svg>
          </div>
          <span class="text-xs font-medium text-kipper-600 dark:text-kipper-400">Target</span>
        </div>
      </div>

      <!-- Step list -->
      <div class="rounded-xl border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900">
        <div class="border-b border-slate-200 px-5 py-3 dark:border-slate-800">
          <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Migration progress</h3>
        </div>
        <div class="divide-y divide-slate-100 dark:divide-slate-800">
          <div v-for="step in steps" :key="step.name" class="flex items-center gap-3 px-5 py-3">
            <!-- Status icon -->
            <span v-if="step.status === 'completed'" class="flex h-5 w-5 items-center justify-center rounded-full bg-emerald-100 dark:bg-emerald-900">
              <Check class="h-3 w-3 text-emerald-600 dark:text-emerald-400" :stroke-width="2.5" />
            </span>
            <span v-else-if="step.status === 'running'" class="h-5 w-5">
              <RefreshCw class="h-5 w-5 animate-spin text-kipper-500" :stroke-width="1.75" />
            </span>
            <span v-else-if="step.status === 'failed'" class="flex h-5 w-5 items-center justify-center rounded-full bg-red-100 dark:bg-red-900">
              <AlertTriangle class="h-3 w-3 text-red-600 dark:text-red-400" :stroke-width="2.5" />
            </span>
            <span v-else-if="step.status === 'skipped'" class="flex h-5 w-5 items-center justify-center rounded-full bg-amber-100 dark:bg-amber-900">
              <AlertTriangle class="h-3 w-3 text-amber-600 dark:text-amber-400" :stroke-width="2.5" />
            </span>
            <span v-else class="h-5 w-5 rounded-full border-2 border-slate-200 dark:border-slate-700" />

            <div class="flex-1 min-w-0">
              <p class="text-sm text-slate-900 dark:text-slate-50" :class="step.status === 'pending' ? 'text-slate-400 dark:text-slate-500' : ''">{{ step.name }}</p>
              <p v-if="step.detail" class="text-xs text-slate-500 dark:text-slate-400 truncate">{{ step.detail }}</p>
              <div v-if="step.manual_steps && step.manual_steps.length > 0" class="mt-2 rounded-lg bg-slate-800 p-3">
                <p class="mb-1.5 text-xs font-medium text-amber-400">Manual steps required:</p>
                <pre class="text-xs leading-relaxed text-slate-300 whitespace-pre-wrap"><template v-for="(line, j) in step.manual_steps" :key="j">{{ line }}
</template></pre>
              </div>
            </div>

            <!-- Byte counter -->
            <span v-if="step.bytes_total && step.bytes_total > 0" class="font-mono text-xs text-slate-400">
              {{ step.bytes_done }}/{{ step.bytes_total }}
            </span>
          </div>
        </div>

        <!-- Current detail -->
        <div v-if="currentDetail && phase === 'migrating'" class="border-t border-slate-200 bg-slate-50 px-5 py-3 dark:border-slate-800 dark:bg-slate-800/50">
          <p class="text-xs text-slate-600 dark:text-slate-400">{{ currentDetail }}</p>
        </div>
      </div>

      <!-- Verification dashboard -->
      <div v-if="phase === 'verifying'" class="rounded-xl border border-emerald-200 bg-white p-6 dark:border-emerald-900 dark:bg-slate-900">
        <div class="mb-2 flex items-center justify-between">
          <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Verify your apps on the new cluster</h3>
          <button
            @click="loadVerification"
            :disabled="loadingVerification"
            class="inline-flex items-center gap-1 text-xs font-medium text-slate-500 hover:text-slate-700 dark:hover:text-slate-300"
          >
            <RefreshCw class="h-3 w-3" :class="loadingVerification ? 'animate-spin' : ''" :stroke-width="1.75" /> Refresh
          </button>
        </div>
        <p class="mb-4 text-sm text-slate-500 dark:text-slate-400">Test each app using the temporary URLs below. Apps deployed from git are rebuilding on the target and come online as their builds finish. Custom domains will be applied after you confirm.</p>

        <div v-if="loadingVerification && verificationApps.length === 0" class="py-4 text-center text-sm text-slate-500">Loading...</div>
        <div v-else class="space-y-2">
          <div v-for="app in verificationApps" :key="app.name" class="flex items-center justify-between rounded-lg border border-slate-100 bg-slate-50 px-4 py-3 dark:border-slate-700 dark:bg-slate-800">
            <div>
              <span class="text-sm font-medium text-slate-900 dark:text-slate-50">{{ app.name }}</span>
              <span v-if="app.custom_domain" class="ml-2 text-xs text-slate-400">{{ app.custom_domain }}</span>
            </div>
            <div class="flex items-center gap-3">
              <span v-if="app.build_phase === 'Succeeded'" class="inline-flex items-center gap-1 text-xs text-emerald-600 dark:text-emerald-400">
                <Check class="h-3 w-3" :stroke-width="2" /> Built
              </span>
              <span v-else-if="app.build_phase === 'Failed'" class="inline-flex items-center gap-1 text-xs text-red-600 dark:text-red-400">
                <OctagonAlert class="h-3 w-3" :stroke-width="2" /> Build failed
              </span>
              <span v-else-if="app.build_phase" class="inline-flex items-center gap-1 text-xs text-amber-600 dark:text-amber-400">
                <RefreshCw class="h-3 w-3 animate-spin" :stroke-width="1.75" />
                Build: {{ app.build_phase }}
              </span>
              <span class="inline-flex items-center gap-1 text-xs">
                <span class="h-2 w-2 rounded-full" :class="app.status === 'Running' ? 'bg-emerald-500' : 'bg-amber-500'" />
                {{ app.status }}
              </span>
              <a v-if="app.temporary_url" :href="app.temporary_url" target="_blank" class="inline-flex items-center gap-1 text-xs font-medium text-kipper-600 hover:text-kipper-700 dark:text-kipper-400">
                Test <ExternalLink class="h-3 w-3" />
              </a>
              <span v-else class="text-xs text-slate-400">URL pending</span>
            </div>
          </div>
        </div>

        <NoticeCallout v-if="pendingBuilds.length > 0" tone="warning" class="mt-4 p-4">
          <p class="text-sm font-medium text-amber-800 dark:text-orange-300">Some rebuilds are not finished</p>
          <ul class="mt-2 space-y-1">
            <li v-for="b in pendingBuilds" :key="b" class="font-mono text-xs text-amber-700 dark:text-slate-300">{{ b }}</li>
          </ul>
          <p class="mt-2 text-xs text-amber-700 dark:text-slate-400">
            Until a build finishes, its custom domain would serve the "building" page. Wait and try again, or cut over anyway.
          </p>
          <button
            @click="handleCutover(true)"
            class="mt-3 rounded-lg border border-amber-300 px-3 py-1.5 text-xs font-medium text-amber-800 hover:bg-amber-100 dark:border-amber-700 dark:text-amber-300 dark:hover:bg-amber-900/40"
          >
            Cut over anyway
          </button>
        </NoticeCallout>

        <!-- Cutover repoints production domains, so it needs a code like the start did -->
        <div class="mt-4 flex flex-wrap items-center gap-3">
          <input
            v-model="cutoverTotp"
            placeholder="6-digit code"
            inputmode="numeric"
            maxlength="6"
            class="w-36 rounded-lg border border-slate-300 bg-white px-3 py-2.5 font-mono text-sm dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
          />
          <button
            @click="handleCutover()"
            :disabled="cutoverTotp.length !== 6"
            class="rounded-lg bg-kipper-600 px-4 py-2.5 text-sm font-semibold text-white hover:bg-kipper-700 disabled:opacity-40"
          >
            Everything looks good — apply custom domains
          </button>
        </div>
      </div>

      <!-- DNS status -->
      <div v-if="phase === 'complete' && domains.length > 0" class="rounded-xl border border-slate-200 bg-white p-6 dark:border-slate-800 dark:bg-slate-900">
        <div class="mb-2 flex items-center justify-between">
          <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Update your DNS records</h3>
          <button
            @click="checkDNS"
            :disabled="checkingDNS"
            class="inline-flex items-center gap-1 text-xs font-medium text-slate-500 hover:text-slate-700 dark:hover:text-slate-300"
          >
            <RefreshCw class="h-3 w-3" :class="checkingDNS ? 'animate-spin' : ''" :stroke-width="1.75" /> Check again
          </button>
        </div>
        <p class="mb-4 text-sm text-slate-500 dark:text-slate-400">
          <span v-if="domains.every(d => d.resolved)">All domains point at the new cluster. Migration complete.</span>
          <span v-else>Point each record below at the new server at your DNS provider. Take your time — press "Check again" once you've made the changes. Certificates are issued once DNS lands.</span>
        </p>
        <div class="space-y-2">
          <div v-for="d in domains" :key="d.domain" class="flex items-center justify-between rounded-lg border border-slate-100 bg-slate-50 px-4 py-3 dark:border-slate-700 dark:bg-slate-800">
            <div class="flex items-center gap-2">
              <Globe class="h-4 w-4 text-slate-400" :stroke-width="1.75" />
              <span class="text-sm font-medium text-slate-900 dark:text-slate-50">{{ d.domain }}</span>
              <span v-if="!d.resolved && d.expected_ips && d.expected_ips.length > 0" class="font-mono text-xs text-slate-400">
                A record: {{ d.expected_ips.join(', ') }}
              </span>
            </div>
            <div class="flex items-center gap-2">
              <span v-if="d.resolved" class="inline-flex items-center gap-1 text-xs text-emerald-600 dark:text-emerald-400">
                <Check class="h-3 w-3" :stroke-width="2.5" /> Points at new cluster
              </span>
              <span v-else class="inline-flex items-center gap-1 text-xs text-amber-600 dark:text-amber-400">
                {{ d.resolved_to ? `Still on ${d.resolved_to}` : 'Pending' }}
              </span>
            </div>
          </div>
        </div>
      </div>

      <!-- Skips, repeated at completion where they cannot be missed -->
      <NoticeCallout v-if="phase === 'complete' && skippedSteps.length > 0" tone="warning" class="p-6">
        <div class="mb-2 flex items-center gap-2">
          <AlertTriangle class="h-5 w-5 text-amber-600 dark:text-orange-300" :stroke-width="1.75" />
          <h3 class="text-sm font-semibold text-amber-900 dark:text-orange-300">Completed with {{ skippedSteps.length }} skipped {{ skippedSteps.length === 1 ? 'item' : 'items' }}</h3>
        </div>
        <p class="mb-3 text-sm text-amber-800 dark:text-slate-400">
          The following stayed on the source cluster. Move them manually before decommissioning anything.
        </p>
        <ul class="space-y-1.5">
          <li v-for="s in skippedSteps" :key="s.name" class="text-sm text-amber-800 dark:text-slate-400">
            <span class="font-medium dark:text-slate-300">{{ s.name }}</span>
            <span v-if="s.detail" class="text-amber-700 dark:text-slate-400"> — {{ s.detail }}</span>
          </li>
        </ul>
      </NoticeCallout>

      <!-- Complete without custom domains -->
      <NoticeCallout
        v-if="phase === 'complete' && domains.length === 0"
        :tone="skippedSteps.length > 0 ? 'warning' : 'success'"
        class="p-6 text-center"
      >
        <Check class="mx-auto mb-3 h-10 w-10" :class="skippedSteps.length > 0 ? 'text-amber-500 dark:text-orange-300' : 'text-emerald-500 dark:text-emerald-300'" :stroke-width="1.75" />
        <h3 class="text-sm font-semibold text-slate-900" :class="skippedSteps.length > 0 ? 'dark:text-orange-300' : 'dark:text-emerald-300'">
          {{ skippedSteps.length > 0 ? 'Migration complete, with skipped data' : 'Migration complete' }}
        </h3>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">
          {{ skippedSteps.length > 0
            ? 'The projects are on the new cluster, but the skipped items above still live only here.'
            : 'All projects have been migrated to the new cluster.' }}
        </p>
      </NoticeCallout>

      <!-- Cancel button -->
      <div v-if="phase === 'migrating'" class="text-center">
        <button @click="handleCancel" class="text-sm text-slate-500 hover:text-red-600 dark:hover:text-red-400">Cancel migration</button>
      </div>
    </div>
  </div>
</template>

<style>
@keyframes migrateFlow {
  0%, 100% { opacity: 0.15; transform: scale(0.7); }
  20%, 40% { opacity: 1; transform: scale(1.15); }
}
</style>
