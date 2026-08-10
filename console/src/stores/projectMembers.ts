import { defineStore } from 'pinia'
import { ref } from 'vue'
import * as api from '@/api/projects'
import type { ProjectMember, ProjectRole } from '@/api/projects'

export const useProjectMembersStore = defineStore('projectMembers', () => {
  const members = ref<ProjectMember[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)

  // The store is a singleton shared across project panels. loadSeq lets the
  // latest load win and drops stale responses; activeScope records which
  // project the state belongs to, so a slow response or a dialog completing
  // after a project switch can never touch the newer project's view.
  let loadSeq = 0
  let activeScope = ''

  async function loadMembers(project: string) {
    const seq = ++loadSeq
    if (project !== activeScope) {
      // Entering a new scope: drop the previous project's rows so the panel
      // never renders them (with this project's permissions) while the load
      // is in flight.
      activeScope = project
      members.value = []
    }
    loading.value = true
    error.value = null
    try {
      const result = await api.fetchMembers(project)
      if (seq !== loadSeq) return
      members.value = result
    } catch (e) {
      if (seq !== loadSeq) return
      error.value = e instanceof Error ? e.message : 'failed to load members'
    } finally {
      if (seq === loadSeq) loading.value = false
    }
  }

  // Add or change a member's role, then reload so the list reflects the server.
  async function setMemberRole(project: string, email: string, role: ProjectRole) {
    await api.setMember(project, email, role)
    if (project === activeScope) await loadMembers(project)
  }

  // Reload rather than filter locally, for the same reason setMemberRole does:
  // dropping the row on the client shows a removal the server may not have made.
  // That is exactly what hid a bug where the API answered "removed" and removed
  // nobody — the row vanished from the screen and the member kept their access
  // until somebody reloaded the page.
  async function removeMember(project: string, email: string) {
    await api.removeMember(project, email)
    if (project === activeScope) await loadMembers(project)
  }

  return { members, loading, error, loadMembers, setMemberRole, removeMember }
})
