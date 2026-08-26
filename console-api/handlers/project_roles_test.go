package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The console's role dropdown is built from this. It used to be a literal in
// the browser, so a cluster that gained a role could not offer it and a console
// older than its cluster would have offered one that no longer existed.
func TestProjectRolesListsTheBuiltInsWithWhatTheyMayDo(t *testing.T) {
	rec := httptest.NewRecorder()
	ProjectRoles(rec, httptest.NewRequest(http.MethodGet, "/api/v1/project-roles", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []projectRoleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d roles, want the three built-ins", len(got))
	}
	// Ordered least to most, which is the order somebody picking one reads.
	want := []string{"viewer", "deployer", "owner"}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("role %d is %q, want %q", i, got[i].Name, w)
		}
		if len(got[i].Capabilities) == 0 {
			t.Errorf("%s is listed with nothing it may do", w)
		}
	}
	if len(got[2].Capabilities) <= len(got[0].Capabilities) {
		t.Error("an owner is not listed as being able to do more than a viewer")
	}
}
