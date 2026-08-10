import client from './client'

export interface ModeResponse {
  mode: 'auto' | 'expert'
}

export interface ResourceLogEntry {
  time: string
  app: string
  namespace: string
  action: string
  from: string
  to: string
  reason: string
}

export interface ResourceLogResponse {
  entries: ResourceLogEntry[]
}

export async function getMode(): Promise<ModeResponse> {
  const { data } = await client.get<ModeResponse>('/settings/mode')
  return data
}

export async function updateMode(mode: 'auto' | 'expert'): Promise<void> {
  await client.put('/settings/mode', { mode })
}

export async function getResourceLog(): Promise<ResourceLogEntry[]> {
  const { data } = await client.get<ResourceLogResponse>('/settings/resource-log')
  return data.entries || []
}
