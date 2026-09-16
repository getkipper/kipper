import client from './client'

export interface RouteHealth {
  ingress_ready: boolean
  tls_ready: boolean
  message: string
}

export interface RouteEntry {
  path: string
  service: string
  port: number
  app: string
  health: RouteHealth
  // Configured refused prefixes and public exceptions, including the route base path.
  refused_paths: string[]
  public_paths: string[]
  // Whether guard Ingresses match the requested policy.
  refusal_ready: boolean
}

export interface RouteGroup {
  name: string
  namespace: string
  host: string
  tls: boolean
  project: string
  environment: string
  routes: RouteEntry[]
  health: RouteHealth
  // Whether the cluster refuses the well-known internal paths on every route.
  route_guard: boolean
}

export async function fetchRoutes(): Promise<RouteGroup[]> {
  const { data } = await client.get<RouteGroup[]>('/routes')
  return data
}

export interface PathMapping {
  path: string
  app: string
}

export interface RouteGroupPayload {
  host?: string
  mappings: PathMapping[]
}

export interface RouteGroupResponse {
  host: string
  url: string
  mappings: PathMapping[]
}

export async function createRouteGroup(project: string, payload: RouteGroupPayload): Promise<RouteGroupResponse> {
  const { data } = await client.post<RouteGroupResponse>(`/projects/${project}/route-groups`, payload)
  return data
}

export async function updateRouteGroup(project: string, payload: RouteGroupPayload): Promise<RouteGroupResponse> {
  const { data } = await client.put<RouteGroupResponse>(`/projects/${project}/route-groups`, payload)
  return data
}

export async function deleteRouteGroup(project: string, host: string): Promise<void> {
  await client.delete(`/projects/${project}/route-groups/${encodeURIComponent(host)}`)
}
