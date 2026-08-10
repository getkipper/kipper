import client from './client'
import type { App, CreateAppPayload, SecretKeyInfo } from './types'

export async function fetchApps(project: string): Promise<App[]> {
  const { data } = await client.get<App[]>(`/projects/${project}/apps`)
  return data
}

export async function createApp(project: string, payload: CreateAppPayload): Promise<App> {
  const { data } = await client.post<App>(`/projects/${project}/apps`, payload)
  return data
}

export async function deleteApp(project: string, app: string): Promise<void> {
  await client.delete(`/projects/${project}/apps/${app}`)
}

export async function restartApp(project: string, app: string): Promise<void> {
  await client.post(`/projects/${project}/apps/${app}/restart`)
}

export async function scaleApp(project: string, app: string, replicas: number): Promise<void> {
  await client.put(`/projects/${project}/apps/${app}/scale`, { replicas })
}

// The Env tab serialises its writes, so one request that neither answers nor
// fails would stop every later edit to that app. These six take part in that
// queue and are bounded for it; the rest of the client is not, because
// `addEnvironment` is given 60 seconds by its own handler and copies resources
// under it.
const envQueueTimeout = { timeout: 30_000 }

export async function fetchEnv(project: string, app: string): Promise<Record<string, string>> {
  const { data } = await client.get<Record<string, string>>(`/projects/${project}/apps/${app}/env`, envQueueTimeout)
  return data
}

/**
 * EnvPreview is what the Env tab shows beyond the raw map: what each template
 * resolves to, where each reference came from, and what an operator may refer
 * to.
 *
 * This is what the next render will produce, not what a running pod holds. The
 * two come apart whenever a source has changed and the reconciler has not
 * republished yet, and the restart banner cannot close that window because it
 * compares the last published environment against the pods, not against the
 * sources this is read from. So the tab says "resolves to" rather than claiming
 * a value is live.
 *
 * Deployer-only. The plain env GET returns the templates as written, which hold
 * no credential; this returns what they resolve to, so a viewer is refused it.
 * Secret-derived substitutions arrive already masked — the server never sends
 * the credential and the console never has one to hide.
 */
export interface EnvPreviewReference {
  name: string
  /** Which kind of source answered: spec.env, secrets, binding, link, runtime. */
  origin?: string
  /** The Secret or Service it came from. */
  source?: string
  resolved: boolean
  /** The value was masked rather than shown. */
  secret: boolean
  /**
   * Names another spec.env entry that is itself a template. Resolution is a
   * single pass, so the inner reference survives into the resolved value.
   */
  transitive: boolean
}

export interface EnvPreviewVariable {
  key: string
  /** The value as written on the CR, references and all. */
  template: string
  /**
   * What the next render will produce, masked. Read it with isTemplate rather
   * than by testing it for content: a template resolving to the empty string is
   * a real result.
   */
  resolved: string
  isTemplate: boolean
  references?: EnvPreviewReference[]
  /** $(NAME) references, which nothing expands and which reach the app as written. */
  shellStyle?: string[]
  /** The source that sets this key too and wins, so the pod never sees this one. */
  shadowedBy?: string
}

export interface EnvPreviewName {
  name: string
  origin: string
  source?: string
  secret: boolean
}

export interface EnvPreviewSnippet {
  service: string
  type: string
  key: string
  value: string
}

export interface EnvPreview {
  variables: EnvPreviewVariable[]
  available: EnvPreviewName[]
  snippets?: EnvPreviewSnippet[]
  /**
   * Bindings whose credentials this app may not read. Their variables are
   * absent from everything above, so a reference to one reads as unresolved.
   * That is what the next render will make of it, and not what pods already
   * running on the last published environment receive.
   */
  refused?: string[]
}

export async function fetchEnvPreview(project: string, app: string): Promise<EnvPreview> {
  const { data } = await client.get<EnvPreview>(`/projects/${project}/apps/${app}/env/preview`)
  return data
}

// AppLink is one dependency this app declares, with the address it currently
// resolves to. The address is derived by the reconciler rather than stored, so
// it comes from the server and is never posted back.
export interface AppLink {
  app: string
  namespace: string
  envVar: string
  url?: string
  // open is whether the link resolves: the target project consents, the target
  // exists and serves a port, and no other link claims its variable.
  open: boolean
  // reason says why a link that does not resolve does not.
  reason?: string
  // injected is whether the address has reached the workload yet. A link can
  // resolve before the pods carrying it have rolled.
  injected: boolean
}

export async function fetchLinks(project: string, app: string): Promise<AppLink[]> {
  const { data } = await client.get<AppLink[]>(`/projects/${project}/apps/${app}/links`)
  return data
}

// Returns what was stored, which is not always what was sent: an address a link
// supersedes is dropped on the way in. Callers should adopt the result rather
// than keep their own copy, or the next edit resends a key the app no longer has.
export async function updateEnv(project: string, app: string, env: Record<string, string>): Promise<Record<string, string>> {
  const { data } = await client.put<Record<string, string>>(`/projects/${project}/apps/${app}/env`, env, envQueueTimeout)
  return data ?? env
}

// fetchEnvRestartPending reports whether the running pods still hold env from
// before the last change. The console shows a "restart to apply" banner when
// this is true, since env is only read at pod startup.
export async function fetchEnvRestartPending(project: string, app: string): Promise<boolean> {
  const { data } = await client.get<{ restartPending: boolean }>(`/projects/${project}/apps/${app}/env/status`)
  return data.restartPending
}

export async function updateImage(project: string, app: string, image: string): Promise<void> {
  await client.put(`/projects/${project}/apps/${app}/image`, { image })
}

export async function fetchEnvConflicts(project: string, app: string): Promise<string[]> {
  const { data } = await client.get<string[]>(`/projects/${project}/apps/${app}/env/conflicts`)
  return data
}

export async function removeEnvConflicts(project: string, app: string): Promise<void> {
  await client.delete(`/projects/${project}/apps/${app}/env/conflicts`)
}

export interface InjectedVar {
  name: string
  service: string
  secret: boolean
  value?: string
}

export async function fetchInjectedEnv(project: string, app: string): Promise<InjectedVar[]> {
  const { data } = await client.get<InjectedVar[]>(`/projects/${project}/apps/${app}/env/injected`)
  return data || []
}

export interface LinkResponse {
  target: string
  app: string
  envVar: string
  url: string
}

export async function linkApp(target: string, app: string, namespace: string, isPublic?: boolean): Promise<LinkResponse> {
  const { data } = await client.post<LinkResponse>('/link', { target, app, namespace, public: isPublic || false }, envQueueTimeout)
  return data
}

export async function unlinkApp(target: string, app: string, namespace: string): Promise<void> {
  await client.post('/unlink', { target, app, namespace }, envQueueTimeout)
}

export async function fetchSecretKeys(project: string, app: string): Promise<SecretKeyInfo[]> {
  const { data } = await client.get<SecretKeyInfo[]>(`/projects/${project}/apps/${app}/secrets`)
  return data || []
}

export async function revealSecret(project: string, app: string, key: string): Promise<string> {
  const { data } = await client.get<{ key: string; value: string }>(`/projects/${project}/apps/${app}/secrets/${key}`)
  return data.value
}

export async function setSecrets(project: string, app: string, secrets: Record<string, string>): Promise<void> {
  await client.put(`/projects/${project}/apps/${app}/secrets`, secrets)
}

export async function deleteSecret(project: string, app: string, key: string): Promise<void> {
  await client.delete(`/projects/${project}/apps/${app}/secrets/${key}`)
}

export interface AppResources {
  memory_limit: string
  memory_request: string
  cpu_limit: string
  cpu_request: string
}

export async function fetchResources(project: string, app: string): Promise<AppResources> {
  const { data } = await client.get<AppResources>(`/projects/${project}/apps/${app}/resources`)
  return data
}

export interface UpdateResourcesPayload {
  memory_request?: string
  memory_limit?: string
  cpu_request?: string
  cpu_limit?: string
}

export async function updateResources(project: string, app: string, resources: UpdateResourcesPayload): Promise<void> {
  await client.put(`/projects/${project}/apps/${app}/resources`, resources)
}

export interface AutoscaleConfig {
  enabled: boolean
  min_replicas: number
  max_replicas: number
  cpu_target: number
  memory_target: number
  current_replicas: number
  current_cpu: string
  current_memory: string
}

export async function fetchAutoscale(project: string, app: string): Promise<AutoscaleConfig> {
  const { data } = await client.get<AutoscaleConfig>(`/projects/${project}/apps/${app}/autoscale`)
  return data
}

export async function setAutoscale(project: string, app: string, config: {
  min_replicas: number; max_replicas: number; cpu_target: number; memory_target: number
}): Promise<void> {
  await client.put(`/projects/${project}/apps/${app}/autoscale`, config)
}

export async function disableAutoscale(project: string, app: string): Promise<void> {
  await client.delete(`/projects/${project}/apps/${app}/autoscale`)
}

export interface ResourceRecommendation {
  active: boolean
  message?: string
  recommended_profile?: string
  since?: string
}

export async function fetchRecommendation(project: string, app: string): Promise<ResourceRecommendation> {
  const { data } = await client.get<ResourceRecommendation>(`/projects/${project}/apps/${app}/recommendation`)
  return data
}

export async function dismissRecommendation(project: string, app: string): Promise<void> {
  await client.post(`/projects/${project}/apps/${app}/recommendation/dismiss`)
}

export async function applyRecommendation(project: string, app: string): Promise<void> {
  await client.post(`/projects/${project}/apps/${app}/recommendation/apply`)
}

export interface GitSource {
  configured: boolean
  url?: string
  branch?: string
  dockerfile_path?: string
  context?: string
  has_token: boolean
}

export interface UpdateGitSourcePayload {
  // Partial update — empty fields are not applied server-side. Pass
  // `token` only when rotating the PAT.
  url?: string
  branch?: string
  dockerfile_path?: string
  context?: string
  token?: string
}

export async function fetchGitSource(project: string, app: string): Promise<GitSource> {
  const { data } = await client.get<GitSource>(`/projects/${project}/apps/${app}/git`)
  return data
}

export async function updateGitSource(project: string, app: string, payload: UpdateGitSourcePayload): Promise<GitSource> {
  const { data } = await client.put<GitSource>(`/projects/${project}/apps/${app}/git`, payload)
  return data
}

export interface BuildStatus {
  git_configured: boolean
  git_url?: string
  git_branch?: string
  credentials_secret?: string
  phase: string
  commit?: string
  startedAt?: string
  completedAt?: string
  message?: string
  registry_valid?: boolean
  git_credential_valid?: boolean
}

export async function fetchBuildStatus(project: string, app: string): Promise<BuildStatus> {
  const { data } = await client.get<BuildStatus>(`/projects/${project}/apps/${app}/build/status`)
  return data
}

export async function triggerRebuild(project: string, app: string, commit?: string): Promise<void> {
  await client.post(`/projects/${project}/apps/${app}/rebuild`, commit ? { commit } : {})
}

export async function cancelBuild(project: string, app: string): Promise<void> {
  await client.post(`/projects/${project}/apps/${app}/build/cancel`)
}

export async function streamBuildLogs(
  project: string,
  app: string,
  onChunk: (text: string) => void,
  signal: AbortSignal,
): Promise<void> {
  const token = localStorage.getItem('kipper_token')
  const baseURL = client.defaults.baseURL || ''

  const response = await fetch(`${baseURL}/projects/${project}/apps/${app}/build/logs`, {
    headers: { Authorization: `Bearer ${token}` },
    signal,
  })

  if (!response.ok) {
    const body = await response.text().catch(() => '')
    throw new Error(body || `HTTP ${response.status}`)
  }

  const reader = response.body?.getReader()
  if (!reader) {
    throw new Error('streaming not supported')
  }

  const decoder = new TextDecoder()
  let buffer = ''

  while (true) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    const lines = buffer.split('\n')
    buffer = lines.pop() || ''
    for (const line of lines) {
      onChunk(line)
    }
  }
  if (buffer.length > 0) {
    onChunk(buffer)
  }
}

export interface LogEntry {
  timestamp: string
  line: string
  pod: string
}

export async function fetchLogs(
  project: string,
  app: string,
  opts?: { search?: string; since?: string; limit?: number }
): Promise<LogEntry[]> {
  const params = new URLSearchParams()
  if (opts?.search) params.set('search', opts.search)
  if (opts?.since) params.set('since', opts.since)
  if (opts?.limit) params.set('limit', String(opts.limit))
  const query = params.toString() ? `?${params.toString()}` : ''
  const { data } = await client.get<LogEntry[]>(`/projects/${project}/apps/${app}/logs${query}`)
  return data || []
}

export interface AppRouteHealth {
  ingress_ready: boolean
  tls_ready: boolean
  message: string
}

export interface AppRoute {
  host: string
  path: string
  redirect_from: string[] | null
  url: string
  enabled: boolean
  health: AppRouteHealth
}

export async function fetchRoute(project: string, app: string): Promise<AppRoute> {
  const { data } = await client.get<AppRoute>(`/projects/${project}/apps/${app}/route`)
  return data
}

export async function setRoute(
  project: string,
  app: string,
  route: { host?: string; path?: string; redirect_from?: string[] },
): Promise<AppRoute> {
  const { data } = await client.put<AppRoute>(`/projects/${project}/apps/${app}/route`, route)
  return data
}

export async function deleteRoute(project: string, app: string): Promise<void> {
  await client.delete(`/projects/${project}/apps/${app}/route`)
}

export type RouteDnsStatus = 'ok' | 'mismatch' | 'unresolved' | 'gateway' | 'wildcard' | 'disabled'

export interface RouteDnsStatusResponse {
  hostname: string
  status: RouteDnsStatus
  message: string
  expected_ips: string[] | null
  resolved_ips: string[] | null
}

export async function fetchRouteDnsStatus(
  project: string,
  app: string,
  opts?: { verify?: boolean },
): Promise<RouteDnsStatusResponse> {
  const query = opts?.verify ? '?verify=true' : ''
  const { data } = await client.get<RouteDnsStatusResponse>(`/projects/${project}/apps/${app}/route/dns-status${query}`)
  return data
}

export interface AppSettings {
  security_headers: boolean
  instance_header: boolean
  rate_limit: number
  require_api_key: boolean
  csp_allowlist: string[]
  redirects: Array<{ source: string; target: string; permanent: boolean }>
  basic_auth: boolean
  // Set by the API only: the key gate is on but not yet confirmed in place, so
  // the route may still be reachable without a key. Read-only for the console.
  api_key_gate_pending?: boolean
}

export async function fetchSettings(project: string, app: string): Promise<AppSettings> {
  const { data } = await client.get<AppSettings>(`/projects/${project}/apps/${app}/settings`)
  return data
}

export async function updateSettings(project: string, app: string, settings: AppSettings): Promise<void> {
  await client.put(`/projects/${project}/apps/${app}/settings`, settings)
}

export interface BasicAuthStatus {
  enabled: boolean
  users: string[]
}

export async function fetchBasicAuth(project: string, app: string): Promise<BasicAuthStatus> {
  const { data } = await client.get<BasicAuthStatus>(`/projects/${project}/apps/${encodeURIComponent(app)}/basic-auth`)
  return data
}

export async function setBasicAuthUser(project: string, app: string, username: string, password: string): Promise<void> {
  await client.put(`/projects/${project}/apps/${encodeURIComponent(app)}/basic-auth`, { username, password })
}

export async function deleteBasicAuth(project: string, app: string): Promise<void> {
  await client.delete(`/projects/${project}/apps/${encodeURIComponent(app)}/basic-auth`)
}

export async function deleteBasicAuthUser(project: string, app: string, username: string): Promise<void> {
  await client.delete(`/projects/${project}/apps/${encodeURIComponent(app)}/basic-auth/${encodeURIComponent(username)}`)
}

export interface DeployEntry {
  revision: number
  image: string
  commit: string
  trigger: string
  timestamp: string
}

export async function fetchDeployHistory(project: string, app: string): Promise<DeployEntry[]> {
  const { data } = await client.get<DeployEntry[]>(`/projects/${project}/apps/${app}/history`)
  return data
}

export async function rollbackApp(project: string, app: string, revision?: number): Promise<void> {
  await client.post(`/projects/${project}/apps/${app}/rollback`, { revision: revision || 0 })
}

export async function fetchPods(project: string, app: string): Promise<string[]> {
  const { data } = await client.get<{ pods: string[] }>(`/projects/${project}/apps/${app}/pods`)
  return data.pods
}

export interface WebhookConfig {
  enabled: boolean
  token?: string
}

export async function fetchWebhookConfig(project: string, app: string): Promise<WebhookConfig> {
  const { data } = await client.get<WebhookConfig>(`/projects/${project}/apps/${app}/webhook`)
  return data
}

export async function generateWebhookToken(project: string, app: string): Promise<{ token: string }> {
  const { data } = await client.post<{ token: string }>(`/projects/${project}/apps/${app}/webhook`)
  return data
}

export async function deleteWebhook(project: string, app: string): Promise<void> {
  await client.delete(`/projects/${project}/apps/${app}/webhook`)
}
