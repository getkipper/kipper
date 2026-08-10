package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/kip/internal/installer"
)

func TestComparableVersionAcceptsOnlyVersionsThatCanBeOrdered(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		ordered bool
	}{
		{"release tag", "v0.9.0", "v0.9.0", true},
		{"release tag without the v", "0.9.0", "v0.9.0", true},
		{"genuine prerelease", "v0.9.0-rc.1", "v0.9.0-rc.1", true},
		{"source build", "dev", "", false},
		{"bare commit hash", "abc1234", "", false},
		{"empty", "", "", false},
		{"not a version at all", "banana", "", false},

		// The trap this helper exists for. `git describe` renders a commit
		// after v0.8.3 as a prerelease of it, which semver orders BEFORE the
		// tag — so ordering these would refuse upgrades that move forward.
		{"commit after a tag", "v0.8.3-5-gabc1234", "", false},
		{"commit after a tag, dirty tree", "v0.8.3-5-gabc1234-dirty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ordered := installer.ComparableVersion(tc.in)
			if ordered != tc.ordered {
				t.Fatalf("installer.ComparableVersion(%q) orderable = %v, want %v", tc.in, ordered, tc.ordered)
			}
			if ordered && got != tc.want {
				t.Errorf("installer.ComparableVersion(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestClusterIsNewerThanOnlyRefusesWhenItCanProveIt(t *testing.T) {
	cases := []struct {
		name    string
		stamp   string
		running string
		newer   bool
	}{
		{"older kip against a newer cluster", "v0.10.0", "v0.9.0", true},
		{"older kip by a patch", "v0.9.1", "v0.9.0", true},
		{"newer kip against an older cluster", "v0.9.0", "v0.10.0", false},
		{"same version", "v0.9.0", "v0.9.0", false},
		{"release beats its own prerelease", "v0.9.0", "v0.9.0-rc.1", true},

		// A cluster written before stamping existed, and the cases where
		// ordering would be a guess. All allow: the guard says nothing rather
		// than refusing on something it cannot establish.
		{"no stamp at all", "", "v0.9.0", false},
		{"stamp is a source build", "dev", "v0.9.0", false},
		{"running is a source build", "v0.10.0", "dev", false},
		{"running is a commit after a tag", "v0.10.0", "v0.9.0-5-gabc1234", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, newer := installer.ClusterIsNewerThan(tc.stamp, tc.running); newer != tc.newer {
				t.Errorf("installer.ClusterIsNewerThan(%q, %q) = %v, want %v", tc.stamp, tc.running, newer, tc.newer)
			}
		})
	}
}

// Describing a commit after a tag that already carries a prerelease appends to
// it, and semver then orders the result ABOVE the next real prerelease because
// a non-numeric identifier outranks a numeric one. A cluster stamped by such a
// build refused the very release it was built towards.
func TestComparableVersionRejectsAGitDescribeSuffixOnAPrereleaseTag(t *testing.T) {
	for _, v := range []string{
		"v0.9.0-rc.1-5-gabc1234",
		"v0.9.0-rc.1-5-gabc1234-dirty",
		"v0.9.0-beta.2-11-gdeadbee",
		"v0.9.0-dirty",
		"v0.9.0-rc.1-dirty",
	} {
		if _, ok := installer.ComparableVersion(v); ok {
			t.Errorf("installer.ComparableVersion(%q) must not be ordered; semver misplaces it", v)
		}
	}
	// A genuine prerelease still orders, or the guard would never apply during
	// a release candidate series.
	for _, v := range []string{"v0.9.0-rc.1", "v0.9.0-rc.2", "v0.9.0"} {
		if _, ok := installer.ComparableVersion(v); !ok {
			t.Errorf("installer.ComparableVersion(%q) must be ordered", v)
		}
	}
}

// Two spellings of one version must not read as two versions.
func TestComparableVersionNormalisesEquivalentSpellings(t *testing.T) {
	cases := [][2]string{
		{"v0.9", "v0.9.0"},
		{"v0.9.0+build.7", "v0.9.0"},
		{"0.9.0", "v0.9.0"},
	}
	for _, c := range cases {
		got, ok := installer.ComparableVersion(c[0])
		if !ok {
			t.Fatalf("installer.ComparableVersion(%q) must be ordered", c[0])
		}
		want, _ := installer.ComparableVersion(c[1])
		if got != want {
			t.Errorf("installer.ComparableVersion(%q) = %q, want the same as %q (%q)", c[0], got, c[1], want)
		}
	}
	// And the pair that started it: rc.2 must be newer than rc.1, both ways.
	if _, _, newer := installer.ClusterIsNewerThan("v0.9.0-rc.1", "v0.9.0-rc.2"); newer {
		t.Error("rc.2 must not be refused as older than rc.1")
	}
	if _, _, newer := installer.ClusterIsNewerThan("v0.9.0-rc.2", "v0.9.0-rc.1"); !newer {
		t.Error("rc.1 must be refused against a cluster written by rc.2")
	}
}

func TestCarryOverMetadataKeepsClusterOwnedKeysAndLetsTheManifestWin(t *testing.T) {
	existing := &unstructured.Unstructured{Object: map[string]any{}}
	existing.SetAnnotations(map[string]string{
		"argocd.argoproj.io/sync-options":   "Prune=false",
		"kipper.run/written-by-kip-version": "v0.9.0",
	})
	existing.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "Helm"})

	incoming := &unstructured.Unstructured{Object: map[string]any{}}
	incoming.SetAnnotations(map[string]string{"controller-gen.kubebuilder.io/version": "v0.19.0"})

	carryOverMetadata(existing, incoming)

	ann := incoming.GetAnnotations()
	assert.Equal(t, "Prune=false", ann["argocd.argoproj.io/sync-options"],
		"a cluster-owned annotation must survive the replace")
	assert.Equal(t, "v0.19.0", ann["controller-gen.kubebuilder.io/version"],
		"and the embedded manifest's own keys must still be applied")
	assert.Equal(t, "Helm", incoming.GetLabels()["app.kubernetes.io/managed-by"],
		"labels are carried the same way")
}
