import { defineStore } from 'pinia'
import { ref } from 'vue'
import type { ClusterStatus, NodeInfo } from '@/api/types'
import * as api from '@/api/cluster'

export const useClusterStore = defineStore('cluster', () => {
  const status = ref<ClusterStatus | null>(null)
  const nodes = ref<NodeInfo[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)

  async function loadStatus() {
    loading.value = true
    error.value = null
    try {
      status.value = await api.fetchClusterStatus()
    } catch (e) {
      error.value = e instanceof Error ? e.message : 'unknown error'
    } finally {
      loading.value = false
    }
  }

  async function loadNodes() {
    loading.value = true
    error.value = null
    try {
      nodes.value = await api.fetchNodes()
    } catch (e) {
      error.value = e instanceof Error ? e.message : 'unknown error'
    } finally {
      loading.value = false
    }
  }

  return { status, nodes, loading, error, loadStatus, loadNodes }
})
