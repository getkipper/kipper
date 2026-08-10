package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func newPod(name, namespace string, labels map[string]string, containers ...corev1.Container) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{Containers: containers},
	}
}

func container(name, cpuReq, cpuLim, memReq, memLim string) corev1.Container {
	c := corev1.Container{Name: name, Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}}
	if cpuReq != "" {
		c.Resources.Requests[corev1.ResourceCPU] = resource.MustParse(cpuReq)
	}
	if cpuLim != "" {
		c.Resources.Limits[corev1.ResourceCPU] = resource.MustParse(cpuLim)
	}
	if memReq != "" {
		c.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(memReq)
	}
	if memLim != "" {
		c.Resources.Limits[corev1.ResourceMemory] = resource.MustParse(memLim)
	}
	return c
}

// stubMetrics returns a fake FetchPodMetricsRaw that responds with the
// supplied JSON. err overrides the response.
func stubMetrics(t *testing.T, payload any, err error) func(ctx context.Context, namespace, podName string) ([]byte, error) {
	t.Helper()
	return func(_ context.Context, _, _ string) ([]byte, error) {
		if err != nil {
			return nil, err
		}
		raw, e := json.Marshal(payload)
		require.NoError(t, e)
		return raw, nil
	}
}

func doRequest(t *testing.T, h *ResourceUsage, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/resources/usage?"+query, nil)
	w := httptest.NewRecorder()
	h.Get(w, req)
	return w
}

func decodeUsage(t *testing.T, w *httptest.ResponseRecorder) UsageResponse {
	t.Helper()
	var resp UsageResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

// metricsListPayload mirrors the metrics.k8s.io list shape.
func metricsListPayload(items ...map[string]any) map[string]any {
	return map[string]any{"items": items}
}

func metricsItem(namespace, pod string, containers ...map[string]any) map[string]any {
	return map[string]any{
		"metadata":   map[string]any{"name": pod, "namespace": namespace},
		"containers": containers,
	}
}

func metricsContainer(name, cpu, mem string) map[string]any {
	return map[string]any{
		"name":  name,
		"usage": map[string]string{"cpu": cpu, "memory": mem},
	}
}

func TestResourceUsage_SinglePod(t *testing.T) {
	pod := newPod("prometheus-0", "monitoring", map[string]string{"app": "prometheus"},
		container("prometheus", "200m", "1", "512Mi", "2Gi"))
	objects := []runtime.Object{pod}

	h := &ResourceUsage{
		Client: fake.NewClientset(objects...),
		FetchPodMetricsRaw: stubMetrics(t, metricsItem("monitoring", "prometheus-0",
			metricsContainer("prometheus", "124m", "542Mi")), nil),
	}

	w := doRequest(t, h, "namespace=monitoring&pod=prometheus-0")
	require.Equal(t, http.StatusOK, w.Code)

	resp := decodeUsage(t, w)
	assert.True(t, resp.MetricsAvailable)
	require.Len(t, resp.Containers, 1)

	c := resp.Containers[0]
	assert.Equal(t, "prometheus-0", c.Pod)
	assert.Equal(t, "monitoring", c.Namespace)
	assert.Equal(t, "prometheus", c.Name)
	assert.True(t, c.MetricsPresent)
	assert.Equal(t, int64(124), c.CPUMillis)
	assert.Equal(t, int64(1000), c.CPULimitMillis)
	assert.Equal(t, int64(200), c.CPURequestMillis)
	assert.Equal(t, int64(542*1024*1024), c.MemoryBytes)
	assert.Equal(t, int64(2*1024*1024*1024), c.MemoryLimitBytes)
	assert.Equal(t, int64(512*1024*1024), c.MemoryRequestBytes)

	assert.Equal(t, 1, resp.Totals.PodCount)
	assert.Equal(t, 1, resp.Totals.ContainerCount)
	assert.Equal(t, 1, resp.Totals.ContainersWithMetrics)
	assert.Equal(t, c.CPUMillis, resp.Totals.CPUMillis)
}

func TestResourceUsage_MissingSampleFlagged(t *testing.T) {
	// Two pods exist in the spec; metrics-server has only emitted a
	// sample for one (e.g. the other just rolled out and the next scrape
	// hasn't happened yet). The endpoint must flag the missing-sample
	// container so the frontend renders an unknown state instead of a
	// healthy 0% gauge.
	objects := []runtime.Object{
		newPod("api-1", "staging", map[string]string{"app": "api"}, container("api", "100m", "500m", "128Mi", "256Mi")),
		newPod("api-2", "staging", map[string]string{"app": "api"}, container("api", "100m", "500m", "128Mi", "256Mi")),
	}

	h := &ResourceUsage{
		Client: fake.NewClientset(objects...),
		FetchPodMetricsRaw: stubMetrics(t, metricsListPayload(
			metricsItem("staging", "api-1", metricsContainer("api", "120m", "150Mi")),
		), nil),
	}

	w := doRequest(t, h, "namespace=staging")
	require.Equal(t, http.StatusOK, w.Code)

	resp := decodeUsage(t, w)
	assert.True(t, resp.MetricsAvailable, "metrics-server responded, so top-level flag stays true")
	require.Len(t, resp.Containers, 2)

	rows := map[string]ContainerUsage{}
	for _, c := range resp.Containers {
		rows[c.Pod] = c
	}
	assert.True(t, rows["api-1"].MetricsPresent)
	assert.False(t, rows["api-2"].MetricsPresent, "api-2 has no sample yet")
	assert.Equal(t, int64(0), rows["api-2"].CPUMillis)
	assert.Equal(t, int64(0), rows["api-2"].MemoryBytes)
	// Limits still come back for both, since they live on the pod spec.
	assert.Equal(t, int64(500), rows["api-2"].CPULimitMillis)
	assert.Equal(t, int64(256*1024*1024), rows["api-2"].MemoryLimitBytes)

	// Totals only sum usage for containers that actually have metrics, but
	// limits/requests aggregate across the full set.
	assert.Equal(t, 2, resp.Totals.ContainerCount)
	assert.Equal(t, 1, resp.Totals.ContainersWithMetrics)
	assert.Equal(t, int64(120), resp.Totals.CPUMillis)
	assert.Equal(t, int64(150*1024*1024), resp.Totals.MemoryBytes)
	assert.Equal(t, int64(1000), resp.Totals.CPULimitMillis)
	assert.Equal(t, int64(512*1024*1024), resp.Totals.MemoryLimitBytes)
}

func TestResourceUsage_NamespaceAggregate(t *testing.T) {
	objects := []runtime.Object{
		newPod("api-1", "staging", map[string]string{"app": "api"}, container("api", "100m", "500m", "128Mi", "256Mi")),
		newPod("api-2", "staging", map[string]string{"app": "api"}, container("api", "100m", "500m", "128Mi", "256Mi")),
	}

	h := &ResourceUsage{
		Client: fake.NewClientset(objects...),
		FetchPodMetricsRaw: stubMetrics(t, metricsListPayload(
			metricsItem("staging", "api-1", metricsContainer("api", "50m", "80Mi")),
			metricsItem("staging", "api-2", metricsContainer("api", "150m", "200Mi")),
		), nil),
	}

	w := doRequest(t, h, "namespace=staging")
	require.Equal(t, http.StatusOK, w.Code)

	resp := decodeUsage(t, w)
	assert.True(t, resp.MetricsAvailable)
	assert.Len(t, resp.Containers, 2)
	assert.Equal(t, 2, resp.Totals.PodCount)
	assert.Equal(t, int64(200), resp.Totals.CPUMillis)
	assert.Equal(t, int64(1000), resp.Totals.CPULimitMillis)
	assert.Equal(t, int64(280*1024*1024), resp.Totals.MemoryBytes)
	assert.Equal(t, int64(512*1024*1024), resp.Totals.MemoryLimitBytes)
}

func TestResourceUsage_LabelSelectorFiltersPods(t *testing.T) {
	objects := []runtime.Object{
		newPod("api-1", "staging", map[string]string{"app": "api"}, container("api", "", "200m", "", "128Mi")),
		newPod("worker-1", "staging", map[string]string{"app": "worker"}, container("worker", "", "200m", "", "256Mi")),
	}

	h := &ResourceUsage{
		Client: fake.NewClientset(objects...),
		FetchPodMetricsRaw: stubMetrics(t, metricsListPayload(
			metricsItem("staging", "api-1", metricsContainer("api", "100m", "64Mi")),
			metricsItem("staging", "worker-1", metricsContainer("worker", "500m", "200Mi")),
		), nil),
	}

	w := doRequest(t, h, "namespace=staging&selector=app%3Dapi")
	require.Equal(t, http.StatusOK, w.Code)

	resp := decodeUsage(t, w)
	require.Len(t, resp.Containers, 1)
	assert.Equal(t, "api-1", resp.Containers[0].Pod)
	assert.Equal(t, int64(100), resp.Totals.CPUMillis)
	assert.Equal(t, int64(64*1024*1024), resp.Totals.MemoryBytes)
}

func TestResourceUsage_MetricsUnavailableStillReturnsLimits(t *testing.T) {
	pod := newPod("api-1", "staging", map[string]string{"app": "api"},
		container("api", "100m", "500m", "128Mi", "256Mi"))

	h := &ResourceUsage{
		Client:             fake.NewClientset(pod),
		FetchPodMetricsRaw: stubMetrics(t, nil, fmt.Errorf("connection refused")),
	}

	w := doRequest(t, h, "namespace=staging&pod=api-1")
	require.Equal(t, http.StatusOK, w.Code)

	resp := decodeUsage(t, w)
	assert.False(t, resp.MetricsAvailable, "metrics_available should be false when metrics-server errors")
	require.Len(t, resp.Containers, 1)
	c := resp.Containers[0]
	assert.False(t, c.MetricsPresent)
	assert.Equal(t, int64(0), c.CPUMillis)
	assert.Equal(t, int64(0), c.MemoryBytes)
	assert.Equal(t, int64(500), c.CPULimitMillis, "limits still surface from pod spec")
	assert.Equal(t, int64(256*1024*1024), c.MemoryLimitBytes)
	assert.Equal(t, 0, resp.Totals.ContainersWithMetrics)
}

func TestResourceUsage_PodWithoutNamespace_400(t *testing.T) {
	h := &ResourceUsage{Client: fake.NewClientset()}
	w := doRequest(t, h, "pod=foo")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestResourceUsage_EmptyScope_400(t *testing.T) {
	h := &ResourceUsage{Client: fake.NewClientset()}
	w := doRequest(t, h, "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestResourceUsage_InvalidSelector_400(t *testing.T) {
	h := &ResourceUsage{Client: fake.NewClientset()}
	w := doRequest(t, h, "namespace=staging&selector=not%20a%20selector")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestResourceUsage_PodNotFound_500(t *testing.T) {
	h := &ResourceUsage{Client: fake.NewClientset()}
	w := doRequest(t, h, "namespace=staging&pod=missing")
	// We intentionally surface backend failures as 500 to keep the
	// contract simple — the gauge degrades on the frontend.
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestResourceUsage_OrphanMetricsItemsIgnored(t *testing.T) {
	// metrics-server reports a pod that no longer exists in the API.
	pod := newPod("api-1", "staging", map[string]string{"app": "api"},
		container("api", "", "200m", "", "128Mi"))

	h := &ResourceUsage{
		Client: fake.NewClientset(pod),
		FetchPodMetricsRaw: stubMetrics(t, metricsListPayload(
			metricsItem("staging", "api-1", metricsContainer("api", "100m", "64Mi")),
			metricsItem("staging", "deleted-already", metricsContainer("api", "999m", "999Mi")),
		), nil),
	}

	w := doRequest(t, h, "namespace=staging")
	require.Equal(t, http.StatusOK, w.Code)

	resp := decodeUsage(t, w)
	assert.Len(t, resp.Containers, 1)
	assert.Equal(t, int64(100), resp.Totals.CPUMillis)
}

func TestParsePodMetricsItems_BothShapes(t *testing.T) {
	listRaw, _ := json.Marshal(metricsListPayload(
		metricsItem("ns", "p1", metricsContainer("c", "10m", "1Mi")),
	))
	items, err := parsePodMetricsItems(listRaw)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "p1", items[0].PodName)

	singleRaw, _ := json.Marshal(metricsItem("ns", "p2", metricsContainer("c", "20m", "2Mi")))
	items, err = parsePodMetricsItems(singleRaw)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "p2", items[0].PodName)
}

func TestResourceUsage_PrometheusEnrichment(t *testing.T) {
	pod := newPod("api-1", "staging", map[string]string{"app": "api"},
		container("api", "100m", "500m", "128Mi", "256Mi"))

	var rangeQueries []string
	var instantQueries []string

	h := &ResourceUsage{
		Client: fake.NewClientset(pod),
		FetchPodMetricsRaw: stubMetrics(t, metricsItem("staging", "api-1",
			metricsContainer("api", "120m", "150Mi")), nil),
		PrometheusBaseURL: "http://stub", // non-empty so enrichment runs
		PromQueryRange: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]PromSample, error) {
			rangeQueries = append(rangeQueries, query)
			if strings.Contains(query, "container_memory_working_set_bytes") {
				return []PromSample{
					{Time: time.Now().Add(-2 * time.Minute), Value: 90 * 1024 * 1024},
					{Time: time.Now().Add(-1 * time.Minute), Value: 100 * 1024 * 1024},
				}, nil
			}
			return []PromSample{{Time: time.Now(), Value: 110}}, nil
		},
		PromQueryInstant: func(_ context.Context, query string, _ time.Time) (float64, error) {
			instantQueries = append(instantQueries, query)
			return 12.5, nil
		},
	}

	req := httptest.NewRequest("GET", "/api/v1/resources/usage?namespace=staging&selector=app%3Dapi&include_prometheus=1", nil)
	w := httptest.NewRecorder()
	h.Get(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	resp := decodeUsage(t, w)
	assert.True(t, resp.PrometheusAvailable)
	require.Len(t, resp.MemorySparkline, 2)
	assert.Equal(t, float64(100*1024*1024), resp.MemorySparkline[1])
	require.Len(t, resp.CPUSparkline, 1)
	assert.Equal(t, 12.5, resp.CPUThrottlingPct)

	// Queries should be pod-scoped via the pod regex.
	require.Len(t, rangeQueries, 2)
	require.Len(t, instantQueries, 1)
	for _, q := range rangeQueries {
		assert.Contains(t, q, `pod=~"api-1"`)
		assert.Contains(t, q, `namespace="staging"`)
	}
	assert.Contains(t, instantQueries[0], `container_cpu_cfs_throttled_periods_total`)
}

func TestResourceUsage_PrometheusEnrichment_SkippedByDefault(t *testing.T) {
	pod := newPod("api-1", "staging", map[string]string{"app": "api"},
		container("api", "", "", "", ""))

	var called bool
	h := &ResourceUsage{
		Client:             fake.NewClientset(pod),
		FetchPodMetricsRaw: stubMetrics(t, metricsListPayload(), nil),
		PrometheusBaseURL:  "http://stub",
		PromQueryRange: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]PromSample, error) {
			called = true
			return nil, nil
		},
		PromQueryInstant: func(_ context.Context, _ string, _ time.Time) (float64, error) {
			called = true
			return 0, nil
		},
	}

	// No include_prometheus param — enrichment must not fire.
	req := httptest.NewRequest("GET", "/api/v1/resources/usage?namespace=staging", nil)
	w := httptest.NewRecorder()
	h.Get(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	resp := decodeUsage(t, w)
	assert.False(t, resp.PrometheusAvailable)
	assert.False(t, called, "Prometheus client must not be invoked without include_prometheus")
}

func TestResourceUsage_PrometheusEnrichment_AllQueriesFail(t *testing.T) {
	pod := newPod("api-1", "staging", map[string]string{"app": "api"},
		container("api", "", "", "", ""))

	h := &ResourceUsage{
		Client:             fake.NewClientset(pod),
		FetchPodMetricsRaw: stubMetrics(t, metricsListPayload(), nil),
		PrometheusBaseURL:  "http://stub",
		PromQueryRange: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]PromSample, error) {
			return nil, fmt.Errorf("connection refused")
		},
		PromQueryInstant: func(_ context.Context, _ string, _ time.Time) (float64, error) {
			return 0, fmt.Errorf("connection refused")
		},
	}

	req := httptest.NewRequest("GET", "/api/v1/resources/usage?namespace=staging&include_prometheus=1", nil)
	w := httptest.NewRecorder()
	h.Get(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	resp := decodeUsage(t, w)
	assert.False(t, resp.PrometheusAvailable, "all-failure path leaves prometheus_available off")
	assert.Empty(t, resp.MemorySparkline)
}

func TestResourceUsage_PrometheusEnrichment_DisabledWhenBaseURLEmpty(t *testing.T) {
	pod := newPod("api-1", "staging", map[string]string{"app": "api"},
		container("api", "", "", "", ""))

	var called bool
	h := &ResourceUsage{
		Client:             fake.NewClientset(pod),
		FetchPodMetricsRaw: stubMetrics(t, metricsListPayload(), nil),
		// PrometheusBaseURL deliberately empty — nano cluster shape.
		PromQueryRange: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]PromSample, error) {
			called = true
			return nil, nil
		},
		PromQueryInstant: func(_ context.Context, _ string, _ time.Time) (float64, error) {
			called = true
			return 0, nil
		},
	}

	req := httptest.NewRequest("GET", "/api/v1/resources/usage?namespace=staging&include_prometheus=1", nil)
	w := httptest.NewRecorder()
	h.Get(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	resp := decodeUsage(t, w)
	assert.False(t, resp.PrometheusAvailable)
	assert.False(t, called, "empty PrometheusBaseURL must short-circuit before any query")
}

func newNode(name, memory, cpu string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse(memory),
				corev1.ResourceCPU:    resource.MustParse(cpu),
			},
		},
	}
}

func TestResourceUsage_Summary_BucketsByNamespace(t *testing.T) {
	objects := []runtime.Object{
		newNode("node-1", "16Gi", "4"),
		// System pods across multiple system namespaces.
		newPod("prometheus-0", "monitoring", nil, container("prometheus", "", "", "", "")),
		newPod("loki-0", "monitoring", nil, container("loki", "", "", "", "")),
		newPod("longhorn-mgr-x", "longhorn-system", nil, container("longhorn-manager", "", "", "", "")),
		// App pods in user-created namespaces.
		newPod("api-1", "staging", nil, container("api", "", "", "", "")),
		newPod("worker-1", "production", nil, container("worker", "", "", "", "")),
	}

	h := &ResourceUsage{
		Client: fake.NewClientset(objects...),
		FetchPodMetricsRaw: stubMetrics(t, metricsListPayload(
			metricsItem("monitoring", "prometheus-0", metricsContainer("prometheus", "200m", "500Mi")),
			metricsItem("monitoring", "loki-0", metricsContainer("loki", "100m", "200Mi")),
			metricsItem("longhorn-system", "longhorn-mgr-x", metricsContainer("longhorn-manager", "50m", "100Mi")),
			metricsItem("staging", "api-1", metricsContainer("api", "300m", "256Mi")),
			metricsItem("production", "worker-1", metricsContainer("worker", "100m", "128Mi")),
		), nil),
	}

	req := httptest.NewRequest("GET", "/api/v1/resources/usage/summary", nil)
	w := httptest.NewRecorder()
	h.Summary(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp ClusterSummaryResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.True(t, resp.MetricsAvailable)
	// System bucket: prometheus + loki (both monitoring) + longhorn-manager.
	assert.Equal(t, 3, resp.System.PodCount)
	assert.Equal(t, int64(350), resp.System.CPUMillis)
	assert.Equal(t, int64(800*1024*1024), resp.System.MemoryBytes)

	// Apps bucket: staging api + production worker.
	assert.Equal(t, 2, resp.Apps.PodCount)
	assert.Equal(t, int64(400), resp.Apps.CPUMillis)
	assert.Equal(t, int64(384*1024*1024), resp.Apps.MemoryBytes)

	// Allocatable: single node with 16Gi/4 cores.
	assert.Equal(t, 1, resp.Allocatable.NodeCount)
	assert.Equal(t, int64(16*1024*1024*1024), resp.Allocatable.MemoryBytes)
	assert.Equal(t, int64(4000), resp.Allocatable.CPUMillis)
}

func TestResourceUsage_Summary_MetricsUnavailable_StillReturnsAllocatable(t *testing.T) {
	objects := []runtime.Object{
		newNode("node-1", "8Gi", "2"),
		newNode("node-2", "8Gi", "2"),
	}

	h := &ResourceUsage{
		Client:             fake.NewClientset(objects...),
		FetchPodMetricsRaw: stubMetrics(t, nil, fmt.Errorf("connection refused")),
	}

	req := httptest.NewRequest("GET", "/api/v1/resources/usage/summary", nil)
	w := httptest.NewRecorder()
	h.Summary(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp ClusterSummaryResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.MetricsAvailable)
	assert.Equal(t, 2, resp.Allocatable.NodeCount)
	assert.Equal(t, int64(16*1024*1024*1024), resp.Allocatable.MemoryBytes)
	assert.Equal(t, int64(4000), resp.Allocatable.CPUMillis)
	// Buckets are zero, gauges render as 0% on the dashboard — acceptable
	// degradation when metrics-server is down.
	assert.Equal(t, int64(0), resp.System.CPUMillis)
	assert.Equal(t, int64(0), resp.Apps.CPUMillis)
}

func TestResourceUsage_Summary_SumsAllocatableAcrossNodes(t *testing.T) {
	objects := []runtime.Object{
		newNode("node-1", "4Gi", "2"),
		newNode("node-2", "4Gi", "2"),
		newNode("node-3", "8Gi", "4"),
	}
	h := &ResourceUsage{
		Client:             fake.NewClientset(objects...),
		FetchPodMetricsRaw: stubMetrics(t, metricsListPayload(), nil),
	}

	req := httptest.NewRequest("GET", "/api/v1/resources/usage/summary", nil)
	w := httptest.NewRecorder()
	h.Summary(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp ClusterSummaryResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 3, resp.Allocatable.NodeCount)
	assert.Equal(t, int64(16*1024*1024*1024), resp.Allocatable.MemoryBytes)
	assert.Equal(t, int64(8000), resp.Allocatable.CPUMillis)
}

func TestParseCPUAndMemoryQuantities(t *testing.T) {
	assert.Equal(t, int64(500), parseCPUToMillis("500m"))
	assert.Equal(t, int64(2000), parseCPUToMillis("2"))
	assert.Equal(t, int64(0), parseCPUToMillis(""))
	assert.Equal(t, int64(0), parseCPUToMillis("nonsense"))

	assert.Equal(t, int64(1024*1024), parseMemoryToBytes("1Mi"))
	assert.Equal(t, int64(2*1024*1024*1024), parseMemoryToBytes("2Gi"))
	assert.Equal(t, int64(0), parseMemoryToBytes(""))
}
