import client from './client'

export interface PlanQuota {
  requests: number
  period: 'day' | 'week' | 'month'
}

export interface UsagePlan {
  name: string
  display_name?: string
  rate: number
  burst: number
  quota?: PlanQuota | null
  keys: number
}

export interface ApiKey {
  name: string
  display_name?: string
  plan: string
  prefix: string
  enabled: boolean
  apps: string[]
  created: string
  // RFC3339 expiry, absent when the key never expires.
  expires_at?: string
  // Present only in the create response; shown once, never again.
  key?: string
}

export interface KeyUsageDay {
  day: string
  allowed: number
  denied_rate: number
  denied_quota: number
}

export interface KeyUsageResponse {
  from: string
  to: string
  retention_days: number
  days: KeyUsageDay[]
}

// All routes are scoped to one environment namespace, same as apps.

export async function fetchPlans(namespace: string): Promise<UsagePlan[]> {
  const { data } = await client.get<UsagePlan[]>(`/projects/${namespace}/usage-plans`)
  return data
}

export async function upsertPlan(namespace: string, plan: Omit<UsagePlan, 'keys'>): Promise<void> {
  await client.put(`/projects/${namespace}/usage-plans`, plan)
}

export async function deletePlan(namespace: string, name: string): Promise<void> {
  await client.delete(`/projects/${namespace}/usage-plans/${name}`)
}

export async function fetchKeys(namespace: string): Promise<ApiKey[]> {
  const { data } = await client.get<ApiKey[]>(`/projects/${namespace}/api-keys`)
  return data
}

export async function createKey(
  namespace: string,
  req: { display_name?: string; plan: string; apps?: string[]; expires_at?: string },
): Promise<ApiKey> {
  const { data } = await client.post<ApiKey>(`/projects/${namespace}/api-keys`, req)
  return data
}

export async function updateKey(
  namespace: string,
  name: string,
  req: { enabled?: boolean; apps?: string[] },
): Promise<ApiKey> {
  const { data } = await client.patch<ApiKey>(`/projects/${namespace}/api-keys/${name}`, req)
  return data
}

export async function deleteKey(namespace: string, name: string): Promise<void> {
  await client.delete(`/projects/${namespace}/api-keys/${name}`)
}

export async function fetchKeyUsage(
  namespace: string,
  name: string,
  window?: { from?: string; to?: string },
): Promise<KeyUsageResponse> {
  const { data } = await client.get<KeyUsageResponse>(
    `/projects/${namespace}/api-keys/${name}/usage`,
    { params: window },
  )
  return data
}
