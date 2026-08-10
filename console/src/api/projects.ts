import client from './client'

export interface AppSummary {
  name: string
  image: string
  status: string
  ready: string
  replicas: number
  url?: string
}

export interface Environment {
  name: string
  namespace: string
  status: string
  apps: AppSummary[]
  order: string
  /**
   * Whether nothing live contradicts this project's claim on the namespace.
   *
   * A declaration is not ownership: two projects can resolve to one namespace
   * and only one of them holds it. The server reads the namespace's own label,
   * which is what authorization reads, so this is the answer rather than a
   * guess made from comparing declarations.
   */
  owned: boolean
}

export type ProjectRole = 'owner' | 'deployer' | 'viewer'

export interface Project {
  name: string
  display_name?: string
  org?: string
  // The current user's role in this project. Admins see 'owner' everywhere.
  role: ProjectRole
  environments: Environment[]
  // Effective environment cap for the project's tier. Adding an environment
  // beyond this needs a cluster admin to raise the tier or the limit.
  env_limit: number
}

export async function fetchProjects(): Promise<Project[]> {
  const { data } = await client.get<Project[]>('/projects')
  return data
}

export interface ProjectMember {
  email: string
  role: ProjectRole
}

export async function fetchMembers(project: string): Promise<ProjectMember[]> {
  const { data } = await client.get<ProjectMember[]>(`/projects/${project}/members`)
  return data
}

export async function setMember(project: string, email: string, role: ProjectRole): Promise<void> {
  await client.put(`/projects/${project}/members`, { email, role })
}

export async function removeMember(project: string, email: string): Promise<void> {
  await client.delete(`/projects/${project}/members/${encodeURIComponent(email)}`)
}

export async function createProject(name: string, environments?: string[], displayName?: string): Promise<void> {
  await client.post('/projects', { name, environments, display_name: displayName })
}

export async function deleteProject(name: string): Promise<void> {
  await client.delete(`/projects/${name}`)
}

export async function updateProjectEnvironments(name: string, environments: string[]): Promise<void> {
  await client.put(`/projects/${name}`, { environments })
}

export interface AppResourcesOverride {
  profile?: string
  cpuRequest?: string
  cpuLimit?: string
  memoryRequest?: string
  memoryLimit?: string
}

export interface AppRouteOverride {
  host: string
  path?: string
}

export interface AppOverride {
  route?: AppRouteOverride
  env?: Record<string, string>
  replicas?: number
  resources?: AppResourcesOverride
}

export interface AddEnvironmentRequest {
  name: string
  copy_from?: string
  assign_default_routes?: boolean
  apps?: Record<string, AppOverride>
}

export interface CopyPreviewApp {
  name: string
  image: string
  port: number
  replicas: number
  env: Record<string, string> | null
  route?: { host: string; path: string } | null
  resources: AppResourcesOverride
}

export interface CopyPreviewService {
  name: string
  type: string
  version?: string
  storage?: string
}

export interface CopyPreviewVolume {
  name: string
  size: string
}

export interface CopyPreviewSecret {
  name: string
  keys: string[]
}

export interface CopyPreview {
  source: string
  source_namespace: string
  target: string
  target_namespace: string
  cluster_domain: string
  default_hosts: Record<string, string>
  apps: CopyPreviewApp[]
  services: CopyPreviewService[]
  volumes: CopyPreviewVolume[]
  functions: string[]
  jobs: string[]
  secrets: CopyPreviewSecret[]
}

export async function fetchCopyPreview(project: string, source: string, target: string): Promise<CopyPreview> {
  const params = new URLSearchParams({ from: source, target })
  const { data } = await client.get<CopyPreview>(`/projects/${project}/copy-preview?${params.toString()}`)
  return data
}

export interface CopySummary {
  apps: number
  services: number
  volumes: number
  functions: number
  jobs: number
  secrets: number
  warnings?: string[] | null
}

export interface AddEnvironmentResponse {
  name: string
  namespace: string
  copy?: CopySummary
}

export async function addEnvironment(project: string, req: AddEnvironmentRequest): Promise<AddEnvironmentResponse> {
  const { data } = await client.post<AddEnvironmentResponse>(`/projects/${project}/environments`, req)
  return data
}

export interface QuotaDimensions {
  cpu_request: string
  cpu_limit: string
  memory_request: string
  memory_limit: string
}

export interface EnvironmentQuota {
  environment: string
  namespace: string
  // "none" means a tierless environment without an override: no quota
  // objects exist and hard carries empty strings.
  source: 'tier' | 'override' | 'none'
  hard: QuotaDimensions
  used?: QuotaDimensions
  /**
   * Whether any dimension is used beyond its hard cap, and null when nothing
   * compared them — no live quota object, or a read that could not run. An
   * outage is exactly when false and unknown have to stay apart.
   */
  over_quota: boolean | null
}

export interface ProjectQuota {
  tier: string
  tiers: Record<string, QuotaDimensions>
  environments: EnvironmentQuota[]
  env_limit: number
  env_count: number
  max_environments?: number
}

export interface QuotaWarning {
  environment: string
  dimension: string
  used: string
  new_cap: string
}

export interface QuotaUpdate {
  tier?: string
  environments?: { name: string; quota: QuotaDimensions | null }[]
  force?: boolean
}

export async function fetchQuota(project: string): Promise<ProjectQuota> {
  const { data } = await client.get<ProjectQuota>(`/projects/${project}/quota`)
  return data
}

export async function updateQuota(project: string, update: QuotaUpdate): Promise<ProjectQuota> {
  const { data } = await client.put<ProjectQuota>(`/projects/${project}/quota`, update)
  return data
}

export async function promoteApp(project: string, app: string, from: string, to: string): Promise<void> {
  await client.post(`/projects/${project}/promote`, { app, from, to })
}

export async function promoteAll(project: string, from: string, to: string): Promise<void> {
  await client.post(`/projects/${project}/promote`, { all: true, from, to })
}
