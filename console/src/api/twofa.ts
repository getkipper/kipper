import client from './client'

export interface TwoFactorStatus {
  state: 'absent' | 'pending' | 'active'
  min_age_days: number
  enrolled_at?: string
  eligible_at?: string
  eligible?: boolean
}

export interface TwoFactorEnrollment {
  otpauth_uri: string
  secret: string
}

export interface TwoFactorConfirmation {
  status: string
  recovery_codes: string[]
  enrolled_at: string
  eligible_at: string
}

export async function getTwoFactorStatus(): Promise<TwoFactorStatus> {
  const { data } = await client.get<TwoFactorStatus>('/auth/2fa/status')
  return data
}

export async function enrollTwoFactor(bootstrapCode: string): Promise<TwoFactorEnrollment> {
  const { data } = await client.post<TwoFactorEnrollment>('/auth/2fa/enroll', {
    bootstrap_code: bootstrapCode,
  })
  return data
}

export async function confirmTwoFactor(code: string): Promise<TwoFactorConfirmation> {
  const { data } = await client.post<TwoFactorConfirmation>('/auth/2fa/confirm', { code })
  return data
}

export async function resetTwoFactor(payload: { code?: string; recovery_code?: string }): Promise<TwoFactorEnrollment> {
  const { data } = await client.post<TwoFactorEnrollment>('/auth/2fa/reset', payload)
  return data
}
