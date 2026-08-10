package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func TestAppCRToResponse(t *testing.T) {
	replicas := func(n int32) *int32 { return &n }

	tests := []struct {
		name           string
		app            kipperv1.App
		expectedName   string
		expectedStatus string
		expectedImage  string
		expectedReady  int32
	}{
		{
			name: "running app",
			app: kipperv1.App{
				ObjectMeta: metav1.ObjectMeta{Name: "web"},
				Spec: kipperv1.AppSpec{
					Image:    "nginx:1.25",
					Replicas: replicas(2),
				},
				Status: kipperv1.AppStatus{
					Phase:         "Running",
					ReadyReplicas: 2,
				},
			},
			expectedName:   "web",
			expectedStatus: "running",
			expectedImage:  "nginx:1.25",
			expectedReady:  2,
		},
		{
			name: "stopped app with zero replicas",
			app: kipperv1.App{
				ObjectMeta: metav1.ObjectMeta{Name: "worker"},
				Spec: kipperv1.AppSpec{
					Image:    "worker:latest",
					Replicas: replicas(0),
				},
				Status: kipperv1.AppStatus{
					Phase: "Stopped",
				},
			},
			expectedName:   "worker",
			expectedStatus: "stopped",
			expectedImage:  "worker:latest",
			expectedReady:  0,
		},
		{
			name: "failed app",
			app: kipperv1.App{
				ObjectMeta: metav1.ObjectMeta{Name: "api"},
				Spec: kipperv1.AppSpec{
					Image:    "api:broken",
					Replicas: replicas(3),
				},
				Status: kipperv1.AppStatus{
					Phase: "Failed",
				},
			},
			expectedName:   "api",
			expectedStatus: "failed",
			expectedImage:  "api:broken",
			expectedReady:  0,
		},
		{
			name: "pending app with no status phase",
			app: kipperv1.App{
				ObjectMeta: metav1.ObjectMeta{Name: "new-app"},
				Spec: kipperv1.AppSpec{
					Image:    "app:v1",
					Replicas: replicas(1),
				},
			},
			expectedName:   "new-app",
			expectedStatus: "pending",
			expectedImage:  "app:v1",
			expectedReady:  0,
		},
		{
			name: "nil replicas pointer",
			app: kipperv1.App{
				ObjectMeta: metav1.ObjectMeta{Name: "nil-rep"},
				Spec: kipperv1.AppSpec{
					Image: "test:v1",
				},
			},
			expectedName:   "nil-rep",
			expectedStatus: "pending",
			expectedImage:  "test:v1",
			expectedReady:  0,
		},
		{
			name: "empty image",
			app: kipperv1.App{
				ObjectMeta: metav1.ObjectMeta{Name: "empty"},
				Spec: kipperv1.AppSpec{
					Replicas: replicas(0),
				},
				Status: kipperv1.AppStatus{
					Phase: "Stopped",
				},
			},
			expectedName:   "empty",
			expectedStatus: "stopped",
			expectedImage:  "",
			expectedReady:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := appCRToResponse(tt.app)

			if result.Name != tt.expectedName {
				t.Errorf("expected name %q, got %q", tt.expectedName, result.Name)
			}
			if result.Status != tt.expectedStatus {
				t.Errorf("expected status %q, got %q", tt.expectedStatus, result.Status)
			}
			if result.Image != tt.expectedImage {
				t.Errorf("expected image %q, got %q", tt.expectedImage, result.Image)
			}
			if result.Ready != tt.expectedReady {
				t.Errorf("expected ready %d, got %d", tt.expectedReady, result.Ready)
			}
		})
	}
}

func TestHealthHandler(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/health", nil)

	Health(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	body := w.Body.String()
	if body == "" {
		t.Error("expected non-empty body")
	}
}

func TestDockerfileRawURL_OnlyTrustedCloudHosts(t *testing.T) {
	// The autodetect fetch carries the app's git token, so a raw URL (and thus
	// the token) must be produced only for the recognised cloud authorities.
	trusted := map[string]string{
		"https://github.com/acme/app.git": "https://raw.githubusercontent.com/acme/app/main/Dockerfile",
		"https://gitlab.com/acme/app.git": "https://gitlab.com/acme/app/-/raw/main/Dockerfile",
		"https://www.gitlab.com/acme/app": "https://gitlab.com/acme/app/-/raw/main/Dockerfile",
	}
	for in, want := range trusted {
		if got := dockerfileRawURL(in, "main"); got != want {
			t.Errorf("dockerfileRawURL(%q) = %q, want %q", in, got, want)
		}
	}

	// A host that merely contains "gitlab", a self-hosted host, or a non-https
	// URL must yield no URL — so no token-bearing request is made to it.
	untrusted := []string{
		"https://gitlab.attacker.example/group/repo.git",
		"https://evil-gitlab.example/group/repo.git",
		"https://notgitlab.example/group/repo",
		"https://gitlab.mycorp.internal/group/repo.git",
		"https://github.enterprise.corp/acme/app.git",
		"http://gitlab.com/acme/app.git", // not https
		"://malformed",
	}
	for _, in := range untrusted {
		if got := dockerfileRawURL(in, "main"); got != "" {
			t.Errorf("dockerfileRawURL(%q) = %q, want empty (no token-bearing fetch)", in, got)
		}
	}
}
