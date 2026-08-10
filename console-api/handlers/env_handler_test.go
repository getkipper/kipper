package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func TestEnvHandler_Get(t *testing.T) {
	t.Run("returns empty map when no app exists", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Env{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/env", handler.Get)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/env", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}

		var resp map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(resp) != 0 {
			t.Errorf("expected empty map, got %v", resp)
		}
	})

	t.Run("returns env vars from app CR", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec: kipperv1.AppSpec{
				Image: "nginx:1.25",
				Port:  80,
				Env: map[string]string{
					"DB_HOST": "postgres.internal",
					"DB_PORT": "5432",
				},
			},
		}
		client := fake.NewClientset()
		handler := &Env{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/env", handler.Get)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/env", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}

		var resp map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp["DB_HOST"] != "postgres.internal" {
			t.Errorf("expected DB_HOST='postgres.internal', got %q", resp["DB_HOST"])
		}
		if resp["DB_PORT"] != "5432" {
			t.Errorf("expected DB_PORT='5432', got %q", resp["DB_PORT"])
		}
	})
}

func TestEnvHandler_Update(t *testing.T) {
	t.Run("updates env vars on app CR", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec: kipperv1.AppSpec{
				Image: "nginx:1.25",
				Port:  80,
				Env: map[string]string{
					"DB_HOST": "old-host",
				},
			},
		}
		client := fake.NewClientset()
		handler := &Env{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/env", handler.Update)

		body := `{"DB_HOST":"new-host","DB_PORT":"5432"}`
		req := httptest.NewRequest("PUT", "/projects/staging/apps/web/env", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp["DB_HOST"] != "new-host" {
			t.Errorf("expected DB_HOST='new-host', got %q", resp["DB_HOST"])
		}
	})

	t.Run("returns 404 when app does not exist", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Env{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/env", handler.Update)

		body := `{"DB_HOST":"localhost"}`
		req := httptest.NewRequest("PUT", "/projects/staging/apps/web/env", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("rejects invalid JSON", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Env{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/env", handler.Update)

		req := httptest.NewRequest("PUT", "/projects/staging/apps/web/env", strings.NewReader(`{{{`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", rec.Code)
		}
	})
}

func TestEnvHandler_DirectEnvConflicts(t *testing.T) {
	t.Run("returns empty when no direct env vars", func(t *testing.T) {
		deploy := newTestDeployment("staging", "web", "nginx:1.25", 1, kipperLabels("web"))
		client := fake.NewClientset(deploy)
		handler := &Env{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/env/conflicts", handler.DirectEnvConflicts)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/env/conflicts", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("returns conflicting env var names", func(t *testing.T) {
		deploy := newTestDeployment("staging", "web", "nginx:1.25", 1, kipperLabels("web"))
		deploy.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{
			{Name: "DB_HOST", Value: "hardcoded"},
			{Name: "API_KEY", Value: "secret"},
		}
		client := fake.NewClientset(deploy)
		handler := &Env{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/env/conflicts", handler.DirectEnvConflicts)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/env/conflicts", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}

		var conflicts []string
		if err := json.Unmarshal(rec.Body.Bytes(), &conflicts); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(conflicts) != 2 {
			t.Errorf("expected 2 conflicts, got %d", len(conflicts))
		}
	})

	t.Run("returns empty for nonexistent deployment", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Env{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/env/conflicts", handler.DirectEnvConflicts)

		req := httptest.NewRequest("GET", "/projects/staging/apps/nonexistent/env/conflicts", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	})
}

func TestEnvHandler_RemoveDirectEnvConflicts(t *testing.T) {
	t.Run("removes direct env vars from deployment", func(t *testing.T) {
		deploy := newTestDeployment("staging", "web", "nginx:1.25", 1, kipperLabels("web"))
		deploy.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{
			{Name: "DB_HOST", Value: "hardcoded"},
		}
		client := fake.NewClientset(deploy)
		handler := &Env{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Delete("/projects/{name}/apps/{app}/env/conflicts", handler.RemoveDirectEnvConflicts)

		req := httptest.NewRequest("DELETE", "/projects/staging/apps/web/env/conflicts", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp map[string]int
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp["removed"] != 1 {
			t.Errorf("expected removed=1, got %d", resp["removed"])
		}
	})

	t.Run("returns 404 for nonexistent deployment", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Env{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Delete("/projects/{name}/apps/{app}/env/conflicts", handler.RemoveDirectEnvConflicts)

		req := httptest.NewRequest("DELETE", "/projects/staging/apps/nonexistent/env/conflicts", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", rec.Code)
		}
	})
}
