package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func kipperNamespace() *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: testIngressNamespace,
			Labels: map[string]string{
				kipperLabel:              kipperValue,
				"kipper.run/project":     "team",
				"kipper.run/environment": "test",
			},
		},
	}
}

func appWithRoute(namespace, name, host, path string, port int32) *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: kipperv1.AppSpec{
			Image: name + ":v1",
			Port:  port,
			Route: &kipperv1.AppRoute{Host: host, Path: path},
		},
	}
}

func TestRoutesList_ReadsFromAppSpec(t *testing.T) {
	ns := kipperNamespace()
	frontend := appWithRoute("team-test", "frontend", "team-test.kipper.run", "/", 3000)
	api := appWithRoute("team-test", "api", "team-test.kipper.run", "/api", 8080)
	// App without a route — must not appear in the listing.
	worker := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "team-test"},
		Spec:       kipperv1.AppSpec{Image: "worker:v1", Port: 8080},
	}

	handler := &Routes{
		Client:   fake.NewClientset(ns),
		CRClient: testCRClient(frontend, api, worker),
	}

	r := chi.NewRouter()
	r.Get("/routes", handler.List)
	req := httptest.NewRequest("GET", "/routes", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var groups []routeGroupResponse
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
	assert.Len(t, groups, 1)

	g := groups[0]
	assert.Equal(t, "team-test.kipper.run", g.Host)
	assert.Equal(t, "team-test", g.Namespace)
	assert.Equal(t, "team", g.Project)
	assert.Equal(t, "test", g.Environment)
	assert.True(t, g.TLS)
	assert.Len(t, g.Routes, 2)

	// Root path comes first; the worker app is absent.
	assert.Equal(t, "/", g.Routes[0].Path)
	assert.Equal(t, "frontend", g.Routes[0].App)
	assert.Equal(t, int32(3000), g.Routes[0].Port)
	assert.Equal(t, "/api", g.Routes[1].Path)
	assert.Equal(t, "api", g.Routes[1].App)
	assert.Equal(t, int32(8080), g.Routes[1].Port)
}

// Apps without Spec.Route are omitted. Apps in unlabeled namespaces (like
// the Kubernetes default namespace, which the CLI does deploy into) still
// appear — the App CR is the source of truth, not the namespace label.
// project/environment fields fall back to empty when the labels aren't
// present, and the frontend handles cosmetic filtering of system
// namespaces from there.
func TestRoutesList_IncludesAppsInUnlabeledNamespaces(t *testing.T) {
	kipperNs := kipperNamespace()
	plainNs := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	labeledApp := appWithRoute("team-test", "web", "web.kipper.run", "/", 3000)
	defaultApp := appWithRoute("default", "stray", "stray.kipper.run", "/", 3000)
	routelessApp := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "team-test"},
		Spec:       kipperv1.AppSpec{Image: "worker:v1", Port: 8080},
	}

	handler := &Routes{
		Client:   fake.NewClientset(kipperNs, plainNs),
		CRClient: testCRClient(labeledApp, defaultApp, routelessApp),
	}

	r := chi.NewRouter()
	r.Get("/routes", handler.List)
	req := httptest.NewRequest("GET", "/routes", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var groups []routeGroupResponse
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
	assert.Len(t, groups, 2)

	byHost := map[string]routeGroupResponse{}
	for _, g := range groups {
		byHost[g.Host] = g
	}
	assert.Equal(t, "team", byHost["web.kipper.run"].Project)
	assert.Equal(t, "test", byHost["web.kipper.run"].Environment)
	// Unlabeled namespace surfaces with empty project/env — the frontend
	// fills the gap by showing the namespace name instead.
	assert.Empty(t, byHost["stray.kipper.run"].Project)
	assert.Empty(t, byHost["stray.kipper.run"].Environment)
}

func TestRoutesList_EmbedsHealth(t *testing.T) {
	ns := kipperNamespace()
	app := appWithRoute("team-test", "web", "web.kipper.run", "/", 3000)
	ing := newTestIngress("web", "web.kipper.run", "web-tls")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "web-tls", Namespace: "team-test"},
		Data:       map[string][]byte{"tls.crt": selfSignedCertFor(t, "web.kipper.run")},
	}

	handler := &Routes{
		Client:   fake.NewClientset(ns, ing, secret),
		CRClient: testCRClient(app),
	}

	r := chi.NewRouter()
	r.Get("/routes", handler.List)
	req := httptest.NewRequest("GET", "/routes", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var groups []routeGroupResponse
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
	assert.Len(t, groups, 1)
	assert.True(t, groups[0].Health.IngressReady)
	assert.True(t, groups[0].Health.TLSReady)
	assert.True(t, groups[0].Routes[0].Health.IngressReady)
	assert.True(t, groups[0].Routes[0].Health.TLSReady)
}

// Shared-host route group with one member fully provisioned and one still
// waiting. Per-app entries show their own state; the group aggregate is
// not-ready until every member is ready.
func TestRoutesList_GroupAggregatesPerAppHealth(t *testing.T) {
	host := "team.kipper.run"
	ns := kipperNamespace()
	frontend := appWithRoute("team-test", "frontend", host, "/", 3000)
	api := appWithRoute("team-test", "api", host, "/api", 8080)
	frontendIng := newTestIngress("frontend", host, "frontend-tls")
	frontendSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend-tls", Namespace: "team-test"},
		Data:       map[string][]byte{"tls.crt": selfSignedCertFor(t, host)},
	}
	// api has no Ingress yet.

	handler := &Routes{
		Client:   fake.NewClientset(ns, frontendIng, frontendSecret),
		CRClient: testCRClient(frontend, api),
	}

	r := chi.NewRouter()
	r.Get("/routes", handler.List)
	req := httptest.NewRequest("GET", "/routes", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var groups []routeGroupResponse
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
	assert.Len(t, groups, 1)

	// Per-app health: frontend is ready, api is not.
	var frontendEntry, apiEntry routeEntry
	for _, e := range groups[0].Routes {
		if e.App == "frontend" {
			frontendEntry = e
		}
		if e.App == "api" {
			apiEntry = e
		}
	}
	assert.True(t, frontendEntry.Health.TLSReady)
	assert.False(t, apiEntry.Health.IngressReady)

	// Group aggregate is not ready because one member is missing.
	assert.False(t, groups[0].Health.IngressReady)
	assert.False(t, groups[0].Health.TLSReady)
}

// An App with Spec.Route set but Host empty has its host derived by the
// reconciler from CLUSTER_DOMAIN. The listing must do the same derivation
// so those routes still appear.
func TestRoutesList_DerivesImplicitHost(t *testing.T) {
	ns := kipperNamespace()
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team-test"},
		Spec: kipperv1.AppSpec{
			Image: "web:v1",
			Port:  3000,
			// Host left empty — should be derived from Domain + env.
			Route: &kipperv1.AppRoute{Path: "/"},
		},
	}

	handler := &Routes{
		Client:   fake.NewClientset(ns),
		CRClient: testCRClient(app),
		Domain:   "kipper.run",
	}

	r := chi.NewRouter()
	r.Get("/routes", handler.List)
	req := httptest.NewRequest("GET", "/routes", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var groups []routeGroupResponse
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
	assert.Len(t, groups, 1)
	// The kipper namespace fixture has env=test; the derived host
	// follows the reconciler's pattern app-env.cluster.domain.
	assert.Equal(t, "web-test.kipper.run", groups[0].Host)
}

func TestRoutesList_HealthReportsMissingIngress(t *testing.T) {
	// App has a route configured but the reconciler has not yet created
	// the Ingress. The group must still appear (config is the source of
	// truth) but health must flag the missing Ingress.
	ns := kipperNamespace()
	app := appWithRoute("team-test", "web", "web.kipper.run", "/", 3000)

	handler := &Routes{
		Client:   fake.NewClientset(ns),
		CRClient: testCRClient(app),
	}

	r := chi.NewRouter()
	r.Get("/routes", handler.List)
	req := httptest.NewRequest("GET", "/routes", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var groups []routeGroupResponse
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
	assert.Len(t, groups, 1)
	assert.False(t, groups[0].Health.IngressReady)
	assert.False(t, groups[0].Health.TLSReady)
}
