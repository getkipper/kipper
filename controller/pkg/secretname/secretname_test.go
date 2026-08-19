package secretname

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// An App, a Function and a Job may all be called "api" in one namespace. Before
// the kind was part of these names all three resolved to the same Secret, so
// two controllers authored it in turn and the third read whichever write landed
// last. These Secrets carry resolved service credentials, so that is one
// workload reading another's database password.
func TestDerivedNamesAreDisjointAcrossKinds(t *testing.T) {
	kinds := []Kind{KindApp, KindFunction, KindJob}

	for _, build := range []struct {
		what string
		name func(Kind) string
	}{
		{"env", func(k Kind) string { return Env(k, "api") }},
		{"secrets", func(k Kind) string { return Secrets(k, "api") }},
		{"binding", func(k Kind) string { return Binding("db", k, "api") }},
	} {
		seen := make(map[string]Kind, len(kinds))
		for _, k := range kinds {
			got := build.name(k)
			if other, clash := seen[got]; clash {
				t.Errorf("%s: %s and %s both derive %q", build.what, other, k, got)
			}
			seen[got] = k
		}
	}
}

func TestNames(t *testing.T) {
	tests := []struct {
		got  string
		want string
	}{
		{Env(KindApp, "api"), "app-api-env"},
		{Env(KindFunction, "api"), "function-api-env"},
		{Env(KindJob, "migrate"), "job-migrate-env"},
		{Secrets(KindApp, "api"), "app-api-secrets"},
		{Secrets(KindFunction, "api"), "function-api-secrets"},
		{Binding("db", KindApp, "api"), "db-app-api-credentials"},
		{Binding("db", KindFunction, "api"), "db-function-api-credentials"},
		{ServiceCredentials("db"), "db-credentials"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// A workload name that happens to start with another kind's prefix must not
// collide with that kind's Secret.
func TestKindPrefixIsNotAmbiguous(t *testing.T) {
	if a, b := Env(KindApp, "function-api"), Env(KindFunction, "api"); a == b {
		t.Errorf("App %q collides with Function %q", a, b)
	}
}

// The name is the pair. A token used for another host has to be another object,
// or the two could be mistaken for one another by anything that reads a name.
func TestGitCredentialNamesThePairRatherThanTheToken(t *testing.T) {
	sameToken := GitCredentialDigest("a-token", "git.example.com")
	otherHost := GitCredentialDigest("a-token", "git.elsewhere.example.com")
	otherToken := GitCredentialDigest("a-different-token", "git.example.com")

	assert.NotEqual(t, sameToken, otherHost, "the same token for another host produced the same name")
	assert.NotEqual(t, sameToken, otherToken, "a different token produced the same name")
	assert.Equal(t, sameToken, GitCredentialDigest("a-token", "git.example.com"),
		"the same pair produced a different name, so rotating twice would churn")
}

// The separator matters: without it, host "a" with token "bc" and host "ab"
// with token "c" are the same input, and one pair could be written under the
// other's name.
func TestGitCredentialDigestCannotBeConfusedAcrossTheBoundary(t *testing.T) {
	assert.NotEqual(t, GitCredentialDigest("bc", "a"), GitCredentialDigest("c", "ab"))
}

// A generation name has to be recognisable as this app's own credential, and
// the legacy name has to stay recognisable too: three clusters reference it.
func TestGitCredentialNamesAreRecognisable(t *testing.T) {
	name := GitCredential("checkout", GitCredentialDigest("a-token", "git.example.com"))

	assert.True(t, strings.HasPrefix(name, GitCredentialPrefix("checkout")))
	assert.Equal(t, "checkout-git-credentials", LegacyGitCredential("checkout"))
	assert.NotEqual(t, LegacyGitCredential("checkout"), name,
		"a generation must not collide with the name it replaces")
	assert.LessOrEqual(t, len(name), 253, "a Secret name is capped at 253 characters")
}

// The predicate decides whether a token may be read for an app at all, so it
// has to separate this app's credential from every other object in a namespace
// where names are global and any member can create one.
func TestIsGitCredentialOfSeparatesThisAppsOwnCredential(t *testing.T) {
	digest := GitCredentialDigest("a-token", "git.example.com")

	assert.True(t, IsGitCredentialOf("web", LegacyGitCredential("web")), "the legacy name is still referenced by live clusters")
	assert.True(t, IsGitCredentialOf("web", GitCredential("web", digest)))

	assert.False(t, IsGitCredentialOf("web", "corp-git"), "a shared credential is not the app's own")
	assert.False(t, IsGitCredentialOf("web", LegacyGitCredential("checkout")), "another app's credential")
	assert.False(t, IsGitCredentialOf("web", GitCredential("checkout", digest)))

	// An app may legally be named so that its own credential sits under another
	// app's prefix. Only a digest completes a generation name, so the longer
	// app's credential is not claimable by the shorter one.
	victim := GitCredential("web-git-credentials-x", digest)
	assert.True(t, IsGitCredentialOf("web-git-credentials-x", victim))
	assert.False(t, IsGitCredentialOf("web", victim),
		"one app claimed another app's credential through the shared prefix")
}
