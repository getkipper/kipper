<script setup lang="ts">
import { ref, computed } from 'vue'
import SidePanel from '@/components/SidePanel.vue'
import TabBar, { type Tab } from '@/components/TabBar.vue'
import ProjectQuota from '@/components/ProjectQuota.vue'
import ProjectApiKeys from '@/components/ProjectApiKeys.vue'
import ProjectMembers from '@/components/ProjectMembers.vue'
import { useAuthStore } from '@/stores/auth'
import type { Project } from '@/api/projects'
import { can } from '@/utils/capabilities'

interface Props {
  project: Project
}
const props = defineProps<Props>()

const emit = defineEmits<{
  close: []
}>()

const authStore = useAuthStore()

const tabs: Tab[] = [
  { key: 'quota', label: 'Quota' },
  { key: 'api-keys', label: 'API keys' },
  { key: 'members', label: 'Members' },
]
const activeTab = ref('quota')

const envNames = computed(() => props.project.environments.map((e) => e.name))
// Environment-derived keys remount the children on env add/remove so their
// per-environment data reloads, matching the previous inline usage.
const envKey = computed(() => envNames.value.join(','))

const canManageQuota = computed(() => authStore.isAdmin)
// What the server says this caller may do here, rather than what this build
// thinks each role name means. Quota stays on the cluster role: raising one
// spends cluster capacity, which is not a project's to grant.
const canManageApiKeys = computed(() => can(props.project, 'apikeys.manage'))
const canManageMembers = computed(() => can(props.project, 'members.manage') || authStore.isAdmin)
</script>

<template>
  <SidePanel :open="true" :label="`Project settings for ${project.display_name || project.name}`" @close="emit('close')">
    <template #header>
      <div class="min-w-0">
        <h2 class="truncate text-base font-semibold text-slate-900 dark:text-slate-50">
          {{ project.display_name || project.name }}
        </h2>
        <p class="truncate font-mono text-xs text-slate-500 dark:text-slate-400">
          {{ project.name }} · Project settings
        </p>
      </div>
    </template>

    <TabBar v-model="activeTab" :tabs="tabs" density="compact" />

    <div class="min-h-0 flex-1 overflow-y-auto">
      <ProjectQuota
        v-if="activeTab === 'quota'"
        :key="project.name + ':' + envKey"
        :project="project.name"
        :can-manage="canManageQuota"
      />
      <ProjectApiKeys
        v-if="activeTab === 'api-keys'"
        :key="'apikeys:' + project.name + ':' + envKey"
        :project="project.name"
        :environments="envNames"
        :can-manage="canManageApiKeys"
      />
      <ProjectMembers
        v-if="activeTab === 'members'"
        :project="project.name"
        :can-manage="canManageMembers"
      />
    </div>
  </SidePanel>
</template>
