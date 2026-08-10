import client from './client'

export interface MigrationStep {
  name: string
  phase: string
  status: 'pending' | 'running' | 'completed' | 'failed' | 'skipped'
  bytes_total?: number
  bytes_done?: number
  detail?: string
  error?: string
  manual_steps?: string[]
}

export interface MigrationSession {
  id: string
  source_cluster: string
  target_cluster: string
  projects: string[]
  steps: MigrationStep[]
  status: 'running' | 'completed' | 'failed' | 'cancelled' | 'verifying'
  error?: string
}

export interface AppVerification {
  name: string
  namespace: string
  status: string
  build_phase?: string
  temporary_url?: string
  custom_domain?: string
}

export interface DomainStatus {
  domain: string
  app: string
  namespace: string
  resolved: boolean
  resolved_to?: string
  expected_ips?: string[]
}

export interface PlanItem {
  kind: string
  name: string
  namespace?: string
  status: 'ok' | 'warn' | 'blocked'
  detail?: string
  // Domain disposition for an app with a public route.
  host?: string
  domain_class?: 'custom' | 'platform' | 'gateway'
  disposition?: 'move' | 'coexist' | 'gateway'
  target_url?: string
}

export interface PlanCapacity {
  need_cpu_millis: number
  need_memory_bytes: number
  need_storage_bytes: number
  free_cpu_millis: number
  free_memory_bytes: number
  free_storage_bytes: number
  storage_known: boolean
}

export interface MigrationPlan {
  target_cluster: string
  target_endpoint: string
  target_version?: string
  blockers: string[]
  warnings: string[]
  will_migrate: PlanItem[]
  will_skip: PlanItem[]
  not_migrated: string[]
  conflicts: string[]
  capacity?: PlanCapacity
  receipt: string
  receipt_expires?: string
  out_of_band_notifications: boolean
  target_base_domain?: string
  move_base_domain?: boolean
}

export async function generateToken(): Promise<string> {
  const { data } = await client.post<{ token: string }>('/migration/token')
  return data.token
}

export async function getPlan(
  token: string,
  projects: string[],
  confirmedOverwrites?: string[],
  keepDomains?: string[],
  moveBaseDomain?: boolean,
): Promise<MigrationPlan> {
  const { data } = await client.post<MigrationPlan>('/migration/plan', {
    token,
    projects,
    confirmed_overwrites: confirmedOverwrites,
    keep_domains: keepDomains,
    move_base_domain: moveBaseDomain,
  })
  return data
}

export async function startMigration(
  token: string,
  projects: string[],
  confirmedOverwrites: string[],
  totpCode: string,
  planReceipt: string,
  keepDomains?: string[],
  moveBaseDomain?: boolean,
): Promise<{
  session_id: string
  status: string
}> {
  const { data } = await client.post('/migration/start', {
    token,
    projects,
    confirmed_overwrites: confirmedOverwrites,
    totp_code: totpCode,
    plan_receipt: planReceipt,
    keep_domains: keepDomains,
    move_base_domain: moveBaseDomain,
  })
  return data
}

export async function getSession(sessionId: string): Promise<MigrationSession> {
  const { data } = await client.get<MigrationSession>(`/migration/${sessionId}`)
  return data
}

export async function getVerification(sessionId: string): Promise<{ apps: AppVerification[]; status: string }> {
  const { data } = await client.get(`/migration/${sessionId}/verify`)
  return data
}

export async function performCutover(sessionId: string, totpCode: string, force = false): Promise<{ domains: DomainStatus[]; dns_warning?: string }> {
  const { data } = await client.post(`/migration/${sessionId}/cutover`, { force, totp_code: totpCode })
  return data
}

export async function getDNSStatus(sessionId: string): Promise<DomainStatus[]> {
  const { data } = await client.get<DomainStatus[]>(`/migration/${sessionId}/dns`)
  return data
}

export async function cancelMigration(sessionId: string): Promise<void> {
  await client.post(`/migration/${sessionId}/cancel`)
}

export function connectProgress(sessionId: string, onStep: (step: MigrationStep) => void, onEnd?: (status: string) => void): WebSocket {
  const wsProtocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const baseUrl = import.meta.env.VITE_API_URL || '/api/v1'
  const wsUrl = `${wsProtocol}//${window.location.host}${baseUrl}/migration/${sessionId}/progress`

  // The JWT rides in the Sec-WebSocket-Protocol header, matching the logs and
  // terminal streams; the server authenticates it and requires admin.
  const token = localStorage.getItem('kipper_token')
  const protocols = token ? ['kipper.auth', token] : ['kipper.auth']
  const ws = new WebSocket(wsUrl, protocols)
  ws.onmessage = (event) => {
    const data = JSON.parse(event.data)
    if (data.type === 'session_end') {
      onEnd?.(data.status)
    } else {
      onStep(data as MigrationStep)
    }
  }
  return ws
}
