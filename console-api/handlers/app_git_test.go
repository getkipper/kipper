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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func appGitRouter(handler *Apps) *chi.Mux {
	r := chi.NewRouter()
	r.Route("/projects/{name}/apps/{app}", func(r chi.Router) {
		r.Get("/git", handler.GetGit)
		r.Put("/git", handler.SetGit)
	})
	return r
}

func TestGetGit_AppWithoutGit(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "nginx", Port: 80, Replicas: int32Ptr(1)},
	}
	handler := &Apps{Client: fake.NewClientset(), CRClient: testCRClient(app)}

	req := httptest.NewRequest("GET", "/projects/default/apps/web/git", nil)
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var resp gitResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	assert.False(t, resp.Configured)
	assert.False(t, resp.HasToken)
	assert.Empty(t, resp.URL)
}

func TestGetGit_ConfiguredAppNeverReturnsTokenValue(t *testing.T) {
	// The token must never appear in API responses, even if the operator
	// happens to read the App's git config. The Settings UI works off
	// `has_token` only — the actual bytes stay inside the cluster.
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: kipperv1.AppSpec{
			Image: "nginx", Port: 80, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{ //nolint:gosec // k8s Secret object name "web-git-credentials", not a credential value
				URL:               "https://github.com/acme/web.git",
				Branch:            "main",
				CredentialsSecret: "web-git-credentials",
				DockerfilePath:    "Dockerfile",
				Context:           ".",
			},
		},
	}
	handler := &Apps{Client: fake.NewClientset(), CRClient: testCRClient(app)}

	req := httptest.NewRequest("GET", "/projects/default/apps/web/git", nil)
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var resp gitResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	assert.True(t, resp.Configured)
	assert.True(t, resp.HasToken)
	assert.Equal(t, "https://github.com/acme/web.git", resp.URL)
	assert.Equal(t, "main", resp.Branch)

	// Wire-format guard: the actual token bytes must never appear in
	// the body. Looking for a `"token":"<value>"` JSON shape rather
	// than just the substring "token" — the legitimate `has_token`
	// field uses the substring too.
	assert.NotContains(t, rec.Body.String(), `"token":"`, "GetGit response must never echo a `token` value field")
	// Also confirm the actual PAT string from the cluster isn't leaking.
	// (We didn't put one in this fixture; this assertion is the
	// regression guard if someone wires a real Secret read into GetGit
	// later.)
	assert.NotContains(t, rec.Body.String(), "github_pat_")
	assert.NotContains(t, rec.Body.String(), "ghp_")
	assert.NotContains(t, rec.Body.String(), "glpat-")
}

func TestSetGit_TokenOnlyRotatesSecret(t *testing.T) {
	// The acme-tools+storefront workflow that prompted this endpoint:
	// expired PAT, App CR otherwise correct. The UI sends `{token: "..."}`
	// and nothing else. Branch + URL must survive untouched, and the
	// credentials Secret's `token` data key must hold the new value.
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: kipperv1.AppSpec{
			Image: "nginx", Port: 80, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{ //nolint:gosec // k8s Secret object name "web-git-credentials", not a credential value
				URL:               "https://github.com/acme/web.git",
				Branch:            "main",
				CredentialsSecret: "web-git-credentials",
			},
		},
	}
	clientset := fake.NewClientset()
	handler := &Apps{Client: clientset, CRClient: testCRClient(app)}

	body := strings.NewReader(`{"token":"github_pat_FRESH"}`)
	req := httptest.NewRequest("PUT", "/projects/default/apps/web/git", body)
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	// App CR's URL + branch unchanged.
	var updated kipperv1.App
	require.NoError(t, handler.CRClient.Get(context.Background(), crclient.ObjectKey{Namespace: "default", Name: "web"}, &updated))
	assert.Equal(t, "https://github.com/acme/web.git", updated.Spec.Git.URL)
	assert.Equal(t, "main", updated.Spec.Git.Branch)
	assert.Equal(t, "web-git-credentials", updated.Spec.Git.CredentialsSecret)

	// Secret created with the fresh token under the `token` data key.
	secret, err := clientset.CoreV1().Secrets("default").Get(context.Background(), "web-git-credentials", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "github_pat_FRESH", string(secret.Data["token"]))
}

func TestSetGit_BranchOnlyDoesNotTouchSecret(t *testing.T) {
	// Partial update: no token in the payload means leave the Secret
	// alone. Confirms the UI can flip a branch without re-supplying the
	// PAT.
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: kipperv1.AppSpec{
			Image: "nginx", Port: 80, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{ //nolint:gosec // k8s Secret object name "web-git-credentials", not a credential value
				URL:               "https://github.com/acme/web.git",
				Branch:            "main",
				CredentialsSecret: "web-git-credentials",
			},
		},
	}
	clientset := fake.NewClientset()
	handler := &Apps{Client: clientset, CRClient: testCRClient(app)}

	body := strings.NewReader(`{"branch":"release/2026"}`)
	req := httptest.NewRequest("PUT", "/projects/default/apps/web/git", body)
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var updated kipperv1.App
	require.NoError(t, handler.CRClient.Get(context.Background(), crclient.ObjectKey{Namespace: "default", Name: "web"}, &updated))
	assert.Equal(t, "release/2026", updated.Spec.Git.Branch, "branch overridden")
	assert.Equal(t, "https://github.com/acme/web.git", updated.Spec.Git.URL, "URL untouched on partial update")

	// No Secret should have been created or modified.
	_, err := clientset.CoreV1().Secrets("default").Get(context.Background(), "web-git-credentials", metav1.GetOptions{})
	assert.Error(t, err, "no Secret should have been written when token is absent from the request")
}

func TestSetGit_AttachGitToImageOnlyApp(t *testing.T) {
	// An operator can move an existing image-based App onto git as a
	// source. The URL is required to bootstrap the AppGitSource block.
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "nginx", Port: 80, Replicas: int32Ptr(1)},
	}
	clientset := fake.NewClientset()
	handler := &Apps{Client: clientset, CRClient: testCRClient(app)}

	body := strings.NewReader(`{"url":"https://github.com/example/web.git","branch":"main","token":"github_pat_A"}`)
	req := httptest.NewRequest("PUT", "/projects/default/apps/web/git", body)
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var updated kipperv1.App
	require.NoError(t, handler.CRClient.Get(context.Background(), crclient.ObjectKey{Namespace: "default", Name: "web"}, &updated))
	require.NotNil(t, updated.Spec.Git, "git source must be created")
	assert.Equal(t, "https://github.com/example/web.git", updated.Spec.Git.URL)
	assert.Equal(t, "web-git-credentials", updated.Spec.Git.CredentialsSecret)
}

func TestSetGit_AttachWithoutURLOnImageOnlyAppRejected(t *testing.T) {
	// Token rotation on an image-only App makes no sense — there's no
	// AppGitSource to attach the credentials to. Reject with 400 rather
	// than silently creating a half-state.
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "nginx", Port: 80, Replicas: int32Ptr(1)},
	}
	handler := &Apps{Client: fake.NewClientset(), CRClient: testCRClient(app)}

	body := strings.NewReader(`{"token":"github_pat_FOO"}`)
	req := httptest.NewRequest("PUT", "/projects/default/apps/web/git", body)
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestGetGit_StripsInlineCredentialsFromURL(t *testing.T) {
	// An App's git URL can carry inline creds (`https://oauth2:<pat>@…/repo.git`)
	// when set via kubectl-apply rather than the form. The GET endpoint
	// must never echo those creds back to the UI — same guarantee the
	// build-status endpoint provides via sanitizeGitURL. Without this
	// scrub a viewer-level caller could read another team's PAT just by
	// fetching the app's git source.
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: kipperv1.AppSpec{
			Image: "nginx", Port: 80, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{ //nolint:gosec // intentional fake credential in URL to exercise URL-scrub logic in this test
				URL:    "https://oauth2:glpat-EXAMPLE-LEAK@gitlab.com/acme/web.git",
				Branch: "main",
			},
		},
	}
	handler := &Apps{Client: fake.NewClientset(), CRClient: testCRClient(app)}

	req := httptest.NewRequest("GET", "/projects/default/apps/web/git", nil)
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "glpat-EXAMPLE-LEAK", "inline creds must never appear in the response body")
	assert.NotContains(t, body, "oauth2:", "the `oauth2:` username prefix is part of the cred URL form and must be stripped")
	assert.Contains(t, body, "https://gitlab.com/acme/web.git")
}

func TestSetGit_RotateOverExistingSecretSucceeds(t *testing.T) {
	// Token rotation calls createGitCredentialsSecret. The pre-fix
	// version did Create then, on AlreadyExists, Update with a fresh
	// object that lacked resourceVersion — real apiservers reject that.
	// Pre-seed a Secret with a rv and confirm a token rotation lands
	// cleanly without dropping it.
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: kipperv1.AppSpec{
			Image: "nginx", Port: 80, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{ //nolint:gosec // k8s Secret object name "web-git-credentials", not a credential value
				URL:               "https://github.com/acme/web.git",
				Branch:            "main",
				CredentialsSecret: "web-git-credentials",
			},
		},
	}
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "web-git-credentials",
			Namespace:       "default",
			ResourceVersion: "100",
			Labels:          map[string]string{"app.kubernetes.io/managed-by": "kipper", "kipper.run/app": "web"},
		},
		Data: map[string][]byte{"token": []byte("github_pat_OLD")},
	}
	clientset := fake.NewClientset(existing)
	handler := &Apps{Client: clientset, CRClient: testCRClient(app)}

	body := strings.NewReader(`{"token":"github_pat_FRESH"}`)
	req := httptest.NewRequest("PUT", "/projects/default/apps/web/git", body)
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "rotation against an existing Secret must not return 500")

	got, err := clientset.CoreV1().Secrets("default").Get(context.Background(), "web-git-credentials", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "github_pat_FRESH", string(got.Data["token"]))
	assert.Equal(t, "kipper", got.Labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, "web", got.Labels["kipper.run/app"])
}

func TestSetGit_AppNotFound(t *testing.T) {
	handler := &Apps{Client: fake.NewClientset(), CRClient: testCRClient()}

	body := strings.NewReader(`{"branch":"main"}`)
	req := httptest.NewRequest("PUT", "/projects/default/apps/missing/git", body)
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}
