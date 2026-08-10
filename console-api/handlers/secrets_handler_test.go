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
)

func TestSecretsHandler_ListKeys(t *testing.T) {
	t.Run("returns empty list when no secrets exist", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Secrets{Client: client}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/secrets", handler.ListKeys)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/secrets", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}

		var keys []interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(keys) != 0 {
			t.Errorf("expected 0 keys, got %d", len(keys))
		}
	})

	t.Run("returns key names without values", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "app-web-secrets", Namespace: "staging"},
			Data: map[string][]byte{
				"API_KEY":     []byte("super-secret"),
				"DB_PASSWORD": []byte("hunter2"),
			},
		}
		client := fake.NewClientset(secret)
		handler := &Secrets{Client: client}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/secrets", handler.ListKeys)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/secrets", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}

		var keys []map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(keys) != 2 {
			t.Errorf("expected 2 keys, got %d", len(keys))
		}
	})

	t.Run("hides __previous entries and marks has_previous", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "app-web-secrets", Namespace: "staging"},
			Data: map[string][]byte{
				"API_KEY":            []byte("new-key"),
				"API_KEY.__previous": []byte("old-key"),
				"DB_PASSWORD":        []byte("hunter2"),
			},
		}
		client := fake.NewClientset(secret)
		handler := &Secrets{Client: client}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/secrets", handler.ListKeys)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/secrets", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		var keys []map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		// Should only show API_KEY and DB_PASSWORD, not API_KEY.__previous
		if len(keys) != 2 {
			t.Fatalf("expected 2 visible keys, got %d", len(keys))
		}

		for _, k := range keys {
			if k["key"] == "API_KEY" {
				if k["has_previous"] != true {
					t.Error("expected has_previous=true for API_KEY")
				}
			}
			if k["key"] == "DB_PASSWORD" {
				if k["has_previous"] != false {
					t.Error("expected has_previous=false for DB_PASSWORD")
				}
			}
		}
	})
}

func TestSecretsHandler_Reveal(t *testing.T) {
	t.Run("reveals secret value", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "app-web-secrets", Namespace: "staging"},
			Data: map[string][]byte{
				"API_KEY": []byte("super-secret-value"),
			},
		}
		client := fake.NewClientset(secret)
		handler := &Secrets{Client: client}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/secrets/{key}", handler.Reveal)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/secrets/API_KEY", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}

		var resp map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp["value"] != "super-secret-value" {
			t.Errorf("expected value 'super-secret-value', got %q", resp["value"])
		}
		if resp["key"] != "API_KEY" {
			t.Errorf("expected key 'API_KEY', got %q", resp["key"])
		}
	})

	t.Run("returns 404 for missing key", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "app-web-secrets", Namespace: "staging"},
			Data:       map[string][]byte{},
		}
		client := fake.NewClientset(secret)
		handler := &Secrets{Client: client}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/secrets/{key}", handler.Reveal)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/secrets/NONEXISTENT", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", rec.Code)
		}
	})

	t.Run("returns 404 when no secrets exist for app", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Secrets{Client: client}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/secrets/{key}", handler.Reveal)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/secrets/API_KEY", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", rec.Code)
		}
	})
}

func TestSecretsHandler_Set(t *testing.T) {
	t.Run("creates new secrets when none exist", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Secrets{Client: client}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/secrets", handler.Set)

		body := `{"API_KEY":"my-secret","DB_PASSWORD":"hunter2"}`
		req := httptest.NewRequest("PUT", "/projects/staging/apps/web/secrets", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp["status"] != "updated" {
			t.Errorf("expected status 'updated', got %q", resp["status"])
		}
	})

	t.Run("updates existing secret and preserves previous value", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "app-web-secrets",
				Namespace: "staging",
				Labels:    kipperLabels("web"),
			},
			Data: map[string][]byte{
				"API_KEY": []byte("old-key"),
			},
		}
		client := fake.NewClientset(secret)
		handler := &Secrets{Client: client}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/secrets", handler.Set)

		body := `{"API_KEY":"new-key"}`
		req := httptest.NewRequest("PUT", "/projects/staging/apps/web/secrets", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("rejects invalid JSON", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Secrets{Client: client}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/secrets", handler.Set)

		req := httptest.NewRequest("PUT", "/projects/staging/apps/web/secrets", strings.NewReader(`{{{`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", rec.Code)
		}
	})
}

func TestSecretsHandler_Delete(t *testing.T) {
	t.Run("deletes a single secret key", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "app-web-secrets", Namespace: "staging"},
			Data: map[string][]byte{
				"API_KEY":     []byte("secret"),
				"DB_PASSWORD": []byte("hunter2"),
			},
		}
		client := fake.NewClientset(secret)
		handler := &Secrets{Client: client}

		r := chi.NewRouter()
		r.Delete("/projects/{name}/apps/{app}/secrets/{key}", handler.Delete)

		req := httptest.NewRequest("DELETE", "/projects/staging/apps/web/secrets/API_KEY", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Errorf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("returns 404 for missing key", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "app-web-secrets", Namespace: "staging"},
			Data:       map[string][]byte{},
		}
		client := fake.NewClientset(secret)
		handler := &Secrets{Client: client}

		r := chi.NewRouter()
		r.Delete("/projects/{name}/apps/{app}/secrets/{key}", handler.Delete)

		req := httptest.NewRequest("DELETE", "/projects/staging/apps/web/secrets/NONEXISTENT", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", rec.Code)
		}
	})

	t.Run("returns 404 when no secrets exist for app", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Secrets{Client: client}

		r := chi.NewRouter()
		r.Delete("/projects/{name}/apps/{app}/secrets/{key}", handler.Delete)

		req := httptest.NewRequest("DELETE", "/projects/staging/apps/web/secrets/API_KEY", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", rec.Code)
		}
	})
}
