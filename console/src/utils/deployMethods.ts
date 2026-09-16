// Derive Deploys-tab card states for image, git, and CI webhook deployment.

import type { BuildStatus } from '@/api/apps'

export type CardState = 'active' | 'inactive' | 'error'

// Image mode is active whenever git is unconfigured. Deploy history is
// incomplete for image updates, so it does not determine this state.
export function imageCardState(buildStatus: BuildStatus | null): CardState {
  if (buildStatus?.git_configured) return 'inactive'
  return 'active'
}

// Git is "active" whenever a source is configured and the latest
// build isn't in a failed state. A failed phase surfaces as the
// distinct "error" state so the card can render a red badge instead
// of the regular kipper-green active one.
export function gitCardState(buildStatus: BuildStatus | null): CardState {
  if (!buildStatus?.git_configured) return 'inactive'
  return buildStatus.phase === 'Failed' ? 'error' : 'active'
}
