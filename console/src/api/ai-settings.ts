import client from './client'

export interface AIConfig {
  provider: string
  api_key: string
  model: string
  ollama_url: string
}

export async function getAISettings(): Promise<AIConfig> {
  const { data } = await client.get<AIConfig>('/settings/ai')
  return data
}

export async function updateAISettings(config: AIConfig): Promise<void> {
  await client.put('/settings/ai', config)
}

export interface BundleDriftResource {
  kind: string
  name: string
  namespace: string
}

export interface BundleStatus {
  installed: boolean
  missing: BundleDriftResource[] | null
}

export interface BundleStatusReport {
  phase1: BundleStatus
  rag: BundleStatus
}

export async function getAIBundleStatus(): Promise<BundleStatusReport> {
  const { data } = await client.get<BundleStatusReport>('/settings/ai/bundle-status')
  return data
}
