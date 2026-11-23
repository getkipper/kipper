import client from './client'

export interface Backup {
  name: string
  status: string
  namespaces: string
  created: string
  ttl: string
  reason: string
}

export interface BackupSchedule {
  name: string
  schedule: string
  status: string
  last_backup: string
}

export interface CreateBackupPayload {
  name?: string
  namespaces?: string
  ttl?: string
}

export async function fetchBackups(): Promise<Backup[]> {
  const { data } = await client.get<Backup[]>('/backups')
  return data || []
}

export async function createBackup(payload: CreateBackupPayload): Promise<void> {
  await client.post('/backups', payload)
}

export interface RestoreOptions {
  namespace?: string
  resources?: string
}

export async function restoreBackup(backupName: string, opts?: RestoreOptions): Promise<void> {
  await client.post(`/backups/${backupName}/restore`, opts || {})
}

/**
 * Requests deletion of a backup and its stored data. `queued` is true when
 * the server creates a Velero deletion request (HTTP 202), or false when it
 * directly deletes a backup that never started (HTTP 204).
 * Rejects with the client error if the request fails.
 */
export async function deleteBackup(name: string): Promise<{ queued: boolean }> {
  const { status } = await client.delete(`/backups/${name}`)
  return { queued: status === 202 }
}

export async function fetchSchedules(): Promise<BackupSchedule[]> {
  const { data } = await client.get<BackupSchedule[]>('/backups/schedules')
  return data || []
}

export async function toggleSchedule(name: string, paused: boolean): Promise<void> {
  await client.put(`/backups/schedules/${name}`, { paused })
}
