package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func TestSettingsHandler_Get(t *testing.T) {
	t.Run("returns defaults when no app exists", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Settings{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/settings", handler.Get)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/settings", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}

		var resp appSettings
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if !resp.SecurityHeaders {
			t.Error("expected SecurityHeaders=true as default")
		}
		if resp.RateLimit != 0 {
			t.Errorf("expected RateLimit=0 as default, got %d", resp.RateLimit)
		}
	})

	t.Run("reads security headers from CR route", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec: kipperv1.AppSpec{
				Image: "nginx:1.25",
				Port:  80,
				Route: &kipperv1.AppRoute{
					NoSecurityHeaders: true,
				},
			},
		}
		client := fake.NewClientset()
		handler := &Settings{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/settings", handler.Get)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/settings", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		var resp appSettings
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.SecurityHeaders {
			t.Error("expected SecurityHeaders=false when NoSecurityHeaders is true")
		}
	})

	t.Run("reads custom rate limit from CR route", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec: kipperv1.AppSpec{
				Image: "nginx:1.25",
				Port:  80,
				Route: &kipperv1.AppRoute{
					RateLimit: 100,
				},
			},
		}
		client := fake.NewClientset()
		handler := &Settings{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/settings", handler.Get)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/settings", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		var resp appSettings
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.RateLimit != 100 {
			t.Errorf("expected RateLimit=100, got %d", resp.RateLimit)
		}
		if !resp.SecurityHeaders {
			t.Error("expected SecurityHeaders=true")
		}
	})

	t.Run("returns instance_header true by default", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Settings{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/settings", handler.Get)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/settings", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		var resp appSettings
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if !resp.InstanceHeader {
			t.Error("expected InstanceHeader=true as default")
		}
	})

	t.Run("reads instance header disabled from CR route", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec: kipperv1.AppSpec{
				Image: "nginx:1.25",
				Port:  80,
				Route: &kipperv1.AppRoute{
					NoInstanceHeader: true,
				},
			},
		}
		client := fake.NewClientset()
		handler := &Settings{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/settings", handler.Get)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/settings", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		var resp appSettings
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.InstanceHeader {
			t.Error("expected InstanceHeader=false when NoInstanceHeader is true")
		}
	})
}

func TestSettingsHandler_Update(t *testing.T) {
	t.Run("disables security headers on CR", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec: kipperv1.AppSpec{
				Image: "nginx:1.25",
				Port:  80,
				Route: &kipperv1.AppRoute{},
			},
		}
		client := fake.NewClientset()
		handler := &Settings{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/settings", handler.Update)

		body := `{"security_headers":false,"rate_limit":0}`
		req := httptest.NewRequest("PUT", "/projects/staging/apps/web/settings", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp appSettings
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.SecurityHeaders {
			t.Error("expected SecurityHeaders=false in response")
		}
	})

	t.Run("sets custom rate limit", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec: kipperv1.AppSpec{
				Image: "nginx:1.25",
				Port:  80,
				Route: &kipperv1.AppRoute{},
			},
		}
		client := fake.NewClientset()
		handler := &Settings{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/settings", handler.Update)

		body := `{"security_headers":true,"rate_limit":200}`
		req := httptest.NewRequest("PUT", "/projects/staging/apps/web/settings", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp appSettings
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.RateLimit != 200 {
			t.Errorf("expected RateLimit=200, got %d", resp.RateLimit)
		}
	})

	t.Run("creates route when nil", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec: kipperv1.AppSpec{
				Image: "nginx:1.25",
				Port:  80,
			},
		}
		client := fake.NewClientset()
		handler := &Settings{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/settings", handler.Update)

		body := `{"security_headers":true,"rate_limit":0}`
		req := httptest.NewRequest("PUT", "/projects/staging/apps/web/settings", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("disables instance header on CR", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec: kipperv1.AppSpec{
				Image: "nginx:1.25",
				Port:  80,
				Route: &kipperv1.AppRoute{},
			},
		}
		client := fake.NewClientset()
		handler := &Settings{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/settings", handler.Update)

		body := `{"security_headers":true,"instance_header":false,"rate_limit":0}`
		req := httptest.NewRequest("PUT", "/projects/staging/apps/web/settings", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp appSettings
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.InstanceHeader {
			t.Error("expected InstanceHeader=false in response")
		}
	})

	t.Run("rejects invalid JSON", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Settings{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/settings", handler.Update)

		req := httptest.NewRequest("PUT", "/projects/staging/apps/web/settings", strings.NewReader(`{{{`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("returns 404 for nonexistent app", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Settings{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/settings", handler.Update)

		body := `{"security_headers":true,"rate_limit":0}`
		req := httptest.NewRequest("PUT", "/projects/staging/apps/nonexistent/settings", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})
}
