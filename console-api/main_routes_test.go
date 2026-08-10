package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

// Under /projects/{name} the segment means a project name on the routes that
// act on the Project and an environment namespace on the routes that act on
// workloads inside one. The two take different gates, and putting a route in
// the wrong group is not a subtle failure: it either locks a project out of its
// own environment or lets a same-named project reach into it.
//
// This drives the router main() actually serves. Mounting the two groups again
// in a test proves only that the test agrees with itself — the classification
// lives in buildRouter, so that is what has to be exercised.
//
// The collision under test: project "shop" owns namespace "shop-prod" for its
// prod environment, and a Project called "shop-prod" exists beside it. Their
// members are disjoint.
//
// Every GET under /projects/{name} that needs no further path segment is
// listed. The ones that do — /apps/{app}, /functions/{fn} and their descendants
// — inherit their group from the parent, so the parent standing in for them is
// sound; a route moved between groups on its own would still escape this, which
// enumerating the tree would catch and this cannot, because buildRouter returns
// a closure over the mux rather than the mux itself.
func TestTheRouterGatesEachProjectRouteByWhatItsNameMeans(t *testing.T) {
	const (
		shopOwner = "shopowner@test.com"
		sprOwner  = "sprowner@test.com"
	)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating a signing key: %v", err)
	}
	dex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA",
			"kid": "test",
			"use": "sig",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	defer dex.Close()

	t.Setenv("DEX_ISSUER", dex.URL)
	t.Setenv("DEX_CLIENT_ID", "kipper-console")
	t.Setenv("CLUSTER_DOMAIN", "example.com")

	tokenFor := func(email string) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": dex.URL, "aud": "kipper-console", "sub": email,
			"email": email, "email_verified": true,
			"exp": time.Now().Add(time.Hour).Unix(),
		})
		tok.Header["kid"] = "test"
		signed, serr := tok.SignedString(key)
		if serr != nil {
			t.Fatalf("signing a token: %v", serr)
		}
		return signed
	}

	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kipper-system"}},
		// shop reached the namespace first, so it carries shop's label.
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: "shop-prod", Labels: map[string]string{kipperlabels.Project: "shop"}}},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "kipper-users", Namespace: "kipper-system"},
			Data: map[string]string{"users": fmt.Sprintf(
				`{%q:"deployer",%q:"deployer"}`, shopOwner, sprOwner)},
		},
	)

	scheme := runtime.NewScheme()
	if err := kipperv1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering the scheme: %v", err)
	}
	member := func(email string) []kipperv1.ProjectMember {
		return []kipperv1.ProjectMember{{Email: email, Role: kipperv1.ProjectRoleOwner}}
	}
	crClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "shop"},
			Spec: kipperv1.ProjectSpec{
				Environments: []kipperv1.ProjectEnvironment{{Name: "prod"}},
				Members:      member(shopOwner),
			},
		},
		&kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "shop-prod"},
			Spec:       kipperv1.ProjectSpec{Members: member(sprOwner)},
		},
	).Build()

	dynClient := dynfake.NewSimpleDynamicClient(scheme)
	// buildRouter starts the migration sweepers, and the first sweep runs at
	// once rather than on the first tick. Stopping them with the test keeps
	// them from reaping against a later test's fixtures.
	ctx, stopSweepers := context.WithCancel(context.Background())
	t.Cleanup(stopSweepers)
	router := buildRouter(ctx, clientset, dynClient, &rest.Config{Host: "https://127.0.0.1:6443"}, crClient)

	call := func(method, path, email string) int {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+tokenFor(email))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	// A route whose {name} is the project's own name answers to that project's
	// members. shop owning the namespace of the same name confers nothing.
	t.Run("project-name routes gate on the project", func(t *testing.T) {
		for _, path := range []string{
			"/api/v1/projects/shop-prod/members",
			"/api/v1/projects/shop-prod/link-consent",
			"/api/v1/projects/shop-prod/quota",
			"/api/v1/projects/shop-prod/requests",
			"/api/v1/projects/shop-prod/copy-preview",
		} {
			if code := call("GET", path, shopOwner); code != http.StatusForbidden {
				t.Errorf("GET %s as shop's owner = %d, want 403: owning the namespace must not reach the Project",
					path, code)
			}
			if code := call("GET", path, sprOwner); code == http.StatusForbidden {
				t.Errorf("GET %s as shop-prod's own owner = 403, want it allowed through the gate", path)
			}
		}
	})

	// A route whose {name} is an environment namespace answers to the project
	// that owns that namespace, which is the other way round.
	t.Run("namespace routes gate on the namespace's owner", func(t *testing.T) {
		for _, path := range []string{
			"/api/v1/projects/shop-prod/apps",
			"/api/v1/projects/shop-prod/functions",
			"/api/v1/projects/shop-prod/volumes",
			"/api/v1/projects/shop-prod/api-keys",
			"/api/v1/projects/shop-prod/usage-plans",
		} {
			if code := call("GET", path, shopOwner); code == http.StatusForbidden {
				t.Errorf("GET %s as shop's owner = 403: shop owns this namespace and must reach its own workloads",
					path)
			}
			if code := call("GET", path, sprOwner); code != http.StatusForbidden {
				t.Errorf("GET %s as shop-prod's owner = %d, want 403: the project of that name owns no such namespace",
					path, code)
			}
		}
	})

	// The gate is the thing under test, so prove it is running at all.
	t.Run("an unauthenticated caller never reaches either group", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/projects/shop-prod/apps", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated = %d, want 401", rec.Code)
		}
	})
}

// buildRouter reads its configuration from the environment, so a test that sets
// none still has to produce a router rather than panicking — the failure mode
// otherwise is a nil map or an empty issuer surfacing at request time.
func TestTheRouterBuildsWithoutConfiguration(t *testing.T) {
	for _, key := range []string{"DEX_ISSUER", "DEX_CLIENT_ID", "CLUSTER_DOMAIN", "CONSOLE_DOMAIN", "KIPPER_RUN_DOMAIN"} {
		t.Setenv(key, "")
	}
	scheme := runtime.NewScheme()
	if err := kipperv1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering the scheme: %v", err)
	}
	ctx, stopSweepers := context.WithCancel(context.Background())
	t.Cleanup(stopSweepers)
	router := buildRouter(
		ctx,
		fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kipper-system"}}),
		dynfake.NewSimpleDynamicClient(scheme),
		&rest.Config{Host: "https://127.0.0.1:6443"},
		crfake.NewClientBuilder().WithScheme(scheme).Build(),
	)
	if router == nil {
		t.Fatal("buildRouter returned no handler")
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /health = %d, want 200", rec.Code)
	}
}

// The env preview returns what a template resolves to, where the GET beside it
// returns the template as written. A viewer may read the second and must not
// read the first: env mutation is deployer-only, so an unmasked preview handed
// to a viewer would widen who can read a credential.
//
// This drives buildRouter rather than the handler, because the gate is in the
// route and a handler test cannot see it. Registering the route without
// deployer() is a one-word mistake that leaves every handler test green.
func TestTheEnvPreviewIsDeployerOnly(t *testing.T) {
	const (
		viewer   = "viewer@test.com"
		deployer = "deployer@test.com"
	)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating a signing key: %v", err)
	}
	dex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "test", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	defer dex.Close()

	t.Setenv("DEX_ISSUER", dex.URL)
	t.Setenv("DEX_CLIENT_ID", "kipper-console")
	t.Setenv("CLUSTER_DOMAIN", "example.com")

	tokenFor := func(email string) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": dex.URL, "aud": "kipper-console", "sub": email,
			"email": email, "email_verified": true,
			"exp": time.Now().Add(time.Hour).Unix(),
		})
		tok.Header["kid"] = "test"
		signed, serr := tok.SignedString(key)
		if serr != nil {
			t.Fatalf("signing a token: %v", serr)
		}
		return signed
	}

	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kipper-system"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: "shop-prod", Labels: map[string]string{kipperlabels.Project: "shop"}}},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "kipper-users", Namespace: "kipper-system"},
			Data: map[string]string{"users": fmt.Sprintf(
				`{%q:"viewer",%q:"deployer"}`, viewer, deployer)},
		},
	)

	scheme := runtime.NewScheme()
	if err := kipperv1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering the scheme: %v", err)
	}
	// The preview reads Secrets to build the table, so the fake client has to
	// know about them. Without this it answers "no kind registered" and the
	// handler 500s, which is not the 403 under test either way.
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering corev1: %v", err)
	}
	crClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "shop"},
			Spec: kipperv1.ProjectSpec{
				Environments: []kipperv1.ProjectEnvironment{{Name: "prod"}},
				Members: []kipperv1.ProjectMember{
					{Email: viewer, Role: kipperv1.ProjectRoleViewer},
					{Email: deployer, Role: kipperv1.ProjectRoleDeployer},
				},
			},
		},
		&kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-prod"},
			Spec: kipperv1.AppSpec{
				Image: "api:v1", Port: 8080,
				Env: map[string]string{"LOG_LEVEL": "debug"},
			},
		},
	).Build()

	ctx, stopSweepers := context.WithCancel(context.Background())
	t.Cleanup(stopSweepers)
	router := buildRouter(ctx, clientset, dynfake.NewSimpleDynamicClient(scheme),
		&rest.Config{Host: "https://127.0.0.1:6443"}, crClient)

	call := func(path, email string) int {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+tokenFor(email))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	const preview = "/api/v1/projects/shop-prod/apps/api/env/preview"
	if code := call(preview, viewer); code != http.StatusForbidden {
		t.Errorf("GET the preview as a viewer = %d, want 403: it returns resolved values", code)
	}
	if code := call(preview, deployer); code != http.StatusOK {
		t.Errorf("GET the preview as a deployer = %d, want 200", code)
	}

	// The templates themselves stay viewer-readable, which is the distinction
	// being drawn rather than a blanket tightening of the Env tab.
	if code := call("/api/v1/projects/shop-prod/apps/api/env", viewer); code != http.StatusOK {
		t.Errorf("GET env as a viewer = %d, want 200: the templates hold no credential", code)
	}
}

// Revealing a secret hands back a credential in plaintext, so it belongs to the
// role that manages secrets rather than to any project member. Listing the keys
// beside it is a different question and stays open.
//
// This drives buildRouter rather than the handler, because the gate is in the
// route: the reveal routes were registered without deployer() while every
// neighbour had it, and no handler test could see that.
func TestRevealingASecretIsDeployerOnly(t *testing.T) {
	const (
		viewer   = "viewer@test.com"
		deployer = "deployer@test.com"
	)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating a signing key: %v", err)
	}
	dex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "test", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	defer dex.Close()

	t.Setenv("DEX_ISSUER", dex.URL)
	t.Setenv("DEX_CLIENT_ID", "kipper-console")
	t.Setenv("CLUSTER_DOMAIN", "example.com")

	tokenFor := func(email string) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": dex.URL, "aud": "kipper-console", "sub": email,
			"email": email, "email_verified": true,
			"exp": time.Now().Add(time.Hour).Unix(),
		})
		tok.Header["kid"] = "test"
		signed, serr := tok.SignedString(key)
		if serr != nil {
			t.Fatalf("signing a token: %v", serr)
		}
		return signed
	}

	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kipper-system"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: "shop-prod", Labels: map[string]string{kipperlabels.Project: "shop"}}},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "kipper-users", Namespace: "kipper-system"},
			Data: map[string]string{"users": fmt.Sprintf(
				`{%q:"viewer",%q:"deployer"}`, viewer, deployer)},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "app-api-secrets", Namespace: "shop-prod"},
			Data:       map[string][]byte{"DATABASE_URL": []byte("postgres://kipper:hunter2@db:5432/api")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "function-resize-secrets", Namespace: "shop-prod"},
			Data:       map[string][]byte{"DATABASE_URL": []byte("postgres://kipper:hunter2@db:5432/fn")},
		},
	)

	scheme := runtime.NewScheme()
	if err := kipperv1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering the scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering corev1: %v", err)
	}
	crClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "shop"},
			Spec: kipperv1.ProjectSpec{
				Environments: []kipperv1.ProjectEnvironment{{Name: "prod"}},
				Members: []kipperv1.ProjectMember{
					{Email: viewer, Role: kipperv1.ProjectRoleViewer},
					{Email: deployer, Role: kipperv1.ProjectRoleDeployer},
				},
			},
		},
		&kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-prod"},
			Spec:       kipperv1.AppSpec{Image: "api:v1", Port: 8080},
		},
		&kipperv1.Function{
			ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "shop-prod"},
			Spec:       kipperv1.FunctionSpec{Image: "resizer:1", Port: 8080},
		},
	).Build()

	ctx, stopSweepers := context.WithCancel(context.Background())
	t.Cleanup(stopSweepers)
	router := buildRouter(ctx, clientset, dynfake.NewSimpleDynamicClient(scheme),
		&rest.Config{Host: "https://127.0.0.1:6443"}, crClient)

	call := func(path, email string) (int, string) {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+tokenFor(email))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	for _, reveal := range []string{
		"/api/v1/projects/shop-prod/apps/api/secrets/DATABASE_URL",
		"/api/v1/projects/shop-prod/functions/resize/secrets/DATABASE_URL",
	} {
		code, body := call(reveal, viewer)
		if code != http.StatusForbidden {
			t.Errorf("GET %s as a viewer = %d, want 403: it returns a credential in plaintext", reveal, code)
		}
		if strings.Contains(body, "hunter2") {
			t.Errorf("GET %s as a viewer returned the secret itself", reveal)
		}
	}

	// Listing the keys is a different question and stays open to any member:
	// knowing that DATABASE_URL is set is not knowing what it is.
	for _, list := range []string{
		"/api/v1/projects/shop-prod/apps/api/secrets",
		"/api/v1/projects/shop-prod/functions/resize/secrets",
	} {
		if code, _ := call(list, viewer); code != http.StatusOK {
			t.Errorf("GET %s as a viewer = %d, want 200: the key names hold no credential", list, code)
		}
	}
}

// dexStandIn starts a JWKS server, points buildRouter's configuration at it and
// returns a function minting a signed token for an address.
func dexStandIn(t *testing.T) func(email string) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating a signing key: %v", err)
	}
	dex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "test", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(dex.Close)

	t.Setenv("DEX_ISSUER", dex.URL)
	t.Setenv("DEX_CLIENT_ID", "kipper-console")
	t.Setenv("CLUSTER_DOMAIN", "example.com")

	return func(email string) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": dex.URL, "aud": "kipper-console", "sub": email,
			"email": email, "email_verified": true,
			"exp": time.Now().Add(time.Hour).Unix(),
		})
		tok.Header["kid"] = "test"
		signed, err := tok.SignedString(key)
		if err != nil {
			t.Fatalf("signing a token: %v", err)
		}
		return signed
	}
}

// Removing a member is owner-gated, and a cluster admin is exempt from the rule
// that keeps a project owned. Neither fact lives in the handler: the gate is on
// the route, and IsAdmin comes from the project-access resolver, so a handler
// test that builds its own ProjectAccess proves neither.
//
// The address arrives percent-encoded, because the console sends
// encodeURIComponent and chi hands the raw segment through. That is the
// spelling that used to match no member and answer 200.
func TestRemovingAMemberIsOwnerGatedAndExemptsAnAdmin(t *testing.T) {
	const (
		projectOwner  = "owner@test.com"
		projectViewer = "viewer@test.com"
		clusterAdmin  = "admin@test.com"
		outsider      = "outsider@test.com"
	)
	tokenFor := dexStandIn(t)

	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kipper-system"}},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "kipper-users", Namespace: "kipper-system"},
			Data: map[string]string{"users": fmt.Sprintf(
				`{%q:"viewer",%q:"viewer",%q:"admin",%q:"viewer"}`, projectOwner, projectViewer, clusterAdmin, outsider)},
		},
	)

	scheme := runtime.NewScheme()
	if err := kipperv1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering the scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering corev1: %v", err)
	}

	// The owner is the project's only owner, so removing them is exactly the
	// case the last-owner rule refuses and the admin exemption steps past.
	newRouter := func() http.Handler {
		crClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
			&kipperv1.Project{
				ObjectMeta: metav1.ObjectMeta{Name: "shop"},
				Spec: kipperv1.ProjectSpec{
					Environments: []kipperv1.ProjectEnvironment{{Name: "prod"}},
					Members: []kipperv1.ProjectMember{
						{Email: projectOwner, Role: kipperv1.ProjectRoleOwner},
						{Email: projectViewer, Role: kipperv1.ProjectRoleViewer},
					},
				},
			},
		).Build()
		ctx, stopSweepers := context.WithCancel(context.Background())
		t.Cleanup(stopSweepers)
		return buildRouter(ctx, clientset, dynfake.NewSimpleDynamicClient(scheme),
			&rest.Config{Host: "https://127.0.0.1:6443"}, crClient)
	}

	// encodeURIComponent's spelling of the address, which is what reaches chi.
	const encoded = "owner%40test.com"
	remove := func(router http.Handler, as string) int {
		req := httptest.NewRequest("DELETE", "/api/v1/projects/shop/members/"+encoded, nil)
		req.Header.Set("Authorization", "Bearer "+tokenFor(as))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := remove(newRouter(), outsider); code != http.StatusForbidden {
		t.Errorf("removing a member as a non-member = %d, want 403", code)
	}
	if code := remove(newRouter(), projectViewer); code != http.StatusForbidden {
		t.Errorf("removing a member as a project viewer = %d, want 403: removal is owner work", code)
	}
	if code := remove(newRouter(), projectOwner); code != http.StatusBadRequest {
		t.Errorf("an owner removing the last owner = %d, want 400: the project would be left ownerless", code)
	}
	if code := remove(newRouter(), clusterAdmin); code != http.StatusOK {
		t.Errorf("a cluster admin removing the last owner = %d, want 200: admins are exempt", code)
	}
}

// Browsing a pod's filesystem splits by what the operation does: listing,
// reading and downloading are open to any project member, while writing into a
// running container is deployer work. The split is in the route wrappers alone,
// so no handler test can see it.
//
// A viewer reaching a read route lands on "no running pod", which is the gate
// letting them through; the writes must not get that far.
func TestBrowsingAPodIsReadableByAViewerAndWritableByADeployer(t *testing.T) {
	const (
		viewer   = "viewer@test.com"
		deployer = "deployer@test.com"
	)
	tokenFor := dexStandIn(t)

	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kipper-system"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: "shop-prod", Labels: map[string]string{kipperlabels.Project: "shop"}}},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "kipper-users", Namespace: "kipper-system"},
			Data: map[string]string{"users": fmt.Sprintf(
				`{%q:"viewer",%q:"deployer"}`, viewer, deployer)},
		},
	)

	scheme := runtime.NewScheme()
	if err := kipperv1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering the scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering corev1: %v", err)
	}
	crClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "shop"},
			Spec: kipperv1.ProjectSpec{
				Environments: []kipperv1.ProjectEnvironment{{Name: "prod"}},
				Members: []kipperv1.ProjectMember{
					{Email: viewer, Role: kipperv1.ProjectRoleViewer},
					{Email: deployer, Role: kipperv1.ProjectRoleDeployer},
				},
			},
		},
		&kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-prod"},
			Spec:       kipperv1.AppSpec{Image: "api:v1", Port: 8080},
		},
	).Build()

	ctx, stopSweepers := context.WithCancel(context.Background())
	t.Cleanup(stopSweepers)
	router := buildRouter(ctx, clientset, dynfake.NewSimpleDynamicClient(scheme),
		&rest.Config{Host: "https://127.0.0.1:6443"}, crClient)

	call := func(method, path, email string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+tokenFor(email))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	const base = "/api/v1/projects/shop-prod/apps/api/files"
	for _, read := range []string{
		base + "?path=/app",
		base + "/content?path=/app/config.json",
		base + "/download?path=/app/config.json",
	} {
		if code := call("GET", read, viewer); code == http.StatusForbidden {
			t.Errorf("GET %s as a viewer = 403, want the gate to pass it through", read)
		}
	}

	if code := call("PUT", base+"/content?path=/app/config.json", viewer); code != http.StatusForbidden {
		t.Errorf("writing a file as a viewer = %d, want 403", code)
	}
	if code := call("POST", base+"/upload?path=/app", viewer); code != http.StatusForbidden {
		t.Errorf("uploading as a viewer = %d, want 403", code)
	}
	if code := call("PUT", base+"/content?path=/app/config.json", deployer); code == http.StatusForbidden {
		t.Errorf("writing a file as a deployer = 403, want the gate to pass it through")
	}
}
