export interface NodeInfo {
  name: string
  status: string
  role: string
  version: string
  ip: string
}

export interface ClusterStatus {
  health: 'healthy' | 'degraded' | 'unknown'
  nodes: NodeInfo[]
}

export interface Project {
  name: string
  status: string
}

export interface App {
  name: string
  status: 'running' | 'pending' | 'failed' | 'stopped'
  image: string
  replicas: number
  ready: number
  namespace?: string
  project?: string
}

export interface CreateAppPayload {
  name: string
  image: string
  port: number
  replicas: number
  env: Record<string, string>
  resource_profile?: string
  cpu_request?: string
  cpu_limit?: string
  memory_request?: string
  memory_limit?: string
  git?: {
    url: string
    branch?: string
    token?: string
    dockerfile_path?: string
    context?: string
  }
  route?: {
    host?: string
    path?: string
  }
}

export interface User {
  email: string
  username: string
}

export interface ComponentStatus {
  name: string
  healthy: boolean
  message: string
}

export interface DashboardNode {
  name: string
  status: string
  cpu_usage: string
  memory_usage: string
  disk_usage: string
}

export interface OOMKill {
  pod: string
  namespace: string
  time: string
}

export interface DashboardResponse {
  components: ComponentStatus[]
  nodes: DashboardNode[]
  oom_kills: OOMKill[]
}

// Secret list entries — values are write-only; has_previous flags keys
// that can be rotated back.
export interface SecretKeyInfo {
  key: string
  has_previous: boolean
}
