<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { storeToRefs } from 'pinia'
import { Users, Trash2, Plus } from 'lucide-vue-next'
import type { ProjectMember, ProjectRole } from '@/api/projects'
import { useProjectMembersStore } from '@/stores/projectMembers'
import { useToast } from '@/composables/useToast'
import { useModal } from '@/composables/useModal'
import ConfirmDialog from '@/components/ConfirmDialog.vue'

interface Props {
  project: string
  canManage: boolean
}
const props = defineProps<Props>()

const toast = useToast()
const modal = useModal()
const store = useProjectMembersStore()
const { members, loading, error } = storeToRefs(store)

const newEmail = ref('')
const newRole = ref<ProjectRole>('deployer')
const saving = ref(false)

const roleLabels: Record<ProjectRole, string> = {
  owner: 'Owner',
  deployer: 'Can deploy',
  viewer: 'View only',
}

const sortedMembers = computed(() =>
  [...members.value].sort((a, b) => a.email.localeCompare(b.email)),
)

function load() {
  return store.loadMembers(props.project)
}

async function add() {
  const email = newEmail.value.trim()
  if (!email) return
  saving.value = true
  try {
    await store.setMemberRole(props.project, email, newRole.value)
    newEmail.value = ''
    newRole.value = 'deployer'
    toast.success(`${email} added to ${props.project}`)
  } catch (e) {
    const msg = e instanceof Error ? e.message : 'failed to add member'
    toast.error(msg)
  } finally {
    saving.value = false
  }
}

async function changeRole(member: ProjectMember, role: ProjectRole) {
  if (role === member.role) return
  try {
    await store.setMemberRole(props.project, member.email, role)
  } catch (e) {
    toast.error(e instanceof Error ? e.message : 'failed to change role')
  }
}

function remove(member: ProjectMember) {
  modal.open(ConfirmDialog, {
    title: `Remove ${member.email}?`,
    message: `${member.email} loses access to this project immediately. You can add them again later.`,
    confirmLabel: 'Remove member',
    onConfirm: async () => {
      modal.close()
      try {
        await store.removeMember(props.project, member.email)
        toast.success(`${member.email} removed`)
      } catch (e) {
        toast.error(e instanceof Error ? e.message : 'failed to remove member')
      }
    },
  })
}

onMounted(load)
watch(() => props.project, load)
</script>

<template>
  <div class="p-5">
    <div class="mb-3 flex items-center gap-2">
      <Users class="h-4 w-4 text-slate-400" :stroke-width="2" />
      <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-50">Members</h3>
    </div>

    <p class="mb-4 text-sm text-slate-500 dark:text-slate-400">
      Give teammates access to this project. Owners manage members and settings, deployers ship
      and change apps, and viewers can look but not touch.
    </p>

    <p v-if="loading" class="text-sm text-slate-500 dark:text-slate-400">Loading members…</p>
    <p v-else-if="error" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>

    <p v-else-if="!sortedMembers.length" class="text-sm text-slate-500 dark:text-slate-400">
      No members yet. Cluster admins can always see this project; anyone else needs to be added
      here first.
    </p>

    <ul v-else class="space-y-1.5">
      <li
        v-for="member in sortedMembers"
        :key="member.email"
        class="flex items-center justify-between gap-3 rounded-lg bg-slate-50 px-3 py-2 dark:bg-slate-800/50"
      >
        <span class="truncate text-sm text-slate-700 dark:text-slate-200">{{ member.email }}</span>
        <div class="flex items-center gap-2">
          <select
            v-if="canManage"
            :value="member.role"
            @change="changeRole(member, ($event.target as HTMLSelectElement).value as ProjectRole)"
            class="rounded-md border border-slate-300 bg-white px-2 py-1 text-xs text-slate-700 focus:border-kipper-500 focus:outline-none dark:border-slate-700 dark:bg-slate-800 dark:text-slate-200"
          >
            <option v-for="(label, value) in roleLabels" :key="value" :value="value">{{ label }}</option>
          </select>
          <span
            v-else
            class="rounded-full bg-slate-200 px-2 py-0.5 text-xs font-medium text-slate-600 dark:bg-slate-700 dark:text-slate-300"
          >
            {{ roleLabels[member.role] }}
          </span>
          <button
            v-if="canManage"
            @click="remove(member)"
            class="rounded p-1 text-slate-400 transition-colors hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-950 dark:hover:text-red-400"
            :title="`Remove ${member.email}`"
          >
            <Trash2 class="h-4 w-4" :stroke-width="2" />
          </button>
        </div>
      </li>
    </ul>

    <form v-if="canManage" @submit.prevent="add" class="mt-3 flex flex-wrap items-center gap-2">
      <input
        v-model="newEmail"
        type="email"
        placeholder="email of an existing user"
        :disabled="saving"
        class="min-w-48 flex-1 rounded-md border border-slate-300 bg-white px-3 py-1.5 text-sm text-slate-900 focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
      />
      <select
        v-model="newRole"
        :disabled="saving"
        class="rounded-md border border-slate-300 bg-white px-2 py-1.5 text-sm text-slate-900 focus:border-kipper-500 focus:outline-none dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
      >
        <option v-for="(label, value) in roleLabels" :key="value" :value="value">{{ label }}</option>
      </select>
      <button
        type="submit"
        :disabled="saving || !newEmail.trim()"
        class="inline-flex items-center gap-1 rounded-md bg-kipper-600 px-3 py-1.5 text-sm font-medium text-white transition-colors hover:bg-kipper-700 disabled:opacity-50"
      >
        <Plus class="h-4 w-4" :stroke-width="2" />
        Add
      </button>
    </form>
  </div>
</template>
