package installer

import (
	"strings"
	"testing"
)

// The wiring the stamping tests cannot reach. If the CLI ever stops supplying a
// version, installs would silently write unstamped CRDs and every later older
// kip would walk over them. This refuses before touching the host.
func TestRunRefusesToInstallWithoutAKipVersion(t *testing.T) {
	_, err := Run(Options{Host: "203.0.113.7", Domain: "example.com"})
	if err == nil {
		t.Fatal("an install with no version must be refused, not written unstamped")
	}
	if !strings.Contains(err.Error(), "kip version") {
		t.Errorf("the refusal must name the cause, got: %v", err)
	}
}
