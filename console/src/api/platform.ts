import client from './client'

export interface PlatformSummaryComponent {
  name: string
  enabled: boolean
  current_memory_limit?: string
  phase?: string
  at_ceiling?: boolean
}

export interface PlatformSummary {
  profile: string
  available_profiles: string[]
  components: PlatformSummaryComponent[]
}

export interface PlatformComponent {
  name: string
  enabled: boolean
  // Whether the component accepts enable/disable. Components that share a
  // chart with another (grafana and kube-state-metrics ride with prometheus,
  // promtail with loki) and always-on ones do not, and the API rejects the
  // update. An API build old enough to omit the field rejects it too, so the
  // card treats the absence as "no" rather than offering an action that fails.
  toggleable?: boolean
  profile_memory_limit?: string
  override_memory_limit?: string
  current_memory_limit?: string
  // Authoritative slider bounds from the backend's helmpaths table.
  // Older console-api builds may omit these; the card falls back to a
  // static table when absent.
  memory_min?: string
  memory_max?: string
  phase?: string
  restart_count_7d?: number
  last_bump_at?: string
  last_bump_from?: string
  last_bump_to?: string
  last_bump_reason?: string
  at_ceiling?: boolean
}

export interface PlatformComponentsResponse {
  profile: string
  components: PlatformComponent[]
}

export interface PlatformComponentPatch {
  memory_limit?: string
  enabled?: boolean
}

export async function fetchPlatformSummary(): Promise<PlatformSummary> {
  const { data } = await client.get<PlatformSummary>('/platform')
  return data
}

export async function fetchPlatformComponents(): Promise<PlatformComponentsResponse> {
  const { data } = await client.get<PlatformComponentsResponse>('/platform/components')
  return data
}

export async function patchPlatformComponent(name: string, patch: PlatformComponentPatch): Promise<PlatformComponent> {
  const { data } = await client.patch<PlatformComponent>(`/platform/components/${name}`, patch)
  return data
}
