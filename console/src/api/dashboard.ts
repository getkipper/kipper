import client from './client'
import type { DashboardResponse } from './types'

export async function fetchDashboard(): Promise<DashboardResponse> {
  const { data } = await client.get<DashboardResponse>('/dashboard')
  return data
}

export interface UsageSnapshot {
  time: string
  cpu_millis: number
  memory_bytes: number
}

export interface NodeHistory {
  allocatable_memory_bytes: number
  allocatable_cpu_millis: number
  history: UsageSnapshot[]
}

export interface WorkloadTrend {
  name: string
  namespace: string
  anomaly: boolean
  growth_pct: number
  history: UsageSnapshot[]
}

export interface UsageHistoryResponse {
  node: NodeHistory
  workloads: WorkloadTrend[]
  // True when a Prometheus query failed, so the chart is showing stale or empty
  // data because monitoring is unavailable, not because the cluster is idle.
  degraded: boolean
}

export async function fetchUsageHistory(): Promise<UsageHistoryResponse> {
  const { data } = await client.get<UsageHistoryResponse>('/dashboard/usage-history')
  return data
}
