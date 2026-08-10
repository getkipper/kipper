<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { Users, Plus, Trash2, RefreshCw, Link, Copy, Check, KeyRound } from 'lucide-vue-next'
import PasswordStrengthBar from '@/components/PasswordStrengthBar.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import NoticeCallout from '@/components/NoticeCallout.vue'
import { usePasswordStrength } from '@/composables/usePasswordStrength'
import { useModal } from '@/composables/useModal'
import { useToast } from '@/composables/useToast'
import { fetchUsers, createUser, updateUserRole, deleteUser, resetUserPassword, type User } from '@/api/users'
import { fetchProjects, type Project } from '@/api/projects'
import { useAuthStore } from '@/stores/auth'
import client from '@/api/client'

const toast = useToast()
const modal = useModal()
const auth = useAuthStore()

const users = ref<User[]>([])
const loading = ref(false)
const showCreate = ref(false)
const creating = ref(false)

const newEmail = ref('')
const newPassword = ref('')
const newRole = ref('deployer')
const { strength: newPasswordStrength } = usePasswordStrength(newPassword)

// Invite
const showInvite = ref(false)
const inviteRole = ref('deployer')
const inviteExpiry = ref('48h')
const inviteEmail = ref('')
const inviteProject = ref('')
const inviteProjectRole = ref('deployer')
const inviteURL = ref('')
const inviteEmailSent = ref(false)
const inviteCopied = ref(false)
const creatingInvite = ref(false)
const projects = ref<Project[]>([])

async function loadProjectsForInvite() {
  try {
    projects.value = await fetchProjects()
  } catch {
    projects.value = []
  }
}

async function handleInvite() {
  creatingInvite.value = true
  try {
    // A project invite grants membership of that project; the global role
    // stays viewer so the account can sign in without cluster-wide powers.
    const { data } = await client.post('/invites', {
      role: inviteProject.value ? 'viewer' : inviteRole.value,
      expires: inviteExpiry.value,
      email: inviteEmail.value.trim(),
      project: inviteProject.value || undefined,
      project_role: inviteProject.value ? inviteProjectRole.value : undefined,
    })
    inviteURL.value = data.url
    inviteEmailSent.value = data.email_sent || false
    await loadPendingInvites()
    if (inviteEmailSent.value) {
      toast.success(`Invite sent to ${inviteEmail.value}`)
    } else {
      toast.success('Invite link created')
    }
  } catch {
    toast.error('Failed to create invite')
  } finally {
    creatingInvite.value = false
  }
}

function copyInvite() {
  navigator.clipboard.writeText(inviteURL.value)
  inviteCopied.value = true
  setTimeout(() => { inviteCopied.value = false }, 2000)
}

// Pending invites
interface PendingInvite {
  token: string
  email: string
  role: string
  expires: string
}
const pendingInvites = ref<PendingInvite[]>([])

async function loadPendingInvites() {
  try {
    const { data } = await client.get('/invites')
    pendingInvites.value = data || []
  } catch {
    pendingInvites.value = []
  }
}

async function revokeInvite(token: string) {
  try {
    await client.delete(`/invites/${encodeURIComponent(token)}`)
    toast.success('Invite revoked')
    await loadPendingInvites()
  } catch {
    toast.error('Failed to revoke invite')
  }
}

function formatExpiry(iso: string): string {
  const exp = new Date(iso)
  const now = new Date()
  const diff = exp.getTime() - now.getTime()
  if (diff <= 0) return 'expired'
  const hours = Math.floor(diff / (1000 * 60 * 60))
  if (hours < 1) return 'in less than an hour'
  if (hours < 24) return `in ${hours}h`
  const days = Math.floor(hours / 24)
  return `in ${days}d`
}

const roles = [
  { value: 'admin', label: 'Admin', description: 'Full access: manage users, settings, backups' },
  { value: 'deployer', label: 'Deployer', description: 'Deploy, scale, manage apps and services' },
  { value: 'viewer', label: 'Viewer', description: 'Read-only: view apps, logs, routes' },
]

onMounted(async () => {
  await Promise.all([loadUsers(), loadPendingInvites(), loadProjectsForInvite()])
})

async function loadUsers() {
  loading.value = true
  try {
    users.value = await fetchUsers()
  } catch {
    toast.error('Failed to load users')
  } finally {
    loading.value = false
  }
}

async function handleCreate() {
  if (!newEmail.value || !newPassword.value) return
  creating.value = true
  try {
    await createUser({
      email: newEmail.value,
      password: newPassword.value,
      role: newRole.value,
    })
    toast.success(`User ${newEmail.value} created`)
    showPasswordFor.value = newEmail.value
    displayPassword.value = newPassword.value
    passwordCopied.value = false
    newEmail.value = ''
    newPassword.value = ''
    newRole.value = 'deployer'
    showCreate.value = false
    await loadUsers()
  } catch {
    toast.error('Failed to create user')
  } finally {
    creating.value = false
  }
}

async function handleRoleChange(email: string, role: string) {
  if (email === auth.email) {
    toast.error('Cannot change your own role')
    return
  }
  try {
    await updateUserRole(email, role)
    toast.success(`Role updated for ${email}`)
    await loadUsers()
  } catch {
    toast.error('Failed to update role')
  }
}

function requestDelete(email: string) {
  if (email === auth.email) {
    toast.error('Cannot delete your own account')
    return
  }
  modal.open(ConfirmDialog, {
    title: `Delete user ${email}?`,
    message: 'This permanently removes the user account. This cannot be undone.',
    confirmLabel: 'Delete user',
    onConfirm: async () => {
      modal.close()
      try {
        await deleteUser(email)
        toast.success(`User ${email} removed`)
        await loadUsers()
      } catch {
        toast.error('Failed to delete user')
      }
    },
  })
}

function requestResetPassword(email: string) {
  modal.open(ConfirmDialog, {
    title: `Reset password for ${email}?`,
    message: 'This generates a new random password. Their current password stops working immediately.',
    confirmLabel: 'Reset password',
    danger: false,
    onConfirm: async () => {
      modal.close()
      try {
        const { password } = await resetUserPassword(email)
        showPasswordFor.value = email
        displayPassword.value = password
        passwordCopied.value = false
        toast.success(`Password reset for ${email}`)
      } catch {
        toast.error('Failed to reset password')
      }
    },
  })
}

// Password display (after create or reset)
const showPasswordFor = ref('')
const displayPassword = ref('')
const passwordCopied = ref(false)

function copyPassword() {
  navigator.clipboard.writeText(displayPassword.value)
  passwordCopied.value = true
  setTimeout(() => { passwordCopied.value = false }, 2000)
}

function roleColor(role: string): string {
  switch (role) {
    case 'admin': return 'bg-red-100 text-red-700 dark:bg-red-900 dark:text-red-300'
    case 'deployer': return 'bg-kipper-100 text-kipper-700 dark:bg-kipper-900 dark:text-kipper-300'
    case 'viewer': return 'bg-slate-100 text-slate-600 dark:bg-slate-700 dark:text-slate-400'
    default: return 'bg-slate-100 text-slate-600'
  }
}
</script>

<template>
  <div class="animate-fade-in">
    <!-- Header -->
    <div class="mb-8 flex items-center justify-between">
      <div>
        <h1 class="text-2xl font-semibold tracking-tight text-slate-900 dark:text-slate-50">Users</h1>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Manage cluster access and roles</p>
      </div>
      <div class="flex items-center gap-2">
        <button
          @click="showInvite = !showInvite; showCreate = false; inviteURL = ''"
          class="inline-flex items-center gap-1.5 rounded-lg border border-kipper-600 px-4 py-2 text-sm font-medium text-kipper-600 transition-colors hover:bg-kipper-50 dark:hover:bg-kipper-950"
        >
          <Link class="h-4 w-4" />
          Invite
        </button>
        <button
          @click="showCreate = !showCreate; showInvite = false"
          class="inline-flex items-center gap-1.5 rounded-lg bg-kipper-600 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-kipper-700"
        >
          <Plus class="h-4 w-4" />
          Add user
        </button>
        <button
          @click="loadUsers"
          :disabled="loading"
          class="rounded-lg border border-slate-300 p-2.5 text-slate-600 transition-colors hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
        >
          <RefreshCw class="h-4 w-4" :class="loading ? 'animate-spin' : ''" :stroke-width="1.75" />
        </button>
      </div>
    </div>

    <!-- Create form -->
    <div v-if="showCreate" class="mb-6 rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
      <h3 class="mb-4 text-sm font-semibold text-slate-900 dark:text-slate-50">Add user</h3>
      <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div>
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Email</label>
          <input v-model="newEmail" type="email" placeholder="dev@example.com" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Password</label>
          <input v-model="newPassword" type="password" placeholder="Letters, numbers, and symbols" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50" />
          <div class="mt-2">
            <PasswordStrengthBar :password="newPassword" />
          </div>
        </div>
        <div class="sm:col-span-2">
          <label class="mb-2 block text-xs font-medium text-slate-600 dark:text-slate-400">Role</label>
          <div class="grid grid-cols-1 gap-3 sm:grid-cols-3">
            <button
              v-for="r in roles"
              :key="r.value"
              @click="newRole = r.value"
              class="rounded-lg border p-3 text-left transition-colors"
              :class="newRole === r.value
                ? 'border-kipper-500 bg-kipper-50 dark:border-kipper-600 dark:bg-kipper-950'
                : 'border-slate-200 hover:border-slate-300 dark:border-slate-700 dark:hover:border-slate-600'"
            >
              <span class="text-sm font-medium text-slate-900 dark:text-slate-50">{{ r.label }}</span>
              <p class="mt-0.5 text-xs text-slate-500 dark:text-slate-400">{{ r.description }}</p>
            </button>
          </div>
        </div>
      </div>
      <div class="mt-4 flex justify-end gap-2">
        <button @click="showCreate = false" class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800">Cancel</button>
        <button @click="handleCreate" :disabled="!newEmail || !newPassword || !newPasswordStrength.allMet || creating" class="rounded-lg bg-kipper-600 px-4 py-2 text-sm font-medium text-white hover:bg-kipper-700 disabled:opacity-50">
          {{ creating ? 'Creating...' : 'Create user' }}
        </button>
      </div>
    </div>

    <!-- Invite form -->
    <div v-if="showInvite" class="mb-6 rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
      <h3 class="mb-4 text-sm font-semibold text-slate-900 dark:text-slate-50">Create invite link</h3>
      <p class="mb-4 text-xs text-slate-500 dark:text-slate-400">
        Generate a one-time link for one person. Only the address you name can accept it,
        and it works once.
      </p>

      <div v-if="!inviteURL" class="space-y-4">
        <div>
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Email address</label>
          <input
            v-model="inviteEmail"
            type="email"
            placeholder="colleague@example.com"
            required
            class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder-slate-400 focus:border-kipper-500 focus:outline-none dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50"
          />
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Add to project</label>
          <select v-model="inviteProject" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50">
            <option value="">No project: cluster access only</option>
            <option v-for="p in projects" :key="p.name" :value="p.name">{{ p.display_name || p.name }}</option>
          </select>
        </div>
        <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <div v-if="inviteProject">
            <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Access in {{ inviteProject }}</label>
            <select v-model="inviteProjectRole" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50">
              <option value="viewer">View only</option>
              <option value="deployer">Can deploy</option>
              <option value="owner">Owner (manages members)</option>
            </select>
          </div>
          <div v-else>
            <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Role</label>
            <select v-model="inviteRole" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50">
              <option v-for="r in roles" :key="r.value" :value="r.value">{{ r.label }}</option>
            </select>
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium text-slate-600 dark:text-slate-400">Expires</label>
            <select v-model="inviteExpiry" class="block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm dark:border-slate-600 dark:bg-slate-800 dark:text-slate-50">
              <option value="24h">24 hours</option>
              <option value="48h">48 hours</option>
              <option value="7d">7 days</option>
            </select>
          </div>
        </div>
        <div class="flex justify-end gap-2">
          <button @click="showInvite = false" class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-600 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800">Cancel</button>
          <button @click="handleInvite" :disabled="creatingInvite || !inviteEmail.trim()" class="rounded-lg bg-kipper-600 px-4 py-2 text-sm font-medium text-white hover:bg-kipper-700 disabled:opacity-50">
            {{ creatingInvite ? 'Sending...' : 'Send invite' }}
          </button>
        </div>
      </div>

      <div v-else-if="inviteEmailSent" class="space-y-3">
        <NoticeCallout tone="success" class="p-3">
          <p class="text-xs font-medium text-emerald-700 dark:text-emerald-300">Invite email sent to {{ inviteEmail }}</p>
          <p class="mt-1 text-xs text-slate-500 dark:text-slate-400">
            One-time use, expires in {{ inviteExpiry }}.
          </p>
        </NoticeCallout>
        <button @click="showInvite = false; inviteURL = ''; inviteEmail = ''; inviteEmailSent = false" class="text-xs text-slate-400 hover:text-slate-600">Done</button>
      </div>

      <div v-else class="space-y-3">
        <NoticeCallout tone="warning" class="p-3">
          <p class="mb-2 text-xs font-medium text-amber-700 dark:text-orange-300">Copy this link before closing. It won't be shown again</p>
          <div class="flex items-center gap-2 rounded-md border border-slate-200 bg-white p-2 dark:border-slate-700 dark:bg-slate-800">
            <input
              :value="inviteURL"
              readonly
              class="flex-1 bg-transparent font-mono text-xs text-slate-700 outline-none dark:text-slate-300"
            />
            <button @click="copyInvite" class="rounded-md bg-kipper-600 px-3 py-1 text-xs font-medium text-white hover:bg-kipper-700">
              <Check v-if="inviteCopied" class="h-3.5 w-3.5" />
              <Copy v-else class="h-3.5 w-3.5" />
            </button>
          </div>
          <p class="mt-2 text-xs text-slate-500 dark:text-slate-400">
            One-time use, expires in {{ inviteExpiry }}.
          </p>
        </NoticeCallout>
        <button v-if="inviteCopied" @click="showInvite = false; inviteURL = ''; inviteEmail = ''" class="text-xs text-slate-400 hover:text-slate-600">Done</button>
      </div>
    </div>

    <!-- Pending invites -->
    <div v-if="pendingInvites.length" class="mb-6">
      <h3 class="mb-3 text-xs font-semibold uppercase tracking-wider text-slate-400 dark:text-slate-500">Pending invites</h3>
      <div class="space-y-2">
        <div
          v-for="inv in pendingInvites"
          :key="inv.token"
          class="flex flex-wrap items-center justify-between gap-y-2 rounded-lg border border-dashed border-slate-300 bg-slate-50 px-5 py-3 dark:border-slate-700 dark:bg-slate-800/50"
        >
          <div class="flex items-center gap-3">
            <span class="inline-flex h-8 w-8 items-center justify-center rounded-full bg-slate-200 text-xs font-semibold text-slate-500 dark:bg-slate-700 dark:text-slate-400">
              {{ inv.email ? inv.email[0].toUpperCase() : '?' }}
            </span>
            <div>
              <span class="text-sm font-medium text-slate-700 dark:text-slate-300">{{ inv.email || 'Link invite' }}</span>
              <span class="ml-2 inline-block rounded px-1.5 py-0.5 text-[10px] font-semibold" :class="roleColor(inv.role)">{{ inv.role }}</span>
            </div>
          </div>
          <div class="flex items-center gap-3">
            <span class="text-xs text-slate-400 dark:text-slate-500">expires {{ formatExpiry(inv.expires) }}</span>
            <button
              @click="revokeInvite(inv.token)"
              class="rounded-lg p-1.5 text-slate-400 transition-colors hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-950 dark:hover:text-red-400"
              title="Revoke invite"
            >
              <Trash2 class="h-4 w-4" />
            </button>
          </div>
        </div>
      </div>
    </div>

    <!-- Loading -->
    <div v-if="loading" class="py-12 text-center text-sm text-slate-500 dark:text-slate-400">Loading...</div>

    <!-- User list -->
    <div v-else-if="users.length" class="space-y-3">
      <div
        v-for="user in users"
        :key="user.email"
        class="flex flex-wrap items-center justify-between gap-y-2 rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900"
      >
        <div class="flex items-center gap-4">
          <span class="inline-flex h-10 w-10 items-center justify-center rounded-full bg-slate-100 text-sm font-semibold text-slate-600 dark:bg-slate-800 dark:text-slate-400">
            {{ user.email[0].toUpperCase() }}
          </span>
          <div>
            <span class="text-sm font-semibold text-slate-900 dark:text-slate-50">{{ user.email }}</span>
            <span
              v-if="user.email === auth.email"
              class="ml-2 text-xs text-slate-400"
            >(you)</span>
          </div>
        </div>

        <div class="flex items-center gap-3">
          <select
            :value="user.role"
            @change="handleRoleChange(user.email, ($event.target as HTMLSelectElement).value)"
            :disabled="user.email === auth.email"
            class="rounded-md border border-slate-300 bg-white px-2 py-1 text-xs font-medium dark:border-slate-600 dark:bg-slate-800"
            :class="roleColor(user.role)"
          >
            <option v-for="r in roles" :key="r.value" :value="r.value">{{ r.label }}</option>
          </select>

          <button
            v-if="user.email !== auth.email"
            @click="requestResetPassword(user.email)"
            class="rounded-lg p-1.5 text-slate-400 transition-colors hover:bg-slate-100 hover:text-slate-700 dark:hover:bg-slate-800"
            title="Reset password"
          >
            <KeyRound class="h-4 w-4" />
          </button>
          <button
            v-if="user.email !== auth.email"
            @click="requestDelete(user.email)"
            class="rounded-lg p-1.5 text-slate-400 transition-colors hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-950 dark:hover:text-red-400"
            title="Delete user"
          >
            <Trash2 class="h-4 w-4" />
          </button>
        </div>

        <!-- Password display (after create or reset) -->
        <NoticeCallout v-if="showPasswordFor === user.email" tone="warning" class="mt-3 p-3">
          <p class="mb-2 text-xs font-medium text-amber-700 dark:text-orange-300">Share this password. It won't be shown again</p>
          <div class="flex items-center gap-2">
            <code class="flex-1 rounded bg-white px-2 py-1 font-mono text-sm text-slate-900 dark:bg-slate-800 dark:text-slate-50">{{ displayPassword }}</code>
            <button @click="copyPassword" class="rounded-md bg-kipper-600 px-3 py-1 text-xs font-medium text-white hover:bg-kipper-700">
              <Check v-if="passwordCopied" class="h-3.5 w-3.5" />
              <Copy v-else class="h-3.5 w-3.5" />
            </button>
          </div>
          <button v-if="passwordCopied" @click="showPasswordFor = ''" class="mt-2 text-xs text-slate-400 hover:text-slate-600">Done</button>
        </NoticeCallout>
      </div>
    </div>

    <!-- Empty state -->
    <div v-else class="rounded-xl border border-dashed border-slate-300 py-16 text-center dark:border-slate-700">
      <Users class="mx-auto mb-3 h-10 w-10 text-slate-400 dark:text-slate-500" :stroke-width="1.5" />
      <p class="text-sm font-medium text-slate-900 dark:text-slate-50">No users configured</p>
      <p class="mt-1 max-w-sm mx-auto text-sm text-slate-500 dark:text-slate-400">
        Add team members and assign roles to control who can deploy, view, and manage your cluster.
      </p>
    </div>

  </div>
</template>
