package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/middleware"
)

func appGitRevealRouter(handler *Apps) *chi.Mux {
	r := chi.NewRouter()
	r.Post("/projects/{name}/apps/{app}/git/reveal", handler.RevealGitToken)
	return r
}

func gitTokenApp() *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: kipperv1.AppSpec{
			Image: "nginx", Port: 80, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{ //nolint:gosec // k8s Secret object name, not a credential value
				URL:               "https://github.com/acme/web.git",
				Branch:            "main",
				CredentialsSecret: "web-git-credentials",
			},
		},
	}
}

const testGitToken = "github_pat_example_value" //nolint:gosec // G101: test fixture value, not a real credential

func seedGitTokenSecret(t *testing.T, client *fake.Clientset) {
	t.Helper()
	_, err := client.CoreV1().Secrets("default").Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "web-git-credentials", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte(testGitToken)},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func revealGitTokenRequest(password string, authenticated bool) *http.Request {
	body, _ := json.Marshal(map[string]string{"password": password})
	req := httptest.NewRequest(http.MethodPost, "/projects/default/apps/web/git/reveal", bytes.NewReader(body))
	if authenticated {
		ctx := context.WithValue(req.Context(), middleware.UserContextKey, &middleware.Claims{Email: testAdminEmail})
		req = req.WithContext(ctx)
	}
	return req
}

func TestRevealGitToken_HappyPath(t *testing.T) {
	client := fake.NewClientset()
	seedDexConfig(t, client)
	seedGitTokenSecret(t, client)
	handler := &Apps{Client: client, CRClient: testCRClient(gitTokenApp())}

	rec := httptest.NewRecorder()
	appGitRevealRouter(handler).ServeHTTP(rec, revealGitTokenRequest(testAdminPassword, true))

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, testGitToken, resp["token"])
}

func TestRevealGitToken_RejectsSharedCredentialName(t *testing.T) {
	// The app references a credential that is not its own <app>-git-credentials
	// (a shared credential, or a leftover fan-out copy named after one). A
	// deployer must not be able to reveal an administrator-managed shared token
	// this way, so it is refused regardless of what Secret sits under the name.
	app := gitTokenApp()
	app.Spec.Git.CredentialsSecret = "shared-github"

	client := fake.NewClientset()
	seedDexConfig(t, client)
	_, err := client.CoreV1().Secrets("default").Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-github", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("leaked-shared-token")},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	handler := &Apps{Client: client, CRClient: testCRClient(app)}

	rec := httptest.NewRecorder()
	appGitRevealRouter(handler).ServeHTTP(rec, revealGitTokenRequest(testAdminPassword, true))

	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.NotContains(t, rec.Body.String(), "leaked-shared-token")
}

func TestRevealGitToken_WrongPassword(t *testing.T) {
	client := fake.NewClientset()
	seedDexConfig(t, client)
	seedGitTokenSecret(t, client)
	handler := &Apps{Client: client, CRClient: testCRClient(gitTokenApp())}

	rec := httptest.NewRecorder()
	appGitRevealRouter(handler).ServeHTTP(rec, revealGitTokenRequest("wrong-password", true))

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	// A failed password check must not leak the token in the body.
	assert.NotContains(t, rec.Body.String(), "github_pat_")
}

func TestRevealGitToken_NotAuthenticated(t *testing.T) {
	client := fake.NewClientset()
	seedDexConfig(t, client)
	seedGitTokenSecret(t, client)
	handler := &Apps{Client: client, CRClient: testCRClient(gitTokenApp())}

	rec := httptest.NewRecorder()
	appGitRevealRouter(handler).ServeHTTP(rec, revealGitTokenRequest(testAdminPassword, false))

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRevealGitToken_MissingPassword(t *testing.T) {
	client := fake.NewClientset()
	seedDexConfig(t, client)
	handler := &Apps{Client: client, CRClient: testCRClient(gitTokenApp())}

	rec := httptest.NewRecorder()
	appGitRevealRouter(handler).ServeHTTP(rec, revealGitTokenRequest("", true))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestRevealGitToken_RoleBoundary composes the real RoleMiddleware + deployer
// RequireRole wrapper around the handler — the same chain main.go applies — to
// guard the authorization decision: viewers must not reveal a token, deployers
// may. The full buildRouter can't run on a fake clientset, so this mirrors its
// wrapper instead.
func TestRevealGitToken_RoleBoundary(t *testing.T) {
	build := func(role string) *chi.Mux {
		client := fake.NewClientset()
		seedDexConfig(t, client)
		seedGitTokenSecret(t, client)
		store := middleware.NewRoleStore(client)
		require.NoError(t, store.SetRole(context.Background(), testAdminEmail, role))
		handler := &Apps{Client: client, CRClient: testCRClient(gitTokenApp())}

		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ctx := context.WithValue(req.Context(), middleware.UserContextKey, &middleware.Claims{Email: testAdminEmail})
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		})
		r.Use(middleware.RoleMiddleware(store))
		requireDeployer := middleware.RequireRole(middleware.RoleAdmin, middleware.RoleDeployer)
		r.With(requireDeployer).Post("/projects/{name}/apps/{app}/git/reveal", handler.RevealGitToken)
		return r
	}

	revealReq := func() *http.Request {
		body, _ := json.Marshal(map[string]string{"password": testAdminPassword})
		return httptest.NewRequest(http.MethodPost, "/projects/default/apps/web/git/reveal", bytes.NewReader(body))
	}

	t.Run("viewer is denied before any token read", func(t *testing.T) {
		rec := httptest.NewRecorder()
		build(middleware.RoleViewer).ServeHTTP(rec, revealReq())
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.NotContains(t, rec.Body.String(), "github_pat_")
	})

	t.Run("deployer passes the role gate", func(t *testing.T) {
		rec := httptest.NewRecorder()
		build(middleware.RoleDeployer).ServeHTTP(rec, revealReq())
		require.Equal(t, http.StatusOK, rec.Code)
		var resp map[string]string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, testGitToken, resp["token"])
	})
}

func TestRevealGitToken_NoCredentialConfigured(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "nginx", Port: 80, Replicas: int32Ptr(1)},
	}
	client := fake.NewClientset()
	seedDexConfig(t, client)
	handler := &Apps{Client: client, CRClient: testCRClient(app)}

	rec := httptest.NewRecorder()
	appGitRevealRouter(handler).ServeHTTP(rec, revealGitTokenRequest(testAdminPassword, true))

	assert.Equal(t, http.StatusNotFound, rec.Code)
}
