import { defineStore } from 'pinia'
import { ref } from 'vue'
import type { App, CreateAppPayload } from '@/api/types'
import * as api from '@/api/apps'
import { useProjectsStore } from './projects'

export const useAppsStore = defineStore('apps', () => {
  const apps = ref<App[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)

  async function loadApps(project: string) {
    loading.value = true
    error.value = null
    try {
      apps.value = await api.fetchApps(project)
    } catch (e) {
      error.value = e instanceof Error ? e.message : 'unknown error'
    } finally {
      loading.value = false
    }
  }

  async function loadAllApps() {
    const projectsStore = useProjectsStore()
    loading.value = true
    error.value = null
    try {
      const namespaces = new Set<string>()
      for (const project of projectsStore.projects) {
        for (const env of project.environments) {
          namespaces.add(env.namespace)
        }
      }

      const results = await Promise.allSettled(
        [...namespaces].map(async (ns) => {
          const nsApps = await api.fetchApps(ns)
          const project = projectsStore.projects.find(p =>
            p.environments.some(e => e.namespace === ns)
          )
          return nsApps.map(app => ({
            ...app,
            namespace: ns,
            project: project?.display_name || project?.name || ns,
          }))
        })
      )

      const allApps: App[] = []
      for (const result of results) {
        if (result.status === 'fulfilled') {
          allApps.push(...result.value)
        }
      }
      apps.value = allApps
    } catch (e) {
      error.value = e instanceof Error ? e.message : 'unknown error'
    } finally {
      loading.value = false
    }
  }

  async function deployApp(project: string, payload: CreateAppPayload) {
    error.value = null
    try {
      const app = await api.createApp(project, payload)
      apps.value.push(app)
    } catch (e) {
      error.value = e instanceof Error ? e.message : 'unknown error'
      throw e
    }
  }

  async function removeApp(project: string, appName: string) {
    error.value = null
    try {
      await api.deleteApp(project, appName)
      apps.value = apps.value.filter(a => a.name !== appName)
    } catch (e) {
      error.value = e instanceof Error ? e.message : 'unknown error'
      throw e
    }
  }

  async function restart(project: string, appName: string) {
    error.value = null
    try {
      await api.restartApp(project, appName)
    } catch (e) {
      error.value = e instanceof Error ? e.message : 'unknown error'
      throw e
    }
  }

  return { apps, loading, error, loadApps, loadAllApps, deployApp, removeApp, restart }
})
