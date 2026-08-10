package controllers

import (
	"strings"
	"testing"
)

// firstPartyImagePrefix is the only registry path Kipper's own images are
// published to. Duplicated from the installer package, which this module cannot
// import.
const firstPartyImagePrefix = "ghcr.io/getkipper/"

// These references are compiled into console-api, so a cluster cannot be patched
// off a wrong one — the reconciler writes it back on the next pass, and only a
// rebuilt image fixes it. A GitHub organisation rename does not redirect
// container-registry package paths, so the wrong organisation here is a 403 on
// every function pod the cluster starts.
func TestFunctionRuntimeImagesArePublishedUnderTheKipperOrg(t *testing.T) {
	refs := map[string]string{
		"node runtime":   runtimeImage("node"),
		"python runtime": runtimeImage("python"),
		"poller sidecar": kipperPollImage,
	}
	for name, ref := range refs {
		if ref == "" {
			t.Errorf("%s resolved to an empty reference", name)
			continue
		}
		if !strings.HasPrefix(ref, firstPartyImagePrefix) {
			t.Errorf("%s is %q, which is not published under %s", name, ref, firstPartyImagePrefix)
		}
	}

	if got := runtimeImage("ruby"); got != "" {
		t.Errorf("an unsupported runtime must resolve to no image, got %q", got)
	}
}
