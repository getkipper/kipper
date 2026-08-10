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
