import client from './client'
import type { TokenHealth } from './registry'

export type { TokenHealth }

export interface GitCredentialEntry {
  name: string
  server: string
  username: string
  token: string
  app_count?: number
}

interface GitCredentialListResponse {
  credentials: GitCredentialEntry[]
}

export async function fetchGitCredentials(): Promise<GitCredentialEntry[]> {
  const { data } = await client.get<GitCredentialListResponse>('/settings/git-credentials')
  return data.credentials || []
}

export async function addGitCredential(entry: { name?: string; server: string; username: string; token: string }): Promise<void> {
  await client.post('/settings/git-credentials', entry)
}

export async function fetchGitCredentialHealth(): Promise<Record<string, TokenHealth>> {
  const { data } = await client.get<{ health: Record<string, TokenHealth> }>('/settings/git-credentials/health')
  return data.health || {}
}

export async function removeGitCredential(name: string): Promise<void> {
  await client.delete(`/settings/git-credentials/${name}`)
}
