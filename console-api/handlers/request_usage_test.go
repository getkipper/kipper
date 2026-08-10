package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

func requestUsageNamespace(name, project, env string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				kipperlabels.Project:     project,
				kipperlabels.Environment: env,
			},
		},
	}
}

func serveRequestUsage(t *testing.T, h *RequestUsage, project string) RequestUsageResponse {
	t.Helper()
	router := chi.NewRouter()
	router.Get("/projects/{name}/requests", h.Get)

	req := httptest.NewRequest(http.MethodGet, "/projects/"+project+"/requests", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp RequestUsageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func TestRequestUsage_GroupsByEnvironmentAndApp(t *testing.T) {
	h := &RequestUsage{
		Client: fake.NewClientset(
			requestUsageNamespace("shop-test", "shop", "test"),
			requestUsageNamespace("shop-prod", "shop", "prod"),
		),
		PromQueryVec: func(ctx context.Context, query string, at time.Time) ([]PromVectorSample, error) {
			assert.Contains(t, query, "traefik_service_requests_total")
			assert.Contains(t, query, "[72h]")
			return []PromVectorSample{
				{Labels: map[string]string{"service": "shop-test-web-8080@kubernetes"}, Value: 1200},
				{Labels: map[string]string{"service": "shop-test-api-gateway-3000@kubernetes"}, Value: 300},
				{Labels: map[string]string{"service": "shop-prod-web-8080@kubernetes"}, Value: 9000},
				{Labels: map[string]string{"service": "other-ns-web-80@kubernetes"}, Value: 555},
			}, nil
		},
	}

	resp := serveRequestUsage(t, h, "shop")

	assert.True(t, resp.PrometheusAvailable)
	assert.Equal(t, 72, resp.WindowHours)
	assert.InDelta(t, 10500, resp.TotalRequests, 0.001, "series outside the project must not count")
	require.Len(t, resp.Environments, 2)

	prod := resp.Environments[0]
	assert.Equal(t, "shop-prod", prod.Namespace)
	assert.Equal(t, "prod", prod.Environment)
	require.Len(t, prod.Apps, 1)
	assert.Equal(t, "web", prod.Apps[0].App)

	test := resp.Environments[1]
	assert.Equal(t, "shop-test", test.Namespace)
	require.Len(t, test.Apps, 2)
	assert.Equal(t, "web", test.Apps[0].App, "apps sorted by request count")
	assert.Equal(t, "api-gateway", test.Apps[1].App, "dashed app names keep everything but the port segment")
	assert.InDelta(t, 1500, test.TotalRequests, 0.001)
}

func TestRequestUsage_DoesNotStealSiblingProjectTraffic(t *testing.T) {
	// Project "billing" owns namespace "billing-prod". A separate project owns
	// "billing-prod-test", whose Traefik service label starts with the same
	// prefix. billing must not be credited with the sibling's traffic.
	h := &RequestUsage{
		Client: fake.NewClientset(
			requestUsageNamespace("billing-prod", "billing", "prod"),
			requestUsageNamespace("billing-prod-test", "billing-prod", "test"),
		),
		PromQueryVec: func(ctx context.Context, query string, at time.Time) ([]PromVectorSample, error) {
			return []PromVectorSample{
				{Labels: map[string]string{"service": "billing-prod-web-8080@kubernetes"}, Value: 100},
				{Labels: map[string]string{"service": "billing-prod-test-web-8080@kubernetes"}, Value: 5000},
			}, nil
		},
	}

	resp := serveRequestUsage(t, h, "billing")

	require.Len(t, resp.Environments, 1)
	env := resp.Environments[0]
	assert.Equal(t, "billing-prod", env.Namespace)
	require.Len(t, env.Apps, 1, "the sibling project's service must not be attributed here")
	assert.Equal(t, "web", env.Apps[0].App)
	assert.InDelta(t, 100, resp.TotalRequests, 0.001)
}

func TestRequestUsage_DegradesWhenPrometheusUnavailable(t *testing.T) {
	h := &RequestUsage{
		Client: fake.NewClientset(requestUsageNamespace("shop-test", "shop", "test")),
		PromQueryVec: func(ctx context.Context, query string, at time.Time) ([]PromVectorSample, error) {
			return nil, context.DeadlineExceeded
		},
	}

	resp := serveRequestUsage(t, h, "shop")

	assert.False(t, resp.PrometheusAvailable)
	assert.Zero(t, resp.TotalRequests)
	require.Len(t, resp.Environments, 1)
	assert.Equal(t, "shop-test", resp.Environments[0].Namespace)
	assert.Empty(t, resp.Environments[0].Apps)
}

func TestRequestUsage_NoNamespaces(t *testing.T) {
	h := &RequestUsage{Client: fake.NewClientset()}

	resp := serveRequestUsage(t, h, "ghost")

	assert.False(t, resp.PrometheusAvailable)
	assert.Empty(t, resp.Environments)
}

func TestSplitTraefikService_PrefersLongestNamespace(t *testing.T) {
	namespaces := []string{"shop", "shop-test"}

	ns, app, ok := splitTraefikService("shop-test-web-8080@kubernetes", namespaces)
	require.True(t, ok)
	assert.Equal(t, "shop-test", ns)
	assert.Equal(t, "web", app)

	ns, app, ok = splitTraefikService("shop-api-80@kubernetes", namespaces)
	require.True(t, ok)
	assert.Equal(t, "shop", ns)
	assert.Equal(t, "api", app)

	_, _, ok = splitTraefikService("unrelated-web-80@kubernetes", namespaces)
	assert.False(t, ok)
}
