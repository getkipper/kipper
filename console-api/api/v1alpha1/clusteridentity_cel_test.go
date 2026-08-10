package v1alpha1

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/getkipper/kipper/controller/pkg/hostnames"
)

// TestClusterIdentityCELMatchesGatewayLabelRule guards the CRD's domain
// XValidation rule against drifting from the gateway's registrable-label
// contract in the shared hostnames package. The CRD must forbid, at the API
// boundary, exactly what the gateway forbids at registration: the derived-route
// separator and every reserved label. If the separator or the reserved set ever
// changes, this forces the CEL marker in clusteridentity_types.go to change in
// lockstep.
func TestClusterIdentityCELMatchesGatewayLabelRule(t *testing.T) {
	src, err := os.ReadFile("clusteridentity_types.go")
	if err != nil {
		t.Fatalf("reading clusteridentity_types.go: %v", err)
	}
	source := string(src)

	// The rule bans the separator in a *.kipper.run label via contains('<sep>').
	sepWant := "contains('" + hostnames.DerivedRouteSeparator + "')"
	if !strings.Contains(source, sepWant) {
		t.Errorf("clusteridentity_types.go XValidation marker does not contain %q; the CEL rule and hostnames.DerivedRouteSeparator are out of sync", sepWant)
	}

	// The reserved-name list in the CEL rule must be exactly hostnames.ReservedLabels.
	celReserved := parseCELReservedLabels(t, source)
	if !stringSetsEqual(celReserved, reservedLabelSet()) {
		t.Errorf("CEL reserved list %v != hostnames.ReservedLabels %v; the CRD rule and the gateway reserved set are out of sync", celReserved, reservedLabelSet())
	}

	// A smart-quote substitution in the marker regenerates as invalid CEL while
	// still passing a stale-CRD comparison, so reject any non-ASCII in the source.
	for _, r := range source {
		if r > 127 {
			t.Errorf("clusteridentity_types.go contains non-ASCII character %q; check for smart-quote substitution in the CEL marker", r)
			break
		}
	}

	// The generated CRD must carry the same rule. The YAML single-quotes the CEL
	// and doubles inner single quotes, and folds it across lines; collapse
	// whitespace and undo the doubling so the comparison sees the CEL as written.
	data, err := os.ReadFile("../../../deploy/crds/kipper.run_clusteridentities.yaml")
	if err != nil {
		t.Fatalf("reading ClusterIdentity CRD: %v", err)
	}
	norm := strings.ReplaceAll(strings.Join(strings.Fields(string(data)), " "), "''", "'")
	if !strings.Contains(norm, sepWant) {
		t.Errorf("ClusterIdentity CRD CEL rule does not contain %q; regenerate the CRDs after editing the XValidation marker", sepWant)
	}
	for label := range hostnames.ReservedLabels {
		lit := "'" + label + ".kipper.run'"
		if !strings.Contains(norm, lit) {
			t.Errorf("generated ClusterIdentity CRD is missing reserved literal %s; regenerate the CRDs", lit)
		}
	}
}

// parseCELReservedLabels extracts the labels from the `self.domain in [...]`
// list literal in the CEL marker: each entry is '<label>.kipper.run'.
func parseCELReservedLabels(t *testing.T, source string) map[string]bool {
	t.Helper()
	const anchor = "self.domain in ["
	i := strings.Index(source, anchor)
	if i < 0 {
		t.Fatalf("CEL marker has no %q list; the reserved-name rule is missing", anchor)
	}
	rest := source[i+len(anchor):]
	end := strings.Index(rest, "]")
	if end < 0 {
		t.Fatalf("CEL reserved list is not closed with ]")
	}
	out := map[string]bool{}
	for _, part := range strings.Split(rest[:end], ",") {
		host := strings.Trim(strings.TrimSpace(part), "'")
		label := strings.TrimSuffix(host, ".kipper.run")
		if label == "" || label == host {
			t.Fatalf("reserved list entry %q is not a <label>.kipper.run literal", part)
		}
		out[label] = true
	}
	return out
}

func reservedLabelSet() map[string]bool {
	out := map[string]bool{}
	for k := range hostnames.ReservedLabels {
		out[k] = true
	}
	return out
}

func stringSetsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	ka, kb := make([]string, 0, len(a)), make([]string, 0, len(b))
	for k := range a {
		ka = append(ka, k)
	}
	for k := range b {
		kb = append(kb, k)
	}
	sort.Strings(ka)
	sort.Strings(kb)
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}
