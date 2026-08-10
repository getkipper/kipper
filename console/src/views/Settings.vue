<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { Sparkles, Eye, EyeOff, Shield, Gauge, AlertTriangle, Bell, Container, GitBranch, Plus, Trash2, Mail } from 'lucide-vue-next'
import SaveButton from '@/components/SaveButton.vue'
import TwoFactorPanel from '@/components/TwoFactorPanel.vue'
import CredentialHealthBadge from '@/components/CredentialHealthBadge.vue'
import NoticeCallout from '@/components/NoticeCallout.vue'
import RevealDialog from '@/components/RevealDialog.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import { useModal } from '@/composables/useModal'
import { formatDateTime } from '@/utils/datetime'

interface RevealTarget {
  type: 'git' | 'registry'
  name: string
  server: string
}
const revealTarget = ref<RevealTarget | null>(null)
import { useToast } from '@/composables/useToast'
import { getAISettings, updateAISettings } from '@/api/ai-settings'
import { getMode, updateMode, getResourceLog, type ResourceLogEntry } from '@/api/mode'
import { getSlackSettings, updateSlackSettings } from '@/api/slack'
import { getSmtpSettings, updateSmtpSettings, testSmtpSettings } from '@/api/smtp'
import client from '@/api/client'

const toast = useToast()
const modal = useModal()

const loading = ref(false)
const saving = ref(false)
const showKey = ref(false)

// Resource management mode
const resourceMode = ref<'auto' | 'expert'>('auto')
const switchingMode = ref(false)
const showModeWarning = ref(false)
const pendingMode = ref<'auto' | 'expert'>('auto')
const resourceLog = ref<ResourceLogEntry[]>([])

function requestModeSwitch(newMode: 'auto' | 'expert') {
  if (newMode === resourceMode.value) return
  pendingMode.value = newMode
  showModeWarning.value = true
}

async function confirmModeSwitch() {
  switchingMode.value = true
  showModeWarning.value = false
  try {
    await updateMode(pendingMode.value)
    resourceMode.value = pendingMode.value
    toast.success(`Switched to ${pendingMode.value} mode`)
  } catch {
    toast.error('Failed to switch mode')
  } finally {
    switchingMode.value = false
  }
}

async function loadResourceLog() {
  try {
    resourceLog.value = await getResourceLog()
  } catch {
    resourceLog.value = []
  }
}

const recentLog = computed(() => resourceLog.value.slice(-10).reverse())

// Slack settings
const slackWebhookUrl = ref('')
const slackSaving = ref(false)
const showSlackUrl = ref(false)
const slackTesting = ref(false)

async function loadSlackSettings() {
  try {
    const config = await getSlackSettings()
    slackWebhookUrl.value = config.webhook_url || ''
  } catch {
    slackWebhookUrl.value = ''
  }
}

async function handleSlackSave() {
  slackSaving.value = true
  try {
    const resp = await updateSlackSettings(slackWebhookUrl.value)
    slackWebhookUrl.value = resp.webhook_url
    showSlackUrl.value = false
    toast.success('Slack webhook saved')
  } catch {
    toast.error('Failed to save Slack webhook')
  } finally {
    slackSaving.value = false
  }
}

async function handleSlackTest() {
  slackTesting.value = true
  try {
    await updateSlackSettings(slackWebhookUrl.value)
    toast.success('Test alert sent to Slack')
  } catch {
    toast.error('Failed to send test alert')
  } finally {
    slackTesting.value = false
  }
}

// SMTP settings
const smtpHost = ref('')
const smtpPort = ref(587)
const smtpUsername = ref('')
const smtpPassword = ref('')
const smtpFrom = ref('')
const smtpTls = ref(true)
const smtpSaving = ref(false)
const showSmtpPassword = ref(false)
const smtpTesting = ref(false)
const smtpTestTo = ref('')

async function loadSmtpSettings() {
  try {
    const config = await getSmtpSettings()
    smtpHost.value = config.host || ''
    smtpPort.value = config.port || 587
    smtpUsername.value = config.username || ''
    smtpPassword.value = config.password || ''
    smtpFrom.value = config.from || ''
    smtpTls.value = config.tls !== false
  } catch {
    // defaults are fine
  }
}

async function handleSmtpSave() {
  smtpSaving.value = true
  try {
    const resp = await updateSmtpSettings({
      host: smtpHost.value,
      port: smtpPort.value,
      username: smtpUsername.value,
      password: smtpPassword.value,
      from: smtpFrom.value,
      tls: smtpTls.value,
    })
    smtpPassword.value = resp.password
    showSmtpPassword.value = false
    toast.success('SMTP settings saved')
  } catch {
    toast.error('Failed to save SMTP settings')
  } finally {
    smtpSaving.value = false
  }
}

async function handleSmtpTest() {
  smtpTesting.value = true
  try {
    await testSmtpSettings(smtpTestTo.value || undefined)
    toast.success(`Test email sent to ${smtpTestTo.value || 'your account'}`)
  } catch {
    toast.error('Failed to send test email')
  } finally {
    smtpTesting.value = false
  }
}

const provider = ref('')
const apiKey = ref('')
const model = ref('')
const ollamaUrl = ref('')

const providers = [
  { value: '', label: 'None — AI features disabled' },
  { value: 'claude', label: 'Claude (Anthropic)' },
  { value: 'openai', label: 'OpenAI' },
  { value: 'ollama', label: 'Ollama (self-hosted)' },
]

const defaultModel = computed(() => {
  switch (provider.value) {
    case 'claude': return 'claude-sonnet-4-5-20250514'
    case 'openai': return 'gpt-4o'
    case 'ollama': return 'llama3.1'
    default: return ''
  }
})

const modelPlaceholder = computed(() => {
  return defaultModel.value ? `Default: ${defaultModel.value}` : ''
})

const needsApiKey = computed(() => ['claude', 'openai'].includes(provider.value))
const needsOllamaUrl = computed(() => provider.value === 'ollama')

async function handleSave() {
  saving.value = true
  try {
    await updateAISettings({
      provider: provider.value,
      api_key: apiKey.value,
      model: model.value,
      ollama_url: ollamaUrl.value,
    })
    toast.success('AI settings saved')
    showKey.value = false
  } catch {
    toast.error('Failed to save settings')
  } finally {
    saving.value = false
  }
}

// Registry credentials
import { fetchRegistries, fetchRegistryHealth, addRegistry, removeRegistry, type RegistryEntry, type TokenHealth } from '@/api/registry'

const registries = ref<RegistryEntry[]>([])
const registryHealth = ref<Record<string, TokenHealth>>({})
const showAddRegistry = ref(false)
const newRegServer = ref('')
const newRegUsername = ref('')
const newRegPassword = ref('')
const newRegName = ref('')
const savingRegistry = ref(false)

async function loadRegistries() {
  try {
    registries.value = await fetchRegistries()
  } catch {
    // ignore
  }
  try {
    registryHealth.value = await fetchRegistryHealth()
  } catch {
    // ignore — badges simply absent if health check fails
  }
}

async function handleAddRegistry() {
  if (!newRegServer.value || !newRegUsername.value || !newRegPassword.value) return
  savingRegistry.value = true
  try {
    await addRegistry({
      name: newRegName.value || undefined,
      server: newRegServer.value,
      username: newRegUsername.value,
      password: newRegPassword.value,
    })
    toast.success('Registry credentials saved')
    newRegServer.value = ''
    newRegUsername.value = ''
    newRegPassword.value = ''
    newRegName.value = ''
    showAddRegistry.value = false
    await loadRegistries()
  } catch {
    toast.error('Failed to save registry credentials')
  } finally {
    savingRegistry.value = false
  }
}

function handleRemoveRegistry(name: string) {
  modal.open(ConfirmDialog, {
    title: `Remove registry credentials ${name}?`,
    message: 'Apps that pull from this registry will fail to deploy until credentials are added again.',
    confirmLabel: 'Remove',
    onConfirm: async () => {
      modal.close()
      try {
        await removeRegistry(name)
        toast.success('Registry removed')
        await loadRegistries()
      } catch {
        toast.error('Failed to remove registry')
      }
    },
  })
}

// Git credentials
import { fetchGitCredentials, fetchGitCredentialHealth, addGitCredential, removeGitCredential, type GitCredentialEntry } from '@/api/git-credentials'

const gitCredentials = ref<GitCredentialEntry[]>([])
const gitCredentialHealth = ref<Record<string, TokenHealth>>({})
const showAddGitCred = ref(false)
const newGitServer = ref('')
const newGitUsername = ref('oauth2')
const newGitToken = ref('')
const newGitName = ref('')
const savingGitCred = ref(false)

async function loadGitCredentials() {
  try {
    gitCredentials.value = await fetchGitCredentials()
  } catch {
    // ignore
  }
  try {
    gitCredentialHealth.value = await fetchGitCredentialHealth()
  } catch {
    // ignore — badges simply absent if health check fails
  }
}

async function handleAddGitCredential() {
  if (!newGitServer.value || !newGitToken.value) return
  savingGitCred.value = true
  try {
    await addGitCredential({
      name: newGitName.value || undefined,
      server: newGitServer.value,
      username: newGitUsername.value || 'oauth2',
      token: newGitToken.value,
    })
    toast.success('Git credential saved')
    newGitServer.value = ''
    newGitUsername.value = 'oauth2'
    newGitToken.value = ''
    newGitName.value = ''
    showAddGitCred.value = false
    await loadGitCredentials()
  } catch {
    toast.error('Failed to save git credential')
  } finally {
    savingGitCred.value = false
  }
}

function handleRemoveGitCredential(name: string) {
  modal.open(ConfirmDialog, {
    title: `Remove git credential ${name}?`,
    message: 'Git-based builds that use this credential will fail until it is added again.',
    confirmLabel: 'Remove',
    onConfirm: async () => {
      modal.close()
      try {
        await removeGitCredential(name)
        toast.success('Git credential removed')
        await loadGitCredentials()
      } catch (e) {
        const message = e instanceof Error ? e.message : 'Failed to remove git credential'
        toast.error(message)
      }
    },
  })
}

// OAuth connectors
interface Connector {
  type: string
  enabled: boolean
  has_keys: boolean
}

const connectors = ref<Connector[]>([])
const editingConnector = ref('')
const connClientId = ref('')
const connClientSecret = ref('')
const connOrg = ref('')
const connDomain = ref('')
const connBaseUrl = ref('')
const savingConnector = ref(false)

async function loadConnectors() {
  try {
    const { data } = await client.get<Connector[]>('/settings/auth')
    connectors.value = data
  } catch {
    // ignore
  }
}

function editConnector(type: string) {
  editingConnector.value = type
  connClientId.value = ''
  connClientSecret.value = ''
  connOrg.value = ''
  connDomain.value = ''
  connBaseUrl.value = ''
}

async function saveConnector(type: string, enabled: boolean) {
  savingConnector.value = true
  try {
    await client.put('/settings/auth', {
      type,
      client_id: connClientId.value,
      client_secret: connClientSecret.value,
      org: connOrg.value,
      domain: connDomain.value,
      base_url: connBaseUrl.value,
      enabled,
    })
    toast.success(`${type} ${enabled ? 'enabled' : 'disabled'} — Dex restarting`)
    editingConnector.value = ''
    await loadConnectors()
  } catch {
    toast.error(`Failed to update ${type}`)
  } finally {
    savingConnector.value = false
  }
}

async function disableConnector(type: string) {
  savingConnector.value = true
  try {
    await client.put('/settings/auth', { type, enabled: false, client_id: '', client_secret: '' })
    toast.success(`${type} disabled`)
    await loadConnectors()
  } catch {
    toast.error(`Failed to disable ${type}`)
  } finally {
    savingConnector.value = false
  }
}

onMounted(async () => {
  loading.value = true
  try {
    const [config, modeResp] = await Promise.all([
      getAISettings().catch(() => ({ provider: '', api_key: '', model: '', ollama_url: '' })),
      getMode().catch(() => ({ mode: 'auto' as const })),
    ])
    provider.value = config.provider || ''
    apiKey.value = config.api_key || ''
    model.value = config.model || ''
    ollamaUrl.value = config.ollama_url || ''
    resourceMode.value = modeResp.mode
  } catch {
    // No config yet
  } finally {
    loading.value = false
  }
  loadConnectors()
  loadRegistries()
  loadGitCredentials()
  loadResourceLog()
  loadSlackSettings()
  loadSmtpSettings()
})
</script>

<template>
  <div class="animate-fade-in">
    <!-- Header -->
    <div class="mb-8">
      <h1 class="text-2xl font-semibold tracking-tight text-slate-900 dark:text-slate-50">Settings</h1>
      <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Cluster-wide configuration</p>
    </div>

    <div v-if="loading" class="py-12 text-center text-sm text-slate-500 dark:text-slate-400">Loading...</div>

    <div v-else class="space-y-8">
      <!-- Two-factor authentication -->
      <TwoFactorPanel />

      <!-- Resource Management -->
      <div class="rounded-xl border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900">
        <div class="flex items-center gap-3 border-b border-slate-200 px-6 py-4 dark:border-slate-800">
          <Gauge class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
          <div>
            <h2 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Resource Management</h2>
            <p class="text-xs text-slate-500 dark:text-slate-400">Control how CPU and memory are managed for your apps</p>
          </div>
        </div>

        <div class="space-y-5 p-6">
          <div>
            <p class="mb-3 text-sm text-slate-600 dark:text-slate-400">
              In auto mode, Kipper monitors your apps and automatically adjusts CPU and memory. In expert mode, you control everything via the Scale and Resources tabs.
            </p>
            <div class="flex items-center gap-3">
              <button
                @click="requestModeSwitch('auto')"
                :disabled="switchingMode"
                class="rounded-lg px-4 py-2 text-sm font-medium transition-colors"
                :class="resourceMode === 'auto'
                  ? 'bg-kipper-600 text-white'
                  : 'border border-slate-300 text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800'"
              >
                Auto
              </button>
              <button
                @click="requestModeSwitch('expert')"
                :disabled="switchingMode"
                class="rounded-lg px-4 py-2 text-sm font-medium transition-colors"
                :class="resourceMode === 'expert'
                  ? 'bg-kipper-600 text-white'
                  : 'border border-slate-300 text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800'"
              >
                Expert
              </button>
            </div>
          </div>

          <!-- Mode switch warning -->
          <NoticeCallout v-if="showModeWarning" tone="warning" class="p-4">
            <div class="flex items-start gap-2">
              <AlertTriangle class="mt-0.5 h-4 w-4 flex-shrink-0 text-amber-600 dark:text-orange-300" />
              <div>
                <p v-if="pendingMode === 'auto'" class="text-sm text-amber-800 dark:text-slate-300">
                  Auto mode will take over resource management. It may change your current settings based on actual usage.
                </p>
                <p v-else class="text-sm text-amber-800 dark:text-slate-300">
                  You are now responsible for managing resources. The current auto-managed values are preserved as your starting point.
                </p>
                <div class="mt-3 flex gap-2">
                  <button
                    @click="confirmModeSwitch"
                    class="rounded-md bg-amber-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-amber-700"
                  >
                    Switch to {{ pendingMode }}
                  </button>
                  <button
                    @click="showModeWarning = false"
                    class="rounded-md border border-slate-300 px-3 py-1.5 text-xs font-medium text-slate-600 dark:border-slate-600 dark:text-slate-400"
                  >
                    Cancel
                  </button>
                </div>
              </div>
            </div>
          </NoticeCallout>

          <!-- Resource log -->
          <div v-if="recentLog.length > 0">
            <h3 class="mb-2 text-xs font-medium text-slate-600 dark:text-slate-400">Recent auto-mode changes</h3>
            <div class="overflow-x-auto rounded-lg border border-slate-200 dark:border-slate-700">
              <table class="w-full text-xs">
                <thead class="bg-slate-50 dark:bg-slate-800">
                  <tr>
                    <th class="px-3 py-2 text-left font-medium text-slate-600 dark:text-slate-400">Time</th>
                    <th class="px-3 py-2 text-left font-medium text-slate-600 dark:text-slate-400">App</th>
                    <th class="px-3 py-2 text-left font-medium text-slate-600 dark:text-slate-400">Action</th>
                    <th class="px-3 py-2 text-left font-medium text-slate-600 dark:text-slate-400">Reason</th>
                  </tr>
                </thead>
                <tbody class="divide-y divide-slate-200 dark:divide-slate-700">
                  <tr v-for="(entry, i) in recentLog" :key="i" class="text-slate-700 dark:text-slate-300">
                    <td class="whitespace-nowrap px-3 py-2 font-mono">{{ formatDateTime(entry.time) }}</td>
                    <td class="px-3 py-2 font-medium">{{ entry.app }}</td>
                    <td class="px-3 py-2">{{ entry.action }} ({{ entry.from }} &rarr; {{ entry.to }})</td>
                    <td class="px-3 py-2 text-slate-500 dark:text-slate-400">{{ entry.reason }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
          </div>
        </div>
      </div>

      <!-- AI Configuration -->
      <div class="rounded-xl border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900">
        <div class="flex items-center gap-3 border-b border-slate-200 px-6 py-4 dark:border-slate-800">
          <Sparkles class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
          <div>
            <h2 class="text-sm font-semibold text-slate-900 dark:text-slate-50">AI Assistant</h2>
            <p class="text-xs text-slate-500 dark:text-slate-400">Powers the code assistant, log analysis, and error diagnosis</p>
          </div>
        </div>

        <div class="space-y-5 p-6">
          <!-- Provider -->
          <div>
            <label class="mb-1.5 block text-xs font-medium text-slate-600 dark:text-slate-400">Provider</label>
            <select
              v-model="provider"
              class="block w-full max-w-md rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
            >
              <option v-for="p in providers" :key="p.value" :value="p.value">{{ p.label }}</option>
            </select>
          </div>

          <!-- API Key -->
          <div v-if="needsApiKey">
            <label class="mb-1.5 block text-xs font-medium text-slate-600 dark:text-slate-400">API Key</label>
            <div class="flex items-center gap-2">
              <input
                v-model="apiKey"
                :type="showKey ? 'text' : 'password'"
                :placeholder="provider === 'claude' ? 'sk-ant-...' : 'sk-...'"
                class="block w-full max-w-md rounded-md border border-slate-300 bg-white px-3 py-2 font-mono text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
              />
              <button
                @click="showKey = !showKey"
                class="rounded-md p-2 text-slate-400 hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800 dark:hover:text-slate-300"
              >
                <EyeOff v-if="showKey" class="h-4 w-4" />
                <Eye v-else class="h-4 w-4" />
              </button>
            </div>
            <p class="mt-1 text-xs text-slate-500 dark:text-slate-400">
              Stored encrypted in the cluster. Never sent to the browser.
            </p>
          </div>

          <!-- Ollama URL -->
          <div v-if="needsOllamaUrl">
            <label class="mb-1.5 block text-xs font-medium text-slate-600 dark:text-slate-400">Ollama URL</label>
            <input
              v-model="ollamaUrl"
              type="text"
              placeholder="http://localhost:11434"
              class="block w-full max-w-md rounded-md border border-slate-300 bg-white px-3 py-2 font-mono text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
            />
          </div>

          <!-- Model -->
          <div v-if="provider">
            <label class="mb-1.5 block text-xs font-medium text-slate-600 dark:text-slate-400">Model (optional)</label>
            <input
              v-model="model"
              type="text"
              :placeholder="modelPlaceholder"
              class="block w-full max-w-md rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
            />
          </div>

          <!-- Save button -->
          <div class="pt-2">
            <SaveButton :saving="saving" @click="handleSave" />
          </div>
        </div>
      <!-- Slack Notifications -->
      <div class="rounded-xl border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900">
        <div class="flex items-center gap-3 border-b border-slate-200 px-6 py-4 dark:border-slate-800">
          <Bell class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
          <div>
            <h2 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Slack Notifications</h2>
            <p class="text-xs text-slate-500 dark:text-slate-400">Receive alerts in Slack when the auto mode controller adjusts resources, detects OOM kills, or recovers stuck pods</p>
          </div>
        </div>

        <div class="space-y-5 p-6">
          <div>
            <label class="mb-1.5 block text-xs font-medium text-slate-600 dark:text-slate-400">Webhook URL</label>
            <div class="flex items-center gap-2">
              <input
                v-model="slackWebhookUrl"
                :type="showSlackUrl ? 'text' : 'password'"
                placeholder="https://hooks.slack.com/services/..."
                class="block w-full max-w-md rounded-md border border-slate-300 bg-white px-3 py-2 font-mono text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
              />
              <button
                @click="showSlackUrl = !showSlackUrl"
                class="rounded-md p-2 text-slate-400 hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800 dark:hover:text-slate-300"
              >
                <EyeOff v-if="showSlackUrl" class="h-4 w-4" />
                <Eye v-else class="h-4 w-4" />
              </button>
            </div>
            <p class="mt-1 text-xs text-slate-500 dark:text-slate-400">
              Create an incoming webhook in your Slack workspace settings and paste the URL here.
            </p>
          </div>

          <div class="flex gap-2 pt-2">
            <SaveButton :saving="slackSaving" @click="handleSlackSave" />
            <button
              @click="handleSlackTest"
              :disabled="slackTesting || !slackWebhookUrl"
              class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-600 transition-colors hover:bg-slate-100 disabled:cursor-not-allowed disabled:opacity-50 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
            >
              {{ slackTesting ? 'Sending...' : 'Send test' }}
            </button>
          </div>
        </div>
      </div>

      <!-- Email (SMTP) -->
      <div class="rounded-xl border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900">
        <div class="flex items-center gap-3 border-b border-slate-200 px-6 py-4 dark:border-slate-800">
          <Mail class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
          <div>
            <h2 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Email (SMTP)</h2>
            <p class="text-xs text-slate-500 dark:text-slate-400">Configure SMTP to send invite emails and notifications</p>
          </div>
        </div>

        <div class="space-y-5 p-6">
          <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div>
              <label class="mb-1.5 block text-xs font-medium text-slate-600 dark:text-slate-400">SMTP Host</label>
              <input
                v-model="smtpHost"
                type="text"
                placeholder="smtp.example.com"
                class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
              />
            </div>
            <div>
              <label class="mb-1.5 block text-xs font-medium text-slate-600 dark:text-slate-400">Port</label>
              <input
                v-model.number="smtpPort"
                type="number"
                placeholder="587"
                class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
              />
            </div>
          </div>

          <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div>
              <label class="mb-1.5 block text-xs font-medium text-slate-600 dark:text-slate-400">Username</label>
              <input
                v-model="smtpUsername"
                type="text"
                placeholder="user@example.com"
                class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
              />
            </div>
            <div>
              <label class="mb-1.5 block text-xs font-medium text-slate-600 dark:text-slate-400">Password</label>
              <div class="flex items-center gap-2">
                <input
                  v-model="smtpPassword"
                  :type="showSmtpPassword ? 'text' : 'password'"
                  placeholder="App password or SMTP password"
                  class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
                />
                <button
                  @click="showSmtpPassword = !showSmtpPassword"
                  class="rounded-md p-2 text-slate-400 hover:bg-slate-100 hover:text-slate-600 dark:hover:bg-slate-800 dark:hover:text-slate-300"
                >
                  <EyeOff v-if="showSmtpPassword" class="h-4 w-4" />
                  <Eye v-else class="h-4 w-4" />
                </button>
              </div>
            </div>
          </div>

          <div>
            <label class="mb-1.5 block text-xs font-medium text-slate-600 dark:text-slate-400">From address</label>
            <input
              v-model="smtpFrom"
              type="text"
              placeholder="Kipper <noreply@example.com>"
              class="block w-full max-w-md rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
            />
          </div>

          <div class="flex items-center gap-3">
            <button
              @click="smtpTls = !smtpTls"
              class="relative inline-flex h-6 w-11 items-center rounded-full transition-colors"
              :class="smtpTls ? 'bg-kipper-600' : 'bg-slate-300 dark:bg-slate-600'"
            >
              <span
                class="inline-block h-4 w-4 rounded-full bg-white transition-transform"
                :class="smtpTls ? 'translate-x-6' : 'translate-x-1'"
              />
            </button>
            <span class="text-xs font-medium text-slate-600 dark:text-slate-400">Use TLS</span>
          </div>

          <div class="flex flex-wrap items-end gap-2 pt-2">
            <SaveButton :saving="smtpSaving" @click="handleSmtpSave" />
            <input
              v-model="smtpTestTo"
              type="email"
              placeholder="test@example.com"
              class="w-48 rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
            />
            <button
              @click="handleSmtpTest"
              :disabled="smtpTesting || !smtpHost"
              class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-600 transition-colors hover:bg-slate-100 disabled:cursor-not-allowed disabled:opacity-50 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
            >
              {{ smtpTesting ? 'Sending...' : 'Send test' }}
            </button>
          </div>
        </div>
      </div>

      <!-- Container Registries -->
      <div class="rounded-xl border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900">
        <div class="flex items-center justify-between border-b border-slate-200 px-6 py-4 dark:border-slate-800">
          <div class="flex items-center gap-3">
            <Container class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
            <div>
              <h2 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Container Registries</h2>
              <p class="text-xs text-slate-500 dark:text-slate-400">Credentials for pulling images from private registries</p>
            </div>
          </div>
          <button
            @click="showAddRegistry = !showAddRegistry"
            class="inline-flex items-center gap-1.5 rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-600 transition-colors hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
          >
            <Plus class="h-3.5 w-3.5" />
            Add registry
          </button>
        </div>
        <div class="px-6 py-4 space-y-4">
          <!-- Add form -->
          <div v-if="showAddRegistry" class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800/50">
            <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div>
                <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Server</label>
                <input v-model="newRegServer" type="text" placeholder="registry.example.com" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
              </div>
              <div>
                <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Name (optional)</label>
                <input v-model="newRegName" type="text" placeholder="auto-generated from server" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
              </div>
              <div>
                <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Username</label>
                <input v-model="newRegUsername" type="text" placeholder="username or token name" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
              </div>
              <div>
                <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Password / Token</label>
                <input v-model="newRegPassword" type="password" placeholder="access token or password" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
              </div>
            </div>
            <div class="mt-3 flex justify-end gap-2">
              <button @click="showAddRegistry = false" class="rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800">Cancel</button>
              <button @click="handleAddRegistry" :disabled="savingRegistry || !newRegServer || !newRegUsername || !newRegPassword" class="rounded-lg bg-kipper-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-kipper-700 disabled:opacity-50">
                {{ savingRegistry ? 'Saving...' : 'Save' }}
              </button>
            </div>
          </div>

          <!-- Registry list -->
          <div v-if="registries.length" class="divide-y divide-slate-100 dark:divide-slate-800">
            <div v-for="reg in registries" :key="reg.name" class="flex items-center justify-between py-3">
              <div class="min-w-0 flex-1">
                <div class="flex items-center gap-2">
                  <span class="text-sm font-medium text-slate-900 dark:text-slate-50">{{ reg.server }}</span>
                  <CredentialHealthBadge :health="registryHealth[reg.name]" />
                </div>
                <div class="text-xs text-slate-500 dark:text-slate-400">{{ reg.username }} &middot; {{ reg.password }}</div>
              </div>
              <button @click="revealTarget = { type: 'registry', name: reg.name, server: reg.server }" class="ml-2 rounded-lg p-1.5 text-slate-400 hover:bg-slate-100 hover:text-slate-700 dark:hover:bg-slate-800 dark:hover:text-slate-300" title="Reveal password">
                <Eye class="h-4 w-4" :stroke-width="1.75" />
              </button>
              <button @click="handleRemoveRegistry(reg.name)" class="ml-1 rounded-lg p-1.5 text-slate-400 hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-950 dark:hover:text-red-400" title="Remove">
                <Trash2 class="h-4 w-4" :stroke-width="1.75" />
              </button>
            </div>
          </div>

          <p v-else-if="!showAddRegistry" class="text-sm text-slate-500 dark:text-slate-400">
            No registries configured. Apps using public images (Docker Hub, ghcr.io) do not need registry credentials.
          </p>
        </div>
      </div>

      <!-- Git Credentials -->
      <div class="rounded-xl border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900">
        <div class="flex items-center justify-between border-b border-slate-200 px-6 py-4 dark:border-slate-800">
          <div class="flex items-center gap-3">
            <GitBranch class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
            <div>
              <h2 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Git Credentials</h2>
              <p class="text-xs text-slate-500 dark:text-slate-400">Tokens for cloning private repositories during builds</p>
            </div>
          </div>
          <button
            @click="showAddGitCred = !showAddGitCred"
            class="inline-flex items-center gap-1.5 rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-600 transition-colors hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
          >
            <Plus class="h-3.5 w-3.5" />
            Add credential
          </button>
        </div>
        <div class="px-6 py-4 space-y-4">
          <!-- Add form -->
          <div v-if="showAddGitCred" class="rounded-lg border border-slate-200 bg-slate-50 p-4 dark:border-slate-700 dark:bg-slate-800/50">
            <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div>
                <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Server</label>
                <input v-model="newGitServer" type="text" placeholder="git.example.com" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
              </div>
              <div>
                <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Name (optional)</label>
                <input v-model="newGitName" type="text" placeholder="auto-generated from server" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
              </div>
              <div>
                <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Username</label>
                <input v-model="newGitUsername" type="text" placeholder="oauth2" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
              </div>
              <div>
                <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Token</label>
                <input v-model="newGitToken" type="password" placeholder="glpat-... or ghp_..." class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
              </div>
            </div>
            <div class="mt-3 flex justify-end gap-2">
              <button @click="showAddGitCred = false" class="rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800">Cancel</button>
              <button @click="handleAddGitCredential" :disabled="savingGitCred || !newGitServer || !newGitToken" class="rounded-lg bg-kipper-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-kipper-700 disabled:opacity-50">
                {{ savingGitCred ? 'Saving...' : 'Save' }}
              </button>
            </div>
          </div>

          <!-- Credential list -->
          <div v-if="gitCredentials.length" class="divide-y divide-slate-100 dark:divide-slate-800">
            <div v-for="cred in gitCredentials" :key="cred.name" class="flex items-center justify-between py-3">
              <div class="min-w-0 flex-1">
                <div class="flex items-center gap-2">
                  <span class="text-sm font-medium text-slate-900 dark:text-slate-50">{{ cred.server }}</span>
                  <span v-if="cred.app_count" class="rounded-full bg-kipper-100 px-2 py-0.5 text-xs font-medium text-kipper-700 dark:bg-kipper-900 dark:text-kipper-300">{{ cred.app_count }} app{{ cred.app_count === 1 ? '' : 's' }}</span>
                  <CredentialHealthBadge :health="gitCredentialHealth[cred.name]" />
                </div>
                <div class="text-xs text-slate-500 dark:text-slate-400">{{ cred.username }} &middot; {{ cred.token }}</div>
              </div>
              <button @click="revealTarget = { type: 'git', name: cred.name, server: cred.server }" class="ml-2 rounded-lg p-1.5 text-slate-400 hover:bg-slate-100 hover:text-slate-700 dark:hover:bg-slate-800 dark:hover:text-slate-300" title="Reveal token">
                <Eye class="h-4 w-4" :stroke-width="1.75" />
              </button>
              <button @click="handleRemoveGitCredential(cred.name)" class="ml-1 rounded-lg p-1.5 text-slate-400 hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-950 dark:hover:text-red-400" title="Remove">
                <Trash2 class="h-4 w-4" :stroke-width="1.75" />
              </button>
            </div>
          </div>

          <p v-else-if="!showAddGitCred" class="text-sm text-slate-500 dark:text-slate-400">
            No git credentials configured. Add a token to enable builds from private repositories.
          </p>
        </div>
      </div>

      <!-- OAuth Connectors -->
      <div class="rounded-xl border border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900">
        <div class="flex items-center gap-3 border-b border-slate-200 px-6 py-4 dark:border-slate-800">
          <Shield class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
          <div>
            <h2 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Single Sign-On</h2>
            <p class="text-xs text-slate-500 dark:text-slate-400">Let team members log in with GitHub, GitLab, or Google</p>
          </div>
        </div>

        <div class="p-6 space-y-4">
          <div
            v-for="conn in connectors"
            :key="conn.type"
            class="flex items-center justify-between rounded-lg border border-slate-200 p-4 dark:border-slate-700"
          >
            <div>
              <span class="text-sm font-medium capitalize text-slate-900 dark:text-slate-50">{{ conn.type }}</span>
              <span
                v-if="conn.enabled"
                class="ml-2 rounded-full bg-emerald-100 px-2 py-0.5 text-xs font-medium text-emerald-700 dark:bg-emerald-900 dark:text-emerald-300"
              >enabled</span>
            </div>
            <div class="flex items-center gap-2">
              <button
                v-if="conn.enabled"
                @click="disableConnector(conn.type)"
                class="rounded-md border border-red-300 px-3 py-1 text-xs font-medium text-red-600 hover:bg-red-50 dark:border-red-700 dark:text-red-400 dark:hover:bg-red-950"
              >Disable</button>
              <button
                @click="editConnector(conn.type)"
                class="rounded-md bg-kipper-600 px-3 py-1 text-xs font-medium text-white hover:bg-kipper-700"
              >{{ conn.enabled ? 'Reconfigure' : 'Enable' }}</button>
            </div>
          </div>

          <!-- Connector edit form -->
          <div v-if="editingConnector" class="rounded-lg border border-kipper-200 bg-kipper-50 p-4 dark:border-kipper-800 dark:bg-kipper-950">
            <h3 class="mb-3 text-sm font-semibold capitalize text-slate-900 dark:text-slate-50">Configure {{ editingConnector }}</h3>
            <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div>
                <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Client ID</label>
                <input v-model="connClientId" type="text" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
              </div>
              <div>
                <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Client Secret</label>
                <input v-model="connClientSecret" type="password" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
              </div>
              <div v-if="editingConnector === 'github'">
                <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Organization (optional)</label>
                <input v-model="connOrg" type="text" placeholder="Restrict to org members" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm placeholder-slate-400 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
              </div>
              <div v-if="editingConnector === 'gitlab'">
                <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Base URL (optional)</label>
                <input v-model="connBaseUrl" type="text" placeholder="https://gitlab.com" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm placeholder-slate-400 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
              </div>
              <div v-if="editingConnector === 'google'">
                <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Domain (optional)</label>
                <input v-model="connDomain" type="text" placeholder="Restrict to domain" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm placeholder-slate-400 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
              </div>
            </div>
            <div class="mt-3 flex justify-end gap-2">
              <button @click="editingConnector = ''" class="rounded-md border border-slate-300 px-3 py-1.5 text-xs font-medium text-slate-600 dark:border-slate-600 dark:text-slate-400">Cancel</button>
              <SaveButton :saving="savingConnector" label="Enable" @click="saveConnector(editingConnector, true)" />
            </div>
          </div>
        </div>
      </div>
      </div>
    </div>

    <RevealDialog
      v-if="revealTarget"
      :type="revealTarget.type"
      :name="revealTarget.name"
      :server="revealTarget.server"
      @close="revealTarget = null"
    />
  </div>
</template>
