package installer

import (
	"regexp"
	"testing"
)

// FirstPartyImagePrefix is the only registry path Kipper's own images are
// published to. It is duplicated in console-api, which ships its own first-party
// references and cannot import this package.
const FirstPartyImagePrefix = "ghcr.io/getkipper/"

// A GitHub organisation rename does not redirect container-registry package
// paths, so every first-party reference under the wrong organisation is a 403 at
// pull time. Checking that the constants and the manifests agree with each other
// does not catch this: renaming the organisation consistently across the tree
// leaves them agreeing perfectly on an org that serves nothing. This is the
// assertion that fails when that happens.
func TestEveryFirstPartyImageIsPublishedUnderTheKipperOrg(t *testing.T) {
	// Third-party images the manifests also carry (traefik, dex, zot) are not
	// ours to publish, so only ghcr.io/*/kipper-* references are in scope.
	ref := regexp.MustCompile(`ghcr\.io/[A-Za-z0-9._-]+/kipper-[A-Za-z0-9._-]+`)

	// Every source here carries at least one first-party reference. A source
	// that stops carrying one has moved rather than become correct, which is
	// why each is checked individually: a single total would let one source
	// empty out while another's count covered for it.
	sources := map[string]string{
		"consoleManifestTemplate": consoleManifestTemplate,
		"authzManifest":           authzManifest,
		"ConsoleAPIImage":         ConsoleAPIImage,
		"ConsoleImage":            ConsoleImage,
		"AuthzImage":              AuthzImage,
	}
	for name, src := range sources {
		found := ref.FindAllString(src, -1)
		if len(found) == 0 {
			t.Errorf("%s carries no first-party reference; this test is no longer reading where they live", name)
			continue
		}
		for _, got := range found {
			if !hasPrefix(got, FirstPartyImagePrefix) {
				t.Errorf("%s carries %q, which is not published under %s", name, got, FirstPartyImagePrefix)
			}
		}
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// The pinned images and the manifest templates are two copies of the same
// string, so a change to one without the other would make `kip upgrade` roll a
// cluster onto a different image than a fresh install renders.
func TestPinnedImagesMatchManifests(t *testing.T) {
	cases := []struct {
		component string
		image     string
		manifest  string
	}{
		{"console-api", ConsoleAPIImage, consoleManifestTemplate},
		{"console", ConsoleImage, consoleManifestTemplate},
		{"kipper-authz", AuthzImage, authzManifest},
	}
	for _, tc := range cases {
		t.Run(tc.component, func(t *testing.T) {
			if !contains(tc.manifest, "image: "+tc.image) {
				t.Errorf("manifest for %s does not carry the pinned image %q", tc.component, tc.image)
			}
			if got := PinnedImage(tc.component); got != tc.image {
				t.Errorf("PinnedImage(%q) = %q, want %q", tc.component, got, tc.image)
			}
		})
	}

	if PinnedImage("traefik") != "" {
		t.Error("PinnedImage must return empty for a component it does not pin")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func TestPinnedImageAt(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct{ component, want string }{
		{"console-api", "ghcr.io/getkipper/kipper-console-api:" + sha},
		{"console", "ghcr.io/getkipper/kipper-console:" + sha},
		{"kipper-authz", "ghcr.io/getkipper/kipper-authz:" + sha},
		{"traefik", ""},
	} {
		if got := PinnedImageAt(tc.component, sha); got != tc.want {
			t.Errorf("PinnedImageAt(%q) = %q, want %q", tc.component, got, tc.want)
		}
	}
	if got := PinnedImageAt("console-api", ""); got != ConsoleAPIImage {
		t.Errorf("without a tag the released image applies, got %q", got)
	}
}

func TestValidateImageTag(t *testing.T) {
	if err := ValidateImageTag("0123456789abcdef0123456789abcdef01234567"); err != nil {
		t.Errorf("a full commit sha is valid: %v", err)
	}
	for _, bad := range []string{"", "latest", "v0.23.0", "0123456", "0123456789ABCDEF0123456789ABCDEF01234567", "0123456789abcdef0123456789abcdef0123456g"} {
		if ValidateImageTag(bad) == nil {
			t.Errorf("ValidateImageTag(%q) must refuse anything but a full lowercase commit sha", bad)
		}
	}
}
