import type { App } from '@/api/types'

// Show rollout progress separately from the health of the current pods.
export function rolloutLabel(app: Pick<App, 'rollout_reason'>): string | null {
  if (!app.rollout_reason) return null
  return app.rollout_reason === 'InProgress' ? 'Rolling out' : 'Rollout waiting'
}
