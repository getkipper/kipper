package installer

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestAdmissionPoliciesMatchDeploy ensures deploy/policies and the installer's
// embedded policies contain the same files with identical contents.
func TestAdmissionPoliciesMatchDeploy(t *testing.T) {
	deployDir := filepath.Join("..", "..", "..", "deploy", "policies")
	deployed, err := os.ReadDir(deployDir)
	if err != nil {
		t.Fatalf("Cannot read %s: %v", deployDir, err)
	}
	embedded, err := admissionPolicies.ReadDir("policies")
	if err != nil {
		t.Fatalf("Cannot read the embedded policies: %v", err)
	}
	if len(deployed) != len(embedded) {
		t.Fatalf("%s has %d files and the installer embeds %d. Keep both copies in sync.", deployDir, len(deployed), len(embedded))
	}
	for _, e := range embedded {
		want, err := os.ReadFile(filepath.Join(deployDir, e.Name()))
		if err != nil {
			t.Fatalf("Cannot read the reference copy of embedded policy %s in %s: %v", e.Name(), deployDir, err)
		}
		got, err := admissionPolicies.ReadFile("policies/" + e.Name())
		if err != nil {
			t.Fatalf("Cannot read the embedded %s: %v", e.Name(), err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("The embedded %s differs from %s. Update both copies to match.", e.Name(), deployDir)
		}
	}
}
