package installer

import (
	"fmt"
	"regexp"
	"strings"
)

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

// commitSHA matches a full lowercase git commit hash, the only tag a test
// image is published under.
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ValidateImageTag accepts only a full commit sha, so a test image names the
// commit it was built from and cannot be mistaken for a release tag.
func ValidateImageTag(tag string) error {
	if !commitSHA.MatchString(tag) {
		return fmt.Errorf("--image-tag must be a full 40-character commit sha, got %q", tag)
	}
	return nil
}

// PinnedImageAt returns the pinned image for a console component at the given
// tag, or the released image when the tag is empty. It returns "" for a
// component this kip build does not pin.
func PinnedImageAt(component, tag string) string {
	pinned := PinnedImage(component)
	if pinned == "" || tag == "" {
		return pinned
	}
	return strings.TrimSuffix(pinned, ":latest") + ":" + tag
}
