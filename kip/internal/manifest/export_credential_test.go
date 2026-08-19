package manifest

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// The app's own credential is machine-managed state now: it is one object per
// token-and-host pair, so rotating the token moves the app onto a new name and
// the sweep collects the old one. A manifest that pinned the name would name a
// deleted object after the first rotation, and applying it would move the CR
// onto that dead name and let the sweep take the working credential too.
//
// The live image is carried the same way and for the same reason: build output,
// not manifest state.
func TestExportOmitsTheAppsOwnGitCredential(t *testing.T) {
	own := secretname.GitCredential("web", secretname.GitCredentialDigest("a-token", "github.com"))

	git := exportAppGit(map[string]interface{}{"git": map[string]interface{}{
		"url": "https://github.com/acme/web.git", "credentialsSecret": own,
	}}, "web")

	assert.Equal(t, "https://github.com/acme/web.git", git.URL)
	assert.Empty(t, git.CredentialsSecret,
		"a manifest that pins a rotating name breaks the app on the first rotation")
}

// A shared credential is the operator's choice and belongs in the manifest: it
// is named in cluster settings, several apps reference it, and nothing rotates
// it out from under them.
func TestExportKeepsASharedGitCredential(t *testing.T) {
	git := exportAppGit(map[string]interface{}{"git": map[string]interface{}{
		"url": "https://github.com/acme/web.git", "credentialsSecret": "corp-git",
	}}, "web")

	assert.Equal(t, "corp-git", git.CredentialsSecret,
		"the operator's choice of shared credential was dropped from the manifest")
}

// The legacy name is the app's own credential under the name it had before
// generations, so it is machine-managed too and its first rotation moves off it.
func TestExportOmitsTheLegacyGitCredential(t *testing.T) {
	git := exportAppGit(map[string]interface{}{"git": map[string]interface{}{
		"url": "https://github.com/acme/web.git", "credentialsSecret": secretname.LegacyGitCredential("web"),
	}}, "web")

	assert.Empty(t, git.CredentialsSecret)
}
