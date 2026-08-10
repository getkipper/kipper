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

func TestResourcesHandler_Get(t *testing.T) {
	t.Run("returns empty when no app exists", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Resources{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/resources", handler.Get)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/resources", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}

		var resp resourcesResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.CPULimit != "" || resp.MemoryLimit != "" {
			t.Errorf("expected empty limits, got cpu=%q memory=%q", resp.CPULimit, resp.MemoryLimit)
		}
	})

	t.Run("returns resource limits from CR spec", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec: kipperv1.AppSpec{
				Image: "nginx:1.25",
				Port:  80,
				Resources: kipperv1.AppResources{
					Profile:       "custom",
					CPURequest:    "500m",
					CPULimit:      "500m",
					MemoryRequest: "256Mi",
					MemoryLimit:   "256Mi",
				},
			},
		}
		client := fake.NewClientset()
		handler := &Resources{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/resources", handler.Get)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/resources", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}

		var resp resourcesResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.CPULimit != "500m" {
			t.Errorf("expected CPULimit='500m', got %q", resp.CPULimit)
		}
		if resp.MemoryLimit != "256Mi" {
			t.Errorf("expected MemoryLimit='256Mi', got %q", resp.MemoryLimit)
		}
		if resp.CPURequest != "500m" {
			t.Errorf("expected CPURequest='500m', got %q", resp.CPURequest)
		}
		if resp.MemoryRequest != "256Mi" {
			t.Errorf("expected MemoryRequest='256Mi', got %q", resp.MemoryRequest)
		}
	})

	t.Run("returns profile defaults when no custom values set", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec: kipperv1.AppSpec{
				Image: "nginx:1.25",
				Port:  80,
				Resources: kipperv1.AppResources{
					Profile: "standard",
				},
			},
		}
		client := fake.NewClientset()
		handler := &Resources{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/resources", handler.Get)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/resources", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}

		var resp resourcesResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.CPULimit != "100m" {
			t.Errorf("expected CPULimit='100m', got %q", resp.CPULimit)
		}
		if resp.MemoryLimit != "128Mi" {
			t.Errorf("expected MemoryLimit='128Mi', got %q", resp.MemoryLimit)
		}
	})

	t.Run("mirrors a one-sided override the way the reconciler does", func(t *testing.T) {
		// Only CPULimit is set. The reconciler mirrors it to the request to
		// keep Guaranteed QoS, so the GET response must report the same
		// rather than pairing the limit with the profile's request.
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec: kipperv1.AppSpec{
				Image: "nginx:1.25",
				Port:  80,
				Resources: kipperv1.AppResources{
					Profile:  "standard",
					CPULimit: "500m",
				},
			},
		}
		client := fake.NewClientset()
		handler := &Resources{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/resources", handler.Get)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/resources", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}

		var resp resourcesResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.CPULimit != "500m" {
			t.Errorf("expected CPULimit='500m', got %q", resp.CPULimit)
		}
		if resp.CPURequest != "500m" {
			t.Errorf("expected CPURequest mirrored to '500m', got %q", resp.CPURequest)
		}
		if resp.MemoryRequest != "128Mi" || resp.MemoryLimit != "128Mi" {
			t.Errorf("expected memory to fall back to profile 128Mi, got req=%q lim=%q", resp.MemoryRequest, resp.MemoryLimit)
		}
	})
}

func TestResourcesHandler_Update(t *testing.T) {
	t.Run("sets memory and CPU limits on CR", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec:       kipperv1.AppSpec{Image: "nginx:1.25", Port: 80},
		}
		client := fake.NewClientset()
		handler := &Resources{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/resources", handler.Update)

		body := `{"memory_limit":"512Mi","cpu_limit":"1"}`
		req := httptest.NewRequest("PUT", "/projects/staging/apps/web/resources", strings.NewReader(body))
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

	t.Run("clears limits when empty strings provided", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec: kipperv1.AppSpec{
				Image: "nginx:1.25",
				Port:  80,
				Resources: kipperv1.AppResources{
					Profile:       "custom",
					CPURequest:    "500m",
					CPULimit:      "500m",
					MemoryRequest: "256Mi",
					MemoryLimit:   "256Mi",
				},
			},
		}
		client := fake.NewClientset()
		handler := &Resources{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/resources", handler.Update)

		body := `{"memory_limit":"","cpu_limit":""}`
		req := httptest.NewRequest("PUT", "/projects/staging/apps/web/resources", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("returns 404 for missing app", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Resources{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/resources", handler.Update)

		body := `{"memory_limit":"256Mi"}`
		req := httptest.NewRequest("PUT", "/projects/staging/apps/nonexistent/resources", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", rec.Code)
		}
	})

	t.Run("rejects invalid JSON", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Resources{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/resources", handler.Update)

		req := httptest.NewRequest("PUT", "/projects/staging/apps/web/resources", strings.NewReader(`{{{`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", rec.Code)
		}
	})
}
