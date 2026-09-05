import client from './client'

export interface AlertDeliveryResponse {
  route: 'slack' | 'email' | 'nowhere'
  going_nowhere: boolean
  // Why nothing leaves: no channel at all, or an SMTP server with no admin
  // address to send to. Absent when alerts are being delivered.
  reason?: 'no_channel' | 'no_recipients'
}

// Where this cluster's alerts leave by, if they leave at all. The response
// names the channel and never the credential.
export async function getAlertDelivery(): Promise<AlertDeliveryResponse> {
  const { data } = await client.get<AlertDeliveryResponse>('/settings/alert-delivery')
  return data
}
