package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/middleware"
)

// A member may hold a role this build does not know: written with kubectl,
// restored from a backup, or carried in by a migration from a cluster that had
// it. All three are real and none is a mistake by whoever is reading the
// console.
//
// What must not happen is the console showing it as though it were a role,
// because the reader would then believe that person holds something. They hold
// nothing: the projection binds only the roles it knows, so an unrecognised
// name grants no access anywhere.
const unrecognisedRole kipperv1.ProjectMemberRole = "acme.support"

func ownerAccess() middleware.ProjectAccess {
	return middleware.ProjectAccess{Project: "blog", Role: middleware.ProjectRoleOwner}
}

func TestAMemberHoldingAnUnrecognisedRoleIsReportedAsHoldingNoAccess(t *testing.T) {
	client := fake.NewClientset(usersConfigMap(`{"lead@test.com":"deployer"}`))
	proj := blogProject(
		kipperv1.ProjectMember{Email: "lead@test.com", Role: kipperv1.ProjectRoleOwner},
		kipperv1.ProjectMember{Email: "stranger@test.com", Role: unrecognisedRole},
	)
	h := &Members{CRClient: testCRClient(proj), RoleStore: middleware.NewRoleStore(client)}

	req := httptest.NewRequest("GET", "/api/v1/projects/blog/members", nil)
	req = req.WithContext(middleware.WithProjectAccess(req.Context(), ownerAccess()))
	rec := httptest.NewRecorder()
	membersRouter(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("listing members = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got []memberEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the member list: %v", err)
	}

	byEmail := map[string]memberEntry{}
	for _, m := range got {
		byEmail[m.Email] = m
	}

	stranger, listed := byEmail["stranger@test.com"]
	if !listed {
		t.Fatal("the member holding an unrecognised role is missing from the list, so nobody can see they are there to remove them")
	}
	if stranger.Role != string(unrecognisedRole) {
		t.Errorf("the list reports the role as %q, and it must say what the object says so an operator can find it", stranger.Role)
	}
	if !stranger.Unrecognised {
		t.Error("the member holding an unrecognised role is not marked unrecognised, so the console presents it as a role that grants something")
	}

	if byEmail["lead@test.com"].Unrecognised {
		t.Error("a member holding a built-in role is marked unrecognised")
	}
}

// Removing a member has to work whatever they hold. A build that refused to
// revoke a role it could not interpret would strand the one member an operator
// most wants gone, and not knowing what they hold is often the reason.
func TestAMemberHoldingAnUnrecognisedRoleCanBeRemoved(t *testing.T) {
	client := fake.NewClientset(usersConfigMap(`{"lead@test.com":"deployer"}`))
	proj := blogProject(
		kipperv1.ProjectMember{Email: "lead@test.com", Role: kipperv1.ProjectRoleOwner},
		kipperv1.ProjectMember{Email: "stranger@test.com", Role: unrecognisedRole},
	)
	cr := testCRClient(proj)
	h := &Members{CRClient: cr, RoleStore: middleware.NewRoleStore(client)}

	req := httptest.NewRequest("DELETE", "/api/v1/projects/blog/members/stranger@test.com", nil)
	req = req.WithContext(middleware.WithProjectAccess(req.Context(), ownerAccess()))
	rec := httptest.NewRecorder()
	membersRouter(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("removing the member = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var p kipperv1.Project
	if err := cr.Get(context.Background(), crclient.ObjectKey{Name: "blog"}, &p); err != nil {
		t.Fatalf("reading the project back: %v", err)
	}
	for _, m := range p.Spec.Members {
		if m.Email == "stranger@test.com" {
			t.Error("the member holding an unrecognised role is still listed after being removed")
		}
	}
}

// An unrecognised role must not count as an owner. The last-owner guard keeps a
// project from being orphaned, and a role nobody can interpret is not evidence
// that somebody can still administer it.
func TestAnUnrecognisedRoleDoesNotCountAsAnOwner(t *testing.T) {
	client := fake.NewClientset(usersConfigMap(`{"lead@test.com":"deployer"}`))
	proj := blogProject(
		kipperv1.ProjectMember{Email: "lead@test.com", Role: kipperv1.ProjectRoleOwner},
		kipperv1.ProjectMember{Email: "stranger@test.com", Role: unrecognisedRole},
	)
	h := &Members{CRClient: testCRClient(proj), RoleStore: middleware.NewRoleStore(client)}

	req := httptest.NewRequest("DELETE", "/api/v1/projects/blog/members/lead@test.com", nil)
	req = req.WithContext(middleware.WithProjectAccess(req.Context(), ownerAccess()))
	rec := httptest.NewRecorder()
	membersRouter(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("removing the only owner = %d, want 400: an unrecognised role was counted as an owner, which would let the project be orphaned", rec.Code)
	}
}

// Changing one member rewrites the whole member list, so a role the build does
// not understand has to survive the trip. Dropping it would silently revoke
// somebody the operator was not touching, and coercing it to a known role would
// grant them something nobody chose.
func TestAMembershipWritePreservesAnUnrecognisedRole(t *testing.T) {
	client := fake.NewClientset(usersConfigMap(`{"lead@test.com":"deployer","new@test.com":"deployer"}`))
	proj := blogProject(
		kipperv1.ProjectMember{Email: "lead@test.com", Role: kipperv1.ProjectRoleOwner},
		kipperv1.ProjectMember{Email: "stranger@test.com", Role: unrecognisedRole},
	)
	cr := testCRClient(proj)
	h := &Members{CRClient: cr, RoleStore: middleware.NewRoleStore(client)}

	body := `{"email":"new@test.com","role":"viewer"}`
	req := httptest.NewRequest("PUT", "/api/v1/projects/blog/members", strings.NewReader(body))
	req = req.WithContext(middleware.WithProjectAccess(req.Context(), ownerAccess()))
	rec := httptest.NewRecorder()
	membersRouter(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("adding a member = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var p kipperv1.Project
	if err := cr.Get(context.Background(), crclient.ObjectKey{Name: "blog"}, &p); err != nil {
		t.Fatalf("reading the project back: %v", err)
	}
	for _, m := range p.Spec.Members {
		if m.Email != "stranger@test.com" {
			continue
		}
		if m.Role != unrecognisedRole {
			t.Errorf("the unrecognised role became %q after an unrelated membership write", m.Role)
		}
		return
	}
	t.Error("the member holding an unrecognised role was dropped by a write that had nothing to do with them")
}
