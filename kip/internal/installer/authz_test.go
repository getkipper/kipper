package installer

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAuthzManifestMatchesDeploy guards against the installer's embedded
// kipper-authz manifest drifting from deploy/authz.yaml. The two are separate
// copies (the installer can't embed a file outside its own module), so an RBAC
// or probe change made in one must be mirrored in the other or installer- and
// kustomize-provisioned clusters diverge silently.
func TestAuthzManifestMatchesDeploy(t *testing.T) {
	// installer dir -> repo root is three levels up (kip/internal/installer).
	deployPath := filepath.Join("..", "..", "..", "deploy", "authz.yaml")
	want, err := os.ReadFile(deployPath)
	if err != nil {
		t.Fatalf("reading %s: %v", deployPath, err)
	}
	if authzManifest != string(want) {
		t.Errorf("authzManifest has drifted from %s; update both copies to match", deployPath)
	}
}
