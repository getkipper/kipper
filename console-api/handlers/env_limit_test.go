package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func envs(names ...string) []kipperv1.ProjectEnvironment {
	out := make([]kipperv1.ProjectEnvironment, 0, len(names))
	for _, n := range names {
		out = append(out, kipperv1.ProjectEnvironment{Name: n})
	}
	return out
}

func TestCheckEnvironmentLimit(t *testing.T) {
	proj := func(tier string, max *int) *kipperv1.Project {
		return &kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "shop"},
			Spec:       kipperv1.ProjectSpec{Tier: tier, MaxEnvironments: max},
		}
	}
	five := 5
	tests := []struct {
		name     string
		project  *kipperv1.Project
		proposed int
		current  int
		wantErr  bool
	}{
		{"small within limit", proj("small", nil), 4, 0, false},
		{"small over limit on create", proj("small", nil), 5, 0, true},
		{"medium within limit", proj("medium", nil), 6, 0, false},
		{"large within limit", proj("large", nil), 10, 0, false},
		{"large over limit", proj("large", nil), 11, 0, true},
		{"tierless within limit", proj("", nil), 6, 0, false},
		{"tierless over limit", proj("", nil), 7, 0, true},
		{"override raises the limit", proj("small", &five), 5, 0, false},
		{"reduction on an over-limit project is allowed", proj("small", nil), 5, 6, false},
		{"same count on an over-limit project is allowed", proj("small", nil), 6, 6, false},
		{"growth on an over-limit project is rejected", proj("small", nil), 7, 6, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkEnvironmentLimit(tt.project, tt.proposed, tt.current)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkEnvironmentLimit(%s, proposed=%d, current=%d) err=%v wantErr=%v",
					tt.project.Spec.Tier, tt.proposed, tt.current, err, tt.wantErr)
			}
		})
	}
}

func TestProjectsHandler_CreateRejectsOverEnvLimit(t *testing.T) {
	// A tierless project (no tier in the request) gets the tierless limit of
	// 6: five environments fit, seven do not. An explicit small tier keeps
	// its cap of 4.
	cases := []struct {
		name string
		body string
		want int
	}{
		{"tierless within limit", `{"name":"webapp","environments":["e1","e2","e3","e4","e5"]}`, http.StatusCreated},
		{"tierless over limit", `{"name":"webapp2","environments":["e1","e2","e3","e4","e5","e6","e7"]}`, http.StatusUnprocessableEntity},
		{"small tier over limit", `{"name":"webapp3","tier":"small","environments":["e1","e2","e3","e4","e5"]}`, http.StatusUnprocessableEntity},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			handler := &Projects{Client: fake.NewClientset(), CRClient: testCRClient()}
			r := chi.NewRouter()
			r.Post("/api/v1/projects", handler.Create)

			req := httptest.NewRequest("POST", "/api/v1/projects", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("expected %d, got %d; body %s", tt.want, rec.Code, rec.Body.String())
			}
		})
	}
}

// The wholesale PUT rewrites the environment list and is the bypass a cap on
// AddEnvironment alone would miss.
func TestProjectsHandler_UpdateRejectsOverEnvLimit(t *testing.T) {
	existing := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec:       kipperv1.ProjectSpec{Tier: "small", Environments: envs("test")},
	}
	handler := &Projects{Client: fake.NewClientset(), CRClient: testCRClient(existing)}
	r := chi.NewRouter()
	r.Put("/api/v1/projects/{name}", handler.Update)

	body := `{"environments":["e1","e2","e3","e4","e5"]}`
	req := httptest.NewRequest("PUT", "/api/v1/projects/shop", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 on the wholesale-update bypass, got %d; body %s", rec.Code, rec.Body.String())
	}
}

func TestProjectsHandler_UpdateAllowsReductionOnOverLimit(t *testing.T) {
	existing := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec:       kipperv1.ProjectSpec{Tier: "small", Environments: envs("e1", "e2", "e3", "e4", "e5", "e6")},
	}
	handler := &Projects{Client: fake.NewClientset(), CRClient: testCRClient(existing)}
	r := chi.NewRouter()
	r.Put("/api/v1/projects/{name}", handler.Update)

	// Six down to five: still over the small limit of four, but a reduction, so
	// it must go through to let an over-limit project be worked back down.
	body := `{"environments":["e1","e2","e3","e4","e5"]}`
	req := httptest.NewRequest("PUT", "/api/v1/projects/shop", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for a reduction on an over-limit project, got %d; body %s", rec.Code, rec.Body.String())
	}
}

func TestProjectsHandler_AddEnvironmentRejectsAtLimit(t *testing.T) {
	existing := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec:       kipperv1.ProjectSpec{Tier: "small", Environments: envs("e1", "e2", "e3", "e4")},
	}
	handler := &Projects{Client: fake.NewClientset(), CRClient: testCRClient(existing)}
	r := chi.NewRouter()
	r.Post("/api/v1/projects/{name}/environments", handler.AddEnvironment)

	req := httptest.NewRequest("POST", "/api/v1/projects/shop/environments", strings.NewReader(`{"name":"e5"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 at the environment limit, got %d; body %s", rec.Code, rec.Body.String())
	}
}

func TestQuotaHandler_DowngradeBelowEnvCount(t *testing.T) {
	existing := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec:       kipperv1.ProjectSpec{Tier: "medium", Environments: envs("e1", "e2", "e3", "e4", "e5")},
	}
	h := &Quota{Client: fake.NewClientset(), CRClient: testCRClient(existing)}
	r := chi.NewRouter()
	r.Put("/api/v1/projects/{name}/quota", h.Set)

	// medium (limit 6) → small (limit 4) with five environments: blocked.
	req := httptest.NewRequest("PUT", "/api/v1/projects/shop/quota", strings.NewReader(`{"tier":"small"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("expected 409 for a downgrade below the environment count, got %d; body %s", rec.Code, rec.Body.String())
	}

	// Same change with force applies.
	req2 := httptest.NewRequest("PUT", "/api/v1/projects/shop/quota", strings.NewReader(`{"tier":"small","force":true}`))
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("expected 200 with force, got %d; body %s", rec2.Code, rec2.Body.String())
	}
}

func TestQuotaHandler_SetMaxEnvironments(t *testing.T) {
	existing := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec:       kipperv1.ProjectSpec{Tier: "small", Environments: envs("e1")},
	}
	h := &Quota{Client: fake.NewClientset(), CRClient: testCRClient(existing)}
	r := chi.NewRouter()
	r.Put("/api/v1/projects/{name}/quota", h.Set)

	req := httptest.NewRequest("PUT", "/api/v1/projects/shop/quota", strings.NewReader(`{"max_environments":6}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 setting max_environments, got %d; body %s", rec.Code, rec.Body.String())
	}

	var p kipperv1.Project
	if err := h.CRClient.Get(context.Background(), crclient.ObjectKey{Name: "shop"}, &p); err != nil {
		t.Fatal(err)
	}
	if p.Spec.MaxEnvironments == nil || *p.Spec.MaxEnvironments != 6 {
		t.Errorf("max_environments not persisted: %v", p.Spec.MaxEnvironments)
	}

	// Clearing with 0 falls back to the tier default.
	req2 := httptest.NewRequest("PUT", "/api/v1/projects/shop/quota", strings.NewReader(`{"max_environments":0}`))
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 clearing max_environments, got %d; body %s", rec2.Code, rec2.Body.String())
	}
	if err := h.CRClient.Get(context.Background(), crclient.ObjectKey{Name: "shop"}, &p); err != nil {
		t.Fatal(err)
	}
	if p.Spec.MaxEnvironments != nil {
		t.Errorf("max_environments should be cleared, got %v", *p.Spec.MaxEnvironments)
	}
}
