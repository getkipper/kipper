package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/middleware"
)

func usersConfigMap(usersJSON string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-users", Namespace: "kipper-system"},
		Data:       map[string]string{"users": usersJSON},
	}
}

func membersRouter(h *Members) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{name}/members", h.List)
	r.Put("/api/v1/projects/{name}/members", h.Set)
	r.Delete("/api/v1/projects/{name}/members/{email}", h.Remove)
	return r
}

func blogProject(members ...kipperv1.ProjectMember) *kipperv1.Project {
	return &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "blog"},
		Spec:       kipperv1.ProjectSpec{Members: members},
	}
}

func TestMembers_Set(t *testing.T) {
	t.Run("adds a known user", func(t *testing.T) {
		client := fake.NewClientset(usersConfigMap(`{"lead@test.com":"admin","dev@test.com":"viewer"}`))
		proj := blogProject(kipperv1.ProjectMember{Email: "lead@test.com", Role: kipperv1.ProjectRoleOwner})
		h := &Members{CRClient: testCRClient(proj), RoleStore: middleware.NewRoleStore(client)}

		req := httptest.NewRequest("PUT", "/api/v1/projects/blog/members", strings.NewReader(`{"email":"dev@test.com","role":"deployer"}`))
		rec := httptest.NewRecorder()
		membersRouter(h).ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("rejects a user with no account", func(t *testing.T) {
		client := fake.NewClientset(usersConfigMap(`{"lead@test.com":"admin"}`))
		proj := blogProject(kipperv1.ProjectMember{Email: "lead@test.com", Role: kipperv1.ProjectRoleOwner})
		h := &Members{CRClient: testCRClient(proj), RoleStore: middleware.NewRoleStore(client)}

		req := httptest.NewRequest("PUT", "/api/v1/projects/blog/members", strings.NewReader(`{"email":"ghost@test.com","role":"viewer"}`))
		rec := httptest.NewRecorder()
		membersRouter(h).ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for unknown user, got %d", rec.Code)
		}
	})

	t.Run("refuses to demote the last owner", func(t *testing.T) {
		client := fake.NewClientset(usersConfigMap(`{"lead@test.com":"admin"}`))
		proj := blogProject(kipperv1.ProjectMember{Email: "lead@test.com", Role: kipperv1.ProjectRoleOwner})
		h := &Members{CRClient: testCRClient(proj), RoleStore: middleware.NewRoleStore(client)}

		req := httptest.NewRequest("PUT", "/api/v1/projects/blog/members", strings.NewReader(`{"email":"lead@test.com","role":"viewer"}`))
		rec := httptest.NewRecorder()
		membersRouter(h).ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 when demoting last owner, got %d", rec.Code)
		}
	})

	t.Run("rejects an invalid role", func(t *testing.T) {
		client := fake.NewClientset(usersConfigMap(`{"lead@test.com":"admin","dev@test.com":"viewer"}`))
		proj := blogProject(kipperv1.ProjectMember{Email: "lead@test.com", Role: kipperv1.ProjectRoleOwner})
		h := &Members{CRClient: testCRClient(proj), RoleStore: middleware.NewRoleStore(client)}

		req := httptest.NewRequest("PUT", "/api/v1/projects/blog/members", strings.NewReader(`{"email":"dev@test.com","role":"superuser"}`))
		rec := httptest.NewRecorder()
		membersRouter(h).ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalid role, got %d", rec.Code)
		}
	})
}

func TestMembers_Remove(t *testing.T) {
	t.Run("removes a member", func(t *testing.T) {
		client := fake.NewClientset(usersConfigMap(`{"lead@test.com":"admin","dev@test.com":"viewer"}`))
		proj := blogProject(
			kipperv1.ProjectMember{Email: "lead@test.com", Role: kipperv1.ProjectRoleOwner},
			kipperv1.ProjectMember{Email: "dev@test.com", Role: kipperv1.ProjectRoleDeployer},
		)
		h := &Members{CRClient: testCRClient(proj), RoleStore: middleware.NewRoleStore(client)}

		req := httptest.NewRequest("DELETE", "/api/v1/projects/blog/members/dev@test.com", nil)
		rec := httptest.NewRecorder()
		membersRouter(h).ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("refuses to remove the last owner", func(t *testing.T) {
		client := fake.NewClientset(usersConfigMap(`{"lead@test.com":"admin"}`))
		proj := blogProject(kipperv1.ProjectMember{Email: "lead@test.com", Role: kipperv1.ProjectRoleOwner})
		h := &Members{CRClient: testCRClient(proj), RoleStore: middleware.NewRoleStore(client)}

		req := httptest.NewRequest("DELETE", "/api/v1/projects/blog/members/lead@test.com", nil)
		rec := httptest.NewRecorder()
		membersRouter(h).ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 when removing last owner, got %d", rec.Code)
		}
	})

	// The guard counts owners by their role in the list and cannot know whether
	// an address belongs to anyone, so a mistyped owner counts: the real owner
	// can be removed against it and the project is left owned by somebody who
	// will never sign in.
	//
	// Adding a replacement owner and removing the bad one recovers that with no
	// exemption at all, and ends with the project owned — the rule only ever
	// refused one order of doing it. This exemption is for removing the phantom
	// before a replacement has been chosen.
	t.Run("a cluster admin may remove the last owner, to recover a stranded project", func(t *testing.T) {
		client := fake.NewClientset(usersConfigMap(`{"boss@test.com":"admin"}`))
		proj := blogProject(kipperv1.ProjectMember{Email: "typo@test.com", Role: kipperv1.ProjectRoleOwner})
		h := &Members{CRClient: testCRClient(proj), RoleStore: middleware.NewRoleStore(client)}

		req := httptest.NewRequest("DELETE", "/api/v1/projects/blog/members/typo@test.com", nil)
		req = req.WithContext(middleware.WithProjectAccess(req.Context(), middleware.ProjectAccess{Project: "blog", Role: middleware.ProjectRoleOwner, IsAdmin: true}))
		rec := httptest.NewRecorder()
		membersRouter(h).ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("a cluster admin must be able to remove a stranded last owner, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	// The escape is for cluster admins, not for the project's own owners — a
	// project owner removing the last owner is the accident the guard is for.
	t.Run("a project owner still may not remove the last owner", func(t *testing.T) {
		client := fake.NewClientset(usersConfigMap(`{"lead@test.com":"deployer"}`))
		proj := blogProject(kipperv1.ProjectMember{Email: "lead@test.com", Role: kipperv1.ProjectRoleOwner})
		h := &Members{CRClient: testCRClient(proj), RoleStore: middleware.NewRoleStore(client)}

		req := httptest.NewRequest("DELETE", "/api/v1/projects/blog/members/lead@test.com", nil)
		req = req.WithContext(middleware.WithProjectAccess(req.Context(), middleware.ProjectAccess{Project: "blog", Role: middleware.ProjectRoleOwner, IsAdmin: false}))
		rec := httptest.NewRecorder()
		membersRouter(h).ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for a non-admin removing the last owner, got %d", rec.Code)
		}
	})
}

// The console sends encodeURIComponent(email), so every real removal arrives
// with `@` as `%40`. chi routes on RawPath when one is set and hands the raw
// segment through, so the handler compared an encoded string against the
// decoded address on the CR, matched nothing, wrote the list back unchanged and
// answered 200 "removed". Every test above builds the path with a literal `@`,
// which leaves RawPath empty — green against a shape the only client never sends.
func TestMembers_Remove_HandlesThePercentEncodedAddressTheConsoleSends(t *testing.T) {
	client := fake.NewClientset(usersConfigMap(`{"lead@test.com":"admin","dev@test.com":"viewer"}`))
	proj := blogProject(
		kipperv1.ProjectMember{Email: "lead@test.com", Role: kipperv1.ProjectRoleOwner},
		kipperv1.ProjectMember{Email: "dev@test.com", Role: kipperv1.ProjectRoleDeployer},
	)
	cr := testCRClient(proj)
	h := &Members{CRClient: cr, RoleStore: middleware.NewRoleStore(client)}

	req := httptest.NewRequest("DELETE", "/api/v1/projects/blog/members/dev%40test.com", nil)
	rec := httptest.NewRecorder()
	membersRouter(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var after kipperv1.Project
	if err := cr.Get(context.Background(), crclient.ObjectKey{Name: "blog"}, &after); err != nil {
		t.Fatalf("reading the project back: %v", err)
	}
	for _, mem := range after.Spec.Members {
		if mem.Email == "dev@test.com" {
			t.Fatalf("reported removed but is still a member: %+v", after.Spec.Members)
		}
	}
}

// Reporting success for a member who was never there is what let the encoding
// bug stay invisible: the console believed every removal it asked for.
func TestMembers_Remove_SaysSoWhenTheAddressIsNotAMember(t *testing.T) {
	client := fake.NewClientset(usersConfigMap(`{"lead@test.com":"admin"}`))
	proj := blogProject(kipperv1.ProjectMember{Email: "lead@test.com", Role: kipperv1.ProjectRoleOwner})
	h := &Members{CRClient: testCRClient(proj), RoleStore: middleware.NewRoleStore(client)}

	req := httptest.NewRequest("DELETE", "/api/v1/projects/blog/members/stranger%40test.com", nil)
	rec := httptest.NewRecorder()
	membersRouter(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a non-member, got %d: %s", rec.Code, rec.Body.String())
	}
}

// A malformed escape is refused rather than half-used: PathUnescape returns a
// partial string alongside its error, and that string is not an address.
//
// net/http rejects a request URI containing an invalid escape before it reaches
// a router, so this cannot arrive over the wire — httptest.NewRequest panics on
// it for the same reason. The branch is defence rather than a live path, and the
// request is built by hand to drive it rather than pretending otherwise.
func TestMembers_Remove_RefusesAMalformedAddress(t *testing.T) {
	client := fake.NewClientset(usersConfigMap(`{"lead@test.com":"admin"}`))
	proj := blogProject(kipperv1.ProjectMember{Email: "lead@test.com", Role: kipperv1.ProjectRoleOwner})
	h := &Members{CRClient: testCRClient(proj), RoleStore: middleware.NewRoleStore(client)}

	req := httptest.NewRequest("DELETE", "/api/v1/projects/blog/members/dev%40test.com", nil)
	req.URL.RawPath = "/api/v1/projects/blog/members/dev%zz"
	rec := httptest.NewRecorder()
	membersRouter(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a malformed address, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMembers_List(t *testing.T) {
	client := fake.NewClientset(usersConfigMap(`{"lead@test.com":"admin"}`))
	proj := blogProject(
		kipperv1.ProjectMember{Email: "lead@test.com", Role: kipperv1.ProjectRoleOwner},
		kipperv1.ProjectMember{Email: "dev@test.com", Role: kipperv1.ProjectRoleViewer},
	)
	h := &Members{CRClient: testCRClient(proj), RoleStore: middleware.NewRoleStore(client)}

	req := httptest.NewRequest("GET", "/api/v1/projects/blog/members", nil)
	rec := httptest.NewRecorder()
	membersRouter(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var members []memberEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &members); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}
}
