// Card-state derivation for the Deploys tab's "Deploy methods" section.
// Pure functions so they can be unit-tested without mounting AppDetail.
//
// Each app has three independent deploy mechanisms — pre-built image
// push, in-cluster git build, and CI webhook. The cards in the UI
// reflect which one is currently driving the app and which are still
// available to configure.

import type { BuildStatus } from '@/api/apps'

export type CardState = 'active' | 'inactive' | 'error'

// "active" means image-mode is the way this app receives versions.
// Any non-git app is image-mode by construction — kip requires either
// --image or --git at create time, so the only way !git_configured is
// also "no image" is a CR in an invalid intermediate state we don't
// surface here. When git is configured the image is built automatically
// so the card becomes informational (inactive in the configurable sense).
//
// NOTE: deploy history length is NOT used to derive this state, because
// UpdateImage (console-api/handlers/apps.go) mutates spec.image without
// writing a history entry. Deriving "active" from history made fresh
// `kip deploy --image`-d apps look unconfigured. The proper fix is for
// the backend to record image deploys as status fields.
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
