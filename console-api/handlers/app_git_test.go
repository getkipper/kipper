package handlers

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/getkipper/kipper/console-api/builder"
	"github.com/getkipper/kipper/console-api/internal/gitcred"
	"github.com/getkipper/kipper/console-api/internal/gitreach"
)

// gitAlwaysReachable stands in for the clone preflight. Without it every test
// in this file would reach the real internet, and what they are about is which
// fields a request writes.
func gitAlwaysReachable(context.Context, string, string, string, string) (gitreach.Result, string) {
	return gitreach.Reachable, ""
}

func appGitRouter(handler *Apps) *chi.Mux {
	r := chi.NewRouter()
	r.Route("/projects/{name}/apps/{app}", func(r chi.Router) {
		r.Get("/git", handler.GetGit)
		r.Put("/git", handler.SetGit)
		r.Delete("/git", handler.DeleteGit)
	})
	return r
}

func TestGetGit_AppWithoutGit(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "nginx", Port: 80, Replicas: int32Ptr(1)},
	}
	handler := &Apps{Client: fake.NewClientset(), CRClient: testCRClient(app), GitReach: gitAlwaysReachable}

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
	handler := &Apps{Client: fake.NewClientset(), CRClient: testCRClient(app), GitReach: gitAlwaysReachable}

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
	handler := &Apps{Client: clientset, CRClient: testCRClient(app), GitReach: gitAlwaysReachable}

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
	handler := &Apps{Client: clientset, CRClient: testCRClient(app), GitReach: gitAlwaysReachable}

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
	handler := &Apps{Client: clientset, CRClient: testCRClient(app), GitReach: gitAlwaysReachable}

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
	handler := &Apps{Client: fake.NewClientset(), CRClient: testCRClient(app), GitReach: gitAlwaysReachable}

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
	handler := &Apps{Client: fake.NewClientset(), CRClient: testCRClient(app), GitReach: gitAlwaysReachable}

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
	handler := &Apps{Client: clientset, CRClient: testCRClient(app), GitReach: gitAlwaysReachable}

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
	handler := &Apps{Client: fake.NewClientset(), CRClient: testCRClient(), GitReach: gitAlwaysReachable}

	body := strings.NewReader(`{"branch":"main"}`)
	req := httptest.NewRequest("PUT", "/projects/default/apps/missing/git", body)
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// The gap an operator hit: a git source set up by mistake could not be removed
// at all. Clearing it is what lets a webhook deploy the image its pipeline
// built, rather than being diverted into a build.
func TestDeleteGit_RemovesTheSource(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{ //nolint:gosec // G101 false positive: credentialsSecret is a K8s Secret name, not a credential value
				URL:               "https://git.example.com/shop/checkout.git",
				Branch:            "main",
				CredentialsSecret: "checkout-git-credentials",
			},
		},
	}
	handler := &Apps{Client: fake.NewClientset(), CRClient: testCRClient(app), GitReach: gitAlwaysReachable}

	req := httptest.NewRequest("DELETE", "/projects/shop-test/apps/checkout/git", nil)
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var stored kipperv1.App
	require.NoError(t, handler.CRClient.Get(context.Background(),
		crclient.ObjectKey{Namespace: "shop-test", Name: "checkout"}, &stored))
	assert.Nil(t, stored.Spec.Git, "the source is what the caller asked to remove")
	assert.Equal(t, "busybox:latest", stored.Spec.Image, "the image it deploys is not the caller's business here")
}

// Removing a source twice is the same as removing it once: a retried request,
// or a second operator clicking the same button, must not fail.
func TestDeleteGit_IsIdempotent(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec:       kipperv1.AppSpec{Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1)},
	}
	handler := &Apps{Client: fake.NewClientset(), CRClient: testCRClient(app), GitReach: gitAlwaysReachable}

	for range 2 {
		req := httptest.NewRequest("DELETE", "/projects/shop-test/apps/checkout/git", nil)
		rec := httptest.NewRecorder()
		appGitRouter(handler).ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
}

func TestDeleteGit_UnknownAppIsNotFound(t *testing.T) {
	handler := &Apps{Client: fake.NewClientset(), CRClient: testCRClient(), GitReach: gitAlwaysReachable}

	req := httptest.NewRequest("DELETE", "/projects/shop-test/apps/missing/git", nil)
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// The gap: a private repository configured with no token produced an app whose
// every build died at clone, with the reason in a job log in another namespace
// and nothing on the app to say so.
func TestSetGit_RefusesASourceItCannotClone(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec:       kipperv1.AppSpec{Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1)},
	}
	handler := &Apps{
		Client: fake.NewClientset(), CRClient: testCRClient(app),
		GitReach: func(context.Context, string, string, string, string) (gitreach.Result, string) {
			return gitreach.NeedsCredential, "this repository is private, so it needs an access token"
		},
	}

	req := httptest.NewRequest("PUT", "/projects/shop-test/apps/checkout/git",
		strings.NewReader(`{"url":"https://git.example.com/shop/checkout.git"}`))
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "access token")

	var stored kipperv1.App
	require.NoError(t, handler.CRClient.Get(context.Background(),
		crclient.ObjectKey{Namespace: "shop-test", Name: "checkout"}, &stored))
	assert.Nil(t, stored.Spec.Git, "nothing is written when the source cannot be cloned")
}

// A repository this cluster cannot reach has said nothing about itself.
// Refusing on that would make the console stop accepting work whenever its own
// egress is unhappy, which is a worse failure than a build reporting it.
func TestSetGit_AllowsASourceItCannotCheck(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec:       kipperv1.AppSpec{Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1)},
	}
	handler := &Apps{
		Client: fake.NewClientset(), CRClient: testCRClient(app),
		GitReach: func(context.Context, string, string, string, string) (gitreach.Result, string) {
			return gitreach.Unknown, "the repository could not be reached from the cluster"
		},
	}

	req := httptest.NewRequest("PUT", "/projects/shop-test/apps/checkout/git",
		strings.NewReader(`{"url":"https://git.example.com/shop/checkout.git"}`))
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// A rotation is checked with the token being rotated to, so a stale one is
// caught here rather than by the next build.
func TestSetGit_ChecksTheTokenBeingRotatedTo(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git"},
		},
	}
	var sawToken string
	handler := &Apps{
		Client: fake.NewClientset(), CRClient: testCRClient(app),
		GitReach: func(_ context.Context, _, _, _, token string) (gitreach.Result, string) {
			sawToken = token
			return gitreach.NeedsCredential, "the access token was refused by the repository"
		},
	}

	req := httptest.NewRequest("PUT", "/projects/shop-test/apps/checkout/git",
		strings.NewReader(`{"token":"stale"}`))
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	assert.Equal(t, "stale", sawToken, "the token being rotated to is the one that has to work")
	require.Equal(t, http.StatusBadRequest, rec.Code)

	_, err := handler.Client.CoreV1().Secrets("shop-test").Get(context.Background(), "checkout-git-credentials", metav1.GetOptions{})
	assert.Error(t, err, "a token the repository refuses must not be stored")
}

// Create is the path most apps arrive by. Checking only the edit path would
// leave the original failure reachable through the front door: a private
// repository with no token, whose every build dies at clone.
func TestCreateApp_RefusesAGitSourceItCannotClone(t *testing.T) {
	handler := &Apps{
		Client: fake.NewClientset(), CRClient: testCRClient(),
		GitReach: func(context.Context, string, string, string, string) (gitreach.Result, string) {
			return gitreach.NeedsCredential, "this repository is private, so it needs an access token"
		},
	}

	body := `{"name":"checkout","port":8080,"git":{"url":"https://git.example.com/shop/checkout.git"}}`
	req := httptest.NewRequest("POST", "/projects/shop-test/apps", strings.NewReader(body))
	rec := httptest.NewRecorder()

	r := chi.NewRouter()
	r.Post("/projects/{name}/apps", handler.Create)
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "access token")

	_, err := handler.Client.CoreV1().Secrets("shop-test").Get(context.Background(), "checkout-git-credentials", metav1.GetOptions{})
	assert.Error(t, err, "no credential is stored for an app that was refused")
}

// An app may clone with an admin-managed shared credential, which lives in the
// cluster's shared list rather than in the app's namespace. Reading only the
// namespace and then probing anonymously would answer 401 for a repository the
// build clones perfectly well, and refuse an unrelated edit with "this
// repository is private" — wrong, and not actionable.
func TestSetGit_DoesNotProbeAnonymouslyForASharedCredential(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{ //nolint:gosec // G101 false positive: credentialsSecret is a K8s Secret name, not a credential value
				URL:               "https://git.example.com/shop/checkout.git",
				Branch:            "main",
				CredentialsSecret: "shared-git-example",
			},
		},
	}
	var probedWith string
	var probed bool
	handler := &Apps{
		Client: fake.NewClientset(), CRClient: testCRClient(app),
		GitReach: func(_ context.Context, _, _, _, token string) (gitreach.Result, string) {
			probed, probedWith = true, token
			return gitreach.NeedsCredential, "this repository is private, so it needs an access token"
		},
	}

	req := httptest.NewRequest("PUT", "/projects/shop-test/apps/checkout/git",
		strings.NewReader(`{"branch":"release"}`))
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	assert.False(t, probed && probedWith == "",
		"a credential that could not be resolved must not become an anonymous probe")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// When the shared credential is readable, it is the one checked, because it is
// the one the build will use.
func TestSetGit_ChecksTheSharedCredentialItWouldCloneWith(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{ //nolint:gosec // G101 false positive: credentialsSecret is a K8s Secret name, not a credential value
				URL:               "https://git.example.com/shop/checkout.git",
				CredentialsSecret: "shared-git-example",
			},
		},
	}
	shared := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: gitcred.ConfigSecretName, Namespace: gitcred.Namespace},
		Data:       map[string][]byte{"credentials": []byte(`[{"name":"shared-git-example","server":"https://git.example.com","username":"kipper","token":"sh4red","allowedProjects":["shop"]}]`)},
	}
	var probedWith string
	handler := &Apps{
		Client: fake.NewClientset(shared, managedNamespace("shop-test", "shop")), CRClient: testCRClient(app),
		GitReach: func(_ context.Context, _, _, _, token string) (gitreach.Result, string) {
			probedWith = token
			return gitreach.Reachable, ""
		},
	}

	req := httptest.NewRequest("PUT", "/projects/shop-test/apps/checkout/git",
		strings.NewReader(`{"branch":"release"}`))
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "sh4red", probedWith, "the credential the build would use is the one to check")
}

// A host that redirects an authenticated clone onto plaintext has told us
// something about itself. Reporting it as merely unreachable would let the
// create through, and the build's own git follows redirects without any of
// these checks — so the token would go out in clear anyway.
func TestSetGit_RefusesAHostThatRedirectsTheCredentialAway(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec:       kipperv1.AppSpec{Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1)},
	}
	handler := &Apps{
		Client: fake.NewClientset(), CRClient: testCRClient(app),
		GitReach: func(context.Context, string, string, string, string) (gitreach.Result, string) {
			return gitreach.Unsafe, "this host redirects the clone somewhere your access token must not follow"
		},
	}

	req := httptest.NewRequest("PUT", "/projects/shop-test/apps/checkout/git",
		strings.NewReader(`{"url":"https://git.example.com/shop/checkout.git","token":"s3cr3t"}`))
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	var stored kipperv1.App
	require.NoError(t, handler.CRClient.Get(context.Background(),
		crclient.ObjectKey{Namespace: "shop-test", Name: "checkout"}, &stored))
	assert.Nil(t, stored.Spec.Git, "nothing is stored for a host that would leak the token")
}

// managedNamespace is what the builder resolves a project from, and the gate
// below re-reads for the same reason.
func managedNamespace(name, project string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name,
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "kipper",
			"kipper.run/project":           project,
		},
	}}
}

// The escalation: any deployer in the project may rewrite an app's clone URL,
// and the preflight sends the effective credential to whatever host it names.
// Resolving a shared credential without the builder's own gates would hand an
// admin-managed token, belonging to another project's host, to a low-privileged
// operator who simply pointed the app at a server they control.
func TestSetGit_NeverSendsASharedCredentialToAnotherHost(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{ //nolint:gosec // G101 false positive: credentialsSecret is a K8s Secret name, not a credential value
				URL:               "https://git.example.com/shop/checkout.git",
				CredentialsSecret: "shared-git-example",
			},
		},
	}
	shared := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: gitcred.ConfigSecretName, Namespace: gitcred.Namespace},
		Data:       map[string][]byte{"credentials": []byte(`[{"name":"shared-git-example","server":"https://git.example.com","username":"kipper","token":"sh4red","allowedProjects":["shop"]}]`)},
	}
	var probedWith string
	handler := &Apps{
		Client: fake.NewClientset(shared, managedNamespace("shop-test", "shop")), CRClient: testCRClient(app),
		GitReach: func(_ context.Context, _, _, _, token string) (gitreach.Result, string) {
			probedWith = token
			return gitreach.Reachable, ""
		},
	}

	// The deployer points the app at a host they control and sends no token.
	req := httptest.NewRequest("PUT", "/projects/shop-test/apps/checkout/git",
		strings.NewReader(`{"url":"https://git.attacker.example/x/y.git"}`))
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	assert.Empty(t, probedWith,
		"the shared token was sent to a host it is not bound to, disclosing it to whoever runs that host")
}

// The other gate: a credential allow-listed to one project must not be usable
// from another, even against its own host.
func TestSetGit_NeverUsesASharedCredentialFromAProjectItIsNotAllowedIn(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "other-test"},
		Spec: kipperv1.AppSpec{
			Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{ //nolint:gosec // G101 false positive: credentialsSecret is a K8s Secret name, not a credential value
				URL:               "https://git.example.com/shop/checkout.git",
				CredentialsSecret: "shared-git-example",
			},
		},
	}
	shared := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: gitcred.ConfigSecretName, Namespace: gitcred.Namespace},
		Data:       map[string][]byte{"credentials": []byte(`[{"name":"shared-git-example","server":"https://git.example.com","username":"kipper","token":"sh4red","allowedProjects":["shop"]}]`)},
	}
	var probedWith string
	handler := &Apps{
		Client: fake.NewClientset(shared, managedNamespace("other-test", "other")), CRClient: testCRClient(app),
		GitReach: func(_ context.Context, _, _, _, token string) (gitreach.Result, string) {
			probedWith = token
			return gitreach.Reachable, ""
		},
	}

	req := httptest.NewRequest("PUT", "/projects/other-test/apps/checkout/git",
		strings.NewReader(`{"branch":"release"}`))
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	assert.Empty(t, probedWith, "a credential allow-listed to one project was used from another")
}

// A branch that cannot exist needs no host to be known impossible. Letting it
// through because the remote check timed out stores input the builder already
// rejects, so the edit succeeds and the app can never build.
func TestSetGit_RefusesAnImpossibleBranchEvenWhenTheHostIsUnreachable(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "main"},
		},
	}
	handler := &Apps{
		Client: fake.NewClientset(), CRClient: testCRClient(app),
		GitReach: func(context.Context, string, string, string, string) (gitreach.Result, string) {
			return gitreach.Unknown, "the repository did not answer in time"
		},
	}

	req := httptest.NewRequest("PUT", "/projects/shop-test/apps/checkout/git",
		strings.NewReader(`{"branch":"release..candidate"}`))
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	var stored kipperv1.App
	require.NoError(t, handler.CRClient.Get(context.Background(),
		crclient.ObjectKey{Namespace: "shop-test", Name: "checkout"}, &stored))
	assert.Equal(t, "main", stored.Spec.Git.Branch, "an impossible branch must not be stored")
}

// A token stored against a host the preflight could not reach is not a token
// that can go anywhere: the build releases it through a credential helper
// bound to the clone URL's own host and to https. Refusing here would mean a
// cluster whose console egress differs from its build namespace's could never
// rotate a token while its builds worked.
func TestSetGit_StoresANewTokenEvenWhenTheHostCannotBeChecked(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "main"},
		},
	}
	handler := &Apps{
		Client: fake.NewClientset(), CRClient: testCRClient(app),
		GitReach: func(context.Context, string, string, string, string) (gitreach.Result, string) {
			return gitreach.Unknown, "the repository did not answer in time"
		},
	}

	req := httptest.NewRequest("PUT", "/projects/shop-test/apps/checkout/git",
		strings.NewReader(`{"token":"ghp_new"}`))
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, err := handler.Client.CoreV1().Secrets("shop-test").Get(context.Background(), "checkout-git-credentials", metav1.GetOptions{})
	assert.NoError(t, err, "an unreachable host must not stop an operator rotating a credential")
}

// An edit that carries no new secret still goes through when the host cannot be
// checked, or a network blip would stop an operator changing a branch.
func TestSetGit_StillAllowsAnEditWithNoNewTokenWhenUnreachable(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "main"},
		},
	}
	handler := &Apps{
		Client: fake.NewClientset(), CRClient: testCRClient(app),
		GitReach: func(context.Context, string, string, string, string) (gitreach.Result, string) {
			return gitreach.Unknown, "the repository could not be reached from the cluster"
		},
	}

	req := httptest.NewRequest("PUT", "/projects/shop-test/apps/checkout/git",
		strings.NewReader(`{"branch":"release"}`))
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// An omitted branch means the repository's default, which the builder resolves
// to main. Validating "main" locally and then asking the remote about "" meant
// the branch-existence check never ran for exactly the apps that did not name
// one, so a repository with no main passed the front door and could not build.
func TestSetGit_AsksTheRemoteAboutTheBranchItWillActuallyBuild(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git"},
		},
	}
	var askedAbout string
	handler := &Apps{
		Client: fake.NewClientset(), CRClient: testCRClient(app),
		GitReach: func(_ context.Context, _, branch, _, _ string) (gitreach.Result, string) {
			askedAbout = branch
			return gitreach.Reachable, ""
		},
	}

	req := httptest.NewRequest("PUT", "/projects/shop-test/apps/checkout/git",
		strings.NewReader(`{"url":"https://git.example.com/shop/checkout.git"}`))
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "main", askedAbout, "an empty branch skips the existence check entirely")
}

func TestCreateApp_AsksTheRemoteAboutTheBranchItWillActuallyBuild(t *testing.T) {
	var askedAbout string
	handler := &Apps{
		Client: fake.NewClientset(), CRClient: testCRClient(),
		GitReach: func(_ context.Context, _, branch, _, _ string) (gitreach.Result, string) {
			askedAbout = branch
			return gitreach.Reachable, ""
		},
	}

	body := `{"name":"checkout","port":8080,"git":{"url":"https://git.example.com/shop/checkout.git"}}`
	req := httptest.NewRequest("POST", "/projects/shop-test/apps", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r := chi.NewRouter()
	r.Post("/projects/{name}/apps", handler.Create)
	r.ServeHTTP(rec, req)

	assert.Equal(t, "main", askedAbout, "an empty branch skips the existence check entirely")
}

// The webhook and both CLI writers refuse to set an image on a git-backed app.
// The console's own image update was the one writer still doing it silently,
// so the console kept exactly the experience the rest of the change ends.
func TestUpdateImage_RefusesWhileTheAppBuildsFromGit(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git"},
		},
	}
	handler := &Apps{Client: fake.NewClientset(), CRClient: testCRClient(app), GitReach: gitAlwaysReachable}

	req := httptest.NewRequest("PUT", "/projects/shop-test/apps/checkout/image",
		strings.NewReader(`{"image":"registry.example.com/shop/checkout:9f2c1a"}`))
	rec := httptest.NewRecorder()
	r := chi.NewRouter()
	r.Put("/projects/{name}/apps/{app}/image", handler.UpdateImage)
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	var stored kipperv1.App
	require.NoError(t, handler.CRClient.Get(context.Background(),
		crclient.ObjectKey{Namespace: "shop-test", Name: "checkout"}, &stored))
	assert.Equal(t, "busybox:latest", stored.Spec.Image, "nothing is written when the app builds its own image")
}

// Create's local validation would survive its own removal without this: the
// preflight spy answers Reachable, so nothing else would refuse an impossible
// branch on the path most apps arrive by.
func TestCreateApp_RefusesAnImpossibleBranchWithoutAskingTheRemote(t *testing.T) {
	asked := false
	handler := &Apps{
		Client: fake.NewClientset(), CRClient: testCRClient(),
		GitReach: func(context.Context, string, string, string, string) (gitreach.Result, string) {
			asked = true
			return gitreach.Reachable, ""
		},
	}

	body := `{"name":"checkout","port":8080,"git":{"url":"https://git.example.com/shop/checkout.git","branch":"release..candidate"}}`
	req := httptest.NewRequest("POST", "/projects/shop-test/apps", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r := chi.NewRouter()
	r.Post("/projects/{name}/apps", handler.Create)
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.False(t, asked, "a branch that cannot exist needs no network round trip to refuse")
}

// A URL and a token changed together are a pair, and the CR is what says which
// host the token is for. If the CR update fails after the token is written, the
// new token sits beside the old URL — and the builder offers a credential to
// whatever host the live CR names, so the next build would hand the new host's
// token to the old one.
func TestSetGit_PutsTheTokenBackWhenTheSourceUpdateFails(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{ //nolint:gosec // G101 false positive: credentialsSecret is a K8s Secret name, not a credential value
				URL:               "https://old-git.example.com/shop/checkout.git",
				CredentialsSecret: "checkout-git-credentials",
			},
		},
	}
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-git-credentials", Namespace: "shop-test"},
		Data:       map[string][]byte{"token": []byte("token-for-the-old-host")},
	}
	handler := &Apps{
		Client:   fake.NewClientset(existing),
		CRClient: refusingCRClient(testCRClient(app)),
		GitReach: gitAlwaysReachable,
	}

	req := httptest.NewRequest("PUT", "/projects/shop-test/apps/checkout/git",
		strings.NewReader(`{"url":"https://new-git.example.com/shop/checkout.git","token":"token-for-the-new-host"}`))
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)

	stored, err := handler.Client.CoreV1().Secrets("shop-test").Get(context.Background(), "checkout-git-credentials", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "token-for-the-old-host", string(stored.Data["token"]),
		"the new host's token was left paired with the old host's URL")
}

// The same, for an app that had no token before: there is nothing to put back,
// so the Secret goes rather than being left behind unreferenced.
func TestSetGit_RemovesAFirstTokenWhenTheSourceUpdateFails(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git"},
		},
	}
	handler := &Apps{
		Client:   fake.NewClientset(),
		CRClient: refusingCRClient(testCRClient(app)),
		GitReach: gitAlwaysReachable,
	}

	req := httptest.NewRequest("PUT", "/projects/shop-test/apps/checkout/git",
		strings.NewReader(`{"token":"ghp_first"}`))
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	_, err := handler.Client.CoreV1().Secrets("shop-test").Get(context.Background(), "checkout-git-credentials", metav1.GetOptions{})
	assert.Error(t, err, "a token nothing references was left in the namespace")
}

// refusingCRClient fails every Update, so a test can drive what the handler
// does when the CR write does not land. A resource-version race and a
// transient API error both arrive this way.
type refusingUpdates struct{ crclient.Client }

func (refusingUpdates) Update(context.Context, crclient.Object, ...crclient.UpdateOption) error {
	return fmt.Errorf("the object has been modified; please apply your changes to the latest version and try again")
}

func refusingCRClient(inner crclient.Client) crclient.Client {
	return refusingUpdates{Client: inner}
}

// Two operators changing the same source overlap by ordinary Kubernetes
// conflict. The one that loses the CR race must not roll its token back over
// the one that won: that would put a token nobody chose beside a URL somebody
// did, which is the mismatch the rollback exists to prevent, on a transaction
// that succeeded.
func TestRestoreGitCredentialLeavesALaterWriteAlone(t *testing.T) {
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout-git-credentials", Namespace: "shop-test",
			ResourceVersion: "200", // somebody wrote after us
		},
		Data: map[string][]byte{"token": []byte("token-b")},
	})
	a := &Apps{Client: client}

	// We wrote version 100 and lost the CR race; version 200 is live.
	err := a.restoreGitCredential(context.Background(), "shop-test", "checkout-git-credentials",
		[]byte("token-old"), "", true, "100", true)

	require.NoError(t, err)
	stored, getErr := client.CoreV1().Secrets("shop-test").Get(context.Background(), "checkout-git-credentials", metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Equal(t, "token-b", string(stored.Data["token"]),
		"a losing writer rolled its token back over a write that had already landed")
}

// When ours is still the live write, the rollback does its job.
func TestRestoreGitCredentialPutsBackItsOwnWrite(t *testing.T) {
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout-git-credentials", Namespace: "shop-test",
			ResourceVersion: "100",
		},
		Data: map[string][]byte{"token": []byte("token-a")},
	})
	a := &Apps{Client: client}

	err := a.restoreGitCredential(context.Background(), "shop-test", "checkout-git-credentials",
		[]byte("token-old"), "", true, "100", true)

	require.NoError(t, err)
	stored, getErr := client.CoreV1().Secrets("shop-test").Get(context.Background(), "checkout-git-credentials", metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Equal(t, "token-old", string(stored.Data["token"]))
}

// A Secret this request created, which another writer has since committed and
// the live CR now references, must not be deleted by the rollback.
func TestRestoreGitCredentialDoesNotDeleteALaterWrite(t *testing.T) {
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout-git-credentials", Namespace: "shop-test",
			ResourceVersion: "200",
		},
		Data: map[string][]byte{"token": []byte("token-b")},
	})
	a := &Apps{Client: client}

	err := a.restoreGitCredential(context.Background(), "shop-test", "checkout-git-credentials",
		nil, "", false, "100", true)

	require.NoError(t, err)
	_, getErr := client.CoreV1().Secrets("shop-test").Get(context.Background(), "checkout-git-credentials", metav1.GetOptions{})
	assert.NoError(t, getErr, "a token another writer committed was deleted by a losing writer's rollback")
}

// The per-app twin of the shared-credential escalation. Any deployer may change
// an app's clone URL and none of them needs to know its token to do it, while
// the API treats that token as write-only: reading it back needs a password
// re-entry.
//
// Declining to probe is not enough. Accepting the change would store the CR
// naming the new host beside a reference to the old host's token, and every
// later build and status probe resolves that token against whatever host the
// CR names — so the move itself is refused, and the assertion is on what was
// persisted rather than on what the handler happened to send.
func TestSetGit_RefusesToMoveAnAppAwayFromItsOwnCredential(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{ //nolint:gosec // G101 false positive: credentialsSecret is a K8s Secret name, not a credential value
				URL:               "https://git.example.com/shop/checkout.git",
				CredentialsSecret: "checkout-git-credentials",
			},
		},
	}
	stored := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-git-credentials", Namespace: "shop-test"},
		Data:       map[string][]byte{"token": []byte("ghp_private")},
	}
	var probedWith string
	handler := &Apps{
		Client: fake.NewClientset(stored), CRClient: testCRClient(app),
		GitReach: func(_ context.Context, _, _, _, token string) (gitreach.Result, string) {
			probedWith = token
			return gitreach.Reachable, ""
		},
	}

	req := httptest.NewRequest("PUT", "/projects/shop-test/apps/checkout/git",
		strings.NewReader(`{"url":"https://attacker.example/x/y.git"}`))
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Empty(t, probedWith, "the app's token was sent to a host the caller named")

	// The state is what matters: a persisted CR naming the new host beside the
	// old credential is a standing exfiltration path through the build and the
	// status probe, whatever this handler did.
	var persisted kipperv1.App
	require.NoError(t, handler.CRClient.Get(context.Background(),
		crclient.ObjectKey{Namespace: "shop-test", Name: "checkout"}, &persisted))
	assert.Equal(t, "https://git.example.com/shop/checkout.git", persisted.Spec.Git.URL,
		"the new host was stored beside a credential belonging to the old one")
	assert.Equal(t, "checkout-git-credentials", persisted.Spec.Git.CredentialsSecret)
}

// Supplying a token for the new host is the way through: the caller has a
// credential for where they are going, so nothing of the old one travels.
func TestSetGit_AllowsAMoveThatBringsItsOwnToken(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{ //nolint:gosec // G101 false positive: credentialsSecret is a K8s Secret name, not a credential value
				URL:               "https://git.example.com/shop/checkout.git",
				CredentialsSecret: "checkout-git-credentials",
			},
		},
	}
	stored := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-git-credentials", Namespace: "shop-test"},
		Data:       map[string][]byte{"token": []byte("ghp_old")},
	}
	var probedWith string
	handler := &Apps{
		Client: fake.NewClientset(stored), CRClient: testCRClient(app),
		GitReach: func(_ context.Context, _, _, _, token string) (gitreach.Result, string) {
			probedWith = token
			return gitreach.Reachable, ""
		},
	}

	req := httptest.NewRequest("PUT", "/projects/shop-test/apps/checkout/git",
		strings.NewReader(`{"url":"https://new-git.example.com/shop/checkout.git","token":"ghp_new"}`))
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "ghp_new", probedWith, "the credential offered to the new host is the one given for it")
}

// The same repository, spelt differently, is the same authority: an edit that
// only changes the branch or normalises the URL still gets checked with the
// credential the build will use.
func TestSetGit_StillUsesThePerAppTokenForItsOwnHost(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec: kipperv1.AppSpec{
			Image: "busybox:latest", Port: 8080, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{ //nolint:gosec // G101 false positive: credentialsSecret is a K8s Secret name, not a credential value
				URL:               "https://git.example.com/shop/checkout.git",
				CredentialsSecret: "checkout-git-credentials",
			},
		},
	}
	stored := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-git-credentials", Namespace: "shop-test"},
		Data:       map[string][]byte{"token": []byte("ghp_private")},
	}
	var probedWith string
	handler := &Apps{
		Client: fake.NewClientset(stored), CRClient: testCRClient(app),
		GitReach: func(_ context.Context, _, _, _, token string) (gitreach.Result, string) {
			probedWith = token
			return gitreach.Reachable, ""
		},
	}

	req := httptest.NewRequest("PUT", "/projects/shop-test/apps/checkout/git",
		strings.NewReader(`{"url":"https://git.example.com/shop/checkout-renamed.git"}`))
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "ghp_private", probedWith, "a credential still travels to its own host")
}

// The write half of the pairing check. Two overlapping source changes whose CR
// updates both fail can leave one host's token beside another host's URL: the
// rollback's resource-version precondition proves another write happened, never
// that its CR update landed. Recording the host the token was stored for is
// what lets the build catch that before the token travels, so every write of a
// per-app credential has to record it, including a move to a new host.
func TestSetGit_RecordsTheHostATokenWasStoredFor(t *testing.T) {
	app := &kipperv1.App{
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
	clientset := fake.NewClientset()
	handler := &Apps{Client: clientset, CRClient: testCRClient(app), GitReach: gitAlwaysReachable}

	body := strings.NewReader(`{"url":"https://git.example.com/acme/web.git","token":"a-token-for-the-new-host"}`)
	req := httptest.NewRequest("PUT", "/projects/default/apps/web/git", body)
	rec := httptest.NewRecorder()
	appGitRouter(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	secret, err := clientset.CoreV1().Secrets("default").Get(context.Background(), "web-git-credentials", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "git.example.com", secret.Annotations[builder.GitAuthorityAnnotation],
		"the token was stored with no record of which host it is for")
}

// The precondition is the whole safety of the conditional rollback, and an
// unreadable version is not a licence to skip it. A transient Get failure after
// the write left `written` empty, which turned the rollback unconditional: it
// then put its old token back over a write that had landed, while leaving that
// winner's recorded host in place. Host and URL agree, so the build sends a
// token belonging to neither, and the binding cannot see it.
func TestRestoreGitCredentialDoesNothingWhenItsOwnWriteCannotBeIdentified(t *testing.T) {
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout-git-credentials", Namespace: "shop-test",
			ResourceVersion: "200",
			Annotations:     map[string]string{builder.GitAuthorityAnnotation: "git.winner.example.com"},
		},
		Data: map[string][]byte{"token": []byte("the-winners-token")},
	})
	a := &Apps{Client: client}

	err := a.restoreGitCredential(context.Background(), "shop-test", "checkout-git-credentials",
		[]byte("our-old-token"), "git.ours.example.com", true, "", false)

	require.NoError(t, err)
	stored, getErr := client.CoreV1().Secrets("shop-test").Get(context.Background(), "checkout-git-credentials", metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Equal(t, "the-winners-token", string(stored.Data["token"]),
		"a rollback that could not identify its own write clobbered one that had landed")
	assert.Equal(t, "git.winner.example.com", stored.Annotations[builder.GitAuthorityAnnotation])
}

// The token and the host it is for are one value in two fields, so a rollback
// that puts back half of them leaves a pairing that is correct and is refused:
// the old token beside the attempted host, which no longer matches the URL the
// app kept.
func TestRestoreGitCredentialPutsBackTheHostAsWellAsTheToken(t *testing.T) {
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout-git-credentials", Namespace: "shop-test",
			ResourceVersion: "100",
			Annotations:     map[string]string{builder.GitAuthorityAnnotation: "git.attempted.example.com"},
		},
		Data: map[string][]byte{"token": []byte("the-token-we-wrote")},
	})
	a := &Apps{Client: client}

	err := a.restoreGitCredential(context.Background(), "shop-test", "checkout-git-credentials",
		[]byte("our-old-token"), "git.original.example.com", true, "100", true)

	require.NoError(t, err)
	stored, getErr := client.CoreV1().Secrets("shop-test").Get(context.Background(), "checkout-git-credentials", metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Equal(t, "our-old-token", string(stored.Data["token"]))
	assert.Equal(t, "git.original.example.com", stored.Annotations[builder.GitAuthorityAnnotation],
		"the token went back but the host it is for did not, so the build refuses a pairing that is correct")
}

// The preflight is the third path that sends the token, after the build and the
// health probe. Its own guard asks whether the credential follows a URL change,
// which is a different question: a branch-only edit leaves the URL alone, so
// that guard passes and a token stranded by a rolled-back move travels to the
// host the app still names.
func TestSetGit_DoesNotSendACredentialBoundElsewhereToThePreflight(t *testing.T) {
	app := &kipperv1.App{
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
	stranded := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-git-credentials", Namespace: "default",
			Annotations: map[string]string{builder.GitAuthorityAnnotation: "git.elsewhere.example.com"},
		},
		Data: map[string][]byte{"token": []byte("a-token-for-the-other-host")},
	}

	var sawToken string
	handler := &Apps{
		Client: fake.NewClientset(stranded), CRClient: testCRClient(app),
		GitReach: func(_ context.Context, _, _, _, token string) (gitreach.Result, string) {
			sawToken = token
			return gitreach.Reachable, ""
		},
	}

	body := strings.NewReader(`{"branch":"release"}`)
	req := httptest.NewRequest("PUT", "/projects/default/apps/web/git", body)
	appGitRouter(handler).ServeHTTP(httptest.NewRecorder(), req)

	assert.Empty(t, sawToken, "a token stored for another host was sent to the one the app names")
}

// The preflight resolved credentials in its own order: any same-named Secret in
// the namespace first, the administrator's shared list second. Secret names are
// namespace-local, so a project holding its own `corp-git` shadows the shared
// entry of that name — and unlike the builder, which demands the conventional
// per-app name before it will read a namespaced Secret, the preflight read
// whatever it found and sent it. A deployer moves the app to a host they
// control, which the move guard refuses so the URL persists unchecked, then
// edits the branch: the URLs now agree, the unrelated Secret carries no
// binding, and its token goes to them as basic auth.
func TestSetGit_DoesNotSendANamespaceSecretShadowingASharedCredential(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: kipperv1.AppSpec{
			Image: "nginx", Port: 80, Replicas: int32Ptr(1),
			Git: &kipperv1.AppGitSource{
				URL:               "https://git.attacker.example.com/acme/web.git",
				Branch:            "main",
				CredentialsSecret: "corp-git",
			},
		},
	}
	// Unrelated to git: a Secret the project happens to own under the name the
	// administrator also used for a shared credential.
	collision := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "corp-git", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("an-unrelated-secret-value")},
	}

	var sawToken string
	handler := &Apps{
		Client: fake.NewClientset(collision), CRClient: testCRClient(app),
		GitReach: func(_ context.Context, _, _, _, token string) (gitreach.Result, string) {
			sawToken = token
			return gitreach.Reachable, ""
		},
	}

	body := strings.NewReader(`{"branch":"release"}`)
	req := httptest.NewRequest("PUT", "/projects/default/apps/web/git", body)
	appGitRouter(handler).ServeHTTP(httptest.NewRecorder(), req)

	assert.Empty(t, sawToken,
		"a namespaced Secret the builder would refuse to read was sent to the clone host")
}
