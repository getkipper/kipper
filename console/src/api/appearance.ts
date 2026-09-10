import client from './client'

export interface AppearanceSettings {
  faviconColour: string
}

export async function getAppearance(): Promise<AppearanceSettings> {
  const { data } = await client.get<AppearanceSettings>('/appearance')
  return data
}

export async function updateAppearance(faviconColour: string): Promise<AppearanceSettings> {
  const { data } = await client.put<AppearanceSettings>('/settings/appearance', { faviconColour })
  return data
}
