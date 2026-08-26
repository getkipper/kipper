package handlers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/middleware"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

// Consent is a list held by the project being reached. Granting must be
// idempotent and ordered, and withdrawing must report whether there was
// anything to withdraw — the difference between "closed" and "was never open"
// is what the caller is told.
func TestApplyLinkConsent(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		project string
		allow   bool
		want    []string
		changed bool
	}{
		{name: "grant to an empty list", project: "hrportal", allow: true, want: []string{"hrportal"}, changed: true},
		{name: "grant keeps it sorted", allowed: []string{"zeta"}, project: "alpha", allow: true, want: []string{"alpha", "zeta"}, changed: true},
		{name: "granting twice changes nothing", allowed: []string{"hrportal"}, project: "hrportal", allow: true, want: []string{"hrportal"}},
		{name: "withdraw", allowed: []string{"other", "hrportal"}, project: "hrportal", want: []string{"other"}, changed: true},
		{name: "withdrawing the last empties it", allowed: []string{"hrportal"}, project: "hrportal", want: []string{}, changed: true},
		{name: "withdrawing what is absent changes nothing", allowed: []string{"other"}, project: "hrportal", want: []string{"other"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := applyLinkConsent(tt.allowed, tt.project, tt.allow)
			assert.Equal(t, tt.changed, changed)
			assert.Equal(t, tt.want, got)
		})
	}
}

// The route's middleware is the feature's authorization boundary: a link opens a
// direct path to a backend in this project, so who may grant that has to be
// enforced before the handler is reached. This mounts the handlers behind the
// same wrappers main.go uses and proves the boundary rather than the helper.
func TestLinkConsent_RoutesEnforceOwnershipBeforeTheHandler(t *testing.T) {
	ns := func(name, project string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: name, Labels: map[string]string{kipperlabels.Project: project}}}
	}
	usersCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-users", Namespace: "kipper-system"},
		Data: map[string]string{"users": `{"owner@test.com":"deployer","deployer@test.com":"deployer",` +
			`"viewer@test.com":"viewer","stranger@test.com":"viewer","root@test.com":"admin"}`},
	}
	client := fake.NewClientset(
		ns("docuseal", "docuseal"), ns("hrportal", "hrportal"),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kipper-system"}}, usersCM)

	scheme := runtime.NewScheme()
	require.NoError(t, kipperv1.AddToScheme(scheme))
	crClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "docuseal"}},
		&kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "hrportal"}},
	).Build()

	resolver := middleware.NewProjectAccessResolver(client, middleware.NewRoleStore(client), stubProjectMembers{
		"docuseal": {
			"owner@test.com":    middleware.ProjectRoleOwner,
			"deployer@test.com": middleware.ProjectRoleDeployer,
			"viewer@test.com":   middleware.ProjectRoleViewer,
		},
	}, handlerOwners(t))
	scope := middleware.ProjectScope(resolver)
	member := func(h http.HandlerFunc) http.HandlerFunc { return scope(h).ServeHTTP }
	owner := func(h http.HandlerFunc) http.HandlerFunc {
		return scope(middleware.RequireCapability("project.settings")(h)).ServeHTTP
	}

	handler := &Projects{Client: client, CRClient: crClient}
	r := chi.NewRouter()
	r.Get("/projects/{name}/link-consent", member(handler.LinkConsent))
	r.Put("/projects/{name}/link-consent", owner(handler.SetLinkConsent))

	call := func(method, target, email, body string) *httptest.ResponseRecorder {
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, target, reader)
		if email != "" {
			req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, &middleware.Claims{Email: email}))
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	grant := `{"project":"hrportal","allow":true}`

	tests := []struct {
		name   string
		method string
		email  string
		body   string
		want   int
	}{
		{"an owner may grant", "PUT", "owner@test.com", grant, http.StatusOK},
		{"a deployer may not grant", "PUT", "deployer@test.com", grant, http.StatusForbidden},
		{"a viewer may not grant", "PUT", "viewer@test.com", grant, http.StatusForbidden},
		{"a non-member may not grant", "PUT", "stranger@test.com", grant, http.StatusForbidden},
		{"an admin grants as an owner does", "PUT", "root@test.com", grant, http.StatusOK},
		{"unauthenticated is rejected", "PUT", "", grant, http.StatusUnauthorized},
		{"a member may read", "GET", "viewer@test.com", "", http.StatusOK},
		{"a non-member may not read", "GET", "stranger@test.com", "", http.StatusForbidden},
		{"omitting allow is refused rather than treated as withdraw", "PUT", "owner@test.com", `{"project":"hrportal"}`, http.StatusBadRequest},
		{"an empty project is refused", "PUT", "owner@test.com", `{"project":"","allow":true}`, http.StatusBadRequest},
		{"consenting to itself is refused", "PUT", "owner@test.com", `{"project":"docuseal","allow":true}`, http.StatusBadRequest},
		{"granting to a project that does not exist is refused", "PUT", "owner@test.com", `{"project":"ghost","allow":true}`, http.StatusNotFound},
		{"withdrawing a project that no longer exists is allowed", "PUT", "owner@test.com", `{"project":"ghost","allow":false}`, http.StatusOK},
		{"malformed json is refused", "PUT", "owner@test.com", `{`, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := call(tt.method, "/projects/docuseal/link-consent", tt.email, tt.body)
			assert.Equal(t, tt.want, rec.Code, rec.Body.String())
		})
	}

	// A granted consent is actually persisted and readable back.
	require.Equal(t, http.StatusOK, call("PUT", "/projects/docuseal/link-consent", "owner@test.com", grant).Code)
	rec := call("GET", "/projects/docuseal/link-consent", "viewer@test.com", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "hrportal")

	// And withdrawing it removes it.
	require.Equal(t, http.StatusOK, call("PUT", "/projects/docuseal/link-consent", "owner@test.com", `{"project":"hrportal","allow":false}`).Code)
	rec = call("GET", "/projects/docuseal/link-consent", "viewer@test.com", "")
	assert.NotContains(t, rec.Body.String(), "hrportal")
}

// The collision the reconciler contains in the cluster has an API-layer half:
// project "shop" with an environment "prod" owns the namespace "shop-prod",
// and a Project "shop-prod" can exist alongside it. Link consent acts on the
// Project named in the path, so the gate in front of it has to be about that
// Project and not about whoever owns the namespace sharing its name. The
// namespace-scoped routes need the opposite answer, which the companion test
// below pins.
func TestLinkConsent_ANamespaceOwnerDoesNotInheritTheSameNamedProject(t *testing.T) {
	usersCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-users", Namespace: "kipper-system"},
		Data: map[string]string{"users": `{"shopowner@test.com":"deployer",` +
			`"sprowner@test.com":"deployer","root@test.com":"admin"}`},
	}
	// shop reached the namespace first, so shop-prod carries shop's label.
	shopProdNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-prod", Labels: map[string]string{kipperlabels.Project: "shop"}}}
	client := fake.NewClientset(shopProdNS,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kipper-system"}}, usersCM)

	scheme := runtime.NewScheme()
	require.NoError(t, kipperv1.AddToScheme(scheme))
	crClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}},
		&kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop-prod"}},
		&kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "hrportal"}},
	).Build()

	resolver := middleware.NewProjectAccessResolver(client, middleware.NewRoleStore(client), stubProjectMembers{
		"shop":      {"shopowner@test.com": middleware.ProjectRoleOwner},
		"shop-prod": {"sprowner@test.com": middleware.ProjectRoleOwner},
	}, handlerOwners(t, shopProdNS))
	scope := middleware.ProjectScope(resolver)
	owner := func(h http.HandlerFunc) http.HandlerFunc {
		return scope(middleware.RequireCapability("project.settings")(h)).ServeHTTP
	}

	handler := &Projects{Client: client, CRClient: crClient}
	r := chi.NewRouter()
	r.Put("/projects/{name}/link-consent", owner(handler.SetLinkConsent))

	call := func(email string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/projects/shop-prod/link-consent",
			strings.NewReader(`{"project":"hrportal","allow":true}`))
		req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, &middleware.Claims{Email: email}))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := call("shopowner@test.com")
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"owning the namespace shop-prod must not grant consent on the project shop-prod")

	rec = call("sprowner@test.com")
	assert.Equal(t, http.StatusOK, rec.Code,
		"shop-prod's own owner must still decide who links into it: %s", rec.Body.String())

	// And the consent landed on shop-prod's Project, not on shop's.
	var shopProd, shop kipperv1.Project
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Name: "shop-prod"}, &shopProd))
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Name: "shop"}, &shop))
	assert.Equal(t, []string{"hrportal"}, shopProd.Spec.AllowLinksFrom)
	assert.Empty(t, shop.Spec.AllowLinksFrom)
}

// The other half of the same collision. Under /projects/{name} the segment is a
// project name on the routes that act on the Project and an environment
// namespace on the routes that act on workloads, so the two need different
// gates. Here "shop-prod" is shop's prod environment, and a Project of that
// name exists beside it: shop's own members must keep reaching their apps, and
// shop-prod's members must not reach into a namespace they do not own.
func TestNamespaceScope_TheNamespacesOwnerReachesItRatherThanTheSameNamedProject(t *testing.T) {
	usersCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-users", Namespace: "kipper-system"},
		Data:       map[string]string{"users": `{"shopdev@test.com":"deployer","sprowner@test.com":"deployer"}`},
	}
	shopProdNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-prod", Labels: map[string]string{kipperlabels.Project: "shop"}}}
	client := fake.NewClientset(shopProdNS,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kipper-system"}}, usersCM)

	scheme := runtime.NewScheme()
	require.NoError(t, kipperv1.AddToScheme(scheme))
	crClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-prod"}},
	).Build()

	resolver := middleware.NewProjectAccessResolver(client, middleware.NewRoleStore(client), stubProjectMembers{
		"shop":      {"shopdev@test.com": middleware.ProjectRoleDeployer},
		"shop-prod": {"sprowner@test.com": middleware.ProjectRoleOwner},
	}, handlerOwners(t, shopProdNS))

	apps := &Apps{Client: client, CRClient: crClient}
	r := chi.NewRouter()
	r.Route("/projects/{name}", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(middleware.NamespaceScope(resolver))
			r.Get("/apps", apps.List)
		})
	})

	call := func(email string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/projects/shop-prod/apps", nil)
		req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, &middleware.Claims{Email: email}))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := call("shopdev@test.com")
	require.Equal(t, http.StatusOK, rec.Code,
		"shop owns the namespace shop-prod, so its members must reach the apps in it: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "checkout")

	rec = call("sprowner@test.com")
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"owning the project shop-prod must not reach into a namespace owned by shop")
}
