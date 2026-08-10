package v1alpha1

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestTierEnvLimit(t *testing.T) {
	cases := map[string]int{
		TierSmall:  4,
		TierMedium: 6,
		TierLarge:  10,
		// Tierless projects have no per-environment quota, so the cap is the
		// only brake on uncapped namespace fan-out: 6 covers dev/test/acc/prod
		// plus a hotfix and a preview lane.
		"":      6,
		"bogus": 4,
	}
	for tier, want := range cases {
		if got := TierEnvLimit(tier); got != want {
			t.Errorf("TierEnvLimit(%q) = %d, want %d", tier, got, want)
		}
	}
}

func TestEffectiveEnvLimit(t *testing.T) {
	p := &Project{}
	p.Spec.Tier = TierMedium
	if got := p.EffectiveEnvLimit(); got != 6 {
		t.Errorf("medium default = %d, want 6", got)
	}

	six := 6
	p.Spec.Tier = TierSmall
	p.Spec.MaxEnvironments = &six
	if got := p.EffectiveEnvLimit(); got != 6 {
		t.Errorf("override = %d, want 6", got)
	}

	zero := 0
	p.Spec.MaxEnvironments = &zero
	if got := p.EffectiveEnvLimit(); got != 4 {
		t.Errorf("non-positive override should fall back to the tier default, got %d, want 4", got)
	}
}

// TestProjectCELMatchesTierEnvLimit guards the CEL rule in the Project CRD
// against drifting from TierEnvLimit. controller-gen writes the rule from the
// marker in project_types.go; changing the caps in Go without updating the
// marker diverges the numbers, and this catches it.
func TestProjectCELMatchesTierEnvLimit(t *testing.T) {
	data, err := os.ReadFile("../../../deploy/crds/kipper.run_projects.yaml")
	if err != nil {
		t.Fatalf("reading Project CRD: %v", err)
	}
	// The YAML single-quotes the rule and doubles inner single quotes, and
	// folds it across lines. Collapse whitespace and undo the quote doubling
	// so the comparison sees the CEL as written.
	norm := strings.ReplaceAll(strings.Join(strings.Fields(string(data)), " "), "''", "'")

	// The tierless branch is written as size(self.tier) == 0 rather than a
	// comparison with an empty string literal: gofmt's doc-comment formatter
	// turns '' inside the marker into a typographic quote, which regenerates
	// as invalid CEL.
	want := fmt.Sprintf("!has(self.tier) || size(self.tier) == 0 ? %d : (self.tier == 'large' ? %d : (self.tier == 'medium' ? %d : %d))",
		TierEnvLimit(""), TierEnvLimit(TierLarge), TierEnvLimit(TierMedium), TierEnvLimit(TierSmall))
	if !strings.Contains(norm, want) {
		t.Errorf("Project CRD CEL rule does not contain %q derived from TierEnvLimit; the XValidation marker in project_types.go is out of sync with the Go caps", want)
	}

	// The tier field must not default to small any more: an omitted tier is
	// the tierless state, and a CRD default would silently re-tier every
	// project on write.
	if strings.Contains(norm, "default: small") {
		t.Error("Project CRD still defaults spec.tier to small; tierless-by-default requires no default on the field")
	}

	// The generated CRD lags the Go marker until controller-gen reruns, so
	// also check the marker itself: it must contain the same branch and no
	// typographic quotes (an editor's smart-quote substitution once turned
	// the empty CEL string literal into a curly quote, which regenerates as
	// invalid CEL while this test kept passing against the stale CRD).
	src, err := os.ReadFile("project_types.go")
	if err != nil {
		t.Fatalf("reading project_types.go: %v", err)
	}
	if !strings.Contains(string(src), want) {
		t.Errorf("project_types.go XValidation marker does not contain %q; marker and TierEnvLimit are out of sync", want)
	}
	for _, r := range string(src) {
		if r > 127 {
			t.Errorf("project_types.go contains non-ASCII character %q; check for smart-quote substitution in the CEL marker", r)
			break
		}
	}
}
