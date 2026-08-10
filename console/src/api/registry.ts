import client from './client'

export interface RegistryEntry {
  name: string
  server: string
  username: string
  password: string
}

export interface TokenHealth {
  valid: boolean
  expires_at?: string
  days_remaining?: number
  error?: string
}

export async function fetchRegistries(): Promise<RegistryEntry[]> {
  const { data } = await client.get<{ registries: RegistryEntry[] }>('/settings/registries')
  return data.registries || []
}

export async function fetchRegistryHealth(): Promise<Record<string, TokenHealth>> {
  const { data } = await client.get<{ health: Record<string, TokenHealth> }>('/settings/registries/health')
  return data.health || {}
}

export async function addRegistry(entry: { name?: string; server: string; username: string; password: string }): Promise<void> {
  await client.post('/settings/registries', entry)
}

export async function removeRegistry(name: string): Promise<void> {
  await client.delete(`/settings/registries/${name}`)
}
