package handlers

import (
	"context"
	"math"
	"net/http"
	"sort"
	"time"

	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

const (
	anomalyThreshold  = 30.0
	growthLookbackPts = 10

	// historyWindow and historyStep reproduce the old ConfigMap's rolling
	// hour at one point per minute.
	historyWindow = time.Hour
	historyStep   = time.Minute

	// Per-workload usage grouped by namespace and the pod "app" label, limited
	// to Kipper-managed pods. label_app / label_app_kubernetes_io_managed_by
	// are how kube-state-metrics exposes those pod labels.
	memHistoryQuery = `sum by (namespace, label_app) (container_memory_working_set_bytes{container!="",container!="POD"} * on (namespace, pod) group_left(label_app) kube_pod_labels{label_app_kubernetes_io_managed_by="kipper"})`
	cpuHistoryQuery = `sum by (namespace, label_app) (rate(container_cpu_usage_seconds_total{container!="",container!="POD"}[2m]) * 1000 * on (namespace, pod) group_left(label_app) kube_pod_labels{label_app_kubernetes_io_managed_by="kipper"})`

	allocMemoryQuery = `sum(kube_node_status_allocatable{resource="memory"})`
	allocCPUQuery    = `sum(kube_node_status_allocatable{resource="cpu"}) * 1000`
)

// UsageHistory serves the workload and node usage history for dashboard charts,
// read live from Prometheus.
type UsageHistory struct {
	PrometheusBaseURL string
	// CRClient reads the PlatformConfig to tell a deliberately disabled
	// monitoring stack apart from a Prometheus outage.
	CRClient crclient.Client
	// Injected in tests; nil in production builds the real client.
	PromQueryRangeSeries PromQueryRangeSeriesFunc
	PromQueryInstant     PromQueryInstantFunc
}

type usageSnapshot struct {
	Time        string `json:"time"`
	CPUMillis   int64  `json:"cpu_millis"`
	MemoryBytes int64  `json:"memory_bytes"`
}

type nodeHistory struct {
	AllocatableMemoryBytes int64           `json:"allocatable_memory_bytes"`
	AllocatableCPUMillis   int64           `json:"allocatable_cpu_millis"`
	History                []usageSnapshot `json:"history"`
}

type workloadHistory struct {
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	History   []usageSnapshot `json:"history"`
}

type usageHistoryResponse struct {
	Node      nodeHistory               `json:"node"`
	Workloads []workloadHistoryResponse `json:"workloads"`
	// Degraded is true when a Prometheus query errored, so the console can
	// distinguish a monitoring outage (show "metrics unavailable") from a
	// genuinely idle cluster (empty series, allocatable known).
	Degraded bool `json:"degraded"`
}

type workloadHistoryResponse struct {
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Anomaly   bool            `json:"anomaly"`
	GrowthPct float64         `json:"growth_pct"`
	History   []usageSnapshot `json:"history"`
}

// Get returns the usage history data.
// GET /api/v1/dashboard/usage-history
func (h *UsageHistory) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// A deliberately disabled monitoring stack (nano profile, or
	// `kip platform disable prometheus`) is not an outage: report a clean
	// empty history instead of a degraded one, and skip the doomed queries.
	if !h.monitoringEnabled(ctx) {
		respondJSON(w, http.StatusOK, usageHistoryResponse{
			Workloads: []workloadHistoryResponse{},
		})
		return
	}

	queryRange, queryInstant := h.prometheusClients()

	end := time.Now()
	start := end.Add(-historyWindow)

	// Degrade to an empty series rather than failing the dashboard when
	// Prometheus is briefly unreachable, but flag it so the console can tell a
	// monitoring outage apart from an idle cluster.
	memSeries, memErr := queryRange(ctx, memHistoryQuery, start, end, historyStep)
	cpuSeries, cpuErr := queryRange(ctx, cpuHistoryQuery, start, end, historyStep)

	// Drop series in projects the caller cannot see before aggregating, so the
	// node history stays the sum of the workloads they are shown and never
	// carries another tenant's usage. Admins resolve every namespace, so they
	// still see the whole cluster.
	memSeries = filterSeriesByAccess(r, memSeries)
	cpuSeries = filterSeriesByAccess(r, cpuSeries)

	nodeSeries, workloads := buildUsageHistory(memSeries, cpuSeries)

	allocMem, allocMemErr := instantValue(ctx, queryInstant, allocMemoryQuery, end)
	allocCPU, allocCPUErr := instantValue(ctx, queryInstant, allocCPUQuery, end)

	resp := usageHistoryResponse{
		Node: nodeHistory{
			AllocatableMemoryBytes: allocMem,
			AllocatableCPUMillis:   allocCPU,
			History:                nodeSeries,
		},
		Workloads: make([]workloadHistoryResponse, 0, len(workloads)),
		Degraded:  memErr != nil || cpuErr != nil || allocMemErr != nil || allocCPUErr != nil,
	}

	for _, wh := range workloads {
		// Only surface workloads in projects the caller belongs to.
		if !canAccessNamespace(r, wh.Namespace) {
			continue
		}
		growth := calcWorkloadGrowth(wh)
		resp.Workloads = append(resp.Workloads, workloadHistoryResponse{
			Name:      wh.Name,
			Namespace: wh.Namespace,
			Anomaly:   growth > anomalyThreshold,
			GrowthPct: growth,
			History:   wh.History,
		})
	}

	respondJSON(w, http.StatusOK, resp)
}

// monitoringEnabled reports whether the prometheus component is effectively
// enabled. Unknown states (no CR client wired, missing or unreadable
// PlatformConfig) count as enabled, so a real query failure still surfaces
// as degraded instead of being masked.
func (h *UsageHistory) monitoringEnabled(ctx context.Context) bool {
	if h.CRClient == nil {
		return true
	}
	var pc kipperv1.PlatformConfig
	if err := h.CRClient.Get(ctx, crclient.ObjectKey{Name: platformConfigName}, &pc); err != nil {
		return true
	}
	overrides := enabledOverrideMap(indexOverrides(pc.Spec.Components))
	return platform.EffectiveEnabled(platform.ComponentPrometheus, overrides, pc.Spec.Profile)
}

func (h *UsageHistory) prometheusClients() (PromQueryRangeSeriesFunc, PromQueryInstantFunc) {
	qrs := h.PromQueryRangeSeries
	qi := h.PromQueryInstant
	if qrs == nil || qi == nil {
		client := &http.Client{Timeout: 5 * time.Second}
		base := h.PrometheusBaseURL
		if base == "" {
			base = defaultPromURL
		}
		if qrs == nil {
			qrs = realPromQueryRangeSeries(client, base)
		}
		if qi == nil {
			qi = realPromQueryInstant(client, base)
		}
	}
	return qrs, qi
}

// filterSeriesByAccess keeps only the series whose namespace the caller can
// read. A series with no namespace label is dropped: it cannot be attributed
// to a project, so it must not reach the node aggregate a tenant sees.
func filterSeriesByAccess(r *http.Request, series []PromSeries) []PromSeries {
	kept := make([]PromSeries, 0, len(series))
	allowed := make(map[string]bool)
	for _, s := range series {
		ns := s.Labels["namespace"]
		if ns == "" {
			continue
		}
		access, seen := allowed[ns]
		if !seen {
			access = canAccessNamespace(r, ns)
			allowed[ns] = access
		}
		if access {
			kept = append(kept, s)
		}
	}
	return kept
}

// buildUsageHistory folds the per-workload memory and CPU series into a
// per-workload history and a node history that is their per-timestamp sum, so
// the node total always equals the sum of the workloads shown.
func buildUsageHistory(mem, cpu []PromSeries) ([]usageSnapshot, []workloadHistory) {
	type key struct{ ns, app string }
	workloads := map[key]map[int64]*usageSnapshot{}
	node := map[int64]*usageSnapshot{}

	apply := func(series []PromSeries, add func(s *usageSnapshot, v float64)) {
		for _, s := range series {
			k := key{s.Labels["namespace"], s.Labels["label_app"]}
			if k.ns == "" || k.app == "" {
				continue
			}
			points := workloads[k]
			if points == nil {
				points = map[int64]*usageSnapshot{}
				workloads[k] = points
			}
			for _, sample := range s.Samples {
				ts := sample.Time.Unix()
				iso := sample.Time.UTC().Format(time.RFC3339)

				ws := points[ts]
				if ws == nil {
					ws = &usageSnapshot{Time: iso}
					points[ts] = ws
				}
				add(ws, sample.Value)

				ns := node[ts]
				if ns == nil {
					ns = &usageSnapshot{Time: iso}
					node[ts] = ns
				}
				add(ns, sample.Value)
			}
		}
	}

	apply(mem, func(s *usageSnapshot, v float64) { s.MemoryBytes += int64(v) })
	apply(cpu, func(s *usageSnapshot, v float64) { s.CPUMillis += int64(v) })

	out := make([]workloadHistory, 0, len(workloads))
	for k, points := range workloads {
		out = append(out, workloadHistory{Name: k.app, Namespace: k.ns, History: sortSnapshots(points)})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Namespace+"/"+out[i].Name < out[j].Namespace+"/"+out[j].Name
	})

	return sortSnapshots(node), out
}

func sortSnapshots(points map[int64]*usageSnapshot) []usageSnapshot {
	times := make([]int64, 0, len(points))
	for ts := range points {
		times = append(times, ts)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })

	out := make([]usageSnapshot, 0, len(times))
	for _, ts := range times {
		out = append(out, *points[ts])
	}
	return out
}

// instantValue returns the scalar result of an instant query. A NaN result is
// "no data" and returns (0, nil); only a query error is reported, so the caller
// can flag a Prometheus outage without treating an idle cluster as degraded.
func instantValue(ctx context.Context, query PromQueryInstantFunc, expr string, at time.Time) (int64, error) {
	v, err := query(ctx, expr, at)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(v) {
		return 0, nil
	}
	return int64(v), nil
}

func calcWorkloadGrowth(wh workloadHistory) float64 {
	if len(wh.History) < 2 {
		return 0
	}
	lookbackIdx := len(wh.History) - growthLookbackPts
	if lookbackIdx < 0 {
		lookbackIdx = 0
	}
	old := wh.History[lookbackIdx].MemoryBytes
	current := wh.History[len(wh.History)-1].MemoryBytes
	if old > 0 {
		return (float64(current) - float64(old)) / float64(old) * 100
	}
	return 0
}
