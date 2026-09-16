package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func listRoutes(t *testing.T, objects ...crclient.Object) []routeGroupResponse {
	t.Helper()
	return listRoutesWith(t, fake.NewClientset(kipperNamespace()), objects...)
}

func listRoutesWith(t *testing.T, kube kubernetes.Interface, objects ...crclient.Object) []routeGroupResponse {
	t.Helper()
	handler := &Routes{Client: kube, CRClient: testCRClient(objects...)}

	r := chi.NewRouter()
	r.Get("/routes", handler.List)
	req := httptest.NewRequest("GET", "/routes", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var groups []routeGroupResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
	return groups
}

func TestRoutesList_SaysWhatTheRouteRefuses(t *testing.T) {
	api := appWithRoute("team-test", "api", "team-test.kipper.run", "/domains-api", 8080)
	api.Spec.Route.PublicPaths = []string{"/actuator/prometheus"}
	guard := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "platform"},
		Spec: kipperv1.PlatformConfigSpec{
			Profile:    "small",
			RouteGuard: &kipperv1.RouteGuardSpec{BlockInternalPaths: true},
		},
	}

	groups := listRoutes(t, api, guard)

	require.Len(t, groups, 1)
	assert.True(t, groups[0].RouteGuard, "the cluster refuses the default list")
	require.Len(t, groups[0].Routes, 1)
	entry := groups[0].Routes[0]
	assert.Equal(t, []string{
		"/domains-api/actuator", "/domains-api/debug/pprof", "/domains-api/internal",
		"/domains-api/.git", "/domains-api/.env",
	}, entry.RefusedPaths, "refusals are shown where a caller would type them")
	assert.Equal(t, []string{"/domains-api/actuator/prometheus"}, entry.PublicPaths)
}

func TestRoutesList_SaysWhenARefusalIsNotInPlaceYet(t *testing.T) {
	api := appWithRoute("team-test", "api", "team-test.kipper.run", "/domains-api", 8080)
	guard := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "platform"},
		Spec: kipperv1.PlatformConfigSpec{
			Profile:    "small",
			RouteGuard: &kipperv1.RouteGuardSpec{BlockInternalPaths: true},
		},
	}

	groups := listRoutes(t, api, guard)
	require.Len(t, groups, 1)
	assert.NotEmpty(t, groups[0].Routes[0].RefusedPaths)
	assert.False(t, groups[0].Routes[0].RefusalReady,
		"nothing in the cluster refuses these yet")

	// Simulate an Ingress with the expected name but different ownership and rules.
	bare := fake.NewClientset(kipperNamespace(), &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "api-internal-paths", Namespace: "team-test"},
	})
	groups = listRoutesWith(t, bare, api, guard)
	assert.False(t, groups[0].Routes[0].RefusalReady,
		"an Ingress under the name that refuses nothing is not a refusal")

}

func TestRoutesList_RefusesNothingWhenTheGuardIsOff(t *testing.T) {
	api := appWithRoute("team-test", "api", "team-test.kipper.run", "/domains-api", 8080)

	groups := listRoutes(t, api)

	require.Len(t, groups, 1)
	assert.False(t, groups[0].RouteGuard)
	assert.Empty(t, groups[0].Routes[0].RefusedPaths)
}
