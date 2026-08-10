package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

func newPlatformConfig(profile string, overrides []kipperv1.ComponentOverride, status kipperv1.PlatformConfigStatus) *kipperv1.PlatformConfig {
	return &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: platformConfigName},
		Spec: kipperv1.PlatformConfigSpec{
			Profile:    profile,
			Components: overrides,
		},
		Status: status,
	}
}

func mountPlatformRoutes(h *Platform) http.Handler {
	r := chi.NewRouter()
	r.Get("/api/v1/platform", h.Summary)
	r.Get("/api/v1/platform/components", h.Components)
	r.Patch("/api/v1/platform/components/{name}", h.UpdateComponent)
	return r
}

func TestPlatform_SummaryReturnsProfileAndComponentList(t *testing.T) {
	pc := newPlatformConfig(platform.ProfileLarge, nil, kipperv1.PlatformConfigStatus{
		Components: []kipperv1.ComponentStatus{
			{Name: "prometheus", CurrentMemoryLimit: "1Gi", Phase: "Running"},
			{Name: "loki", CurrentMemoryLimit: "512Mi", Phase: "Running"},
		},
	})
	h := &Platform{CRClient: testCRClient(pc)}

	req := httptest.NewRequest("GET", "/api/v1/platform", nil)
	rec := httptest.NewRecorder()
	mountPlatformRoutes(h).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp platformSummaryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, platform.ProfileLarge, resp.Profile)
	assert.ElementsMatch(t,
		[]string{"nano", "small", "medium", "large", "xlarge"},
		resp.AvailableProfiles)
	// Every chart-managed component in the shared path table shows up.
	assert.Len(t, resp.Components, len(platform.SupportedComponents()))

	byName := map[string]componentSummaryEntry{}
	for _, c := range resp.Components {
		byName[c.Name] = c
	}
	assert.Equal(t, "1Gi", byName["prometheus"].CurrentMemoryLimit)
	assert.Equal(t, "512Mi", byName["loki"].CurrentMemoryLimit)
	assert.True(t, byName["prometheus"].Enabled, "non-nano profile defaults to enabled")
}

func TestPlatform_SummaryNoCRReturnsAvailableProfiles(t *testing.T) {
	h := &Platform{CRClient: testCRClient()}

	req := httptest.NewRequest("GET", "/api/v1/platform", nil)
	rec := httptest.NewRecorder()
	mountPlatformRoutes(h).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp platformSummaryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.Profile)
	assert.NotEmpty(t, resp.AvailableProfiles)
}

func TestPlatform_ComponentsIncludesProfileDefaultAndOverride(t *testing.T) {
	bumpedAt := metav1.NewTime(time.Now().Add(-30 * time.Minute))
	pc := newPlatformConfig(platform.ProfileXLarge,
		[]kipperv1.ComponentOverride{
			{Name: "prometheus", MemoryLimit: "3Gi"},
		},
		kipperv1.PlatformConfigStatus{
			Components: []kipperv1.ComponentStatus{
				{
					Name:               "prometheus",
					CurrentMemoryLimit: "3Gi",
					Phase:              "Running",
					LastBumpAt:         &bumpedAt,
					LastBumpFrom:       "2Gi",
					LastBumpTo:         "3Gi",
					LastBumpReason:     "OOMKilled detected",
				},
			},
		})

	h := &Platform{CRClient: testCRClient(pc)}

	req := httptest.NewRequest("GET", "/api/v1/platform/components", nil)
	rec := httptest.NewRecorder()
	mountPlatformRoutes(h).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp componentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, platform.ProfileXLarge, resp.Profile)

	byName := map[string]componentDetailEntry{}
	for _, c := range resp.Components {
		byName[c.Name] = c
	}

	prom := byName["prometheus"]
	assert.Equal(t, "2Gi", prom.ProfileMemoryLimit, "xlarge default for Prometheus")
	assert.Equal(t, "3Gi", prom.OverrideMemoryLimit, "user override surfaces here")
	assert.Equal(t, "3Gi", prom.CurrentMemoryLimit, "status reflects what is actually applied")
	assert.Equal(t, "OOMKilled detected", prom.LastBumpReason)
	assert.NotEmpty(t, prom.LastBumpAt, "LastBumpAt should be formatted as RFC3339")
	assert.Equal(t, "256Mi", prom.MemoryMin, "bounds come from helmpaths.go")
	assert.Equal(t, "8Gi", prom.MemoryMax)

	loki := byName["loki"]
	assert.Equal(t, "1Gi", loki.ProfileMemoryLimit, "xlarge default for Loki")
	assert.Empty(t, loki.OverrideMemoryLimit, "no override set")
	assert.Equal(t, "128Mi", loki.MemoryMin)
	assert.Equal(t, "2Gi", loki.MemoryMax)

	// New chart-managed components surface too, with flat defaults.
	grafana := byName["grafana"]
	assert.Equal(t, "128Mi", grafana.ProfileMemoryLimit)
	assert.Equal(t, "64Mi", grafana.MemoryMin)
	assert.Equal(t, "512Mi", grafana.MemoryMax)
}

func TestPlatform_UpdateComponentSetsOverride(t *testing.T) {
	pc := newPlatformConfig(platform.ProfileMedium, nil, kipperv1.PlatformConfigStatus{})
	crClient := testCRClient(pc)
	h := &Platform{CRClient: crClient}

	body := bytes.NewReader([]byte(`{"memory_limit": "2Gi"}`))
	req := httptest.NewRequest("PATCH", "/api/v1/platform/components/prometheus", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mountPlatformRoutes(h).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var got kipperv1.PlatformConfig
	require.NoError(t, crClient.Get(context.Background(),
		crclient.ObjectKey{Name: platformConfigName}, &got))
	require.Len(t, got.Spec.Components, 1)
	assert.Equal(t, "prometheus", got.Spec.Components[0].Name)
	assert.Equal(t, "2Gi", got.Spec.Components[0].MemoryLimit)
}

func TestPlatform_UpdateComponentDisable(t *testing.T) {
	pc := newPlatformConfig(platform.ProfileMedium, nil, kipperv1.PlatformConfigStatus{})
	crClient := testCRClient(pc)
	h := &Platform{CRClient: crClient}

	body := bytes.NewReader([]byte(`{"enabled": false}`))
	req := httptest.NewRequest("PATCH", "/api/v1/platform/components/loki", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mountPlatformRoutes(h).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var got kipperv1.PlatformConfig
	require.NoError(t, crClient.Get(context.Background(),
		crclient.ObjectKey{Name: platformConfigName}, &got))
	require.Len(t, got.Spec.Components, 1)
	assert.Equal(t, "loki", got.Spec.Components[0].Name)
	require.NotNil(t, got.Spec.Components[0].Enabled,
		"explicit false must be persisted as a non-nil pointer")
	assert.False(t, *got.Spec.Components[0].Enabled)
}

func TestPlatform_UpdateComponentRejectsUnknownName(t *testing.T) {
	pc := newPlatformConfig(platform.ProfileMedium, nil, kipperv1.PlatformConfigStatus{})
	h := &Platform{CRClient: testCRClient(pc)}

	body := bytes.NewReader([]byte(`{"memory_limit": "1Gi"}`))
	req := httptest.NewRequest("PATCH", "/api/v1/platform/components/imaginary", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mountPlatformRoutes(h).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPlatform_UpdateComponentValidatesQuantity(t *testing.T) {
	pc := newPlatformConfig(platform.ProfileMedium, nil, kipperv1.PlatformConfigStatus{})
	h := &Platform{CRClient: testCRClient(pc)}

	body := bytes.NewReader([]byte(`{"memory_limit": "12 gigabytes please"}`))
	req := httptest.NewRequest("PATCH", "/api/v1/platform/components/prometheus", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mountPlatformRoutes(h).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "valid Kubernetes quantity")
}

func TestPlatform_UpdateComponentRejectsBelowMin(t *testing.T) {
	pc := newPlatformConfig(platform.ProfileMedium, nil, kipperv1.PlatformConfigStatus{})
	h := &Platform{CRClient: testCRClient(pc)}

	// Prometheus' min is 256Mi.
	body := bytes.NewReader([]byte(`{"memory_limit": "64Mi"}`))
	req := httptest.NewRequest("PATCH", "/api/v1/platform/components/prometheus", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mountPlatformRoutes(h).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "below")
	assert.Contains(t, rec.Body.String(), "minimum")
}

func TestPlatform_UpdateComponentRejectsAboveMax(t *testing.T) {
	pc := newPlatformConfig(platform.ProfileMedium, nil, kipperv1.PlatformConfigStatus{})
	h := &Platform{CRClient: testCRClient(pc)}

	// Grafana's max is 512Mi.
	body := bytes.NewReader([]byte(`{"memory_limit": "4Gi"}`))
	req := httptest.NewRequest("PATCH", "/api/v1/platform/components/grafana", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mountPlatformRoutes(h).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "above")
	assert.Contains(t, rec.Body.String(), "maximum")
}

func TestPlatform_UpdateComponentRejectsEnabledForNonToggleable(t *testing.T) {
	pc := newPlatformConfig(platform.ProfileMedium, nil, kipperv1.PlatformConfigStatus{})
	h := &Platform{CRClient: testCRClient(pc)}

	// Grafana follows Prometheus and can't be toggled independently.
	body := bytes.NewReader([]byte(`{"enabled": false}`))
	req := httptest.NewRequest("PATCH", "/api/v1/platform/components/grafana", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mountPlatformRoutes(h).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "not independently toggleable")
}

func TestPlatform_SummaryReportsFollowingComponentsCorrectly(t *testing.T) {
	// Prometheus disabled → grafana effectively off too. Traefik stays on
	// even though it never carries an explicit Enabled.
	no := false
	pc := newPlatformConfig(platform.ProfileMedium,
		[]kipperv1.ComponentOverride{
			{Name: "prometheus", Enabled: &no},
		},
		kipperv1.PlatformConfigStatus{})
	h := &Platform{CRClient: testCRClient(pc)}

	req := httptest.NewRequest("GET", "/api/v1/platform", nil)
	rec := httptest.NewRecorder()
	mountPlatformRoutes(h).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp platformSummaryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	byName := map[string]componentSummaryEntry{}
	for _, c := range resp.Components {
		byName[c.Name] = c
	}
	assert.False(t, byName["prometheus"].Enabled)
	assert.False(t, byName["grafana"].Enabled, "grafana shares the kube-prometheus-stack chart")
	assert.True(t, byName["loki"].Enabled, "loki is unaffected")
	assert.True(t, byName["traefik"].Enabled, "always-on")
}

func TestPlatform_SummaryReportsAlwaysOnComponentsOnNano(t *testing.T) {
	// Nano disables monitoring by default but installers still run
	// traefik/keda/velero; the summary should reflect that.
	pc := newPlatformConfig(platform.ProfileNano, nil, kipperv1.PlatformConfigStatus{})
	h := &Platform{CRClient: testCRClient(pc)}

	req := httptest.NewRequest("GET", "/api/v1/platform", nil)
	rec := httptest.NewRecorder()
	mountPlatformRoutes(h).ServeHTTP(rec, req)

	var resp platformSummaryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	byName := map[string]componentSummaryEntry{}
	for _, c := range resp.Components {
		byName[c.Name] = c
	}
	assert.False(t, byName["prometheus"].Enabled)
	assert.False(t, byName["loki"].Enabled)
	assert.False(t, byName["grafana"].Enabled)
	assert.False(t, byName["promtail"].Enabled)
	assert.True(t, byName["traefik"].Enabled, "traefik installer still runs on nano")
	assert.True(t, byName["keda"].Enabled)
	assert.True(t, byName["velero"].Enabled)
}

func TestPlatform_UpdateComponentAcceptsNewComponent(t *testing.T) {
	// A chart-managed component that wasn't in the original two-entry
	// allowlist now goes through cleanly.
	pc := newPlatformConfig(platform.ProfileMedium, nil, kipperv1.PlatformConfigStatus{})
	crClient := testCRClient(pc)
	h := &Platform{CRClient: crClient}

	body := bytes.NewReader([]byte(`{"memory_limit": "256Mi"}`))
	req := httptest.NewRequest("PATCH", "/api/v1/platform/components/grafana", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mountPlatformRoutes(h).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got kipperv1.PlatformConfig
	require.NoError(t, crClient.Get(context.Background(),
		crclient.ObjectKey{Name: platformConfigName}, &got))
	require.Len(t, got.Spec.Components, 1)
	assert.Equal(t, "grafana", got.Spec.Components[0].Name)
	assert.Equal(t, "256Mi", got.Spec.Components[0].MemoryLimit)
}

func TestPlatform_UpdateComponentReturns404WhenNoPlatformConfig(t *testing.T) {
	h := &Platform{CRClient: testCRClient()}

	body := bytes.NewReader([]byte(`{"memory_limit": "1Gi"}`))
	req := httptest.NewRequest("PATCH", "/api/v1/platform/components/prometheus", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mountPlatformRoutes(h).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestPlatform_UpdateComponentPreservesOtherOverrides(t *testing.T) {
	// PATCH on prometheus must not touch the existing loki override.
	yes := true
	pc := newPlatformConfig(platform.ProfileMedium,
		[]kipperv1.ComponentOverride{
			{Name: "loki", MemoryLimit: "768Mi", Enabled: &yes},
		},
		kipperv1.PlatformConfigStatus{})

	crClient := testCRClient(pc)
	h := &Platform{CRClient: crClient}

	body := bytes.NewReader([]byte(`{"memory_limit": "2Gi"}`))
	req := httptest.NewRequest("PATCH", "/api/v1/platform/components/prometheus", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mountPlatformRoutes(h).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got kipperv1.PlatformConfig
	require.NoError(t, crClient.Get(context.Background(),
		crclient.ObjectKey{Name: platformConfigName}, &got))
	require.Len(t, got.Spec.Components, 2)

	byName := map[string]kipperv1.ComponentOverride{}
	for _, c := range got.Spec.Components {
		byName[c.Name] = c
	}
	assert.Equal(t, "2Gi", byName["prometheus"].MemoryLimit)
	assert.Equal(t, "768Mi", byName["loki"].MemoryLimit, "existing loki override must survive a prometheus PATCH")
	require.NotNil(t, byName["loki"].Enabled)
	assert.True(t, *byName["loki"].Enabled)
}
