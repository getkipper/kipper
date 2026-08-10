import { defineStore } from 'pinia'
import { ref } from 'vue'
import * as api from '@/api/platform'
import type { PlatformComponent, PlatformComponentPatch } from '@/api/platform'

export const usePlatformStore = defineStore('platform', () => {
  const profile = ref<string>('')
  const availableProfiles = ref<string[]>([])
  const components = ref<PlatformComponent[]>([])
  const loading = ref(false)
  const updating = ref<Record<string, boolean>>({})
  const error = ref<string | null>(null)

  async function loadComponents() {
    loading.value = true
    error.value = null
    try {
      const summary = await api.fetchPlatformSummary()
      const detail = await api.fetchPlatformComponents()
      profile.value = summary.profile || detail.profile
      availableProfiles.value = summary.available_profiles
      components.value = detail.components
    } catch (e) {
      error.value = e instanceof Error ? e.message : 'unknown error'
    } finally {
      loading.value = false
    }
  }

  async function updateComponent(name: string, patch: PlatformComponentPatch) {
    updating.value = { ...updating.value, [name]: true }
    error.value = null
    try {
      const updated = await api.patchPlatformComponent(name, patch)
      const idx = components.value.findIndex((c) => c.name === name)
      if (idx >= 0) {
        components.value[idx] = { ...components.value[idx], ...updated }
      }
    } catch (e) {
      error.value = e instanceof Error ? e.message : 'unknown error'
      throw e
    } finally {
      updating.value = { ...updating.value, [name]: false }
    }
  }

  return {
    profile,
    availableProfiles,
    components,
    loading,
    updating,
    error,
    loadComponents,
    updateComponent,
  }
})
