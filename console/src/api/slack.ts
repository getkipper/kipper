import client from './client'

interface SlackSettingsResponse {
  webhook_url: string
}

export async function getSlackSettings(): Promise<SlackSettingsResponse> {
  const { data } = await client.get<SlackSettingsResponse>('/settings/slack')
  return data
}

export async function updateSlackSettings(webhookUrl: string): Promise<SlackSettingsResponse> {
  const { data } = await client.put<SlackSettingsResponse>('/settings/slack', { webhook_url: webhookUrl })
  return data
}
