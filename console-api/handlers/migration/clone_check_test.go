package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/builder"
	"github.com/getkipper/kipper/console-api/internal/gitreach"
	"github.com/getkipper/kipper/controller/pkg/sharedcred"
)

// stubReachGit rejects URLs listed in refused and accepts all others, recording
// the token used for each URL. Read the record only after checkClones returns.
func stubReachGit(t *testing.T, refused map[string]string) map[string]string {
	t.Helper()
	asked := map[string]string{}
	var mu sync.Mutex
	original := builder.ReachGit
	builder.ReachGit = func(_ context.Context, url, _, _, token string) (gitreach.Result, string) {
		mu.Lock()
		defer mu.Unlock()
		asked[url] = token
		if detail, ok := refused[url]; ok {
			return gitreach.NeedsCredential, detail
		}
		return gitreach.Reachable, ""
	}
	t.Cleanup(func() { builder.ReachGit = original })
	return asked
}

func gitApp(name, url, credential string) *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop-prod"},
		Spec: kipperv1.AppSpec{
			Image: "registry.example.com/" + name + ":latest",
			Git:   &kipperv1.AppGitSource{URL: url, CredentialsSecret: credential},
		},
	}
}

// Checking the token during planning prevents a refusal during target app
// creation after the databases have already been copied.
func TestPlanBlocksAnAppWhoseRepositoryRefusesItsToken(t *testing.T) {
	asked := stubReachGit(t, map[string]string{
		"https://git.example.com/shop/web.git": "the access token was refused by the repository",
	})
	web := gitApp("web", "https://git.example.com/shop/web.git", "web-git-credentials")
	api := gitApp("api", "https://git.example.com/shop/api.git", "")
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "web-git-credentials", Namespace: "shop-prod"},
		Data:       map[string][]byte{"token": []byte("a-revoked-token")},
	}
	h := &Handler{
		Client: fake.NewSimpleClientset(projectNamespace("shop-prod", "shop"), token),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).
			WithObjects(web, api, ownerOf("shop-prod")).Build(),
		Domain: "source.example.com",
	}
	tok, _ := testToken(t, "https://api.target.example.com")
	resp := &planResponse{}
	require.NoError(t, h.planProject(context.Background(), "shop", tok, nil, resp))

	h.checkClones(context.Background(), resp)

	var blockers []string
	for _, b := range resp.Blockers {
		if strings.Contains(b, "cannot be rebuilt") {
			blockers = append(blockers, b)
		}
	}
	require.Len(t, blockers, 1, "only the refused app blocks: %q", resp.Blockers)
	assert.Contains(t, blockers[0], "shop-prod/web")
	assert.Contains(t, blockers[0], "refused")
	assert.Equal(t, "a-revoked-token", asked["https://git.example.com/shop/web.git"])
	assert.Contains(t, asked, "https://git.example.com/shop/api.git", "an app without a credential is asked anonymously")
}

// The target uses its own shared token, so checking the source's token would
// not establish whether the target can build the app.
func TestPlanLeavesAnAppOnASharedCredentialToTheTarget(t *testing.T) {
	asked := stubReachGit(t, nil)
	app := gitApp("web", "https://git.example.com/shop/web.git", "forge")
	h := &Handler{
		Client: fake.NewSimpleClientset(projectNamespace("shop-prod", "shop"),
			sharedCredentials(t, sharedcred.Entry{Name: "forge", Server: "https://git.example.com", Token: "source-token", AllowedProjects: []string{"shop"}})),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).
			WithObjects(app, ownerOf("shop-prod")).Build(),
		Domain: "source.example.com",
	}
	shared, err := sharedcred.Load(context.Background(), h.Client)
	require.NoError(t, err)
	tok, _ := testToken(t, "https://api.target.example.com")
	resp := &planResponse{sourceCredentials: shared}
	require.NoError(t, h.planProject(context.Background(), "shop", tok, nil, resp))

	h.checkClones(context.Background(), resp)

	assert.Empty(t, asked)
	assert.Len(t, resp.sharedGitCredentials, 1)
}

func TestPlanBlocksAnAppWhoseOwnCredentialIsMissing(t *testing.T) {
	stubReachGit(t, nil)
	app := gitApp("web", "https://git.example.com/shop/web.git", "web-git-credentials")
	h := &Handler{
		Client: fake.NewSimpleClientset(projectNamespace("shop-prod", "shop")),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).
			WithObjects(app, ownerOf("shop-prod")).Build(),
	}
	resp := &planResponse{cloneChecks: []*kipperv1.App{app}}

	h.checkClones(context.Background(), resp)

	require.Len(t, resp.Blockers, 1)
	assert.Contains(t, resp.Blockers[0], "shop-prod/web")
}

func TestPlanBlocksEveryRefusedAppInInventoryOrder(t *testing.T) {
	refused := map[string]string{}
	var apps []*kipperv1.App
	for i := range 12 {
		url := fmt.Sprintf("https://git.example.com/shop/app%02d.git", i)
		if i%3 == 0 {
			refused[url] = "this repository is private, so it needs an access token"
		}
		apps = append(apps, gitApp(fmt.Sprintf("app%02d", i), url, ""))
	}
	stubReachGit(t, refused)
	h := &Handler{Client: fake.NewSimpleClientset(), CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).Build()}
	resp := &planResponse{cloneChecks: apps}

	h.checkClones(context.Background(), resp)

	require.Len(t, resp.Blockers, 4)
	for i, want := range []string{"app00", "app03", "app06", "app09"} {
		assert.Contains(t, resp.Blockers[i], "shop-prod/"+want)
	}
	assert.Empty(t, resp.Warnings)
}

// A token can sit in the query string of a git URL, which the source
// validation accepts, so a blocker must never quote the URL.
func TestPlanBlockerNeverQuotesTheGitURL(t *testing.T) {
	const url = "https://git.example.com/shop/web.git?access_token=sentinel-secret"
	stubReachGit(t, map[string]string{url: "the access token was refused by the repository"})
	h := &Handler{Client: fake.NewSimpleClientset(), CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).Build()}
	resp := &planResponse{cloneChecks: []*kipperv1.App{gitApp("web", url, "")}}

	h.checkClones(context.Background(), resp)

	require.Len(t, resp.Blockers, 1)
	assert.NotContains(t, resp.Blockers[0], "sentinel-secret")
}

// Canceled checks allow the plan to proceed, as with unreachable hosts,
// whether or not the app has its own credential.
func TestPlanWarnsAboutAppsItRanOutOfTimeToCheck(t *testing.T) {
	asked := stubReachGit(t, nil)
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "web-git-credentials", Namespace: "shop-prod"},
		Data:       map[string][]byte{"token": []byte("a-token")},
	}
	var apps []*kipperv1.App
	for i := range 10 {
		apps = append(apps, gitApp(fmt.Sprintf("app%d", i), fmt.Sprintf("https://git.example.com/shop/app%d.git", i), ""))
	}
	apps = append(apps, gitApp("web", "https://git.example.com/shop/web.git", "web-git-credentials"))
	h := &Handler{Client: fake.NewSimpleClientset(token), CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).Build()}
	resp := &planResponse{cloneChecks: apps}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h.checkClones(ctx, resp)

	assert.Empty(t, resp.Blockers)
	require.Len(t, resp.Warnings, 1)
	assert.Contains(t, resp.Warnings[0], "shop-prod/app0")
	assert.Contains(t, resp.Warnings[0], "shop-prod/web")
	assert.Empty(t, asked)
}

// TestRanOutOfTimeChecksLeaveThePlanDigestAlone verifies that slow git hosts
// leave the digest stable, even when they change the timeout warning or could
// affect later checks. Start rebuilds the plan and compares digests, so timing
// differences must not cause it to report a changed plan.
func TestRanOutOfTimeChecksLeaveThePlanDigestAlone(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/capacity"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"allocatable_cpu_millis": 8000, "allocatable_memory_bytes": 16 << 30, "allocatable_storage_bytes": 100 << 30,
				"requested_cpu_millis": 0, "requested_memory_bytes": 0, "requested_storage_bytes": 0,
				"target_version": "dev", "keeps_stops": true,
			})
		case strings.HasSuffix(r.URL.Path, "/projects"):
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(target.Close)
	tok, _ := testToken(t, target.URL)

	// Honor cancellation in the fake client so later reads fail as they would
	// with a canceled API request.
	honorCancel := interceptor.Funcs{
		Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c crclient.WithWatch, list crclient.ObjectList, opts ...crclient.ListOption) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return c.List(ctx, list, opts...)
		},
	}
	plan := func(slowHost bool) *planResponse {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		original := builder.ReachGit
		builder.ReachGit = func(context.Context, string, string, string, string) (gitreach.Result, string) {
			if slowHost {
				cancel()
			}
			return gitreach.Reachable, ""
		}
		defer func() { builder.ReachGit = original }()
		h := &Handler{
			Client: fakeClientWithProject(t),
			CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).
				WithObjects(ownerOf("shop-prod"), gitApp("web", "https://git.example.com/shop/web.git", "")).
				WithInterceptorFuncs(honorCancel).Build(),
			Sessions: NewSessionStore(),
			Domain:   "source.example.com",
		}
		return h.buildPlan(ctx, planClaims("admin@example.com"), tok, []string{"shop"}, nil, nil, false)
	}

	quick, slow := plan(false), plan(true)

	require.Empty(t, quick.Blockers)
	require.Empty(t, slow.Blockers)
	assert.Len(t, slow.Warnings, len(quick.Warnings)+1, "the slow plan should have exactly one additional warning for unfinished repository checks")
	assert.Equal(t, planDigest(quick), planDigest(slow))
}
