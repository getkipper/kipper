package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	k8stesting "k8s.io/client-go/testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/labels"
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

// A credential written for an App that then fails to be created is stranded:
// no App means no reconcile, so the controller's sweep never visits it and the
// plaintext token sits in the tenant's namespace until somebody happens to
// reuse the name. The two sibling writers of this Secret both clean up after a
// failed CR write; this one did not.
func TestCreateApp_RemovesTheGitCredentialWhenTheAppCannotBeCreated(t *testing.T) {
	clientset := fake.NewClientset()
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			// Only the App create fails. Failing every create would stop the
			// name reservation that runs first, and the credential write this
			// is about would never be reached.
			Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
				if _, isApp := obj.(*kipperv1.App); isApp {
					return apierrors.NewInternalError(errors.New("the apiserver said no"))
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	handler := &Apps{Client: clientset, CRClient: crClient, GitReach: gitAlwaysReachable}

	body := strings.NewReader(`{"name":"web","port":80,` +
		`"git":{"url":"https://git.example.com/acme/web.git","branch":"main","token":"a-token"}}`)
	r := chi.NewRouter()
	r.Post("/projects/{name}/apps", handler.Create)

	req := httptest.NewRequest("POST", "/projects/shop-test/apps", body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)

	_, err := clientset.CoreV1().Secrets("shop-test").Get(context.Background(), "web-git-credentials", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err),
		"a plaintext token was left in the namespace for an app that does not exist")
}

// A duplicate create is an ordinary stale form or a retry, and the name
// reservation deliberately succeeds when the same kind already holds the name,
// so nothing stopped the request from replacing the live app's token before the
// create discovered the conflict. The response says only that nothing was
// created, while the app it named has lost the credential it was building with.
func TestCreateApp_ADuplicateNameDoesNotReplaceTheLiveAppsCredential(t *testing.T) {
	existing := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Port: 80,
			Git: &kipperv1.AppGitSource{ //nolint:gosec // k8s Secret object name, not a credential value
				URL: "https://git.example.com/acme/web.git", Branch: "main",
				CredentialsSecret: "web-git-credentials",
			},
		},
	}
	working := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-git-credentials", Namespace: "shop-test",
			Annotations: map[string]string{labels.AnnoGitAuthority: "git.example.com"},
		},
		Data: map[string][]byte{"token": []byte("the-token-that-works")},
	}
	clientset := fake.NewClientset(working)
	handler := &Apps{
		Client:   clientset,
		CRClient: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(existing).Build(),
		GitReach: gitAlwaysReachable,
	}

	r := chi.NewRouter()
	r.Post("/projects/{name}/apps", handler.Create)

	body := strings.NewReader(`{"name":"web","port":80,` +
		`"git":{"url":"https://git.attacker.example.com/acme/web.git","branch":"main","token":"a-different-token"}}`)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/projects/shop-test/apps", body))

	assert.Equal(t, http.StatusConflict, rec.Code)

	secret, err := clientset.CoreV1().Secrets("shop-test").Get(context.Background(), "web-git-credentials", metav1.GetOptions{})
	require.NoError(t, err, "the live app's credential was deleted by a rejected duplicate create")
	assert.Equal(t, "the-token-that-works", string(secret.Data["token"]),
		"a rejected duplicate create replaced the token the live app builds with")
	assert.Equal(t, "git.example.com", secret.Annotations[labels.AnnoGitAuthority])
}

// The losing half of a create/create race: both requests find no App, so the
// pre-check lets both through, and only the create tells them apart. While the
// credential was written before the create, the loser had already replaced the
// winner's token under the one fixed name — and then answered 409, having
// mutated a workload it did not create. The credential is written after the
// create now, so the loser writes nothing.
func TestCreateApp_TheLoserOfACreateRaceWritesNoCredential(t *testing.T) {
	clientset := fake.NewClientset()
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			// The winner created the App between this request's pre-check and
			// its own create.
			Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
				if _, isApp := obj.(*kipperv1.App); isApp {
					return apierrors.NewAlreadyExists(
						schema.GroupResource{Group: "kipper.run", Resource: "apps"}, "web")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	handler := &Apps{Client: clientset, CRClient: crClient, GitReach: gitAlwaysReachable}

	r := chi.NewRouter()
	r.Post("/projects/{name}/apps", handler.Create)

	body := strings.NewReader(`{"name":"web","port":80,` +
		`"git":{"url":"https://git.example.com/acme/web.git","branch":"main","token":"the-losers-token"}}`)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/projects/shop-test/apps", body))

	require.Equal(t, http.StatusConflict, rec.Code)

	_, err := clientset.CoreV1().Secrets("shop-test").Get(context.Background(), "web-git-credentials", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err),
		"the loser of a create race wrote a credential over the app it did not create")
}

// A remedy is read by somebody already stuck, so a command that does not run
// costs them more than saying nothing would. This one has been wrong twice:
// `kip app deploy` takes the app name as --name rather than a positional, and
// it requires --port. The flags are pinned here because the message is the only
// place they are written down outside kip itself.
func TestCreateApp_TheRemedyForAnUnstoredTokenNamesFlagsKipActuallyHas(t *testing.T) {
	clientset := fake.NewClientset()
	clientset.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("the apiserver said no"))
	})
	handler := &Apps{
		Client:   clientset,
		CRClient: crfake.NewClientBuilder().WithScheme(testScheme()).Build(),
		GitReach: gitAlwaysReachable,
	}

	r := chi.NewRouter()
	r.Post("/projects/{name}/apps", handler.Create)

	body := strings.NewReader(`{"name":"web","port":8080,` +
		`"git":{"url":"https://git.example.com/acme/web.git","branch":"main","token":"a-token"}}`)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/projects/shop-test/apps", body))

	require.Equal(t, http.StatusInternalServerError, rec.Code)

	// The angle brackets are escaped in the JSON body, so read the field.
	var decoded struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &decoded))
	message := decoded.Error

	assert.Contains(t, message, "--name web", "kip app deploy takes the app name as a flag, not a positional")
	assert.Contains(t, message, "--port 8080", "kip app deploy refuses to run without --port")
	assert.Contains(t, message, "--project", "the namespace here cannot be decomposed, so the operator supplies it")
	assert.Contains(t, message, "--git-token <token>")
	assert.NotContains(t, message, "deploy web ", "a positional app name is silently discarded by cobra")
}
