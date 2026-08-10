package installer

// Component images this kip build pins. `kip upgrade` reconciles the running
// deployments onto these, so a cluster that was moved onto a hand-built or
// sideloaded tag during an incident comes back to the released image instead
// of silently staying behind.
//
// The manifest templates carry the same strings literally, because they are
// positional Sprintf templates; TestPinnedImagesMatchManifests keeps the two
// in step.
const (
	ConsoleAPIImage = "ghcr.io/getkipper/kipper-console-api:latest"
	ConsoleImage    = "ghcr.io/getkipper/kipper-console:latest"
	AuthzImage      = "ghcr.io/getkipper/kipper-authz:latest"
)

// PinnedImage returns the image this kip build pins for a console component,
// or "" when the component has no pinned image.
func PinnedImage(component string) string {
	switch component {
	case "console-api":
		return ConsoleAPIImage
	case "console":
		return ConsoleImage
	case "kipper-authz":
		return AuthzImage
	default:
		return ""
	}
}
