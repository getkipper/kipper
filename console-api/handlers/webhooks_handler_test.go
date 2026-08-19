package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/builder"
)

func TestWebhooksHandler_RejectsOversizedBody(t *testing.T) {
	// The webhook body is capped at 1 MiB. An oversized payload must be
	// rejected rather than read into memory, on both the valid-token JSON
	// path and the token-mismatch HMAC path.
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
		Spec:       kipperv1.AppSpec{Image: "web:v1", Port: 8080, Replicas: int32Ptr(2)},
	}
	webhookSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "web-webhook", Namespace: "staging"},
		Data:       map[string][]byte{"token": []byte("valid-token")},
	}
	oversized := `{"image":"web:v2","pad":"` + strings.Repeat("A", 2<<20) + `"}`

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"valid token, decoder path", "valid-token", http.StatusBadRequest},
		{"wrong token, HMAC read path", "wrong-token", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := &Webhooks{Client: fake.NewClientset(webhookSecret), CRClient: testCRClient(appCR)}
			r := chi.NewRouter()
			r.Post("/api/v1/webhook/{namespace}/{app}", handler.Receive)

			req := httptest.NewRequest("POST", "/api/v1/webhook/staging/web", strings.NewReader(oversized))
			req.Header.Set("X-Kipper-Token", tc.token)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("expected %d for an oversized body, got %d", tc.want, rec.Code)
			}
			if rec.Code == http.StatusOK {
				t.Fatal("an oversized body must never be processed")
			}
		})
	}
}

func TestWebhooksHandler_Receive(t *testing.T) {
	t.Run("deploys with valid token", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec:       kipperv1.AppSpec{Image: "web:v1", Port: 8080, Replicas: int32Ptr(2)},
		}
		webhookSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "web-webhook", Namespace: "staging"},
			Data:       map[string][]byte{"token": []byte("valid-token")},
		}
		client := fake.NewClientset(webhookSecret)
		handler := &Webhooks{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Post("/api/v1/webhook/{namespace}/{app}", handler.Receive)

		body := `{"image":"web:v2","commit":"abc123"}`
		req := httptest.NewRequest("POST", "/api/v1/webhook/staging/web", strings.NewReader(body))
		req.Header.Set("X-Kipper-Token", "valid-token")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp["status"] != "deployed" {
			t.Errorf("expected status 'deployed', got %q", resp["status"])
		}
		if resp["image"] != "web:v2" {
			t.Errorf("expected image 'web:v2', got %q", resp["image"])
		}
		if resp["commit"] != "abc123" {
			t.Errorf("expected commit 'abc123', got %q", resp["commit"])
		}
		if rev, ok := resp["revision"].(float64); !ok || rev != 1 {
			t.Errorf("expected revision 1, got %v", resp["revision"])
		}
	})

	t.Run("accepts X-Gitlab-Token header as fallback", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec:       kipperv1.AppSpec{Image: "web:v1", Port: 8080, Replicas: int32Ptr(2)},
		}
		webhookSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "web-webhook", Namespace: "staging"},
			Data:       map[string][]byte{"token": []byte("gitlab-token")},
		}
		client := fake.NewClientset(webhookSecret)
		handler := &Webhooks{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Post("/api/v1/webhook/{namespace}/{app}", handler.Receive)

		body := `{"image":"web:v3"}`
		req := httptest.NewRequest("POST", "/api/v1/webhook/staging/web", strings.NewReader(body))
		req.Header.Set("X-Gitlab-Token", "gitlab-token")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("rejects missing token", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Webhooks{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Post("/api/v1/webhook/{namespace}/{app}", handler.Receive)

		body := `{"image":"web:v2"}`
		req := httptest.NewRequest("POST", "/api/v1/webhook/staging/web", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", rec.Code)
		}
	})

	t.Run("rejects invalid token", func(t *testing.T) {
		webhookSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "web-webhook", Namespace: "staging"},
			Data:       map[string][]byte{"token": []byte("correct-token")},
		}
		client := fake.NewClientset(webhookSecret)
		handler := &Webhooks{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Post("/api/v1/webhook/{namespace}/{app}", handler.Receive)

		body := `{"image":"web:v2"}`
		req := httptest.NewRequest("POST", "/api/v1/webhook/staging/web", strings.NewReader(body))
		req.Header.Set("X-Kipper-Token", "wrong-token")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", rec.Code)
		}
	})

	t.Run("rejects when webhook not configured", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Webhooks{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Post("/api/v1/webhook/{namespace}/{app}", handler.Receive)

		body := `{"image":"web:v2"}`
		req := httptest.NewRequest("POST", "/api/v1/webhook/staging/web", strings.NewReader(body))
		req.Header.Set("X-Kipper-Token", "some-token")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", rec.Code)
		}
	})

	t.Run("rejects empty image", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec:       kipperv1.AppSpec{Image: "web:v1", Port: 8080, Replicas: int32Ptr(2)},
		}
		webhookSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "web-webhook", Namespace: "staging"},
			Data:       map[string][]byte{"token": []byte("valid-token")},
		}
		client := fake.NewClientset(webhookSecret)
		handler := &Webhooks{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Post("/api/v1/webhook/{namespace}/{app}", handler.Receive)

		body := `{"image":""}`
		req := httptest.NewRequest("POST", "/api/v1/webhook/staging/web", strings.NewReader(body))
		req.Header.Set("X-Kipper-Token", "valid-token")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("returns 404 for nonexistent app", func(t *testing.T) {
		webhookSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "web-webhook", Namespace: "staging"},
			Data:       map[string][]byte{"token": []byte("valid-token")},
		}
		client := fake.NewClientset(webhookSecret)
		handler := &Webhooks{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Post("/api/v1/webhook/{namespace}/{app}", handler.Receive)

		body := `{"image":"web:v2"}`
		req := httptest.NewRequest("POST", "/api/v1/webhook/staging/web", strings.NewReader(body))
		req.Header.Set("X-Kipper-Token", "valid-token")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("increments revision from existing history", func(t *testing.T) {
		history := `[{"revision":3,"image":"web:v3","trigger":"webhook","timestamp":"2025-01-01T00:00:00Z"}]`
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "web",
				Namespace:   "staging",
				Annotations: map[string]string{historyAnnotation: history},
			},
			Spec: kipperv1.AppSpec{Image: "web:v3", Port: 8080, Replicas: int32Ptr(1)},
		}
		webhookSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "web-webhook", Namespace: "staging"},
			Data:       map[string][]byte{"token": []byte("valid-token")},
		}
		client := fake.NewClientset(webhookSecret)
		handler := &Webhooks{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Post("/api/v1/webhook/{namespace}/{app}", handler.Receive)

		body := `{"image":"web:v4","commit":"def456"}`
		req := httptest.NewRequest("POST", "/api/v1/webhook/staging/web", strings.NewReader(body))
		req.Header.Set("X-Kipper-Token", "valid-token")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if rev, ok := resp["revision"].(float64); !ok || rev != 4 {
			t.Errorf("expected revision 4, got %v", resp["revision"])
		}
	})
}

func TestWebhooksHandler_History(t *testing.T) {
	t.Run("returns empty list when no history", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec:       kipperv1.AppSpec{Image: "web:v1", Port: 8080, Replicas: int32Ptr(1)},
		}
		client := fake.NewClientset()
		handler := &Webhooks{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/history", handler.History)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/history", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("returns deploy history from CR annotations", func(t *testing.T) {
		history := `[{"revision":2,"image":"web:v2","trigger":"webhook"},{"revision":1,"image":"web:v1","trigger":"webhook"}]`
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "web",
				Namespace:   "staging",
				Annotations: map[string]string{historyAnnotation: history},
			},
			Spec: kipperv1.AppSpec{Image: "web:v2", Port: 8080, Replicas: int32Ptr(1)},
		}
		client := fake.NewClientset()
		handler := &Webhooks{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/history", handler.History)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/history", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}

		var entries []deployEntry
		if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(entries) != 2 {
			t.Errorf("expected 2 history entries, got %d", len(entries))
		}
	})

	t.Run("returns empty list for nonexistent app", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Webhooks{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/history", handler.History)

		req := httptest.NewRequest("GET", "/projects/staging/apps/nonexistent/history", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	})
}

func TestWebhooksHandler_Rollback(t *testing.T) {
	t.Run("rolls back to previous version", func(t *testing.T) {
		history := `[{"revision":2,"image":"web:v2","commit":"def","trigger":"webhook"},{"revision":1,"image":"web:v1","commit":"abc","trigger":"webhook"}]`
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "web",
				Namespace:   "staging",
				Annotations: map[string]string{historyAnnotation: history},
			},
			Spec: kipperv1.AppSpec{Image: "web:v2", Port: 8080, Replicas: int32Ptr(1)},
		}
		client := fake.NewClientset()
		handler := &Webhooks{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Post("/projects/{name}/apps/{app}/rollback", handler.Rollback)

		body := `{}`
		req := httptest.NewRequest("POST", "/projects/staging/apps/web/rollback", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp["status"] != "rolled_back" {
			t.Errorf("expected status 'rolled_back', got %q", resp["status"])
		}
		if resp["image"] != "web:v1" {
			t.Errorf("expected image 'web:v1', got %q", resp["image"])
		}
		if rev, ok := resp["revision"].(float64); !ok || rev != 3 {
			t.Errorf("expected revision 3, got %v", resp["revision"])
		}
	})

	t.Run("rolls back to specific revision", func(t *testing.T) {
		history := `[{"revision":3,"image":"web:v3","trigger":"webhook"},{"revision":2,"image":"web:v2","trigger":"webhook"},{"revision":1,"image":"web:v1","trigger":"webhook"}]`
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "web",
				Namespace:   "staging",
				Annotations: map[string]string{historyAnnotation: history},
			},
			Spec: kipperv1.AppSpec{Image: "web:v3", Port: 8080, Replicas: int32Ptr(1)},
		}
		client := fake.NewClientset()
		handler := &Webhooks{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Post("/projects/{name}/apps/{app}/rollback", handler.Rollback)

		body := `{"revision":1}`
		req := httptest.NewRequest("POST", "/projects/staging/apps/web/rollback", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp["image"] != "web:v1" {
			t.Errorf("expected image 'web:v1', got %q", resp["image"])
		}
	})

	t.Run("rejects rollback when no history", func(t *testing.T) {
		appCR := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec:       kipperv1.AppSpec{Image: "web:v1", Port: 8080, Replicas: int32Ptr(1)},
		}
		client := fake.NewClientset()
		handler := &Webhooks{Client: client, CRClient: testCRClient(appCR)}

		r := chi.NewRouter()
		r.Post("/projects/{name}/apps/{app}/rollback", handler.Rollback)

		body := `{}`
		req := httptest.NewRequest("POST", "/projects/staging/apps/web/rollback", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("returns 404 for nonexistent app", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Webhooks{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Post("/projects/{name}/apps/{app}/rollback", handler.Rollback)

		body := `{}`
		req := httptest.NewRequest("POST", "/projects/staging/apps/nonexistent/rollback", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", rec.Code)
		}
	})
}

func TestWebhooksHandler_GetConfig(t *testing.T) {
	t.Run("returns disabled when no webhook secret exists", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Webhooks{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/webhook", handler.GetConfig)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/webhook", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}

		var resp map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp["enabled"] != false {
			t.Errorf("expected enabled=false, got %v", resp["enabled"])
		}
	})

	t.Run("returns enabled with token when secret exists", func(t *testing.T) {
		webhookSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "web-webhook", Namespace: "staging"},
			Data:       map[string][]byte{"token": []byte("my-secret-token")},
		}
		client := fake.NewClientset(webhookSecret)
		handler := &Webhooks{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps/{app}/webhook", handler.GetConfig)

		req := httptest.NewRequest("GET", "/projects/staging/apps/web/webhook", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}

		var resp map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp["enabled"] != true {
			t.Errorf("expected enabled=true, got %v", resp["enabled"])
		}
		if resp["token"] != "my-secret-token" {
			t.Errorf("expected token 'my-secret-token', got %q", resp["token"])
		}
	})
}

func TestWebhooksHandler_GenerateToken(t *testing.T) {
	t.Run("creates webhook secret for new app", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Webhooks{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Post("/projects/{name}/apps/{app}/webhook", handler.GenerateToken)

		req := httptest.NewRequest("POST", "/projects/staging/apps/web/webhook", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(resp["token"]) != 64 {
			t.Errorf("expected 64-char hex token, got %d chars", len(resp["token"]))
		}
	})

	t.Run("regenerates token for existing webhook", func(t *testing.T) {
		webhookSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "web-webhook", Namespace: "staging"},
			Data:       map[string][]byte{"token": []byte("old-token")},
		}
		client := fake.NewClientset(webhookSecret)
		handler := &Webhooks{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Post("/projects/{name}/apps/{app}/webhook", handler.GenerateToken)

		req := httptest.NewRequest("POST", "/projects/staging/apps/web/webhook", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp["token"] == "old-token" {
			t.Error("expected token to be regenerated, but got the old token")
		}
		if len(resp["token"]) != 64 {
			t.Errorf("expected 64-char hex token, got %d chars", len(resp["token"]))
		}
	})
}

func TestWebhooksHandler_DeleteWebhook(t *testing.T) {
	t.Run("deletes existing webhook secret", func(t *testing.T) {
		webhookSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "web-webhook", Namespace: "staging"},
			Data:       map[string][]byte{"token": []byte("my-token")},
		}
		client := fake.NewClientset(webhookSecret)
		handler := &Webhooks{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Delete("/projects/{name}/apps/{app}/webhook", handler.DeleteWebhook)

		req := httptest.NewRequest("DELETE", "/projects/staging/apps/web/webhook", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("returns 200 for nonexistent webhook", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Webhooks{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Delete("/projects/{name}/apps/{app}/webhook", handler.DeleteWebhook)

		req := httptest.NewRequest("DELETE", "/projects/staging/apps/web/webhook", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})
}

// buildStatusResponse drives the BuildStatus handler and returns the decoded JSON.
func buildStatusResponse(t *testing.T, client *fake.Clientset, appCR *kipperv1.App) map[string]interface{} {
	t.Helper()
	handler := &Webhooks{Client: client, CRClient: testCRClient(appCR)}
	r := chi.NewRouter()
	r.Get("/projects/{name}/apps/{app}/build/status", handler.BuildStatus)
	req := httptest.NewRequest(http.MethodGet, "/projects/blog/apps/web/build/status", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func buildStatusApp(credentialsSecret string) *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "blog"},
		Spec: kipperv1.AppSpec{
			Port: 80,
			Git: &kipperv1.AppGitSource{ //nolint:gosec // k8s Secret object name, not a credential value
				URL: "https://github.com/acme/web.git", Branch: "main", CredentialsSecret: credentialsSecret,
			},
		},
	}
}

// A shared credential named on the App CR must NOT be probed by the viewer-facing
// build-status endpoint: doing so would authenticate an admin-managed token
// without the allow-list/host check and expose a validity oracle. So no
// git_credential_valid appears for it.
func TestBuildStatus_DoesNotProbeSharedCredential(t *testing.T) {
	client := fake.NewClientset()
	resp := buildStatusResponse(t, client, buildStatusApp("shared-github"))
	if _, ok := resp["git_credential_valid"]; ok {
		t.Error("a shared credential must not be probed by the per-app build-status endpoint")
	}
}

// A discarded build is a Job with Succeeded: 1 that lives
// out its hour of TTL, and the Job-derived phase knows nothing about the
// fingerprint. Serving it means the console shows a green Succeeded for the
// very build whose image was refused, which is a sharper lie than the stuck
// Building the phase was added to replace.
func TestWebhooksHandler_BuildStatusServesTheDiscardedVerdictOverTheJob(t *testing.T) {
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Image: "registry.example.com/shop/checkout:current",
			Git:   &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "release"},
		},
		Status: kipperv1.AppStatus{Build: &kipperv1.AppBuildStatus{
			Phase:   "Discarded",
			Message: "The git source changed while this build was running.",
			Build:   "checkout-build-abc12345",
		}},
	}
	succeeded := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout-build-abc12345", Namespace: "kipper-builds",
			Labels: map[string]string{"kipper.run/build": "true", "kipper.run/app": "checkout", "kipper.run/source-namespace": "shop-test"},
		},
		Status: batchv1.JobStatus{Succeeded: 1},
	}

	handler := &Webhooks{Client: fake.NewClientset(succeeded), CRClient: testCRClient(appCR)}
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{name}/apps/{app}/build/status", handler.BuildStatus)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/projects/shop-test/apps/checkout/build/status", nil))

	var got map[string]interface{}
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "Discarded", got["phase"],
		"the console was told the refused build succeeded")
	assert.Contains(t, got["message"], "git source changed")
}

// Preferring the stored verdict on phase alone assumes it
// describes the job being reported. A build started after the discard whose
// pod is refused — by a quota, say — never reaches Active, so nothing writes
// over the Discarded, and the operator is shown the previous build's verdict
// while the one they are waiting on sits pending.
func TestWebhooksHandler_BuildStatusServesABuildStartedAfterTheDiscard(t *testing.T) {
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Image: "registry.example.com/shop/checkout:current",
			Git:   &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "release"},
		},
		Status: kipperv1.AppStatus{Build: &kipperv1.AppBuildStatus{
			Phase:   "Discarded",
			Message: "The git source changed while this build was running.",
			Build:   "checkout-build-abc12345",
		}},
	}
	queued := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout-build-def67890", Namespace: "kipper-builds",
			Labels: map[string]string{"kipper.run/build": "true", "kipper.run/app": "checkout", "kipper.run/source-namespace": "shop-test"},
		},
	}

	handler := &Webhooks{Client: fake.NewClientset(queued), CRClient: testCRClient(appCR)}
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{name}/apps/{app}/build/status", handler.BuildStatus)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/projects/shop-test/apps/checkout/build/status", nil))

	var got map[string]interface{}
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "Pending", got["phase"],
		"a build started after the discard was hidden behind the previous build's verdict")
}

// The three structs that marshal the deploy-history annotation are mirrors, and
// this one round-trips the whole list on rollback. A field present only on the
// controller's copy is stripped from every entry the first time somebody rolls
// back, which would silently disarm the build-id dedupe rather than fail.
func TestDeployEntryCarriesEveryFieldTheControllerWrites(t *testing.T) {
	written := `[{"revision":2,"image":"registry.example.com/shop/checkout:def678","commit":"def678","trigger":"build","timestamp":"2026-08-19T04:00:00Z","build":"checkout-build-def678"}]`

	var roundTripped []deployEntry
	require.NoError(t, json.Unmarshal([]byte(written), &roundTripped))
	back, err := json.Marshal(roundTripped)
	require.NoError(t, err)

	var before, after []map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(written), &before))
	require.NoError(t, json.Unmarshal(back, &after))
	assert.Equal(t, before, after, "a field the controller writes was dropped on the way through this handler")
}

// webhookTokenSecret is the Secret the handler reads a webhook token from.
func webhookTokenSecret(namespace, app, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: app + "-webhook", Namespace: namespace},
		Data:       map[string][]byte{"token": []byte(token)},
	}
}

// postWebhook drives the real route, so a test exercises what a pipeline hits.
func postWebhook(t *testing.T, wh *Webhooks, namespace, app, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/api/v1/webhook/{namespace}/{app}", wh.Receive)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhook/"+namespace+"/"+app, strings.NewReader(body))
	req.Header.Set("X-Kipper-Token", token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// The defect that started this: a pipeline posted the image it had just built,
// the handler threw it away because the app also had a git source, started a
// build instead, and answered 200 "building". The job went green, nothing
// deployed, and the app sat on its placeholder for days.
func TestWebhookRefusesAnImageWhileAGitSourceIsAttached(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "main"},
		},
	}
	client := fake.NewClientset(webhookTokenSecret("shop-test", "checkout", "s3cr3t"))
	wh := &Webhooks{Client: client, CRClient: testCRClient(app)}

	rec := postWebhook(t, wh, "shop-test", "checkout", "s3cr3t",
		`{"image":"registry.example.com/shop/checkout:9f2c1a","commit":"9f2c1a"}`)

	assert.Equal(t, http.StatusConflict, rec.Code,
		"a pipeline naming an exact image must fail loudly rather than be diverted into a build")
	assert.Contains(t, rec.Body.String(), "kip app git remove", "the refusal names the way out")
}

// A commit with no image is the git-backed pipeline's own signal, and still
// starts a build.
func TestWebhookStillBuildsWhenOnlyACommitIsPosted(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "main"},
		},
	}
	client := fake.NewClientset(
		webhookTokenSecret("shop-test", "checkout", "s3cr3t"),
		// What the builder reads to push the image it produces.
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "zot-push-credentials", Namespace: "kipper-system"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"password": []byte("placeholder")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "zot-tls", Namespace: "kipper-system"},
			Data:       map[string][]byte{"ca.crt": []byte("placeholder")},
		},
	)
	wh := &Webhooks{Client: client, CRClient: testCRClient(app)}

	rec := postWebhook(t, wh, "shop-test", "checkout", "s3cr3t", `{"commit":"9f2c1a"}`)

	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "building")
}

// The credential binding is about every use of the token, not only the build.
// The health probe authenticates against whatever URL the app currently names,
// so a token left over from a rolled-back move is disclosed by opening the
// Deployments tab, before any build runs. The probe must not be reached at all
// when the recorded host disagrees.
func TestWebhooksHandler_BuildStatusDoesNotProbeACredentialBoundElsewhere(t *testing.T) {
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{ //nolint:gosec // k8s Secret object name, not a credential value
			URL:               "https://git.example.com/shop/checkout.git",
			CredentialsSecret: "checkout-git-credentials",
		}},
	}
	stranded := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout-git-credentials", Namespace: "shop-test",
			Annotations: map[string]string{builder.GitAuthorityAnnotation: "git.elsewhere.example.com"},
		},
		Data: map[string][]byte{"token": []byte("a-token-for-the-other-host")},
	}

	probed := false
	handler := &Webhooks{
		Client: fake.NewClientset(stranded), CRClient: testCRClient(appCR),
		ProbeGit: func(context.Context, string, string) tokenHealth {
			probed = true
			return tokenHealth{Valid: true}
		},
	}
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{name}/apps/{app}/build/status", handler.BuildStatus)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/projects/shop-test/apps/checkout/build/status", nil))

	assert.False(t, probed, "a token stored for another host was sent to the one the app names")

	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, false, got["git_credential_valid"],
		"the console was not told the credential is unusable")
}
