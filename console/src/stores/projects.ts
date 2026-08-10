import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import * as api from '@/api/projects'
import type { Project } from '@/api/projects'

export type { Project }

export const useProjectsStore = defineStore('projects', () => {
  const projects = ref<Project[]>([])
  const currentProject = ref<string | null>(null)
  const loading = ref(false)
  const error = ref<string | null>(null)

  // Global namespace — persisted in localStorage
  const globalNamespace = ref<string>(typeof localStorage !== 'undefined' ? localStorage.getItem('kipper_namespace') || '' : '')

  // System namespaces that should be hidden from the user
  const systemNamespaces = new Set([
    'kube-system', 'kube-public', 'kube-node-lease',
    'keda', 'monitoring', 'longhorn-system', 'cert-manager',
    'traefik', 'dex', 'kipper-system', 'velero',
  ])

  const namespaceOptions = computed(() => {
    const options: { label: string; value: string }[] = [
      { label: 'All projects', value: '' },
      { label: 'default', value: 'default' },
    ]
    for (const p of projects.value) {
      for (const env of p.environments) {
        options.push({
          label: `${p.display_name || p.name} — ${env.name}`,
          value: env.namespace,
        })
      }
    }
    return options
  })

  function setGlobalNamespace(ns: string) {
    globalNamespace.value = ns
    if (typeof localStorage !== 'undefined') {
      localStorage.setItem('kipper_namespace', ns)
    }
  }

  function isSystemNamespace(ns: string): boolean {
    return systemNamespaces.has(ns)
  }

  async function loadProjects() {
    loading.value = true
    error.value = null
    try {
      projects.value = await api.fetchProjects()
    } catch (e) {
      error.value = e instanceof Error ? e.message : 'unknown error'
    } finally {
      loading.value = false
    }
  }

  async function addProject(name: string, environments?: string[]) {
    error.value = null
    try {
      await api.createProject(name, environments)
      await loadProjects()
    } catch (e) {
      error.value = e instanceof Error ? e.message : 'unknown error'
      throw e
    }
  }

  async function removeProject(name: string) {
    error.value = null
    try {
      await api.deleteProject(name)
      projects.value = projects.value.filter(p => p.name !== name)
      if (currentProject.value === name) {
        currentProject.value = null
      }
    } catch (e) {
      error.value = e instanceof Error ? e.message : 'unknown error'
      throw e
    }
  }

  function selectProject(name: string) {
    currentProject.value = name
  }

  return {
    projects, currentProject, loading, error,
    globalNamespace, namespaceOptions,
    setGlobalNamespace, isSystemNamespace,
    loadProjects, addProject, removeProject, selectProject,
  }
})
