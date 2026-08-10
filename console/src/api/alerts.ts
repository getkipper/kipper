import client from './client'

export interface Alert {
  id: string
  time: string
  app: string
  namespace: string
  action: string
  severity: 'info' | 'warning' | 'critical'
  reason: string
}

interface AlertsListResponse {
  alerts: Alert[]
}

interface UnreadCountResponse {
  count: number
}

export async function fetchAlerts(): Promise<Alert[]> {
  const { data } = await client.get<AlertsListResponse>('/alerts')
  return data.alerts || []
}

export async function fetchUnreadCount(): Promise<number> {
  const { data } = await client.get<UnreadCountResponse>('/alerts/unread-count')
  return data.count
}

export async function dismissAlerts(): Promise<void> {
  await client.post('/alerts/dismiss')
}
