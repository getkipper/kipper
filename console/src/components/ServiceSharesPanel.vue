<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { Link2, Copy, Trash2, RefreshCw, KeyRound, Loader2, ShieldAlert } from 'lucide-vue-next'
import { useToast } from '@/composables/useToast'
import { useModal } from '@/composables/useModal'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import {
  createShare,
  fetchShares,
  revokeShare,
  revokeAllShares,
  rotateShareKey,
  type ShareLink,
} from '@/api/services'

const props = defineProps<{
  serviceName: string
  namespace: string
}>()

const toast = useToast()
const modal = useModal()

const links = ref<ShareLink[]>([])
const loading = ref(false)
const creating = ref(false)

// The just-minted link, shown once with its token. Listings never carry a
// URL — the token can't be recovered, so this is the only chance to copy it.
const minted = ref<ShareLink | null>(null)

const expiresOptions = [
  { label: '24 hours', value: '24h' },
  { label: '3 days', value: '72h' },
  { label: '7 days', value: '168h' },
  { label: '30 days', value: '720h' },
]
const expiresIn = ref('72h')
const label = ref('')

async function load() {
  loading.value = true
  try {
    links.value = await fetchShares(props.serviceName, props.namespace)
  } catch (err) {
    toast.error(`Failed to load share links: ${message(err)}`)
  } finally {
    loading.value = false
  }
}

async function mint() {
  creating.value = true
  try {
    const link = await createShare(props.serviceName, props.namespace, {
      expires_in: expiresIn.value,
      label: label.value.trim(),
    })
    minted.value = link
    label.value = ''
    toast.success('Share link created')
    await load()
  } catch (err) {
    toast.error(`Failed to create share link: ${message(err)}`)
  } finally {
    creating.value = false
  }
}

async function copy(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    toast.success('Link copied to clipboard')
  } catch {
    toast.error('Could not copy — select the link and copy it manually')
  }
}

function confirmRevoke(link: ShareLink) {
  modal.open(ConfirmDialog, {
    title: 'Revoke this share link?',
    message: `Anyone still holding it loses access to the ${props.serviceName} UI immediately.`,
    confirmLabel: 'Revoke link',
    onConfirm: async () => {
      modal.close()
      try {
        await revokeShare(props.serviceName, props.namespace, link.id)
        if (minted.value?.id === link.id) minted.value = null
        toast.success('Share link revoked')
        await load()
      } catch (err) {
        toast.error(`Failed to revoke: ${message(err)}`)
      }
    },
  })
}

function confirmRevokeAll() {
  modal.open(ConfirmDialog, {
    title: 'Revoke every share link in the cluster?',
    message: 'This kills all links for all services at once. Use it if a link leaked. To fully retire a leaked signing key, also rotate it twice.',
    confirmLabel: 'Revoke all links',
    confirmPhrase: 'revoke all',
    onConfirm: async () => {
      modal.close()
      try {
        await revokeAllShares()
        minted.value = null
        toast.success('Revoked every share link in the cluster')
        await load()
      } catch (err) {
        toast.error(`Failed to revoke all: ${message(err)}`)
      }
    },
  })
}

function confirmRotate() {
  modal.open(ConfirmDialog, {
    title: 'Rotate the share signing key?',
    message: 'Existing links stay valid until they expire or the next rotation. Rotate a second time to retire a leaked key completely.',
    confirmLabel: 'Rotate key',
    danger: false,
    onConfirm: async () => {
      modal.close()
      try {
        const res = await rotateShareKey()
        toast.success(`Signing key rotated (now ${res.current_kid})`)
      } catch (err) {
        toast.error(`Failed to rotate key: ${message(err)}`)
      }
    },
  })
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err)
}

function formatExpiry(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}

function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id
}

onMounted(load)
</script>

<template>
  <div class="space-y-5 p-5">
    <div>
      <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Share the {{ serviceName }} UI</h3>
      <p class="mt-1 text-xs text-slate-500 dark:text-slate-400">
        Create a signed, expiring link that opens the UI without a Kipper login. Hand it to
        someone who needs to see the UI but should not have console access. Anyone with the link
        can open it until it expires or you revoke it.
      </p>
    </div>

    <!-- Mint form -->
    <div class="rounded-lg border border-slate-200 p-4 dark:border-slate-700">
      <div class="flex flex-wrap items-end gap-3">
        <div>
          <label class="block text-xs font-medium text-slate-600 dark:text-slate-400">Expires</label>
          <select
            v-model="expiresIn"
            class="mt-1 rounded-lg border border-slate-300 bg-white px-3 py-2 text-sm dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
          >
            <option v-for="o in expiresOptions" :key="o.value" :value="o.value">{{ o.label }}</option>
          </select>
        </div>
        <div class="min-w-[12rem] flex-1">
          <label class="block text-xs font-medium text-slate-600 dark:text-slate-400">Label (optional)</label>
          <input
            v-model="label"
            type="text"
            maxlength="100"
            placeholder="e.g. PO review"
            class="mt-1 block w-full rounded-lg border border-slate-300 bg-white px-3 py-2 text-sm dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
            @keyup.enter="mint"
          />
        </div>
        <button
          :disabled="creating"
          class="inline-flex items-center gap-1.5 rounded-lg bg-kipper-600 px-3.5 py-2 text-sm font-medium text-white hover:bg-kipper-700 disabled:opacity-50"
          @click="mint"
        >
          <Loader2 v-if="creating" class="h-4 w-4 animate-spin" />
          <Link2 v-else class="h-4 w-4" />
          Create link
        </button>
      </div>

      <!-- Just-minted link: shown once with its token. -->
      <div
        v-if="minted"
        class="mt-4 rounded-lg border border-kipper-200 bg-kipper-50 p-3 dark:border-kipper-900 dark:bg-kipper-950/40"
      >
        <p class="text-xs font-medium text-kipper-800 dark:text-kipper-300">
          Copy this link now — it carries the token and won't be shown again.
        </p>
        <div class="mt-2 flex items-center gap-2">
          <code class="flex-1 truncate rounded bg-white px-2 py-1.5 font-mono text-xs text-slate-700 dark:bg-slate-900 dark:text-slate-200">{{ minted.url }}</code>
          <button
            class="inline-flex items-center gap-1 rounded-md border border-kipper-300 px-2 py-1.5 text-xs font-medium text-kipper-700 hover:bg-white dark:border-kipper-800 dark:text-kipper-300 dark:hover:bg-slate-900"
            @click="minted.url && copy(minted.url)"
          >
            <Copy class="h-3.5 w-3.5" /> Copy
          </button>
        </div>
      </div>
    </div>

    <!-- Existing links -->
    <div>
      <div class="mb-2 flex items-center justify-between">
        <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Active links</h3>
        <button
          class="rounded p-1 text-slate-400 hover:bg-slate-100 dark:hover:bg-slate-800"
          title="Refresh"
          @click="load"
        >
          <RefreshCw class="h-3.5 w-3.5" :class="{ 'animate-spin': loading }" />
        </button>
      </div>

      <div v-if="loading && links.length === 0" class="text-sm text-slate-500">Loading…</div>
      <div
        v-else-if="links.length === 0"
        class="rounded-lg border border-dashed border-slate-300 p-4 text-center text-sm text-slate-500 dark:border-slate-700"
      >
        No active share links for {{ serviceName }}.
      </div>
      <div v-else class="space-y-2">
        <div
          v-for="link in links"
          :key="link.id"
          class="flex items-center justify-between rounded-lg border border-slate-200 bg-slate-50 px-3 py-2 dark:border-slate-700 dark:bg-slate-800"
        >
          <div class="min-w-0">
            <div class="flex items-center gap-2">
              <span class="font-mono text-xs text-slate-500 dark:text-slate-400">{{ shortId(link.id) }}</span>
              <span v-if="link.label" class="truncate text-sm font-medium text-slate-900 dark:text-slate-50">{{ link.label }}</span>
            </div>
            <div class="mt-0.5 text-xs text-slate-500 dark:text-slate-400">
              Expires {{ formatExpiry(link.expires_at) }}<span v-if="link.created_by"> · {{ link.created_by }}</span>
            </div>
          </div>
          <button
            class="inline-flex items-center gap-1 rounded-md border border-slate-300 px-2 py-1.5 text-xs font-medium text-red-600 hover:bg-red-50 dark:border-slate-600 dark:text-red-400 dark:hover:bg-red-950/40"
            @click="confirmRevoke(link)"
          >
            <Trash2 class="h-3.5 w-3.5" /> Revoke
          </button>
        </div>
      </div>
    </div>

    <!-- Emergency controls -->
    <div class="rounded-lg border border-red-200 p-4 dark:border-red-900/60">
      <div class="flex items-center gap-2">
        <ShieldAlert class="h-4 w-4 text-red-500" />
        <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-50">If a link leaked</h3>
      </div>
      <p class="mt-1 text-xs text-slate-500 dark:text-slate-400">
        Revoke every link at once, then rotate the signing key twice to retire it completely.
      </p>
      <div class="mt-3 flex flex-wrap gap-2">
        <button
          class="inline-flex items-center gap-1.5 rounded-lg border border-red-300 px-3 py-2 text-xs font-medium text-red-700 hover:bg-red-50 dark:border-red-800 dark:text-red-300 dark:hover:bg-red-950/40"
          @click="confirmRevokeAll"
        >
          <Trash2 class="h-3.5 w-3.5" /> Revoke all links
        </button>
        <button
          class="inline-flex items-center gap-1.5 rounded-lg border border-slate-300 px-3 py-2 text-xs font-medium text-slate-700 hover:bg-slate-50 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-800"
          @click="confirmRotate"
        >
          <KeyRound class="h-3.5 w-3.5" /> Rotate signing key
        </button>
      </div>
    </div>
  </div>
</template>
