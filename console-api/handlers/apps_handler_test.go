package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func int32Ptr(n int32) *int32 { return &n }

func newTestDeployment(namespace, name, image string, replicas int32, labels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: name, Image: image, Ports: []corev1.ContainerPort{{ContainerPort: 8080}}},
					},
				},
			},
		},
		Status: appsv1.DeploymentStatus{
			AvailableReplicas: replicas,
			ReadyReplicas:     replicas,
		},
	}
}

func kipperLabels(appName string) map[string]string {
	return map[string]string{
		kipperLabel: kipperValue,
		"app":       appName,
	}
}

// testScheme carries the core types as well as Kipper's, because production's
// CR client does (main.go registers clientgoscheme into it) and handlers reach
// for Secrets through it. A scheme with only the CRDs would fail a Secret
// delete here that works in production, which is a test lying about the code.
func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = kipperv1.AddToScheme(s)
	return s
}

func testCRClient(objs ...crclient.Object) crclient.Client {
	return crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).Build()
}

func TestAppsHandler_List(t *testing.T) {
	t.Run("returns empty list when no apps exist", func(t *testing.T) {
		client := fake.NewClientset()
		handler := &Apps{Client: client, CRClient: testCRClient()}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps", handler.List)

		req := httptest.NewRequest("GET", "/projects/staging/apps", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rec.Code)
		}

		var apps []appResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &apps); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(apps) != 0 {
			t.Errorf("expected 0 apps, got %d", len(apps))
		}
	})

	t.Run("returns apps from CRs", func(t *testing.T) {
		web := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec:       kipperv1.AppSpec{Image: "nginx:1.25", Replicas: int32Ptr(2)},
			Status:     kipperv1.AppStatus{Phase: "Running", ReadyReplicas: 2},
		}
		api := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "staging"},
			Spec:       kipperv1.AppSpec{Image: "api:v1", Replicas: int32Ptr(1)},
			Status:     kipperv1.AppStatus{Phase: "Running", ReadyReplicas: 1},
		}
		client := fake.NewClientset()
		handler := &Apps{Client: client, CRClient: testCRClient(web, api)}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps", handler.List)

		req := httptest.NewRequest("GET", "/projects/staging/apps", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rec.Code)
		}

		var apps []appResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &apps); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(apps) != 2 {
			t.Errorf("expected 2 apps, got %d", len(apps))
		}
	})

	t.Run("returns correct app status fields", func(t *testing.T) {
		web := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
			Spec:       kipperv1.AppSpec{Image: "nginx:1.25", Replicas: int32Ptr(2)},
			Status:     kipperv1.AppStatus{Phase: "Running", ReadyReplicas: 2},
		}
		client := fake.NewClientset()
		handler := &Apps{Client: client, CRClient: testCRClient(web)}

		r := chi.NewRouter()
		r.Get("/projects/{name}/apps", handler.List)

		req := httptest.NewRequest("GET", "/projects/staging/apps", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		var apps []appResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &apps); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		found := false
		for _, app := range apps {
			if app.Name == "web" {
				found = true
				if app.Image != "nginx:1.25" {
					t.Errorf("expected image 'nginx:1.25', got %q", app.Image)
				}
				if app.Replicas != 2 {
					t.Errorf("expected 2 replicas, got %d", app.Replicas)
				}
				if app.Status != "running" {
					t.Errorf("expected status 'running', got %q", app.Status)
				}
			}
		}
		if !found {
			t.Error("expected to find app 'web' in response")
		}
	})
}

func TestAppsHandler_Create(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		expectedStatus int
		expectedErr    string
	}{
		{
			name:           "creates app with all required fields",
			body:           `{"name":"web","image":"nginx:1.25","port":80,"replicas":2}`,
			expectedStatus: http.StatusCreated,
		},
		{
			name:           "defaults to 1 replica when not specified",
			body:           `{"name":"api","image":"api:v1","port":8080}`,
			expectedStatus: http.StatusCreated,
		},
		{
			name:           "creates app with env vars",
			body:           `{"name":"worker","image":"worker:v1","port":3000,"replicas":1,"env":{"DB_HOST":"postgres","DB_PORT":"5432"}}`,
			expectedStatus: http.StatusCreated,
		},
		{
			name:           "creates app from git source",
			body:           `{"name":"myapp","port":3000,"git":{"url":"https://github.com/user/repo.git","branch":"main"}}`,
			expectedStatus: http.StatusCreated,
		},
		{
			name:           "creates app with git source and route",
			body:           `{"name":"myapp2","port":8080,"git":{"url":"https://github.com/user/repo.git"},"route":{"host":"myapp2.example.com"}}`,
			expectedStatus: http.StatusCreated,
		},
		{
			name:           "rejects missing name",
			body:           `{"image":"nginx:1.25","port":80}`,
			expectedStatus: http.StatusBadRequest,
			expectedErr:    "name is required",
		},
		{
			name:           "rejects missing image and git",
			body:           `{"name":"web","port":80}`,
			expectedStatus: http.StatusBadRequest,
			expectedErr:    "either image or git source is required",
		},
		{
			name:           "rejects both image and git",
			body:           `{"name":"web","image":"nginx:1.25","port":80,"git":{"url":"https://github.com/user/repo.git"}}`,
			expectedStatus: http.StatusBadRequest,
			expectedErr:    "image and git source are mutually exclusive",
		},
		{
			name:           "rejects missing port",
			body:           `{"name":"web","image":"nginx:1.25"}`,
			expectedStatus: http.StatusBadRequest,
			expectedErr:    "port is required (could not auto-detect from Dockerfile)",
		},
		{
			name:           "rejects git source without url",
			body:           `{"name":"web","port":80,"git":{"branch":"main"}}`,
			expectedStatus: http.StatusBadRequest,
			expectedErr:    "git url is required",
		},
		{
			name:           "rejects a non-https git url",
			body:           `{"name":"web","port":80,"git":{"url":"http://github.com/user/repo.git"}}`,
			expectedStatus: http.StatusBadRequest,
			expectedErr:    `invalid git url: git url must use https, got "http"`,
		},
		{
			name:           "rejects a git url with embedded userinfo",
			body:           `{"name":"web","port":80,"git":{"url":"https://x-access-token@github.com/user/repo.git"}}`,
			expectedStatus: http.StatusBadRequest,
			expectedErr:    "invalid git url: git url must not contain userinfo",
		},
		{
			name:           "rejects invalid JSON",
			body:           `{{{invalid`,
			expectedStatus: http.StatusBadRequest,
			expectedErr:    "invalid request body",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset()
			handler := &Apps{Client: client, CRClient: testCRClient()}

			r := chi.NewRouter()
			r.Post("/projects/{name}/apps", handler.Create)

			req := httptest.NewRequest("POST", "/projects/staging/apps", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d; body: %s", tt.expectedStatus, rec.Code, rec.Body.String())
			}

			if tt.expectedErr != "" {
				var errResp map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
					t.Fatalf("failed to decode error: %v", err)
				}
				if errResp["error"] != tt.expectedErr {
					t.Errorf("expected error %q, got %q", tt.expectedErr, errResp["error"])
				}
			}
		})
	}
}

func TestAppsHandler_CreateConflict(t *testing.T) {
	existing := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "staging",
			Labels:    kipperLabels("web"),
		},
		Spec: kipperv1.AppSpec{
			Image: "nginx:1.25",
			Port:  80,
		},
	}
	client := fake.NewClientset()
	handler := &Apps{Client: client, CRClient: testCRClient(existing)}

	r := chi.NewRouter()
	r.Post("/projects/{name}/apps", handler.Create)

	body := `{"name":"web","image":"nginx:1.25","port":80}`
	req := httptest.NewRequest("POST", "/projects/staging/apps", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("expected status %d, got %d; body: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}
}

func TestAppsHandler_Scale(t *testing.T) {
	tests := []struct {
		name           string
		app            string
		body           string
		expectedStatus int
	}{
		{
			name:           "scales to 3 replicas",
			app:            "web",
			body:           `{"replicas":3}`,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "scales to zero",
			app:            "web",
			body:           `{"replicas":0}`,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "returns 404 for missing app",
			app:            "nonexistent",
			body:           `{"replicas":2}`,
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "rejects invalid body",
			app:            "web",
			body:           `{{{`,
			expectedStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			appCR := &kipperv1.App{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "web",
					Namespace: "staging",
					Labels:    kipperLabels("web"),
				},
				Spec: kipperv1.AppSpec{
					Image:    "nginx:1.25",
					Port:     80,
					Replicas: int32Ptr(1),
				},
			}
			client := fake.NewClientset()
			handler := &Apps{Client: client, CRClient: testCRClient(appCR)}

			r := chi.NewRouter()
			r.Put("/projects/{name}/apps/{app}/scale", handler.Scale)

			req := httptest.NewRequest("PUT", "/projects/staging/apps/"+tt.app+"/scale", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d; body: %s", tt.expectedStatus, rec.Code, rec.Body.String())
			}

			if tt.expectedStatus == http.StatusOK {
				var resp map[string]interface{}
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("failed to decode: %v", err)
				}
				if resp["name"] != tt.app {
					t.Errorf("expected name %q, got %q", tt.app, resp["name"])
				}
			}
		})
	}
}

func TestAppsHandler_Delete(t *testing.T) {
	tests := []struct {
		name           string
		app            string
		expectedStatus int
	}{
		{
			name:           "deletes existing app",
			app:            "web",
			expectedStatus: http.StatusNoContent,
		},
		{
			name:           "returns 404 for missing app",
			app:            "nonexistent",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			appCR := &kipperv1.App{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "web",
					Namespace: "staging",
					Labels:    kipperLabels("web"),
				},
				Spec: kipperv1.AppSpec{
					Image: "nginx:1.25",
					Port:  80,
				},
			}
			client := fake.NewClientset()
			handler := &Apps{Client: client, CRClient: testCRClient(appCR)}

			r := chi.NewRouter()
			r.Delete("/projects/{name}/apps/{app}", handler.Delete)

			req := httptest.NewRequest("DELETE", "/projects/staging/apps/"+tt.app, nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d; body: %s", tt.expectedStatus, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAppsHandler_Restart(t *testing.T) {
	tests := []struct {
		name           string
		app            string
		expectedStatus int
	}{
		{
			name:           "restarts existing app",
			app:            "web",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "returns 404 for missing app",
			app:            "nonexistent",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			appCR := &kipperv1.App{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "web",
					Namespace: "staging",
					Labels:    kipperLabels("web"),
				},
				Spec: kipperv1.AppSpec{
					Image: "nginx:1.25",
					Port:  80,
				},
			}
			client := fake.NewClientset()
			handler := &Apps{Client: client, CRClient: testCRClient(appCR)}

			r := chi.NewRouter()
			r.Post("/projects/{name}/apps/{app}/restart", handler.Restart)

			req := httptest.NewRequest("POST", "/projects/staging/apps/"+tt.app+"/restart", nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d; body: %s", tt.expectedStatus, rec.Code, rec.Body.String())
			}

			if tt.expectedStatus == http.StatusOK {
				var resp map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("failed to decode response: %v", err)
				}
				if resp["status"] != "restarting" {
					t.Errorf("expected status 'restarting', got %q", resp["status"])
				}
			}
		})
	}
}

func TestAppsHandler_UpdateImage(t *testing.T) {
	tests := []struct {
		name           string
		app            string
		body           string
		expectedStatus int
	}{
		{
			name:           "updates image successfully",
			app:            "web",
			body:           `{"image":"nginx:1.26"}`,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "rejects empty image",
			app:            "web",
			body:           `{"image":""}`,
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "rejects missing image field",
			app:            "web",
			body:           `{}`,
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "returns 404 for missing app",
			app:            "nonexistent",
			body:           `{"image":"nginx:1.26"}`,
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			appCR := &kipperv1.App{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "web",
					Namespace: "staging",
					Labels:    kipperLabels("web"),
				},
				Spec: kipperv1.AppSpec{
					Image: "nginx:1.25",
					Port:  80,
				},
			}
			client := fake.NewClientset()
			handler := &Apps{Client: client, CRClient: testCRClient(appCR)}

			r := chi.NewRouter()
			r.Put("/projects/{name}/apps/{app}/image", handler.UpdateImage)

			req := httptest.NewRequest("PUT", "/projects/staging/apps/"+tt.app+"/image", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d; body: %s", tt.expectedStatus, rec.Code, rec.Body.String())
			}

			if tt.expectedStatus == http.StatusOK {
				var resp map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("failed to decode response: %v", err)
				}
				if resp["image"] != "nginx:1.26" {
					t.Errorf("expected image 'nginx:1.26', got %q", resp["image"])
				}
			}
		})
	}
}
