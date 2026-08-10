package handlers

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// sampleSeries builds one labelled series from (unix-offset, value) points.
func sampleSeries(namespace, app string, base time.Time, values ...float64) PromSeries {
	samples := make([]PromSample, len(values))
	for i, v := range values {
		samples[i] = PromSample{Time: base.Add(time.Duration(i) * time.Minute), Value: v}
	}
	return PromSeries{
		Labels:  map[string]string{"namespace": namespace, "label_app": app},
		Samples: samples,
	}
}

func TestUsageHistory_BuildsFromPrometheus(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()

	mem := []PromSeries{
		sampleSeries("blog", "web", base, 100e6, 150e6, 200e6),
		sampleSeries("blog", "api", base, 50e6, 50e6, 50e6),
	}
	cpu := []PromSeries{
		sampleSeries("blog", "web", base, 500, 600, 700),
		sampleSeries("blog", "api", base, 100, 100, 100),
	}

	h := &UsageHistory{
		PromQueryRangeSeries: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]PromSeries, error) {
			if strings.Contains(query, "container_memory") {
				return mem, nil
			}
			return cpu, nil
		},
		PromQueryInstant: func(_ context.Context, query string, _ time.Time) (float64, error) {
			if strings.Contains(query, "memory") {
				return 8e9, nil
			}
			return 4000, nil
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard/usage-history", nil)
	w := httptest.NewRecorder()
	h.Get(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp usageHistoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Node.AllocatableMemoryBytes != 8e9 || resp.Node.AllocatableCPUMillis != 4000 {
		t.Errorf("allocatable = %d mem / %d cpu, want 8e9 / 4000", resp.Node.AllocatableMemoryBytes, resp.Node.AllocatableCPUMillis)
	}

	// Node history is the per-timestamp sum of the two workloads.
	if len(resp.Node.History) != 3 {
		t.Fatalf("expected 3 node points, got %d", len(resp.Node.History))
	}
	if got := resp.Node.History[0].MemoryBytes; got != 150e6 {
		t.Errorf("node mem[0] = %d, want 150e6", got)
	}
	if got := resp.Node.History[2].MemoryBytes; got != 250e6 {
		t.Errorf("node mem[2] = %d, want 250e6", got)
	}
	if got := resp.Node.History[1].CPUMillis; got != 700 {
		t.Errorf("node cpu[1] = %d, want 700", got)
	}

	// Workloads are sorted by namespace/name: api before web.
	if len(resp.Workloads) != 2 {
		t.Fatalf("expected 2 workloads, got %d", len(resp.Workloads))
	}
	if resp.Workloads[0].Name != "api" || resp.Workloads[1].Name != "web" {
		t.Fatalf("expected [api, web], got [%s, %s]", resp.Workloads[0].Name, resp.Workloads[1].Name)
	}

	web := resp.Workloads[1]
	if web.History[2].MemoryBytes != 200e6 || web.History[2].CPUMillis != 700 {
		t.Errorf("web last point = %d mem / %d cpu, want 200e6 / 700", web.History[2].MemoryBytes, web.History[2].CPUMillis)
	}
	// web grew 100MB -> 200MB = 100% -> flagged anomalous.
	if !web.Anomaly || web.GrowthPct < 99 || web.GrowthPct > 101 {
		t.Errorf("web growth = %.1f%% anomaly=%v, want ~100%% and true", web.GrowthPct, web.Anomaly)
	}
	if resp.Workloads[0].Anomaly {
		t.Error("flat api workload should not be anomalous")
	}
}

func TestUsageHistory_PrometheusUnavailableReturnsEmpty(t *testing.T) {
	h := &UsageHistory{
		PromQueryRangeSeries: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]PromSeries, error) {
			return nil, context.DeadlineExceeded
		},
		PromQueryInstant: func(_ context.Context, _ string, _ time.Time) (float64, error) {
			return 0, context.DeadlineExceeded
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard/usage-history", nil)
	w := httptest.NewRecorder()
	h.Get(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (degrade, not fail), got %d", w.Code)
	}
	var resp usageHistoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Workloads) != 0 || len(resp.Node.History) != 0 {
		t.Errorf("expected empty history when Prometheus is down, got %d workloads / %d node points", len(resp.Workloads), len(resp.Node.History))
	}
	if !resp.Degraded {
		t.Error("expected degraded=true when every Prometheus query errors")
	}
}

// An idle cluster returns empty series with no query error, which must not be
// reported as degraded — that is reserved for an actual Prometheus outage.
func TestUsageHistory_IdleClusterNotDegraded(t *testing.T) {
	h := &UsageHistory{
		PromQueryRangeSeries: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]PromSeries, error) {
			return nil, nil
		},
		PromQueryInstant: func(_ context.Context, _ string, _ time.Time) (float64, error) {
			return math.NaN(), nil
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard/usage-history", nil)
	w := httptest.NewRecorder()
	h.Get(w, req)

	var resp usageHistoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Degraded {
		t.Error("expected degraded=false for an idle cluster with no query errors")
	}
}

// A deliberately disabled monitoring stack (nano profile, or an explicit
// prometheus disable) is not an outage: the response must be clean-empty, not
// degraded, and no Prometheus query may run.
func TestUsageHistory_DisabledMonitoringNotDegraded(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := kipperv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	enabled := false
	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: platformConfigName},
		Spec: kipperv1.PlatformConfigSpec{
			Profile: "small",
			Components: []kipperv1.ComponentOverride{
				{Name: "prometheus", Enabled: &enabled},
			},
		},
	}
	queried := false
	h := &UsageHistory{
		CRClient: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(pc).Build(),
		PromQueryRangeSeries: func(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]PromSeries, error) {
			queried = true
			return nil, context.DeadlineExceeded
		},
		PromQueryInstant: func(_ context.Context, _ string, _ time.Time) (float64, error) {
			queried = true
			return 0, context.DeadlineExceeded
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard/usage-history", nil)
	w := httptest.NewRecorder()
	h.Get(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp usageHistoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Degraded {
		t.Error("expected degraded=false when monitoring is deliberately disabled")
	}
	if queried {
		t.Error("expected no Prometheus queries when monitoring is disabled")
	}
}

func TestUsageHistory_FiltersByNamespaceAccess(t *testing.T) {
	withResolver(t)

	base := time.Unix(1700000000, 0).UTC()
	mem := []PromSeries{
		sampleSeries("blog", "web", base, 100e6, 120e6),
		sampleSeries("shop", "cart", base, 100e6, 120e6),
	}
	h := &UsageHistory{
		PromQueryRangeSeries: func(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]PromSeries, error) {
			if strings.Contains(query, "container_memory") {
				return mem, nil
			}
			return nil, nil
		},
		PromQueryInstant: func(_ context.Context, _ string, _ time.Time) (float64, error) { return 0, nil },
	}

	// dev@test.com is a member of "blog" only (see withResolver).
	req := reqAs("dev@test.com", "")
	w := httptest.NewRecorder()
	h.Get(w, req)

	var resp usageHistoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Workloads) != 1 || resp.Workloads[0].Namespace != "blog" {
		t.Fatalf("expected only the blog workload, got %+v", resp.Workloads)
	}
}

func TestDoMatrixSeriesQuery_ParsesMultipleSeries(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"matrix","result":[
		{"metric":{"namespace":"blog","label_app":"web"},"values":[[1700000000,"100"],[1700000060,"150"]]},
		{"metric":{"namespace":"blog","label_app":"api"},"values":[[1700000000,"50"]]}
	]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	query := realPromQueryRangeSeries(srv.Client(), srv.URL)
	series, err := query(context.Background(), "up", time.Unix(1700000000, 0), time.Unix(1700000060, 0), time.Minute)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 series, got %d", len(series))
	}
	if series[0].Labels["label_app"] != "web" || len(series[0].Samples) != 2 {
		t.Errorf("series[0] = %+v, want web with 2 samples", series[0])
	}
	if series[0].Samples[1].Value != 150 {
		t.Errorf("series[0].Samples[1] = %v, want 150", series[0].Samples[1].Value)
	}
	if series[1].Labels["label_app"] != "api" || len(series[1].Samples) != 1 {
		t.Errorf("series[1] = %+v, want api with 1 sample", series[1])
	}
}
