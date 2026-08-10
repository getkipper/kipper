import client from './client'

export interface SmtpConfig {
  host: string
  port: number
  username: string
  password: string
  from: string
  tls: boolean
}

export async function getSmtpSettings(): Promise<SmtpConfig> {
  const { data } = await client.get<SmtpConfig>('/settings/smtp')
  return data
}

export async function updateSmtpSettings(config: SmtpConfig): Promise<SmtpConfig> {
  const { data } = await client.put<SmtpConfig>('/settings/smtp', config)
  return data
}

export async function testSmtpSettings(to?: string): Promise<void> {
  await client.post('/settings/smtp/test', to ? { to } : {})
}
