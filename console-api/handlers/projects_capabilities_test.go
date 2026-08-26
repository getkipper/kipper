package handlers

import (
	"encoding/json"
	"testing"

	"github.com/getkipper/kipper/controller/pkg/capability"
)

// The console gates its own controls on what the caller may do. It used to
// decide that from the role name, with its own copy of the ladder; now it is
// told, which is the only way a role this build does not enumerate can ever
// render correctly.
func TestProjectResponseCarriesTheCallersCapabilities(t *testing.T) {
	for _, tt := range []struct {
		role     string
		holds    capability.Name
		notHolds capability.Name
	}{
		{"viewer", "project.read", "kipper.write"},
		{"deployer", "kipper.write", "members.manage"},
		{"owner", "members.manage", ""},
	} {
		got := capabilitiesFor(tt.role)
		if !contains(got, string(tt.holds)) {
			t.Errorf("a %s is not told they hold %s", tt.role, tt.holds)
		}
		if tt.notHolds != "" && contains(got, string(tt.notHolds)) {
			t.Errorf("a %s is told they hold %s", tt.role, tt.notHolds)
		}
	}
}

// A role this build does not know holds nothing, and the console renders that
// as no access. Sending null instead of an empty list would make the console
// have to tell "no capabilities" from "the server did not say".
func TestAnUnknownRoleIsToldItHoldsNothing(t *testing.T) {
	got := capabilitiesFor("auditor")
	if len(got) != 0 {
		t.Errorf("an unknown role was told it holds %v", got)
	}
	encoded, err := json.Marshal(projectResponse{Capabilities: got})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	if !containsSub(string(encoded), `"capabilities":[]`) {
		t.Errorf("an empty capability set did not encode as an empty list: %s", encoded)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func containsSub(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
