package handlers

import (
	"os"
	"strings"
	"testing"
)

// A scope the CRD's enum does not list is refused by admission, so the change
// applies, the response says "updated", and the adjustment never reaches the
// audit trail. Record logs that now rather than discarding it, but a log is not
// the audit trail and nobody reads one that nothing warned them about. This is
// what stops the loss happening at all, and nothing else can: the fake client
// in these tests enforces no schema.
func TestEveryAdjustmentScopeIsInTheCRDEnum(t *testing.T) {
	allowed := scopeEnumFromCRD(t)

	// Every scope any handler passes to Adjustments.Record.
	for _, scope := range []string{
		"platform", // platform.go, the shared components
		"service",  // job_resources.go, a service StatefulSet
		adjustmentScope(ResourceKindApp),
		adjustmentScope(ResourceKindFunction),
		adjustmentScope(ResourceKindJob),
	} {
		if !allowed[scope] {
			t.Errorf("handlers record scope %q, which the ResourceAdjustment CRD does not allow; add it to the enum and regenerate all three copies", scope)
		}
	}
}

// scopeEnumFromCRD reads the committed schema rather than the Go marker, because
// the marker only matters once controller-gen has been run and the three copies
// are what a cluster actually admits against.
func scopeEnumFromCRD(t *testing.T) map[string]bool {
	t.Helper()
	const path = "../../deploy/crds/kipper.run_resourceadjustments.yaml"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	lines := strings.Split(string(raw), "\n")
	scope := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "scope:" {
			scope = i
			break
		}
	}
	if scope < 0 {
		t.Fatalf("no scope property in %s", path)
	}

	allowed := map[string]bool{}
	for _, line := range lines[scope:] {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "- ") {
			allowed[strings.TrimPrefix(t, "- ")] = true
			continue
		}
		if len(allowed) > 0 {
			break
		}
	}
	if len(allowed) == 0 {
		t.Fatalf("no enum under scope in %s", path)
	}
	return allowed
}
