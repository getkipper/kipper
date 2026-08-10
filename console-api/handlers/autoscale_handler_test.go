package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func TestAutoscaleHandler_Get(t *testing.T) {
	t.Run("returns disabled when no app exists", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Autoscale{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/autoscale", handler.Get)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/autoscale", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}

		var resp autoscaleResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.Enabled {
			t.Error("expected Enabled=false when no app exists")
		}
	})

	t.Run("returns disabled when autoscale not configured", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec:       kipperv1.AppSpec{Image: "nginx:1.25", Port: 80},
		}
		client := fake.NewClientset()
		handler := &Autoscale{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/autoscale", handler.Get)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/autoscale", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}

		var resp autoscaleResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.Enabled {
			t.Error("expected Enabled=false when autoscale not configured")
		}
	})

	t.Run("returns autoscale config from CR", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec: kipperv1.AppSpec{
				Image: "nginx:1.25",
				Port:  80,
				Autoscale: &kipperv1.AppAutoscale{
					Enabled:     true,
					MinReplicas: 2,
					MaxReplicas: 10,
					CPUTarget:   80,
				},
			},
		}
		// HPA with current metrics
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Status: autoscalingv2.HorizontalPodAutoscalerStatus{
				CurrentReplicas: 5,
			},
		}
		client := fake.NewClientset(hpa)
		handler := &Autoscale{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/autoscale", handler.Get)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/autoscale", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp autoscaleResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if !resp.Enabled {
			t.Error("expected Enabled=true")
		}
		if resp.MinReplicas != 2 {
			t.Errorf("expected MinReplicas=2, got %d", resp.MinReplicas)
		}
		if resp.MaxReplicas != 10 {
			t.Errorf("expected MaxReplicas=10, got %d", resp.MaxReplicas)
		}
		if resp.CPUTarget != 80 {
			t.Errorf("expected CPUTarget=80, got %d", resp.CPUTarget)
		}
		if resp.CurrentReplicas != 5 {
			t.Errorf("expected CurrentReplicas=5, got %d", resp.CurrentReplicas)
		}
	})
}

func TestAutoscaleHandler_Set(t *testing.T) {
	t.Run("enables autoscaling on app CR", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec:       kipperv1.AppSpec{Image: "nginx:1.25", Port: 80, Replicas: int32Ptr(2)},
		}
		client := fake.NewClientset()
		handler := &Autoscale{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/autoscale", handler.Set)

		body := `{"min_replicas":2,"max_replicas":10,"cpu_target":80}`
		req := httptest.NewRequest("PUT", "/projects/staging/apps/web/autoscale", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp["status"] != "enabled" {
			t.Errorf("expected status 'enabled', got %q", resp["status"])
		}
	})

	t.Run("sets both CPU and memory targets", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec:       kipperv1.AppSpec{Image: "nginx:1.25", Port: 80, Replicas: int32Ptr(2)},
		}
		client := fake.NewClientset()
		handler := &Autoscale{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/autoscale", handler.Set)

		body := `{"min_replicas":1,"max_replicas":5,"cpu_target":70,"memory_target":85}`
		req := httptest.NewRequest("PUT", "/projects/staging/apps/web/autoscale", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("rejects max_replicas less than 1", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Autoscale{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/autoscale", handler.Set)

		body := `{"min_replicas":1,"max_replicas":0,"cpu_target":70}`
		req := httptest.NewRequest("PUT", "/projects/staging/apps/web/autoscale", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("rejects request with no metric targets", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec:       kipperv1.AppSpec{Image: "nginx:1.25", Port: 80, Replicas: int32Ptr(2)},
		}
		client := fake.NewClientset()
		handler := &Autoscale{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/autoscale", handler.Set)

		body := `{"min_replicas":1,"max_replicas":5}`
		req := httptest.NewRequest("PUT", "/projects/staging/apps/web/autoscale", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("returns 404 when app does not exist", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Autoscale{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/autoscale", handler.Set)

		body := `{"min_replicas":1,"max_replicas":5,"cpu_target":70}`
		req := httptest.NewRequest("PUT", "/projects/staging/apps/nonexistent/autoscale", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("updates existing autoscale config", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec: kipperv1.AppSpec{
				Image:    "nginx:1.25",
				Port:     80,
				Replicas: int32Ptr(2),
				Autoscale: &kipperv1.AppAutoscale{
					Enabled:     true,
					MinReplicas: 1,
					MaxReplicas: 3,
					CPUTarget:   50,
				},
			},
		}
		client := fake.NewClientset()
		handler := &Autoscale{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Put("/projects/{name}/apps/{app}/autoscale", handler.Set)

		body := `{"min_replicas":2,"max_replicas":10,"cpu_target":80}`
		req := httptest.NewRequest("PUT", "/projects/staging/apps/web/autoscale", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestAutoscaleHandler_Delete(t *testing.T) {
	t.Run("disables autoscaling on app CR", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec: kipperv1.AppSpec{
				Image: "nginx:1.25",
				Port:  80,
				Autoscale: &kipperv1.AppAutoscale{
					Enabled:     true,
					MinReplicas: 1,
					MaxReplicas: 5,
					CPUTarget:   70,
				},
			},
		}
		client := fake.NewClientset()
		handler := &Autoscale{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Delete("/projects/{name}/apps/{app}/autoscale", handler.Delete)

		req := httptest.NewRequest("DELETE", "/projects/staging/apps/web/autoscale", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp["status"] != "disabled" {
			t.Errorf("expected status 'disabled', got %q", resp["status"])
		}
	})

	t.Run("succeeds even when no app exists", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Autoscale{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Delete("/projects/{name}/apps/{app}/autoscale", handler.Delete)

		req := httptest.NewRequest("DELETE", "/projects/staging/apps/web/autoscale", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})
}
