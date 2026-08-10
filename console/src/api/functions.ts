import client from './client'
import type { SecretKeyInfo } from './types'

export interface FunctionInfo {
  name: string
  trigger: string
  image: string
  ready: number
  status: string
  url: string
}

export interface CreateFunctionPayload {
  name: string
  image: string
  port: number
  trigger: string
  schedule?: string
  idle_timeout: number
  source?: string
  query?: string
  mark_done?: string
  redis_list?: string
  bucket?: string
}

export interface FunctionLogEntry {
  timestamp: string
  line: string
  pod: string
}

export async function fetchFunctions(project: string): Promise<FunctionInfo[]> {
  const { data } = await client.get<FunctionInfo[]>(`/projects/${project}/functions`)
  return data
}

export async function createFunction(project: string, payload: CreateFunctionPayload): Promise<void> {
  await client.post(`/projects/${project}/functions`, payload)
}

export async function deleteFunction(project: string, name: string): Promise<void> {
  await client.delete(`/projects/${project}/functions/${name}`)
}

export interface FunctionResources {
  memory_limit: string
  memory_request: string
  cpu_limit: string
  cpu_request: string
}

export async function fetchFunctionResources(project: string, name: string): Promise<FunctionResources> {
  const { data } = await client.get<FunctionResources>(`/projects/${project}/functions/${name}/resources`)
  return data
}

export interface UpdateFunctionResourcesPayload {
  memory_request?: string
  memory_limit?: string
  cpu_request?: string
  cpu_limit?: string
}

export async function updateFunctionResources(project: string, name: string, resources: UpdateFunctionResourcesPayload): Promise<void> {
  await client.put(`/projects/${project}/functions/${name}/resources`, resources)
}

export async function fetchFunctionLogs(
  project: string,
  name: string,
  opts?: { search?: string; since?: string }
): Promise<FunctionLogEntry[]> {
  const params = new URLSearchParams()
  if (opts?.search) params.set('search', opts.search)
  if (opts?.since) params.set('since', opts.since)
  const query = params.toString() ? `?${params.toString()}` : ''
  const { data } = await client.get<FunctionLogEntry[]>(`/projects/${project}/apps/${name}/logs${query}`)
  return data || []
}

export interface TestRunResponse {
  job_name: string
  namespace: string
}

// runFunctionTest kicks off a one-off run of a cron-triggered function.
// The CronJob and its schedule are untouched — the backend creates a
// separate Job from the same pod template with KIPPER_TRIGGER=test.
export async function runFunctionTest(project: string, name: string): Promise<TestRunResponse> {
  const { data } = await client.post<TestRunResponse>(`/projects/${project}/functions/${name}/test`)
  return data
}

export interface FunctionSettings {
  security_headers: boolean
  csp_allowlist: string[]
}

export async function fetchFunctionSettings(project: string, name: string): Promise<FunctionSettings> {
  const { data } = await client.get<FunctionSettings>(`/projects/${project}/functions/${name}/settings`)
  return data
}

export async function updateFunctionSettings(project: string, name: string, settings: FunctionSettings): Promise<void> {
  await client.put(`/projects/${project}/functions/${name}/settings`, settings)
}

// Plain env vars (FunctionSpec.Env) — non-sensitive key/value pairs.

export async function fetchFunctionEnv(project: string, name: string): Promise<Record<string, string>> {
  const { data } = await client.get<Record<string, string>>(`/projects/${project}/functions/${name}/env`)
  return data || {}
}

export async function updateFunctionEnv(project: string, name: string, env: Record<string, string>): Promise<void> {
  await client.put(`/projects/${project}/functions/${name}/env`, env)
}

// Secrets — values are write-only. The list endpoint returns names + a
// has_previous flag so the UI can offer "rotate back".

export async function fetchFunctionSecretKeys(project: string, name: string): Promise<SecretKeyInfo[]> {
  const { data } = await client.get<SecretKeyInfo[]>(`/projects/${project}/functions/${name}/secrets`)
  return data || []
}

export async function setFunctionSecrets(project: string, name: string, secrets: Record<string, string>): Promise<void> {
  await client.put(`/projects/${project}/functions/${name}/secrets`, secrets)
}

export async function deleteFunctionSecret(project: string, name: string, key: string): Promise<void> {
  await client.delete(`/projects/${project}/functions/${name}/secrets/${encodeURIComponent(key)}`)
}

// Inline source dependencies (npm or pip names mapped to version specifiers).

export async function fetchFunctionDependencies(project: string, name: string): Promise<Record<string, string>> {
  const { data } = await client.get<Record<string, string>>(`/projects/${project}/functions/${name}/dependencies`)
  return data || {}
}

export async function updateFunctionDependencies(project: string, name: string, deps: Record<string, string>): Promise<void> {
  await client.put(`/projects/${project}/functions/${name}/dependencies`, deps)
}
