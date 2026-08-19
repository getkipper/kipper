package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

// The carve-out lived only where apply writes, so the
// preflight and `kip diff` still saw the credential as a field the manifest
// takes away. The round trip the docs describe — export, which omits it, then
// apply — was refused on every app with a token, and the refusal told the
// operator to pin the name that rotates, which is the break this whole change
// exists to close.
func TestPreservedPathsCoversTheCredentialTheClusterOwns(t *testing.T) {
	ownLive := map[string]interface{}{"git": map[string]interface{}{
		"credentialsSecret": secretname.GitCredential("web", secretname.GitCredentialDigest("live", "github.com")),
	}}
	gitApp := func(credential string, named bool) map[string]interface{} {
		git := map[string]interface{}{"url": "https://github.com/acme/web.git"}
		if named {
			git["credentialsSecret"] = credential
		}
		return map[string]interface{}{"git": git}
	}

	assert.Contains(t, preservedPaths("web", gitApp("", false), ownLive), "git.credentialsSecret",
		"an exported manifest omits the credential, so apply must not offer to clear it")
	assert.Contains(t, preservedPaths("web", gitApp(secretname.LegacyGitCredential("web"), true), ownLive), "git.credentialsSecret",
		"a manifest exported before this change pins the app's own name and must not be applied verbatim")
	assert.Contains(t, preservedPaths("web", gitApp(secretname.GitCredential("web", secretname.GitCredentialDigest("t", "h")), true), ownLive), "git.credentialsSecret")

	assert.NotContains(t, preservedPaths("web", gitApp("corp-git", true), ownLive), "git.credentialsSecret",
		"a shared credential is the operator's choice and stays declarative")
	assert.NotContains(t, preservedPaths("web", gitApp(secretname.LegacyGitCredential("other"), true), ownLive), "git.credentialsSecret",
		"another app's credential is not this app's machine state")

	assert.Nil(t, preservedPaths("web", map[string]interface{}{"image": "nginx"}, ownLive),
		"an image app has no git block to preserve anything from")
}

// The decision has to come from the manifest as written. A
// retry re-runs over the same map, so reading it back sees what the previous
// attempt injected and pins a credential the cluster has since moved off.
func TestManifestGitCredentialReadsTheManifestRatherThanTheInjectedValue(t *testing.T) {
	object := map[string]interface{}{"spec": map[string]interface{}{
		"git": map[string]interface{}{"url": "https://github.com/acme/web.git"},
	}}

	value, named := manifestGitCredential(object)
	assert.False(t, named)
	assert.Empty(t, value)

	// What an attempt injects. The next attempt must still decide from the
	// manifest, which named nothing.
	object["spec"].(map[string]interface{})["git"].(map[string]interface{})["credentialsSecret"] = "web-git-credentials-abc"
	_, present := effectiveGitCredential("web", value, named, secretname.LegacyGitCredential("web"))
	assert.True(t, present,
		"a retry read back its own injection and stopped carrying the live credential forward")
}

// The rule read the manifest only, so dropping a shared
// credential from a manifest silently kept it: no diff, no refusal, and --force
// could not clear it either, because the carry-forward runs before the force
// check. That closed the only route off a shared credential and contradicted
// the docs this change edits.
// The live value decides alongside the manifest. A name the app owns is not an
// assertion either way: it is machine state that a manifest exported before
// credentials were named this way happens to carry.
func TestPreservedPathsWeighsTheLiveCredentialToo(t *testing.T) {
	own := secretname.GitCredential("web", secretname.GitCredentialDigest("t", "github.com"))
	legacy := secretname.LegacyGitCredential("web")

	spec := func(credential string, named bool) map[string]interface{} {
		git := map[string]interface{}{"url": "https://github.com/acme/web.git"}
		if named {
			git["credentialsSecret"] = credential
		}
		return map[string]interface{}{"git": git}
	}
	live := func(credential string) map[string]interface{} {
		git := map[string]interface{}{"url": "https://github.com/acme/web.git"}
		if credential != "" {
			git["credentialsSecret"] = credential
		}
		return map[string]interface{}{"git": git}
	}

	for _, tc := range []struct {
		name          string
		manifest      map[string]interface{}
		liveSpec      map[string]interface{}
		wantPreserved bool
		why           string
	}{
		{"omitted over the app's own", spec("", false), live(own), true,
			"an exported manifest omits it, so the round trip must not be refused"},
		{"omitted over the legacy name", spec("", false), live(legacy), true,
			"a cluster that has not rotated yet still round-trips"},
		{"omitted over nothing", spec("", false), live(""), true,
			"there is nothing to clear"},
		{"stale app-owned name over the app's own", spec(legacy, true), live(own), true,
			"a name the app owns is machine state, not an assertion"},
		{"omitted over a shared credential", spec("", false), live("corp-git"), false,
			"dropping a shared credential from the manifest is a real clear and must be reported"},
		{"shared credential asserted", spec("corp-git", true), live(own), false,
			"the operator's choice of shared credential is declarative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := preservedPaths("web", tc.manifest, tc.liveSpec)
			if tc.wantPreserved {
				assert.Contains(t, got, "git.credentialsSecret", tc.why)
			} else {
				assert.NotContains(t, got, "git.credentialsSecret", tc.why)
			}
		})
	}
}

// Manifest names an app-owned credential, live is a shared
// one. The rule says an app-owned name is not an assertion, so this must come
// out as the operator dropping the shared credential — reported and refused.
// Fusing the two decisions into one boolean left it as a plain modify instead,
// which apply does not gate, so a manifest exported before this change moved an
// app off the operator's shared credential onto a collected name in silence.
func TestEffectiveGitCredential_DecidesEveryCell(t *testing.T) {
	own := secretname.GitCredential("web", secretname.GitCredentialDigest("t", "github.com"))
	legacy := secretname.LegacyGitCredential("web")

	for _, tc := range []struct {
		name        string
		manifest    string
		named       bool
		live        string
		wantValue   string
		wantPresent bool
	}{
		{"omitted over the app's own", "", false, own, own, true},
		{"omitted over the legacy name", "", false, legacy, legacy, true},
		{"omitted over nothing", "", false, "", "", false},
		{"omitted over a shared credential", "", false, "corp-git", "", false},
		{"stale app-owned over the app's own", legacy, true, own, own, true},
		{"stale app-owned over a shared credential", legacy, true, "corp-git", "", false},
		{"shared asserted over the app's own", "corp-git", true, own, "corp-git", true},
		{"another app's name is an assertion", secretname.LegacyGitCredential("other"), true, own, secretname.LegacyGitCredential("other"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, present := effectiveGitCredential("web", tc.manifest, tc.named, tc.live)
			assert.Equal(t, tc.wantPresent, present)
			assert.Equal(t, tc.wantValue, value)
		})
	}
}

// The decision inputs were captured before the retry loop,
// but its output was not: the closure mutates the manifest map in place, so an
// attempt that injected the live credential left it there, and a retry taking
// the other branch wrote that stale name over whatever the cluster had moved
// to. Each attempt has to start from the manifest as written.
func TestApplyGitCredential_EachAttemptStartsFromTheManifest(t *testing.T) {
	git := map[string]interface{}{"url": "https://github.com/acme/web.git"}
	manifestValue, manifestNamed := "", false

	// Attempt one: live is the app's own, so it is carried in.
	applyGitCredential(git, "web", manifestValue, manifestNamed,
		secretname.GitCredential("web", secretname.GitCredentialDigest("t", "github.com")))
	assert.NotEmpty(t, git["credentialsSecret"])

	// Attempt two, after a conflict: the cluster has moved to a shared
	// credential, which the manifest does not assert, so the field goes.
	applyGitCredential(git, "web", manifestValue, manifestNamed, "corp-git")
	_, present := git["credentialsSecret"]
	assert.False(t, present,
		"a retry wrote the previous attempt's injection over the credential the cluster had moved to")
}

// The scan diffed the manifest as written while the write
// diffed it as it would be applied, so one cell disagreed: a manifest naming an
// app-owned credential over a live shared one showed in `kip diff` and
// `--dry-run` as a modify the write never performs, the preflight passed, and a
// directory apply started writing before stopping part-way on a clear that no
// re-run would ever show. The scan diffs the effective spec now.
func TestScanDiffsTheSpecThatWillBeApplied(t *testing.T) {
	// A manifest exported before credentials were named this way.
	written := map[string]interface{}{"git": map[string]interface{}{
		"url": "https://github.com/acme/web.git", "credentialsSecret": secretname.LegacyGitCredential("web"),
	}}

	effective := effectiveSpecForDiff("web", written)

	git, _ := effective["git"].(map[string]interface{})
	_, present := git["credentialsSecret"]
	assert.False(t, present,
		"the scan still diffs a name the write strips, so it reports a modify the apply never makes")

	// And the manifest itself is untouched, because the write reads it after.
	original, _ := written["git"].(map[string]interface{})
	assert.Equal(t, secretname.LegacyGitCredential("web"), original["credentialsSecret"],
		"the scan mutated the manifest the write path reads")
}

// The create branch passed the manifest straight to
// the apiserver, so it was the one surface still honouring a name every other
// surface rules is not an assertion. Applying a manifest exported before this
// change into a fresh environment made an App naming a Secret that is not
// there, and in the source namespace one the sweep has collected.
// Driven through applyResource rather than the helper, because the helper being
// right says nothing about the create path calling it.
func TestApplyResourceCreateDropsACredentialTheManifestDoesNotAssert(t *testing.T) {
	for _, tc := range []struct {
		name    string
		secret  string
		wantOut interface{}
	}{
		{"the app's own name is machine state", secretname.LegacyGitCredential("web"), nil},
		{"a shared credential is the operator's assertion", "corp-git", "corp-git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dyn := dynamicfake.NewSimpleDynamicClient(applyCollisionScheme())
			res := manifest.Resource{GVR: manifest.AppGVR, Object: &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "kipper.run/v1alpha1", "kind": "App",
				"metadata": map[string]interface{}{"name": "web", "namespace": "shop-prod"},
				"spec": map[string]interface{}{"git": map[string]interface{}{
					"url": "https://github.com/acme/web.git", "credentialsSecret": tc.secret,
				}},
			}}}

			outcome, err := applyResource(context.Background(), dyn, "shop-prod", res, false, nil, false)
			require.NoError(t, err)
			require.Equal(t, "created", outcome)

			live, getErr := dyn.Resource(manifest.AppGVR).Namespace("shop-prod").
				Get(context.Background(), "web", metav1.GetOptions{})
			require.NoError(t, getErr)
			got, _, _ := unstructured.NestedFieldNoCopy(live.Object, "spec", "git", "credentialsSecret")
			assert.Equal(t, tc.wantOut, got,
				"the created app names a credential the manifest does not assert")

			// The manifest is untouched, because the update path reads it after.
			original, _, _ := unstructured.NestedString(res.Object.Object, "spec", "git", "credentialsSecret")
			assert.Equal(t, tc.secret, original, "the create mutated the manifest")
		})
	}
}
