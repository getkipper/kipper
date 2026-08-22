package main

// The routes under /api/v1/projects/{name} split into two groups that take
// different resolvers, and the segment cannot say which: a project called
// shop-prod and project shop's prod environment both put "shop-prod" there.
// Getting it wrong hands one tenant's workloads to another, so every route
// declares which principal it means and this proves the router agrees.
//
// The fixture is that collision. Project shop owns namespace shop-prod; a
// Project named shop-prod exists beside it with a disjoint member list. A route
// that admits shop's owner resolves the namespace; one that admits shop-prod's
// own owner resolves the Project. It walks every method, not only GET, because
// a mutation moved between the groups is the worst version of this bug.

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

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

func TestEachProjectRouteResolvesTheDeclaredPrincipal(t *testing.T) {
	const shopOwner, sprOwner = "shopowner@test.com", "sprowner@test.com"
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	dex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	tok := func(email string) string {
		tk := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": dex.URL, "aud": "kipper-console", "sub": email, "email": email,
			"email_verified": true, "exp": time.Now().Add(time.Hour).Unix()})
		tk.Header["kid"] = "test"
		s, _ := tk.SignedString(key)
		return s
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-prod", Labels: map[string]string{kipperlabels.Project: "shop"}}}
	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kipper-system"}}, ns,
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kipper-users", Namespace: "kipper-system"},
			Data: map[string]string{"users": fmt.Sprintf(`{%q:"deployer",%q:"deployer"}`, shopOwner, sprOwner)}})
	scheme := runtime.NewScheme()
	_ = kipperv1.AddToScheme(scheme)
	m := func(e string) []kipperv1.ProjectMember {
		return []kipperv1.ProjectMember{{Email: e, Role: kipperv1.ProjectRoleOwner}}
	}
	shop := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec: kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "prod"}}, Members: m(shopOwner)}}
	spr := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop-prod"},
		Spec: kipperv1.ProjectSpec{Members: m(sprOwner)}}
	crClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(shop, spr).Build()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	router := buildRouter(ctx, clientset, dynfake.NewSimpleDynamicClient(scheme), &rest.Config{Host: "https://127.0.0.1:6443"}, crClient)

	restore := func() {
		c := context.Background()
		if _, err := clientset.CoreV1().Namespaces().Get(c, "shop-prod", metav1.GetOptions{}); err != nil {
			f := ns.DeepCopy()
			f.ResourceVersion = ""
			_, _ = clientset.CoreV1().Namespaces().Create(c, f, metav1.CreateOptions{})
		}
		for _, want := range []*kipperv1.Project{shop, spr} {
			var live kipperv1.Project
			if err := crClient.Get(c, crclient.ObjectKey{Name: want.Name}, &live); err == nil {
				live.Spec = *want.Spec.DeepCopy()
				_ = crClient.Update(c, &live)
			} else {
				f := want.DeepCopy()
				f.ResourceVersion = ""
				_ = crClient.Create(c, f)
			}
		}
	}
	call := func(method, path, email string) int {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req := httptest.NewRequest(method, path+"?namespace=shop-prod", strings.NewReader("{}")).WithContext(c)
		req.Header.Set("Authorization", "Bearer "+tok(email))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		done := make(chan int, 1)
		go func() {
			defer func() {
				if p := recover(); p != nil {
					done <- 599
				}
			}()
			router.ServeHTTP(rec, req)
			done <- rec.Code
		}()
		select {
		case code := <-done:
			return code
		case <-c.Done():
			return 598
		}
	}
	fill := strings.NewReplacer("{name}", "shop-prod", "{app}", "a", "{fn}", "f", "{service}", "s",
		"{vol}", "v", "{key}", "k", "{schema}", "public", "{table}", "t", "{indexName}", "i",
		"{snippetName}", "sn", "{email}", sprOwner, "{transfer}", "tr", "{session}", "se",
		"{plan}", "p", "{id}", "id", "{host}", "h", "{backup}", "b", "{token}", "tk",
		"{username}", "u", "{namespace}", "shop-prod", "{bucket}", "bk", "{schedule}", "sc", "*", "w")
	cr := router.(consoleRouter)
	checked := 0
	err := chi.Walk(cr.api, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(pattern, "/api/v1/projects/{name}") {
			return nil
		}
		route := method + " " + pattern
		declared, ok := routeAuthz[route]
		if !ok {
			return nil // the totality test reports this
		}

		p := fill.Replace(pattern)
		shopCode := call(method, p, shopOwner)
		restore()
		sprCode := call(method, p, sprOwner)
		restore()

		denied := func(c int) bool { return c == http.StatusUnauthorized || c == http.StatusForbidden }
		var resolved routeScope
		switch {
		case !denied(shopCode) && denied(sprCode):
			resolved = scopeNamespace
		case denied(shopCode) && !denied(sprCode):
			resolved = scopeProject
		case !denied(shopCode) && !denied(sprCode):
			t.Errorf("%s admits shop's owner (%d) and shop-prod's owner (%d): one path, two tenants",
				route, shopCode, sprCode)
			return nil
		default:
			resolved = scopeNone
		}

		checked++
		if declared.scope != resolved {
			t.Errorf("%s is declared %s-scoped and resolves the %s (shop=%d shop-prod=%d)",
				route, declared.scope, resolved, shopCode, sprCode)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the router: %v", err)
	}
	if checked == 0 {
		t.Fatal("no project routes were walked, so this proves nothing")
	}
	t.Logf("checked %d routes under /projects/{name}", checked)
}
