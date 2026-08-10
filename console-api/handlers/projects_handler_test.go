package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
	"github.com/getkipper/kipper/console-api/middleware"
)

// asUser returns req with an authenticated user and global role in context,
// mimicking what the auth and role middleware inject in production.
func asUser(req *http.Request, email, role string) *http.Request {
	ctx := context.WithValue(req.Context(), middleware.UserContextKey, &middleware.Claims{Email: email})
	ctx = context.WithValue(ctx, middleware.RoleContextKey, role)
	return req.WithContext(ctx)
}

func newKipperNamespace(name, project, env, order string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				kipperLabel:              kipperValue,
				"kipper.run/project":     project,
				"kipper.run/environment": env,
				"kipper.run/env-order":   order,
			},
		},
		Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	}
}

func TestProjectsHandler_List(t *testing.T) {
	t.Run("returns empty list when no projects exist", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Projects{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Get("/api/v1/projects", handler.List)

		req := httptest.NewRequest("GET", "/api/v1/projects", nil)
		req = asUser(req, "admin@test.com", middleware.RoleAdmin)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rec.Code)
		}

		var projects []projectResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &projects); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(projects) != 0 {
			t.Errorf("expected 0 projects, got %d", len(projects))
		}
	})

	t.Run("returns projects from CRs", func(t *testing.T) {
		myapp := &kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "myapp"},
			Spec: kipperv1.ProjectSpec{
				Environments: []kipperv1.ProjectEnvironment{
					{Name: "staging"},
					{Name: "production"},
				},
			},
		}
		other := &kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "other"},
			Spec: kipperv1.ProjectSpec{
				Environments: []kipperv1.ProjectEnvironment{
					{Name: "staging"},
				},
			},
		}
		client := fake.NewClientset()
		handler := &Projects{Client: client, CRClient: testCRClient(myapp, other)}

		r := chi.NewRouter()
		r.Get("/api/v1/projects", handler.List)

		req := httptest.NewRequest("GET", "/api/v1/projects", nil)
		req = asUser(req, "admin@test.com", middleware.RoleAdmin)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rec.Code)
		}

		var projects []projectResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &projects); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(projects) != 2 {
			t.Fatalf("expected 2 projects, got %d", len(projects))
		}
	})

	t.Run("non-admin sees only projects they are a member of", func(t *testing.T) {
		mine := &kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "mine"},
			Spec: kipperv1.ProjectSpec{
				Environments: []kipperv1.ProjectEnvironment{{Name: "default"}},
				Members:      []kipperv1.ProjectMember{{Email: "dev@test.com", Role: kipperv1.ProjectRoleDeployer}},
			},
		}
		theirs := &kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "theirs"},
			Spec: kipperv1.ProjectSpec{
				Environments: []kipperv1.ProjectEnvironment{{Name: "default"}},
				Members:      []kipperv1.ProjectMember{{Email: "other@test.com", Role: kipperv1.ProjectRoleOwner}},
			},
		}
		client := fake.NewClientset()
		handler := &Projects{Client: client, CRClient: testCRClient(mine, theirs)}

		r := chi.NewRouter()
		r.Get("/api/v1/projects", handler.List)

		req := asUser(httptest.NewRequest("GET", "/api/v1/projects", nil), "dev@test.com", middleware.RoleDeployer)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		var projects []projectResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &projects); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(projects) != 1 || projects[0].Name != "mine" {
			t.Fatalf("expected only project 'mine', got %+v", projects)
		}
	})

	t.Run("preserves environment order from spec", func(t *testing.T) {
		proj := &kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "myapp"},
			Spec: kipperv1.ProjectSpec{
				Environments: []kipperv1.ProjectEnvironment{
					{Name: "staging"},
					{Name: "preview"},
					{Name: "production"},
				},
			},
		}
		client := fake.NewClientset()
		handler := &Projects{Client: client, CRClient: testCRClient(proj)}

		r := chi.NewRouter()
		r.Get("/api/v1/projects", handler.List)

		req := httptest.NewRequest("GET", "/api/v1/projects", nil)
		req = asUser(req, "admin@test.com", middleware.RoleAdmin)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		var projects []projectResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &projects); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		for _, p := range projects {
			if p.Name == "myapp" && len(p.Environments) == 3 {
				if p.Environments[0].Name != "staging" {
					t.Errorf("expected first environment 'staging', got %q", p.Environments[0].Name)
				}
				if p.Environments[1].Name != "preview" {
					t.Errorf("expected second environment 'preview', got %q", p.Environments[1].Name)
				}
				if p.Environments[2].Name != "production" {
					t.Errorf("expected third environment 'production', got %q", p.Environments[2].Name)
				}
			}
		}
	})

	t.Run("includes app summaries per environment", func(t *testing.T) {
		proj := &kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "staging"},
			Spec: kipperv1.ProjectSpec{
				Environments: []kipperv1.ProjectEnvironment{
					{Name: "default"},
				},
			},
		}
		webApp := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec:       kipperv1.AppSpec{Image: "nginx:1.25", Replicas: int32Ptr(2)},
			Status:     kipperv1.AppStatus{Phase: "Running", ReadyReplicas: 2},
		}
		client := fake.NewClientset()
		handler := &Projects{Client: client, CRClient: testCRClient(proj, webApp)}

		r := chi.NewRouter()
		r.Get("/api/v1/projects", handler.List)

		req := httptest.NewRequest("GET", "/api/v1/projects", nil)
		req = asUser(req, "admin@test.com", middleware.RoleAdmin)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		var projects []projectResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &projects); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		for _, p := range projects {
			for _, env := range p.Environments {
				if env.Namespace == "staging" && len(env.Apps) > 0 {
					found := false
					for _, app := range env.Apps {
						if app.Name == "web" {
							found = true
							if app.Image != "nginx:1.25" {
								t.Errorf("expected image 'nginx:1.25', got %q", app.Image)
							}
						}
					}
					if !found {
						t.Error("expected app 'web' in staging environment")
					}
				}
			}
		}
	})
}

func TestAppPublicURL(t *testing.T) {
	cases := []struct {
		name   string
		route  *kipperv1.AppRoute
		domain string
		env    string
		want   string
	}{
		{"no route", nil, "acme.dev", "prod", ""},
		{"explicit host with empty path", &kipperv1.AppRoute{Host: "api.acme.dev"}, "acme.dev", "prod", "https://api.acme.dev"},
		{"explicit host with root path", &kipperv1.AppRoute{Host: "api.acme.dev", Path: "/"}, "acme.dev", "prod", "https://api.acme.dev"},
		{"explicit host with path", &kipperv1.AppRoute{Host: "acme.dev", Path: "/api"}, "acme.dev", "prod", "https://acme.dev/api"},
		{"implicit host derives from domain and environment", &kipperv1.AppRoute{}, "acme.dev", "prod", "https://shop-prod.acme.dev"},
		{"implicit host without environment", &kipperv1.AppRoute{}, "acme.dev", "", "https://shop.acme.dev"},
		{"implicit host with path", &kipperv1.AppRoute{Path: "/api"}, "acme.dev", "prod", "https://shop-prod.acme.dev/api"},
		{"implicit host without cluster domain", &kipperv1.AppRoute{}, "", "prod", ""},
		{"implicit host on a kipper.run domain uses the double-dash form", &kipperv1.AppRoute{}, "203-0-113-10.kipper.run", "prod", "https://shop-prod--203-0-113-10.kipper.run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Projects{Domain: tc.domain}
			app := &kipperv1.App{
				ObjectMeta: metav1.ObjectMeta{Name: "shop"},
				Spec:       kipperv1.AppSpec{Route: tc.route},
			}
			if got := p.appPublicURL(app, tc.env); got != tc.want {
				t.Errorf("appPublicURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProjectsHandler_Create(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		expectedStatus int
		expectedErr    string
	}{
		{
			name:           "creates project with default environment",
			body:           `{"name":"myapp"}`,
			expectedStatus: http.StatusCreated,
		},
		{
			name:           "creates project with multiple environments",
			body:           `{"name":"webapp","environments":["staging","production"]}`,
			expectedStatus: http.StatusCreated,
		},
		{
			name:           "rejects missing name",
			body:           `{"environments":["staging"]}`,
			expectedStatus: http.StatusBadRequest,
			expectedErr:    "name is required",
		},
		{
			name:           "rejects invalid JSON",
			body:           `{{{`,
			expectedStatus: http.StatusBadRequest,
			expectedErr:    "invalid request body",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset()
			handler := &Projects{Client: client, CRClient: testCRClient()}

			r := chi.NewRouter()
			r.Post("/api/v1/projects", handler.Create)

			req := httptest.NewRequest("POST", "/api/v1/projects", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d; body: %s", tt.expectedStatus, rec.Code, rec.Body.String())
			}

			if tt.expectedErr != "" {
				var errResp map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
					t.Fatalf("failed to decode error response: %v", err)
				}
				if errResp["error"] != tt.expectedErr {
					t.Errorf("expected error %q, got %q", tt.expectedErr, errResp["error"])
				}
			}
		})
	}
}

func TestProjectsHandler_CreateConflict(t *testing.T) {
	existing := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name: "myapp",
			Labels: map[string]string{
				kipperLabel: kipperValue,
			},
		},
		Spec: kipperv1.ProjectSpec{
			Environments: []kipperv1.ProjectEnvironment{{Name: "default"}},
		},
	}
	client := fake.NewClientset()
	handler := &Projects{Client: client, CRClient: testCRClient(existing)}

	r := chi.NewRouter()
	r.Post("/api/v1/projects", handler.Create)

	req := httptest.NewRequest("POST", "/api/v1/projects", strings.NewReader(`{"name":"myapp"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("expected status %d, got %d; body: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}
}

func TestProjectsHandler_CreateMultiEnvNamespaces(t *testing.T) {
	client := fake.NewClientset()
	handler := &Projects{Client: client, CRClient: testCRClient()}

	r := chi.NewRouter()
	r.Post("/api/v1/projects", handler.Create)

	body := `{"name":"webapp","environments":["staging","production"]}`
	req := httptest.NewRequest("POST", "/api/v1/projects", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	namespaces, ok := resp["namespaces"].([]interface{})
	if !ok {
		t.Fatal("expected namespaces in response")
	}
	if len(namespaces) != 2 {
		t.Errorf("expected 2 namespaces, got %d", len(namespaces))
	}

	expected := map[string]bool{"webapp-staging": false, "webapp-production": false}
	for _, ns := range namespaces {
		expected[ns.(string)] = true
	}
	for name, found := range expected {
		if !found {
			t.Errorf("expected namespace %q in response", name)
		}
	}
}

func TestProjectsHandler_CopyPreview(t *testing.T) {
	apps := []crclient.Object{
		&kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo-test"},
			Spec: kipperv1.AppSpec{
				Image:     "nginx:1.27",
				Port:      80,
				Replicas:  int32Ptr(2),
				Env:       map[string]string{"FOO": "bar"},
				Route:     &kipperv1.AppRoute{Host: "web-test.example.com", Path: "/"},
				Resources: kipperv1.AppResources{Profile: "standard"},
			},
		},
		&kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "demo-test"},
			Spec:       kipperv1.AppSpec{Image: "worker:v1", Port: 8080, Replicas: int32Ptr(1)},
		},
		&kipperv1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: "demo-test"},
			Spec:       kipperv1.ServiceSpec{Type: "postgres", Version: "16", Storage: "5Gi"},
		},
	}
	stripe := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stripe-keys",
			Namespace: "demo-test",
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper"},
		},
		Data: map[string][]byte{"STRIPE_KEY": []byte("sk_test"), "STRIPE_WEBHOOK": []byte("whsec_test")},
	}
	credentials := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backend-credentials",
			Namespace: "demo-test",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kipper",
				"kipper.run/service-type":      "postgres",
			},
		},
	}
	// The source namespace itself, which the preview now establishes the
	// project owns before reading anything out of it.
	sourceNs := newKipperNamespace("demo-test", "demo", "test", "0")
	client := fake.NewClientset(stripe, credentials, sourceNs)
	handler := &Projects{Client: client, CRClient: testCRClient(apps...), Domain: "example.com"}

	r := chi.NewRouter()
	r.Get("/api/v1/projects/{name}/copy-preview", handler.CopyPreview)

	req := httptest.NewRequest("GET", "/api/v1/projects/demo/copy-preview?from=test&target=prod", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp copyPreviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Source != "test" || resp.Target != "prod" {
		t.Errorf("source/target wrong: %+v", resp)
	}
	if resp.SourceNamespace != "demo-test" || resp.TargetNamespace != "demo-prod" {
		t.Errorf("namespaces wrong: %+v", resp)
	}
	if len(resp.Apps) != 2 {
		t.Errorf("expected 2 apps, got %d", len(resp.Apps))
	}
	if resp.DefaultHosts["web"] != "web-prod.example.com" {
		t.Errorf("expected default_hosts[web] = web-prod.example.com, got %q", resp.DefaultHosts["web"])
	}
	if _, ok := resp.DefaultHosts["worker"]; ok {
		t.Errorf("worker has no source route — should not have a default_hosts entry")
	}
	if len(resp.Services) != 1 || resp.Services[0].Type != "postgres" {
		t.Errorf("expected 1 postgres service, got %+v", resp.Services)
	}
	if len(resp.Secrets) != 1 || resp.Secrets[0].Name != "stripe-keys" {
		t.Fatalf("expected stripe-keys secret only (credentials must be hidden), got %+v", resp.Secrets)
	}
	if len(resp.Secrets[0].Keys) != 2 {
		t.Errorf("expected 2 keys, got %v", resp.Secrets[0].Keys)
	}
}

func TestProjectsHandler_AddEnvironment(t *testing.T) {
	t.Run("creates a blank environment when copy_from is omitted", func(t *testing.T) {
		project := &kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "demo"},
			Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "test"}}},
		}
		// Pre-create the namespaces the project reconciler would create — the
		// handler waits for the new env's namespace before returning.
		nsTest := newKipperNamespace("demo-test", "demo", "test", "0")
		nsProd := newKipperNamespace("demo-prod", "demo", "prod", "1")
		client := fake.NewClientset(nsTest, nsProd)
		crClient := testCRClient(project)
		handler := &Projects{Client: client, CRClient: crClient, Domain: "example.com"}

		r := chi.NewRouter()
		r.Post("/api/v1/projects/{name}/environments", handler.AddEnvironment)

		req := httptest.NewRequest("POST", "/api/v1/projects/demo/environments", strings.NewReader(`{"name":"prod"}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var updated kipperv1.Project
		if err := crClient.Get(req.Context(), crclient.ObjectKey{Name: "demo"}, &updated); err != nil {
			t.Fatalf("failed to read project: %v", err)
		}
		if len(updated.Spec.Environments) != 2 {
			t.Errorf("expected 2 envs, got %d", len(updated.Spec.Environments))
		}
	})

	t.Run("copies apps from source env when copy_from is provided", func(t *testing.T) {
		project := &kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "demo"},
			Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "test"}}},
		}
		webApp := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo-test"},
			Spec: kipperv1.AppSpec{
				Image: "nginx", Port: 80,
				Env:   map[string]string{"FOO": "bar"},
				Route: &kipperv1.AppRoute{Host: "web.example.com", Path: "/"},
			},
		}
		nsTest := newKipperNamespace("demo-test", "demo", "test", "0")
		nsProd := newKipperNamespace("demo-prod", "demo", "prod", "1")
		client := fake.NewClientset(nsTest, nsProd)
		crClient := testCRClient(project, webApp)
		handler := &Projects{Client: client, CRClient: crClient, Domain: "example.com"}

		r := chi.NewRouter()
		r.Post("/api/v1/projects/{name}/environments", handler.AddEnvironment)

		req := httptest.NewRequest("POST", "/api/v1/projects/demo/environments", strings.NewReader(`{"name":"prod","copy_from":"test"}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var copiedApp kipperv1.App
		if err := crClient.Get(req.Context(), crclient.ObjectKey{Namespace: "demo-prod", Name: "web"}, &copiedApp); err != nil {
			t.Fatalf("expected web app to be copied to demo-prod: %v", err)
		}
		if copiedApp.Spec.Env["FOO"] != "bar" {
			t.Errorf("expected env FOO=bar to be copied, got %v", copiedApp.Spec.Env)
		}
		if copiedApp.Spec.Route == nil {
			t.Fatal("expected fresh route on copied app")
		}
		if copiedApp.Spec.Route.Host != "web-prod.example.com" {
			t.Errorf("expected fresh hostname web-prod.example.com, got %q", copiedApp.Spec.Route.Host)
		}
	})

	t.Run("rejects copy_from referencing a non-existent environment", func(t *testing.T) {
		project := &kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "demo"},
			Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "test"}}},
		}
		client := fake.NewClientset()
		handler := &Projects{Client: client, CRClient: testCRClient(project)}

		r := chi.NewRouter()
		r.Post("/api/v1/projects/{name}/environments", handler.AddEnvironment)

		req := httptest.NewRequest("POST", "/api/v1/projects/demo/environments", strings.NewReader(`{"name":"prod","copy_from":"acc"}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("rejects when target env already exists", func(t *testing.T) {
		project := &kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "demo"},
			Spec: kipperv1.ProjectSpec{
				Environments: []kipperv1.ProjectEnvironment{{Name: "test"}, {Name: "prod"}},
			},
		}
		client := fake.NewClientset()
		handler := &Projects{Client: client, CRClient: testCRClient(project)}

		r := chi.NewRouter()
		r.Post("/api/v1/projects/{name}/environments", handler.AddEnvironment)

		req := httptest.NewRequest("POST", "/api/v1/projects/demo/environments", strings.NewReader(`{"name":"prod"}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusConflict {
			t.Errorf("expected 409, got %d", rec.Code)
		}
	})
}

func TestProjectsHandler_Update(t *testing.T) {
	t.Run("adds an environment to an existing project", func(t *testing.T) {
		existing := &kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
			Spec: kipperv1.ProjectSpec{
				Environments: []kipperv1.ProjectEnvironment{{Name: "test"}},
			},
		}
		crClient := testCRClient(existing)
		handler := &Projects{Client: fake.NewClientset(), CRClient: crClient}

		r := chi.NewRouter()
		r.Put("/api/v1/projects/{name}", handler.Update)

		body := `{"environments":["test","prod"]}`
		req := httptest.NewRequest("PUT", "/api/v1/projects/webapp", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var updated kipperv1.Project
		if err := crClient.Get(req.Context(), crclient.ObjectKey{Name: "webapp"}, &updated); err != nil {
			t.Fatalf("failed to read project: %v", err)
		}
		if len(updated.Spec.Environments) != 2 {
			t.Fatalf("expected 2 envs, got %d", len(updated.Spec.Environments))
		}
		if updated.Spec.Environments[0].Name != "test" || updated.Spec.Environments[1].Name != "prod" {
			t.Errorf("env order not preserved: %+v", updated.Spec.Environments)
		}
	})

	t.Run("keeps quota overrides on surviving environments", func(t *testing.T) {
		override := &kipperv1.EnvQuota{CPURequest: "6", CPULimit: "12", MemoryRequest: "12Gi", MemoryLimit: "24Gi"}
		existing := &kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
			Spec: kipperv1.ProjectSpec{
				Environments: []kipperv1.ProjectEnvironment{
					{Name: "test"},
					{Name: "prod", Quota: override},
				},
			},
		}
		crClient := testCRClient(existing)
		handler := &Projects{Client: fake.NewClientset(), CRClient: crClient}

		r := chi.NewRouter()
		r.Put("/api/v1/projects/{name}", handler.Update)

		body := `{"environments":["test","prod","acc"]}`
		req := httptest.NewRequest("PUT", "/api/v1/projects/webapp", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var updated kipperv1.Project
		if err := crClient.Get(req.Context(), crclient.ObjectKey{Name: "webapp"}, &updated); err != nil {
			t.Fatalf("failed to read project: %v", err)
		}
		if len(updated.Spec.Environments) != 3 {
			t.Fatalf("expected 3 envs, got %d", len(updated.Spec.Environments))
		}
		prod := updated.Spec.Environments[1]
		if prod.Name != "prod" || prod.Quota == nil || prod.Quota.CPURequest != "6" {
			t.Errorf("prod quota override lost on update: %+v", prod)
		}
		if updated.Spec.Environments[2].Quota != nil {
			t.Errorf("new env should have no quota override, got %+v", updated.Spec.Environments[2].Quota)
		}
	})

	t.Run("removes an environment from an existing project", func(t *testing.T) {
		existing := &kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
			Spec: kipperv1.ProjectSpec{
				Environments: []kipperv1.ProjectEnvironment{{Name: "test"}, {Name: "prod"}},
			},
		}
		crClient := testCRClient(existing)
		handler := &Projects{Client: fake.NewClientset(), CRClient: crClient}

		r := chi.NewRouter()
		r.Put("/api/v1/projects/{name}", handler.Update)

		body := `{"environments":["test"]}`
		req := httptest.NewRequest("PUT", "/api/v1/projects/webapp", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var updated kipperv1.Project
		if err := crClient.Get(req.Context(), crclient.ObjectKey{Name: "webapp"}, &updated); err != nil {
			t.Fatalf("failed to read project: %v", err)
		}
		if len(updated.Spec.Environments) != 1 || updated.Spec.Environments[0].Name != "test" {
			t.Errorf("expected only 'test' env, got %+v", updated.Spec.Environments)
		}
	})

	t.Run("rejects unknown project", func(t *testing.T) {
		handler := &Projects{Client: fake.NewClientset(), CRClient: testCRClient()}
		r := chi.NewRouter()
		r.Put("/api/v1/projects/{name}", handler.Update)

		req := httptest.NewRequest("PUT", "/api/v1/projects/missing", strings.NewReader(`{"environments":["test"]}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", rec.Code)
		}
	})

	t.Run("rejects empty env list", func(t *testing.T) {
		existing := &kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
			Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "test"}}},
		}
		handler := &Projects{Client: fake.NewClientset(), CRClient: testCRClient(existing)}
		r := chi.NewRouter()
		r.Put("/api/v1/projects/{name}", handler.Update)

		req := httptest.NewRequest("PUT", "/api/v1/projects/webapp", strings.NewReader(`{"environments":[]}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("rejects duplicate envs", func(t *testing.T) {
		existing := &kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
			Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "test"}}},
		}
		handler := &Projects{Client: fake.NewClientset(), CRClient: testCRClient(existing)}
		r := chi.NewRouter()
		r.Put("/api/v1/projects/{name}", handler.Update)

		req := httptest.NewRequest("PUT", "/api/v1/projects/webapp", strings.NewReader(`{"environments":["test","test"]}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("rejects env names that produce invalid namespaces", func(t *testing.T) {
		existing := &kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
			Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "test"}}},
		}
		handler := &Projects{Client: fake.NewClientset(), CRClient: testCRClient(existing)}
		r := chi.NewRouter()
		r.Put("/api/v1/projects/{name}", handler.Update)

		req := httptest.NewRequest("PUT", "/api/v1/projects/webapp", strings.NewReader(`{"environments":["Bad Env"]}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestProjectsHandler_Delete(t *testing.T) {
	t.Run("deletes project with multiple namespaces", func(t *testing.T) {
		staging := newKipperNamespace("myapp-staging", "myapp", "staging", "0")
		prod := newKipperNamespace("myapp-production", "myapp", "production", "1")
		projectCR := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "myapp"}}
		client := fake.NewClientset(staging, prod)
		handler := &Projects{Client: client, CRClient: testCRClient(projectCR)}

		r := chi.NewRouter()
		r.Delete("/api/v1/projects/{name}", handler.Delete)

		req := httptest.NewRequest("DELETE", "/api/v1/projects/myapp", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Errorf("expected status 204, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("deletes single-namespace project", func(t *testing.T) {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "simple",
				Labels: map[string]string{
					kipperLabel: kipperValue,
				},
			},
		}
		projectCR := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "simple"}}
		client := fake.NewClientset(ns)
		handler := &Projects{Client: client, CRClient: testCRClient(projectCR)}

		r := chi.NewRouter()
		r.Delete("/api/v1/projects/{name}", handler.Delete)

		req := httptest.NewRequest("DELETE", "/api/v1/projects/simple", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Errorf("expected status 204, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("returns 404 for non-kipper namespace (no Project CR)", func(t *testing.T) {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "kube-system",
				Labels: map[string]string{},
			},
		}
		client := fake.NewClientset(ns)
		handler := &Projects{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Delete("/api/v1/projects/{name}", handler.Delete)

		req := httptest.NewRequest("DELETE", "/api/v1/projects/kube-system", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("expected status 404, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("returns 404 for nonexistent project", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Projects{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Delete("/api/v1/projects/{name}", handler.Delete)

		req := httptest.NewRequest("DELETE", "/api/v1/projects/nonexistent", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("expected status 404, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})
}

// promoteProject is the Project the promote fixtures' namespaces belong to.
// Promote resolves both ends against the environments a project declares, so a
// namespace with no Project behind it is not a state the platform produces.
func promoteProject() *kipperv1.Project {
	return &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp"},
		Spec: kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{
			{Name: "staging"}, {Name: "production"},
		}},
	}
}

// promoteNamespaces is the pair of live namespaces promoteProject holds.
func promoteNamespaces() []runtime.Object {
	return []runtime.Object{
		newKipperNamespace("myapp-staging", "myapp", "staging", "0"),
		newKipperNamespace("myapp-production", "myapp", "production", "1"),
	}
}

func TestProjectsHandler_Promote(t *testing.T) {
	t.Run("promotes single app image from staging to production", func(t *testing.T) {
		sourceApp := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "myapp-staging", Labels: kipperLabels("web")},
			Spec:       kipperv1.AppSpec{Image: "web:v3", Port: 8080, Replicas: int32Ptr(2)},
		}
		targetApp := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "myapp-production", Labels: kipperLabels("web")},
			Spec:       kipperv1.AppSpec{Image: "web:v2", Port: 8080, Replicas: int32Ptr(3)},
		}
		// The namespaces myapp holds. Promote establishes ownership of both
		// ends before reading or writing an app in either.
		client := fake.NewClientset(promoteNamespaces()...)
		handler := &Projects{Client: client, CRClient: testCRClient(promoteProject(), sourceApp, targetApp)}

		r := chi.NewRouter()
		r.Post("/api/v1/projects/{name}/promote", handler.Promote)

		body := `{"app":"web","from":"staging","to":"production"}`
		req := httptest.NewRequest("POST", "/api/v1/projects/myapp/promote", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		promoted, ok := resp["promoted"].([]interface{})
		if !ok || len(promoted) != 1 {
			t.Fatalf("expected 1 promoted app, got %v", resp["promoted"])
		}
		if promoted[0] != "web" {
			t.Errorf("expected promoted app 'web', got %q", promoted[0])
		}
	})

	t.Run("creates target app CR when it does not exist", func(t *testing.T) {
		sourceApp := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "myapp-staging", Labels: kipperLabels("api")},
			Spec:       kipperv1.AppSpec{Image: "api:v5", Port: 8080, Replicas: int32Ptr(2)},
		}
		// The namespaces myapp holds. Promote establishes ownership of both
		// ends before reading or writing an app in either.
		client := fake.NewClientset(promoteNamespaces()...)
		handler := &Projects{Client: client, CRClient: testCRClient(promoteProject(), sourceApp)}

		r := chi.NewRouter()
		r.Post("/api/v1/projects/{name}/promote", handler.Promote)

		body := `{"app":"api","from":"staging","to":"production"}`
		req := httptest.NewRequest("POST", "/api/v1/projects/myapp/promote", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		promoted := resp["promoted"].([]interface{})
		if len(promoted) != 1 || promoted[0] != "api" {
			t.Errorf("expected promoted app 'api', got %v", promoted)
		}
	})

	t.Run("first-time promotion seeds env vars, secret refs, bindings and resources", func(t *testing.T) {
		sourceApp := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "myapp-staging", Labels: kipperLabels("api")},
			Spec: kipperv1.AppSpec{
				Image:      "api:v5",
				Port:       8080,
				Replicas:   int32Ptr(3),
				Env:        map[string]string{"FOO": "bar", "LOG_LEVEL": "info"},
				SecretRefs: []string{"api-secrets"},
				ServiceBindings: []kipperv1.ServiceBinding{
					{Name: "backend", Prefix: "DB_"},
				},
				Resources: kipperv1.AppResources{Profile: "standard", MemoryLimit: "512Mi"},
				Route:     &kipperv1.AppRoute{Host: "api-staging.example.com", Path: "/"},
			},
		}
		// The namespaces myapp holds. Promote establishes ownership of both
		// ends before reading or writing an app in either.
		client := fake.NewClientset(promoteNamespaces()...)
		crClient := testCRClient(promoteProject(), sourceApp)
		handler := &Projects{Client: client, CRClient: crClient}

		r := chi.NewRouter()
		r.Post("/api/v1/projects/{name}/promote", handler.Promote)

		body := `{"app":"api","from":"staging","to":"production"}`
		req := httptest.NewRequest("POST", "/api/v1/projects/myapp/promote", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var promoted kipperv1.App
		if err := crClient.Get(req.Context(), crclient.ObjectKey{Namespace: "myapp-production", Name: "api"}, &promoted); err != nil {
			t.Fatalf("expected api to be created in myapp-production: %v", err)
		}
		if promoted.Spec.Env["FOO"] != "bar" {
			t.Errorf("expected env FOO=bar, got %v", promoted.Spec.Env)
		}
		if len(promoted.Spec.SecretRefs) != 1 || promoted.Spec.SecretRefs[0] != "api-secrets" {
			t.Errorf("expected SecretRefs to carry over, got %v", promoted.Spec.SecretRefs)
		}
		if len(promoted.Spec.ServiceBindings) != 1 || promoted.Spec.ServiceBindings[0].Name != "backend" {
			t.Errorf("expected ServiceBindings to carry over, got %v", promoted.Spec.ServiceBindings)
		}
		if promoted.Spec.Resources.MemoryLimit != "512Mi" {
			t.Errorf("expected resources to carry over, got %v", promoted.Spec.Resources)
		}
		if promoted.Spec.Route != nil {
			t.Errorf("route must NOT be promoted (env-specific hostname), got %v", promoted.Spec.Route)
		}
	})

	t.Run("promotes all apps when all=true", func(t *testing.T) {
		web := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "myapp-staging", Labels: kipperLabels("web")},
			Spec:       kipperv1.AppSpec{Image: "web:v3", Port: 8080, Replicas: int32Ptr(2)},
		}
		api := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "myapp-staging", Labels: kipperLabels("api")},
			Spec:       kipperv1.AppSpec{Image: "api:v2", Port: 8080, Replicas: int32Ptr(1)},
		}
		// The namespaces myapp holds. Promote establishes ownership of both
		// ends before reading or writing an app in either.
		client := fake.NewClientset(promoteNamespaces()...)
		handler := &Projects{Client: client, CRClient: testCRClient(promoteProject(), web, api)}

		r := chi.NewRouter()
		r.Post("/api/v1/projects/{name}/promote", handler.Promote)

		body := `{"all":true,"from":"staging","to":"production"}`
		req := httptest.NewRequest("POST", "/api/v1/projects/myapp/promote", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		promoted := resp["promoted"].([]interface{})
		if len(promoted) != 2 {
			t.Errorf("expected 2 promoted apps, got %d", len(promoted))
		}
	})

	t.Run("records deploy history on promoted app CR", func(t *testing.T) {
		sourceApp := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "myapp-staging", Labels: kipperLabels("web")},
			Spec:       kipperv1.AppSpec{Image: "web:v3", Port: 8080, Replicas: int32Ptr(2)},
		}
		targetApp := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "web",
				Namespace:   "myapp-production",
				Labels:      kipperLabels("web"),
				Annotations: map[string]string{},
			},
			Spec: kipperv1.AppSpec{Image: "web:v2", Port: 8080, Replicas: int32Ptr(3)},
		}
		// The namespaces myapp holds. Promote establishes ownership of both
		// ends before reading or writing an app in either.
		client := fake.NewClientset(promoteNamespaces()...)
		handler := &Projects{Client: client, CRClient: testCRClient(promoteProject(), sourceApp, targetApp)}

		r := chi.NewRouter()
		r.Post("/api/v1/projects/{name}/promote", handler.Promote)

		body := `{"app":"web","from":"staging","to":"production"}`
		req := httptest.NewRequest("POST", "/api/v1/projects/myapp/promote", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("rejects missing from field", func(t *testing.T) {
		// The namespaces myapp holds. Promote establishes ownership of both
		// ends before reading or writing an app in either.
		client := fake.NewClientset(promoteNamespaces()...)
		handler := &Projects{Client: client, CRClient: testCRClient(promoteProject())}

		r := chi.NewRouter()
		r.Post("/api/v1/projects/{name}/promote", handler.Promote)

		body := `{"app":"web","to":"production"}`
		req := httptest.NewRequest("POST", "/api/v1/projects/myapp/promote", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("rejects missing to field", func(t *testing.T) {
		// The namespaces myapp holds. Promote establishes ownership of both
		// ends before reading or writing an app in either.
		client := fake.NewClientset(promoteNamespaces()...)
		handler := &Projects{Client: client, CRClient: testCRClient(promoteProject())}

		r := chi.NewRouter()
		r.Post("/api/v1/projects/{name}/promote", handler.Promote)

		body := `{"app":"web","from":"staging"}`
		req := httptest.NewRequest("POST", "/api/v1/projects/myapp/promote", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("requires app name when all is false", func(t *testing.T) {
		// The namespaces myapp holds. Promote establishes ownership of both
		// ends before reading or writing an app in either.
		client := fake.NewClientset(promoteNamespaces()...)
		handler := &Projects{Client: client, CRClient: testCRClient(promoteProject())}

		r := chi.NewRouter()
		r.Post("/api/v1/projects/{name}/promote", handler.Promote)

		body := `{"from":"staging","to":"production"}`
		req := httptest.NewRequest("POST", "/api/v1/projects/myapp/promote", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", rec.Code)
		}
	})
}

// A Project that declares no environments still gets one: the reconciler
// substitutes "test" and creates <project>-test. Listing the spec directly
// reported that project as owning no namespace at all, so anything reading this
// response to answer "who owns this namespace" got no answer for it.
//
// The console reads it for exactly that, to decide whether the caller is a
// deployer in the project owning the namespace it is showing.
func TestProjectsHandler_ListReportsTheEnvironmentAProjectGetsByDefault(t *testing.T) {
	declared := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec: kipperv1.ProjectSpec{
			Environments: []kipperv1.ProjectEnvironment{{Name: "prod"}},
		},
	}
	// No environments declared, so the reconciler gives it "test".
	bare := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "scratch"}}

	handler := &Projects{Client: fake.NewClientset(), CRClient: testCRClient(declared, bare)}
	r := chi.NewRouter()
	r.Get("/api/v1/projects", handler.List)

	req := asUser(httptest.NewRequest("GET", "/api/v1/projects", nil), "admin@test.com", middleware.RoleAdmin)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	var projects []projectResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &projects); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}

	byName := map[string]projectResponse{}
	for _, p := range projects {
		byName[p.Name] = p
	}

	if got := byName["scratch"].Environments; len(got) != 1 || got[0].Namespace != "scratch-test" {
		t.Errorf("a project declaring no environments must still report the one it has, got %+v", got)
	}
	if got := byName["shop"].Environments; len(got) != 1 || got[0].Namespace != "shop-prod" {
		t.Errorf("a declared environment must be unaffected, got %+v", got)
	}
}

// Two projects can resolve to one namespace, and only one of them holds it. The
// reconciler records the loser as a conflict and leaves both CRs listable, so a
// client comparing declarations cannot tell them apart — and it cannot detect
// the conflict by counting either, because this response holds only the
// projects the caller belongs to. A caller in the losing project alone sees one
// claim and nothing to compare it with.
//
// So the server answers from the namespace's own label, which is what
// authorization reads.
func TestProjectsHandler_ListSaysWhichProjectActuallyHoldsANamespace(t *testing.T) {
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec: kipperv1.ProjectSpec{
			Environments: []kipperv1.ProjectEnvironment{{Name: "prod"}},
		},
	}
	shopProd := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-prod"},
		Spec: kipperv1.ProjectSpec{
			Environments: []kipperv1.ProjectEnvironment{{Name: "default"}},
		},
	}
	// shop-prod got there first, so the live namespace carries its label.
	ns := newKipperNamespace("shop-prod", "shop-prod", "default", "0")

	handler := &Projects{Client: fake.NewClientset(ns), CRClient: testCRClient(shop, shopProd)}
	r := chi.NewRouter()
	r.Get("/api/v1/projects", handler.List)

	req := asUser(httptest.NewRequest("GET", "/api/v1/projects", nil), "admin@test.com", middleware.RoleAdmin)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	var projects []projectResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &projects); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	owned := map[string]bool{}
	for _, p := range projects {
		for _, e := range p.Environments {
			if e.Namespace == "shop-prod" {
				owned[p.Name] = e.Owned
			}
		}
	}

	if owned["shop"] {
		t.Error("shop declares shop-prod but does not hold it, so its claim must be reported as contradicted")
	}
	if !owned["shop-prod"] {
		t.Error("shop-prod holds the namespace its label names, so its claim stands")
	}
}

// A namespace the reconciler has not created yet contradicts nothing, so a
// project reported before its first reconcile looks exactly as it did before
// this field existed.
func TestProjectsHandler_ListLeavesAnUnclaimedNamespaceStanding(t *testing.T) {
	fresh := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "brand-new"},
		Spec: kipperv1.ProjectSpec{
			Environments: []kipperv1.ProjectEnvironment{{Name: "prod"}},
		},
	}
	handler := &Projects{Client: fake.NewClientset(), CRClient: testCRClient(fresh)}
	r := chi.NewRouter()
	r.Get("/api/v1/projects", handler.List)

	req := asUser(httptest.NewRequest("GET", "/api/v1/projects", nil), "admin@test.com", middleware.RoleAdmin)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	var projects []projectResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &projects); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if len(projects) != 1 || len(projects[0].Environments) != 1 || !projects[0].Environments[0].Owned {
		t.Errorf("a namespace nothing has taken must leave the claim standing, got %+v", projects)
	}
}

// A namespace that exists without Kipper's managed-by label still occupies the
// name. The reconciler refuses to adopt it and the authorization resolver
// resolves requests to it regardless of that label, so a claim on it is
// contradicted like any other. Selecting only Kipper-managed namespaces left
// this one case — the one the field exists for — looking like a free name.
func TestProjectsHandler_ListCountsAForeignNamespaceAsContradicting(t *testing.T) {
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec: kipperv1.ProjectSpec{
			Environments: []kipperv1.ProjectEnvironment{{Name: "prod"}},
		},
	}
	// Created outside Kipper, carrying neither label.
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-prod"}}

	handler := &Projects{Client: fake.NewClientset(foreign), CRClient: testCRClient(shop)}
	r := chi.NewRouter()
	r.Get("/api/v1/projects", handler.List)

	req := asUser(httptest.NewRequest("GET", "/api/v1/projects", nil), "admin@test.com", middleware.RoleAdmin)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	var projects []projectResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &projects); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if len(projects) != 1 || len(projects[0].Environments) != 1 {
		t.Fatalf("expected one project with one environment, got %+v", projects)
	}
	if projects[0].Environments[0].Owned {
		t.Error("a namespace shop does not hold contradicts its claim, whatever labels that namespace carries")
	}
}

// A project that declares no environments still has one: the reconciler
// substitutes "test" and creates <project>-test, and apps live in it.
//
// Adding a second environment used to replace that list rather than extend it.
// The spec went from empty to ["acc"], so the next reconcile built its keep-list
// from ["acc"] alone, found <project>-test unlisted, and deleted the namespace
// along with every app, service and volume in it.
//
// This drives the real handler and then asks the reconciler's own keep-list
// question, because the deletion is the reconciler acting on what the handler
// wrote — a test that only inspected the response would have passed throughout.
func TestProjectsHandler_AddEnvironmentKeepsTheOneTheProjectAlreadyHas(t *testing.T) {
	bare := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "scratch"}}
	// Both namespaces the reconciler would have: the implicit one the project
	// already runs in, and the one this addition asks for. The handler waits
	// for the second before returning.
	nsTest := newKipperNamespace("scratch-test", "scratch", "test", "0")
	nsAcc := newKipperNamespace("scratch-acc", "scratch", "acc", "1")
	handler := &Projects{
		Client:   fake.NewClientset(nsTest, nsAcc),
		CRClient: testCRClient(bare),
		Domain:   "example.com",
	}

	r := chi.NewRouter()
	r.Post("/api/v1/projects/{name}/environments", handler.AddEnvironment)

	body := strings.NewReader(`{"name":"acc"}`)
	req := asUser(httptest.NewRequest("POST", "/api/v1/projects/scratch/environments", body),
		"admin@test.com", middleware.RoleAdmin)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("adding an environment = %d: %s", rec.Code, rec.Body.String())
	}

	var stored kipperv1.Project
	if err := handler.CRClient.Get(context.Background(), crclient.ObjectKey{Name: "scratch"}, &stored); err != nil {
		t.Fatalf("reading the project back: %v", err)
	}

	names := map[string]bool{}
	for _, env := range controllers.ProjectEnvironments(&stored) {
		names[env.Name] = true
	}
	if !names["test"] {
		t.Errorf("the environment the project already had must survive the addition, got %+v", stored.Spec.Environments)
	}
	if !names["acc"] {
		t.Errorf("the new environment must be there too, got %+v", stored.Spec.Environments)
	}

	// The namespace the reconciler would keep, which is what the deletion reads.
	kept := map[string]bool{}
	for _, env := range controllers.ProjectEnvironments(&stored) {
		kept[controllers.ResolveNamespace("scratch", env.Name)] = true
	}
	if !kept["scratch-test"] {
		t.Error("scratch-test must stay on the keep-list, or the reconciler deletes it and everything in it")
	}
}

// A namespace nobody declared is still somebody's, and the copy writes into it
// under the console's own service account rather than the caller's.
//
// refuseNamespaceCollision compared Project declarations only, so a namespace
// created outside Kipper stopped nothing: the handler updated the project,
// waitForNamespace saw the name already existed and returned at once, and
// Copier.Run created Apps, Services, Volumes, Functions, Jobs and Secrets
// inside it. The reconciler refuses to adopt such a namespace, but that governs
// the reconciler's writes and happens after this one.
func TestProjectsHandler_AddEnvironmentRefusesAForeignTargetNamespace(t *testing.T) {
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec: kipperv1.ProjectSpec{
			Environments: []kipperv1.ProjectEnvironment{{Name: "test"}},
		},
	}
	shopTest := newKipperNamespace("shop-test", "shop", "test", "0")
	// Created outside Kipper, or left by a deleted project. Declared by nobody.
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-prod"}}

	client := fake.NewClientset(shopTest, foreign)
	handler := &Projects{Client: client, CRClient: testCRClient(shop), Domain: "example.com"}

	r := chi.NewRouter()
	r.Post("/api/v1/projects/{name}/environments", handler.AddEnvironment)

	body := strings.NewReader(`{"name":"prod","copy_from":"test"}`)
	req := asUser(httptest.NewRequest("POST", "/api/v1/projects/shop/environments", body),
		"owner@test.com", middleware.RoleAdmin)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("adding an environment onto somebody else's namespace is a 403, got %d: %s",
			rec.Code, rec.Body.String())
	}

	// Nothing may have been created in it. This is the assertion that matters:
	// a refusal that still copied would read as a pass on the status code alone.
	var apps kipperv1.AppList
	if err := handler.CRClient.List(context.Background(), &apps, crclient.InNamespace("shop-prod")); err != nil {
		t.Fatalf("listing apps in the foreign namespace: %v", err)
	}
	if len(apps.Items) != 0 {
		t.Errorf("the copy wrote %d apps into a namespace the project does not own", len(apps.Items))
	}
}

// The same check on the way in. The source is read rather than written, and
// reading a namespace this project does not own copies its apps, services and
// secrets into one the caller does.
func TestProjectsHandler_AddEnvironmentRefusesAForeignSourceNamespace(t *testing.T) {
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec: kipperv1.ProjectSpec{
			Environments: []kipperv1.ProjectEnvironment{{Name: "test"}},
		},
	}
	// shop-test resolves into another project's namespace.
	foreignSource := newKipperNamespace("shop-test", "somebody-else", "test", "0")
	target := newKipperNamespace("shop-prod", "shop", "prod", "1")

	client := fake.NewClientset(foreignSource, target)
	handler := &Projects{Client: client, CRClient: testCRClient(shop), Domain: "example.com"}

	r := chi.NewRouter()
	r.Post("/api/v1/projects/{name}/environments", handler.AddEnvironment)

	body := strings.NewReader(`{"name":"prod","copy_from":"test"}`)
	req := asUser(httptest.NewRequest("POST", "/api/v1/projects/shop/environments", body),
		"owner@test.com", middleware.RoleAdmin)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("copying out of somebody else's namespace is a 403, got %d: %s",
			rec.Code, rec.Body.String())
	}

	// And refused before the project was changed. The status alone cannot show
	// that: the check before Copier.Run would answer 403 just the same, having
	// already added the environment and let the reconciler act on it. That is
	// the race guard doing the early check's job and masking its removal.
	var stored kipperv1.Project
	if err := handler.CRClient.Get(context.Background(), crclient.ObjectKey{Name: "shop"}, &stored); err != nil {
		t.Fatalf("reading the project back: %v", err)
	}
	for _, env := range stored.Spec.Environments {
		if env.Name == "prod" {
			t.Error("the environment was added despite the request being refused")
		}
	}
}

// The preview reads the same six kinds and returns every Secret's name and key
// names, so it needs the same establishment before it reads anything.
func TestProjectsHandler_CopyPreviewRefusesAForeignSource(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "stripe-keys", Namespace: "shop-test",
			Labels: map[string]string{"app.kubernetes.io/managed-by": "kipper"},
		},
		Data: map[string][]byte{"STRIPE_SECRET_KEY": []byte("sk_live_notyours")},
	}
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-test"},
		Spec:       kipperv1.AppSpec{Image: "nginx:1.27", Port: 80},
	}
	foreignSource := newKipperNamespace("shop-test", "somebody-else", "test", "0")

	handler := &Projects{
		Client:   fake.NewClientset(secret, foreignSource),
		CRClient: testCRClient(app),
		Domain:   "example.com",
	}
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{name}/copy-preview", handler.CopyPreview)

	req := httptest.NewRequest("GET", "/api/v1/projects/shop/copy-preview?from=test&target=prod", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("previewing somebody else's namespace must be refused, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "stripe") || strings.Contains(rec.Body.String(), "web") {
		t.Errorf("the refusal must carry none of the namespace's contents, got %s", rec.Body.String())
	}
}

// An unlabelled namespace is refused like a differently-labelled one, because
// it is exactly what the reconciler will not adopt: the environment would be
// created and never work.
func TestProjectsHandler_AddEnvironmentRefusesAnUnlabelledNamespaceBeforeWriting(t *testing.T) {
	shop := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-prod"}}

	crClient := testCRClient(shop)
	handler := &Projects{Client: fake.NewClientset(foreign), CRClient: crClient, Domain: "example.com"}

	r := chi.NewRouter()
	r.Post("/api/v1/projects/{name}/environments", handler.AddEnvironment)

	body := strings.NewReader(`{"name":"prod"}`)
	req := asUser(httptest.NewRequest("POST", "/api/v1/projects/shop/environments", body),
		"owner@test.com", middleware.RoleAdmin)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("an environment onto an unlabelled namespace is a 403, got %d: %s",
			rec.Code, rec.Body.String())
	}

	// Refused before the CR was written, so the project is left as it was.
	var stored kipperv1.Project
	if err := crClient.Get(context.Background(), crclient.ObjectKey{Name: "shop"}, &stored); err != nil {
		t.Fatalf("reading the project back: %v", err)
	}
	for _, env := range stored.Spec.Environments {
		if env.Name == "prod" {
			t.Error("the environment was recorded on the project despite the refusal")
		}
	}
}

// The pre-check in refuseNamespaceCollision runs before the Project is written,
// which closes the ordinary case but not the window after it: the namespace can
// appear between that check and the copy, and the copy is the privileged act.
//
// So the assertion immediately before the copy is a separate guard, and this
// drives the window rather than the ordinary case. Without it the earlier
// foreign-target test passes on the pre-check alone, which is how a guard that
// does nothing looks exactly like a guard that works.
func TestProjectsHandler_AddEnvironmentRefusesANamespaceThatAppearsMidFlight(t *testing.T) {
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec: kipperv1.ProjectSpec{
			Environments: []kipperv1.ProjectEnvironment{{Name: "test"}},
		},
	}
	shopTest := newKipperNamespace("shop-test", "shop", "test", "0")
	client := fake.NewClientset(shopTest)

	// shop-prod is absent when the collision check looks, and somebody else's
	// by the time the handler waits for it.
	var looks int
	client.PrependReactor("get", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get, ok := action.(k8stesting.GetAction)
		if !ok || get.GetName() != "shop-prod" {
			return false, nil, nil
		}
		looks++
		if looks == 1 {
			return true, nil, apierrors.NewNotFound(corev1.Resource("namespaces"), "shop-prod")
		}
		return true, newKipperNamespace("shop-prod", "somebody-else", "prod", "1"), nil
	})

	handler := &Projects{Client: client, CRClient: testCRClient(shop), Domain: "example.com"}
	r := chi.NewRouter()
	r.Post("/api/v1/projects/{name}/environments", handler.AddEnvironment)

	body := strings.NewReader(`{"name":"prod","copy_from":"test"}`)
	req := asUser(httptest.NewRequest("POST", "/api/v1/projects/shop/environments", body),
		"owner@test.com", middleware.RoleAdmin)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if looks < 2 {
		t.Fatalf("the handler asked about the namespace %d times; the window this covers was never opened", looks)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a namespace that became somebody else's mid-flight is a 403, got %d: %s",
			rec.Code, rec.Body.String())
	}

	var apps kipperv1.AppList
	if err := handler.CRClient.List(context.Background(), &apps, crclient.InNamespace("shop-prod")); err != nil {
		t.Fatalf("listing apps in the foreign namespace: %v", err)
	}
	if len(apps.Items) != 0 {
		t.Errorf("the copy wrote %d apps into a namespace the project does not own", len(apps.Items))
	}
}

// The preview's target half, pinned on its own. The foreign-source case proves
// only the source guard, and the ordinary case has an absent target, so
// removing the target check left both of them green.
func TestProjectsHandler_CopyPreviewRefusesAForeignTarget(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-test"},
		Spec:       kipperv1.AppSpec{Image: "nginx:1.27", Port: 80},
	}
	ownSource := newKipperNamespace("shop-test", "shop", "test", "0")
	foreignTarget := newKipperNamespace("shop-prod", "somebody-else", "prod", "1")

	handler := &Projects{
		Client:   fake.NewClientset(ownSource, foreignTarget),
		CRClient: testCRClient(app),
		Domain:   "example.com",
	}
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{name}/copy-preview", handler.CopyPreview)

	req := httptest.NewRequest("GET", "/api/v1/projects/shop/copy-preview?from=test&target=prod", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("previewing a copy into somebody else's namespace is a 403, got %d: %s",
			rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "web") {
		t.Errorf("the refusal must carry none of the source's contents, got %s", rec.Body.String())
	}
}

// The fail-closed claim rests on what happens when the namespace cannot be read
// at all, which none of the cases above exercise: they all get a clean answer.
func TestProjectsHandler_CopyPreviewRefusesWhenOwnershipCannotBeRead(t *testing.T) {
	client := fake.NewClientset(newKipperNamespace("shop-test", "shop", "test", "0"))
	client.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("the apiserver is having a moment")
	})

	handler := &Projects{Client: client, CRClient: testCRClient(), Domain: "example.com"}
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{name}/copy-preview", handler.CopyPreview)

	req := httptest.NewRequest("GET", "/api/v1/projects/shop/copy-preview?from=test&target=prod", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatal("a namespace whose ownership could not be read must not be previewed")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("a read that failed is a 500 rather than a refusal about the caller, got %d: %s",
			rec.Code, rec.Body.String())
	}
}

// A project's own list must not carry the workloads of a namespace it declares
// but does not hold. Computing owned and then reading anyway was the shape of
// it: the boolean was right and the payload was somebody else's.
func TestProjectsHandler_ListDoesNotReadAForeignNamespacesWorkloads(t *testing.T) {
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec: kipperv1.ProjectSpec{
			Environments: []kipperv1.ProjectEnvironment{{Name: "prod"}},
		},
	}
	// shop declares prod; somebody else holds the namespace it resolves to.
	foreign := newKipperNamespace("shop-prod", "somebody-else", "prod", "0")
	theirApp := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "their-secret-service", Namespace: "shop-prod"},
		Spec: kipperv1.AppSpec{
			Image: "internal/billing:v9", Port: 8080,
			Route: &kipperv1.AppRoute{Host: "billing.somebody-else.example.com", Path: "/"},
		},
	}

	// Counted rather than inferred from the response. A refactor that listed
	// the apps and then discarded them before building the JSON would leave
	// every response assertion below green while the privileged read still went
	// out against a namespace known to be somebody else's.
	appLists := 0
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(shop, theirApp).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c crclient.WithWatch, list crclient.ObjectList, opts ...crclient.ListOption) error {
				if _, isApps := list.(*kipperv1.AppList); isApps {
					appLists++
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

	handler := &Projects{Client: fake.NewClientset(foreign), CRClient: crClient}
	r := chi.NewRouter()
	r.Get("/api/v1/projects", handler.List)

	req := asUser(httptest.NewRequest("GET", "/api/v1/projects", nil), "admin@test.com", middleware.RoleAdmin)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if appLists != 0 {
		t.Errorf("apps were listed %d times against a namespace known to belong to another project", appLists)
	}

	body := rec.Body.String()
	for _, leaked := range []string{"their-secret-service", "internal/billing", "billing.somebody-else"} {
		if strings.Contains(body, leaked) {
			t.Errorf("the list carried %q out of a namespace shop does not own: %s", leaked, body)
		}
	}

	var projects []projectResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &projects); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if len(projects) != 1 || len(projects[0].Environments) != 1 {
		t.Fatalf("expected one project with one environment, got %+v", projects)
	}
	if projects[0].Environments[0].Owned {
		t.Error("the claim is contradicted, so it must be reported as such")
	}
}

// Promote resolves both ends from the project name, and the gate in front of it
// authorised the caller against the Project rather than against each namespace.
func TestProjectsHandler_PromoteRefusesAForeignEnvironment(t *testing.T) {
	sourceApp := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "myapp-staging", Labels: kipperLabels("web")},
		Spec:       kipperv1.AppSpec{Image: "web:v3", Port: 8080, Replicas: int32Ptr(2)},
	}
	// myapp declares production; somebody else holds its namespace.
	client := fake.NewClientset(
		newKipperNamespace("myapp-staging", "myapp", "staging", "0"),
		newKipperNamespace("myapp-production", "somebody-else", "production", "1"),
	)
	crClient := testCRClient(promoteProject(), sourceApp)
	handler := &Projects{Client: client, CRClient: crClient}

	r := chi.NewRouter()
	r.Post("/api/v1/projects/{name}/promote", handler.Promote)

	body := `{"app":"web","from":"staging","to":"production"}`
	req := httptest.NewRequest("POST", "/api/v1/projects/myapp/promote", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("promoting into somebody else's namespace is a 403, got %d: %s", rec.Code, rec.Body.String())
	}

	var apps kipperv1.AppList
	if err := crClient.List(context.Background(), &apps, crclient.InNamespace("myapp-production")); err != nil {
		t.Fatalf("listing apps in the foreign namespace: %v", err)
	}
	if len(apps.Items) != 0 {
		t.Errorf("promote wrote %d apps into a namespace the project does not own", len(apps.Items))
	}
}

// The ownersKnown half of the List gate, pinned on its own. When the namespace
// inventory cannot be read, nothing is established about any claim, so no
// namespace's workloads may be listed. The foreign-owner test above exercises
// only a successful inventory, so removing this half left it green.
func TestProjectsHandler_ListReadsNoWorkloadsWhenOwnershipIsUnknown(t *testing.T) {
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec: kipperv1.ProjectSpec{
			Environments: []kipperv1.ProjectEnvironment{{Name: "prod"}},
		},
	}
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Image: "web:v1", Port: 8080},
	}

	client := fake.NewClientset()
	client.PrependReactor("list", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("the apiserver is having a moment")
	})

	// Fails the test if the handler lists Apps despite not knowing who owns
	// anything. Asserting on the JSON alone would not distinguish "did not
	// read" from "read and found nothing".
	var listedNamespaces []string
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(shop, app).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c crclient.WithWatch, list crclient.ObjectList, opts ...crclient.ListOption) error {
				if _, isApps := list.(*kipperv1.AppList); isApps {
					for _, o := range opts {
						if ns, ok := o.(crclient.InNamespace); ok {
							listedNamespaces = append(listedNamespaces, string(ns))
						}
					}
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

	handler := &Projects{Client: client, CRClient: crClient}
	r := chi.NewRouter()
	r.Get("/api/v1/projects", handler.List)

	req := asUser(httptest.NewRequest("GET", "/api/v1/projects", nil), "admin@test.com", middleware.RoleAdmin)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if len(listedNamespaces) != 0 {
		t.Errorf("apps were listed in %v while nothing was known about who owns them", listedNamespaces)
	}
	if strings.Contains(rec.Body.String(), "web:v1") {
		t.Errorf("the response carried a workload read without established ownership: %s", rec.Body.String())
	}
}

// A check that could not run is the server's failure, not a bad request, and
// its message is not the caller's to read.
func TestProjectsHandler_PromoteReportsAnOwnershipOutageAsAServerFailure(t *testing.T) {
	client := fake.NewClientset(newKipperNamespace("myapp-staging", "myapp", "staging", "0"))
	client.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("etcd leader election in progress")
	})
	handler := &Projects{Client: client, CRClient: testCRClient(promoteProject())}

	r := chi.NewRouter()
	r.Post("/api/v1/projects/{name}/promote", handler.Promote)

	body := `{"app":"web","from":"staging","to":"production"}`
	req := httptest.NewRequest("POST", "/api/v1/projects/myapp/promote", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("an ownership check that could not run is a 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "etcd") {
		t.Errorf("the underlying failure must not be echoed to the caller, got %s", rec.Body.String())
	}
}

// Three answers that had collapsed into one. An administrator reaches this
// handler for any project name, so a project that does not exist is a 404, a
// project without that environment is a malformed request, and a check that
// could not run is the server's failure.
func TestProjectsHandler_PromoteDistinguishesMissingFromUndeclared(t *testing.T) {
	client := fake.NewClientset(
		newKipperNamespace("myapp-staging", "myapp", "staging", "0"),
		newKipperNamespace("myapp-production", "myapp", "production", "1"),
	)
	handler := &Projects{Client: client, CRClient: testCRClient(promoteProject())}
	r := chi.NewRouter()
	r.Post("/api/v1/projects/{name}/promote", handler.Promote)

	call := func(project, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/v1/projects/"+project+"/promote", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	if rec := call("nosuchproject", `{"app":"web","from":"staging","to":"production"}`); rec.Code != http.StatusNotFound {
		t.Errorf("a project that does not exist is a 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := call("myapp", `{"app":"web","from":"staging","to":"nosuchenv"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("an environment the project does not declare is a 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Redacting the cause from the caller is only half of it. Without the other
// half an operator sees a 500 with no explanation anywhere: the request logger
// records method, path, status and duration, and the cause was discarded.
//
// Not observable in the response by construction, which is the point, so this
// captures what the server wrote.
func TestProjectsHandler_OwnershipOutageIsRedactedButRecorded(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	client := fake.NewClientset(newKipperNamespace("myapp-staging", "myapp", "staging", "0"))
	client.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("etcd leader election in progress")
	})
	handler := &Projects{Client: client, CRClient: testCRClient(promoteProject())}

	r := chi.NewRouter()
	r.Post("/api/v1/projects/{name}/promote", handler.Promote)

	body := `{"app":"web","from":"staging","to":"production"}`
	req := httptest.NewRequest("POST", "/api/v1/projects/myapp/promote", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "etcd") {
		t.Errorf("the cause must not reach the caller, got %s", rec.Body.String())
	}
	if !strings.Contains(logged.String(), "etcd") {
		t.Errorf("the cause must reach the server log, got %q", logged.String())
	}
}
