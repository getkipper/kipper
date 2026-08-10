import client from './client'

export interface ServiceStatus {
  name: string
  namespace: string
  type: string
  status: string
  ready: string
  storage: string
}

export interface ServiceInfo {
  name: string
  type: string
  host: string
  port: string
  username: string
  database: string
  // injected_env_template lists the env var names a binding will inject for
  // this service when bound with the default prefix. The bindings panel
  // uses it to preview the binding before the user commits to it.
  injected_env_template?: string[]
  // default_prefix is the conventional env var prefix for this service
  // type (e.g. "DB_" for postgres). The user can override it on bind.
  default_prefix?: string
  // ui_url, when present, is the browseable URL for the service's
  // web UI (MailHog inbox, future RabbitMQ Management, etc.).
  // Routed through Traefik forwardAuth so only Dex-authenticated
  // users reach the backend. Empty for services without a UI or
  // when the cluster has no base domain configured.
  ui_url?: string
}

export interface FunctionBinding {
  service: string
  type: string
  prefix: string
  database?: string
  injected_env: string[]
}

export async function fetchFunctionBindings(project: string, fn: string): Promise<FunctionBinding[]> {
  const { data } = await client.get<FunctionBinding[]>(`/projects/${project}/functions/${fn}/bindings`)
  return data || []
}

export interface CreateServicePayload {
  name: string
  type: string
  namespace: string
  storage?: string
  memory?: string
  cpu?: string
  version?: string
}

export async function createService(payload: CreateServicePayload): Promise<void> {
  await client.post('/services', payload)
}

export async function deleteService(name: string, namespace: string): Promise<void> {
  await client.delete(`/services/${name}?namespace=${encodeURIComponent(namespace)}&confirm=true`)
}

export async function fetchServices(namespace?: string): Promise<ServiceStatus[]> {
  const params = namespace ? `?namespace=${encodeURIComponent(namespace)}` : ''
  const { data } = await client.get<ServiceStatus[]>(`/services${params}`)
  return data
}

export interface ServiceLogEntry {
  timestamp: string
  line: string
  pod: string
}

export async function fetchServiceLogs(
  name: string,
  namespace: string,
  opts?: { search?: string; since?: string }
): Promise<ServiceLogEntry[]> {
  const params = new URLSearchParams({ namespace })
  if (opts?.search) params.set('search', opts.search)
  if (opts?.since) params.set('since', opts.since)
  const { data } = await client.get<ServiceLogEntry[]>(`/services/${name}/logs?${params.toString()}`)
  return data || []
}

export interface BindPayload {
  service: string
  app: string
  namespace: string
  prefix?: string
  database?: string
  // target picks which CR carries the binding: "app" (default) or "function".
  // The `app` field above carries the target name in either case.
  target?: 'app' | 'function'
}

// Both take part in the Env tab's write queue, which one unsettled request
// would otherwise stop for good. See envQueueTimeout in api/apps.ts.
const envQueueTimeout = { timeout: 30_000 }

export async function bindService(payload: BindPayload): Promise<void> {
  await client.post('/bind', payload, envQueueTimeout)
}

export async function unbindService(payload: BindPayload): Promise<void> {
  await client.post('/unbind', payload, envQueueTimeout)
}

// RabbitMQVhost mirrors DBDatabaseEntry's shape so the bind form's
// existing dropdown logic can render either endpoint's response
// without branching. `default` is true for the conventional "/".
export interface RabbitMQVhost {
  name: string
  default: boolean
}

export async function fetchRabbitMQVhosts(name: string, namespace: string): Promise<RabbitMQVhost[]> {
  const { data } = await client.get<RabbitMQVhost[]>(
    `/services/${name}/rabbitmq/vhosts?namespace=${encodeURIComponent(namespace)}`,
  )
  return data || []
}

export async function fetchServiceInfo(name: string, namespace: string): Promise<ServiceInfo> {
  const { data } = await client.get<ServiceInfo>(`/services/${name}?namespace=${encodeURIComponent(namespace)}`)
  return data
}

export interface ServiceResources {
  memory_limit: string
  memory_request: string
  cpu_limit: string
  cpu_request: string
}

export async function fetchServiceResources(name: string, namespace: string): Promise<ServiceResources> {
  const { data } = await client.get<ServiceResources>(`/services/${name}/resources?namespace=${encodeURIComponent(namespace)}`)
  return data
}

export async function updateServiceResources(name: string, namespace: string, resources: { memory_limit: string; cpu_limit: string }): Promise<void> {
  await client.put(`/services/${name}/resources?namespace=${encodeURIComponent(namespace)}`, resources)
}

export interface RolloutStatus {
  ready: boolean
  desired: number
  updated: number
  current: number
}

export async function fetchRolloutStatus(name: string, namespace: string): Promise<RolloutStatus> {
  const { data } = await client.get<RolloutStatus>(`/services/${name}/rollout?namespace=${encodeURIComponent(namespace)}`)
  return data
}

export type MigrationPhase = '' | 'Pending' | 'Running' | 'Succeeded' | 'Failed'

export interface MigrationStatus {
  job_name?: string
  phase: MigrationPhase
  started_at?: string
  completed_at?: string
  message?: string
  logs?: string[] | null
}

export async function startServiceMigration(
  name: string,
  namespace: string,
  sourceNamespace: string,
  confirm: string,
): Promise<{ job_name: string; phase: MigrationPhase }> {
  const { data } = await client.post<{ job_name: string; phase: MigrationPhase }>(
    `/services/${name}/migrate-data?namespace=${encodeURIComponent(namespace)}`,
    { source_namespace: sourceNamespace, confirm },
  )
  return data
}

export async function fetchServiceMigrationStatus(name: string, namespace: string): Promise<MigrationStatus> {
  const { data } = await client.get<MigrationStatus>(
    `/services/${name}/migrate-data/status?namespace=${encodeURIComponent(namespace)}`,
  )
  return data
}

// ShareLink is a capability link to a service's browseable UI. url is only
// populated on mint — listings identify links without exposing the token,
// which can never be reconstructed from the id.
export interface ShareLink {
  id: string
  url?: string
  label?: string
  created_by: string
  created_at: string
  expires_at: string
}

export interface CreateSharePayload {
  // expires_in is a Go duration string ("72h"). Empty means the default.
  expires_in?: string
  label?: string
}

export async function createShare(
  name: string,
  namespace: string,
  payload: CreateSharePayload,
): Promise<ShareLink> {
  const { data } = await client.post<ShareLink>(
    `/services/${encodeURIComponent(name)}/shares?namespace=${encodeURIComponent(namespace)}`,
    payload,
  )
  return data
}

export async function fetchShares(name: string, namespace: string): Promise<ShareLink[]> {
  const { data } = await client.get<ShareLink[]>(
    `/services/${encodeURIComponent(name)}/shares?namespace=${encodeURIComponent(namespace)}`,
  )
  return data || []
}

export async function revokeShare(name: string, namespace: string, id: string): Promise<void> {
  await client.delete(
    `/services/${encodeURIComponent(name)}/shares/${encodeURIComponent(id)}?namespace=${encodeURIComponent(namespace)}`,
  )
}

// revokeAllShares clears every share link in the cluster — the emergency
// lever, paired with rotateShareKey in the compromise runbook.
export async function revokeAllShares(): Promise<void> {
  await client.delete('/shares')
}

export async function rotateShareKey(): Promise<{ current_kid: string }> {
  const { data } = await client.post<{ current_kid: string }>('/shares/rotate-key')
  return data
}

export async function streamServiceDiagnosis(
  name: string,
  namespace: string,
  onChunk: (content: string) => void,
  onDone: () => void,
  onError: (error: string) => void,
): Promise<void> {
  const token = localStorage.getItem('kipper_token')
  const baseURL = client.defaults.baseURL || ''

  const response = await fetch(`${baseURL}/services/${name}/diagnose?namespace=${encodeURIComponent(namespace)}`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'Authorization': `Bearer ${token}`,
    },
  })

  if (!response.ok) {
    const body = await response.json().catch(() => ({ error: 'unknown error' }))
    onError(body.error || `HTTP ${response.status}`)
    return
  }

  const reader = response.body?.getReader()
  if (!reader) { onError('streaming not supported'); return }

  const decoder = new TextDecoder()
  let buffer = ''

  while (true) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    const lines = buffer.split('\n')
    buffer = lines.pop() || ''
    for (const line of lines) {
      if (!line.startsWith('data: ')) continue
      try {
        const parsed = JSON.parse(line.slice(6))
        if (parsed.done) { onDone(); return }
        if (parsed.error) { onError(parsed.error); return }
        if (parsed.content) onChunk(parsed.content)
      } catch { /* skip */ }
    }
  }
  onDone()
}
