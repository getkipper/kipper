import client from './client'

export interface ContainerUsage {
  pod: string
  namespace: string
  name: string
  metrics_present: boolean
  memory_bytes: number
  memory_limit_bytes: number
  memory_request_bytes: number
  cpu_millis: number
  cpu_limit_millis: number
  cpu_request_millis: number
}

export interface UsageTotals {
  memory_bytes: number
  memory_limit_bytes: number
  memory_request_bytes: number
  cpu_millis: number
  cpu_limit_millis: number
  cpu_request_millis: number
  pod_count: number
  container_count: number
  containers_with_metrics: number
}

export interface UsageResponse {
  metrics_available: boolean
  containers: ContainerUsage[]
  totals: UsageTotals
  // Prometheus enrichment, only present when the caller passed
  // include_prometheus=1 and Prometheus answered.
  prometheus_available?: boolean
  memory_sparkline?: number[]
  cpu_sparkline?: number[]
  cpu_throttling_pct?: number
}

export interface UsageScope {
  namespace?: string
  pod?: string
  selector?: string
  // includePrometheus toggles the optional 1h sparkline + CPU
  // throttling enrichment. Off by default so cluster summary polls
  // don't fan out into Prometheus.
  includePrometheus?: boolean
}

export async function fetchResourceUsage(scope: UsageScope): Promise<UsageResponse> {
  const params: Record<string, string> = {}
  if (scope.namespace) params.namespace = scope.namespace
  if (scope.pod) params.pod = scope.pod
  if (scope.selector) params.selector = scope.selector
  if (scope.includePrometheus) params.include_prometheus = '1'
  const { data } = await client.get<UsageResponse>('/resources/usage', { params })
  return data
}

export interface ClusterSummaryBucket {
  memory_bytes: number
  cpu_millis: number
  pod_count: number
}

export interface AllocatableBucket {
  memory_bytes: number
  cpu_millis: number
  node_count: number
}

export interface ClusterResourceSummary {
  metrics_available: boolean
  system: ClusterSummaryBucket
  apps: ClusterSummaryBucket
  allocatable: AllocatableBucket
}

export async function fetchClusterResourceSummary(): Promise<ClusterResourceSummary> {
  const { data } = await client.get<ClusterResourceSummary>('/resources/usage/summary')
  return data
}
