// Derive Deploys-tab card states for image, git, and CI webhook deployment.

import type { BuildStatus } from '@/api/apps'

export type CardState = 'active' | 'inactive' | 'error'

/**
 * Returns active when Git is unconfigured or build status is unavailable.
 * Image deployment history can be incomplete, so only Git configuration determines the state.
 */
export function imageCardState(buildStatus: BuildStatus | null): CardState {
  if (buildStatus?.git_configured) return 'inactive'
  return 'active'
}

/**
 * Returns inactive when build status is unavailable or Git is unconfigured.
 * A configured source is active unless its latest build failed, which returns error
 * so the card shows a red badge instead of the usual Kipper green active badge.
 */
export function gitCardState(buildStatus: BuildStatus | null): CardState {
  if (!buildStatus?.git_configured) return 'inactive'
  return buildStatus.phase === 'Failed' ? 'error' : 'active'
}

/**
 * Treats Pending messages as normal progress and Discarded messages as warnings.
 * All other phases use the danger tone for their messages.
 */
export function buildMessageTone(phase: string): 'info' | 'warning' | 'danger' {
  if (phase === 'Pending') return 'info'
  if (phase === 'Discarded') return 'warning'
  return 'danger'
}
