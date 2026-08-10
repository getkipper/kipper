package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/console-api/middleware"
)

// ResourceUsage serves live CPU and memory usage for any pod (or set of
// pods) that the caller can describe. It powers the gauges in
// <ResourceControl>: the slider/PATCH path is unchanged, but the gauge
// needs both the current limit (from pod.spec) and the current usage
// (from metrics-server). Both come back from this endpoint.
type ResourceUsage struct {
	Client kubernetes.Interface

	// FetchPodMetricsRaw lets tests substitute the metrics-server call.
	// nil → use the real metrics.k8s.io/v1beta1 endpoint.
	//
	// namespace == "" && podName == "" → cluster-wide.
	// namespace != "" && podName == "" → all pods in namespace.
	// namespace != "" && podName != "" → exactly one pod.
	FetchPodMetricsRaw func(ctx context.Context, namespace, podName string) ([]byte, error)

	// PrometheusBaseURL is where Prometheus listens in-cluster. Empty
	// disables Prometheus enrichment regardless of the request's
	// include_prometheus param — useful for nano clusters where
	// monitoring is off.
	PrometheusBaseURL string

	// PromQueryRange / PromQueryInstant let tests stub the Prometheus
	// client. nil → built lazily from PrometheusBaseURL.
	PromQueryRange   PromQueryRangeFunc
	PromQueryInstant PromQueryInstantFunc
}

// systemNamespaces is the set of namespaces whose pods count as
// "platform" rather than "apps" for the dashboard cluster summary.
// Membership is by exact match — apps live in user-created project
// namespaces, which never overlap with these.
var systemNamespaces = map[string]bool{
	"monitoring":      true,
	"kipper-system":   true,
	"longhorn-system": true,
	"dex":             true,
	"zot":             true,
	"kube-system":     true,
	"traefik":         true,
	"cert-manager":    true,
	"keda":            true,
	"velero":          true,
}

// ContainerUsage is one container's current footprint plus the limits and
// requests configured on its pod spec. MetricsPresent distinguishes a real
// zero sample from "metrics-server has not emitted a sample for this
// container yet" — common during scrape lag right after a rollout or while
// a pod is still pending — so the frontend can render an unknown state
// instead of a healthy 0% gauge.
type ContainerUsage struct {
	Pod                string `json:"pod"`
	Namespace          string `json:"namespace"`
	Name               string `json:"name"`
	MetricsPresent     bool   `json:"metrics_present"`
	MemoryBytes        int64  `json:"memory_bytes"`
	MemoryLimitBytes   int64  `json:"memory_limit_bytes"`
	MemoryRequestBytes int64  `json:"memory_request_bytes"`
	CPUMillis          int64  `json:"cpu_millis"`
	CPULimitMillis     int64  `json:"cpu_limit_millis"`
	CPURequestMillis   int64  `json:"cpu_request_millis"`
}

// UsageTotals is the per-call aggregate. The frontend uses it for any
// gauge that covers more than one container (per-workload, per-namespace,
// dashboard rollup). Usage numbers only include containers whose metrics
// have actually been sampled; ContainersWithMetrics vs ContainerCount lets
// the frontend tell partial coverage from full.
type UsageTotals struct {
	MemoryBytes           int64 `json:"memory_bytes"`
	MemoryLimitBytes      int64 `json:"memory_limit_bytes"`
	MemoryRequestBytes    int64 `json:"memory_request_bytes"`
	CPUMillis             int64 `json:"cpu_millis"`
	CPULimitMillis        int64 `json:"cpu_limit_millis"`
	CPURequestMillis      int64 `json:"cpu_request_millis"`
	PodCount              int   `json:"pod_count"`
	ContainerCount        int   `json:"container_count"`
	ContainersWithMetrics int   `json:"containers_with_metrics"`
}

// UsageResponse is the wire shape returned by the handler. metrics_available
// is false when metrics-server is unreachable; the response still includes
// limits/requests (those come from pod.spec, not metrics-server) so the
// frontend can render the gauge denominator even when usage is unknown.
//
// PrometheusAvailable + the *Sparkline fields + CPUThrottlingPct are
// populated only when the request included ?include_prometheus=1 AND
// the in-cluster Prometheus answered. Otherwise these are omitted.
type UsageResponse struct {
	MetricsAvailable    bool             `json:"metrics_available"`
	Containers          []ContainerUsage `json:"containers"`
	Totals              UsageTotals      `json:"totals"`
	PrometheusAvailable bool             `json:"prometheus_available,omitempty"`
	MemorySparkline     []float64        `json:"memory_sparkline,omitempty"`
	CPUSparkline        []float64        `json:"cpu_sparkline,omitempty"`
	CPUThrottlingPct    float64          `json:"cpu_throttling_pct,omitempty"`
}

// Get serves GET /api/v1/resources/usage with these query params:
//
//	namespace=<ns>         — required unless `selector` is set without `pod`
//	pod=<name>             — single-pod mode (requires namespace)
//	selector=<labelSel>    — standard k8s label selector
//
// Empty namespace + selector is cluster-wide. Empty namespace + empty
// selector + empty pod is a 400 — the dashboard summary endpoint should
// pass a sensible selector instead of leaking everything in the cluster.
func (h *ResourceUsage) Get(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	namespace := q.Get("namespace")
	podName := q.Get("pod")
	selector := q.Get("selector")

	if podName != "" && namespace == "" {
		respondError(w, http.StatusBadRequest, "pod requires namespace")
		return
	}
	if namespace == "" && selector == "" {
		respondError(w, http.StatusBadRequest, "namespace or selector required")
		return
	}
	// A named namespace must belong to the caller's project. Cluster-wide
	// selector mode is filtered to accessible namespaces after collection.
	if namespace != "" && !enforceProjectRole(w, r, namespace, middleware.ProjectRoleViewer) {
		return
	}

	sel := labels.Everything()
	if selector != "" {
		parsed, err := labels.Parse(selector)
		if err != nil {
			respondError(w, http.StatusBadRequest, "invalid selector: "+err.Error())
			return
		}
		sel = parsed
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	pods, err := h.listPods(ctx, namespace, podName, sel)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to collect resource usage")
		return
	}
	pods = filterPodsByAccess(r, pods)
	resp := h.collect(ctx, namespace, podName, pods)
	if r.URL.Query().Get("include_prometheus") == "1" {
		h.enrichWithPrometheus(ctx, namespace, pods, &resp)
	}
	respondJSON(w, http.StatusOK, resp)
}

func (h *ResourceUsage) collect(ctx context.Context, namespace, podName string, pods []corev1.Pod) UsageResponse {
	usageByKey, metricsAvailable := h.fetchMetrics(ctx, namespace, podName, pods)

	resp := UsageResponse{
		MetricsAvailable: metricsAvailable,
		Containers:       make([]ContainerUsage, 0),
	}
	podSet := make(map[string]struct{}, len(pods))

	for i := range pods {
		pod := &pods[i]
		for _, c := range pod.Spec.Containers {
			row := ContainerUsage{
				Pod:                pod.Name,
				Namespace:          pod.Namespace,
				Name:               c.Name,
				MemoryLimitBytes:   quantityBytes(c.Resources.Limits, corev1.ResourceMemory),
				MemoryRequestBytes: quantityBytes(c.Resources.Requests, corev1.ResourceMemory),
				CPULimitMillis:     quantityMillis(c.Resources.Limits, corev1.ResourceCPU),
				CPURequestMillis:   quantityMillis(c.Resources.Requests, corev1.ResourceCPU),
			}
			if u, ok := usageByKey[containerKey(pod.Namespace, pod.Name, c.Name)]; ok {
				row.MemoryBytes = u.memoryBytes
				row.CPUMillis = u.cpuMillis
				row.MetricsPresent = true
			}
			resp.Containers = append(resp.Containers, row)

			// Limits/requests aggregate unconditionally — they come from the
			// pod spec and are valid regardless of whether metrics-server
			// has sampled the container yet.
			resp.Totals.MemoryLimitBytes += row.MemoryLimitBytes
			resp.Totals.MemoryRequestBytes += row.MemoryRequestBytes
			resp.Totals.CPULimitMillis += row.CPULimitMillis
			resp.Totals.CPURequestMillis += row.CPURequestMillis
			resp.Totals.ContainerCount++
			if row.MetricsPresent {
				resp.Totals.MemoryBytes += row.MemoryBytes
				resp.Totals.CPUMillis += row.CPUMillis
				resp.Totals.ContainersWithMetrics++
			}
			podSet[pod.Namespace+"/"+pod.Name] = struct{}{}
		}
	}
	resp.Totals.PodCount = len(podSet)
	return resp
}

func (h *ResourceUsage) listPods(ctx context.Context, namespace, podName string, sel labels.Selector) ([]corev1.Pod, error) {
	if podName != "" {
		pod, err := h.Client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if !sel.Matches(labels.Set(pod.Labels)) {
			return nil, nil
		}
		return []corev1.Pod{*pod}, nil
	}
	list, err := h.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: sel.String(),
	})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

type containerUsage struct {
	memoryBytes int64
	cpuMillis   int64
}

func containerKey(namespace, pod, container string) string {
	return namespace + "/" + pod + "/" + container
}

func (h *ResourceUsage) fetchMetrics(ctx context.Context, namespace, podName string, pods []corev1.Pod) (map[string]containerUsage, bool) {
	fetch := h.FetchPodMetricsRaw
	if fetch == nil {
		fetch = h.realFetchPodMetrics
	}

	// Pick the narrowest scope we can. When the caller scoped by selector
	// across the cluster, we still hit the cluster-wide path; the
	// returned items are filtered to the pods we actually listed.
	raw, err := fetch(ctx, namespace, podName)
	if err != nil || len(raw) == 0 {
		return nil, false
	}

	items, err := parsePodMetricsItems(raw)
	if err != nil {
		return nil, false
	}

	wanted := make(map[string]struct{}, len(pods))
	for i := range pods {
		wanted[pods[i].Namespace+"/"+pods[i].Name] = struct{}{}
	}

	out := make(map[string]containerUsage)
	for _, item := range items {
		if _, ok := wanted[item.Namespace+"/"+item.PodName]; !ok {
			continue
		}
		for _, c := range item.Containers {
			out[containerKey(item.Namespace, item.PodName, c.Name)] = containerUsage{
				memoryBytes: parseMemoryToBytes(c.Memory),
				cpuMillis:   parseCPUToMillis(c.CPU),
			}
		}
	}
	return out, true
}

func (h *ResourceUsage) realFetchPodMetrics(ctx context.Context, namespace, podName string) (raw []byte, err error) {
	// The fake client's RESTClient() panics. Recover so unit tests that
	// forget to inject a fetcher still degrade gracefully into the
	// "metrics not available" path.
	defer func() {
		if r := recover(); r != nil {
			raw = nil
			err = fmt.Errorf("metrics-server unavailable")
		}
	}()

	var path string
	switch {
	case namespace != "" && podName != "":
		path = fmt.Sprintf("/apis/metrics.k8s.io/v1beta1/namespaces/%s/pods/%s", namespace, podName)
	case namespace != "":
		path = fmt.Sprintf("/apis/metrics.k8s.io/v1beta1/namespaces/%s/pods", namespace)
	default:
		path = "/apis/metrics.k8s.io/v1beta1/pods"
	}

	return h.Client.Discovery().RESTClient().Get().AbsPath(path).DoRaw(ctx)
}

type rawContainer struct {
	Name  string `json:"name"`
	Usage struct {
		CPU    string `json:"cpu"`
		Memory string `json:"memory"`
	} `json:"usage"`
}

type rawMetadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// podMetricsItem is the normalised intermediate form: regardless of whether
// the response was a single pod or a list, callers see a flat slice.
type podMetricsItem struct {
	Namespace  string
	PodName    string
	Containers []containerMetric
}

type containerMetric struct {
	Name   string
	CPU    string
	Memory string
}

func parsePodMetricsItems(raw []byte) ([]podMetricsItem, error) {
	// The single-pod and list responses have different top-level shapes,
	// so try the list shape first (which has "items") and fall back to
	// the single-pod shape.
	var listShape struct {
		Items []struct {
			Metadata   rawMetadata    `json:"metadata"`
			Containers []rawContainer `json:"containers"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &listShape); err == nil && listShape.Items != nil {
		out := make([]podMetricsItem, 0, len(listShape.Items))
		for _, it := range listShape.Items {
			out = append(out, toItem(it.Metadata, it.Containers))
		}
		return out, nil
	}

	var singleShape struct {
		Metadata   rawMetadata    `json:"metadata"`
		Containers []rawContainer `json:"containers"`
	}
	if err := json.Unmarshal(raw, &singleShape); err != nil {
		return nil, err
	}
	if singleShape.Metadata.Name == "" {
		return nil, fmt.Errorf("metrics response has no pod metadata")
	}
	return []podMetricsItem{toItem(singleShape.Metadata, singleShape.Containers)}, nil
}

func toItem(meta rawMetadata, raws []rawContainer) podMetricsItem {
	cs := make([]containerMetric, 0, len(raws))
	for _, c := range raws {
		cs = append(cs, containerMetric{Name: c.Name, CPU: c.Usage.CPU, Memory: c.Usage.Memory})
	}
	return podMetricsItem{Namespace: meta.Namespace, PodName: meta.Name, Containers: cs}
}

func parseCPUToMillis(s string) int64 {
	if s == "" {
		return 0
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return q.MilliValue()
}

func parseMemoryToBytes(s string) int64 {
	if s == "" {
		return 0
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return q.Value()
}

func quantityBytes(list corev1.ResourceList, key corev1.ResourceName) int64 {
	if list == nil {
		return 0
	}
	q, ok := list[key]
	if !ok {
		return 0
	}
	return q.Value()
}

func quantityMillis(list corev1.ResourceList, key corev1.ResourceName) int64 {
	if list == nil {
		return 0
	}
	q, ok := list[key]
	if !ok {
		return 0
	}
	return q.MilliValue()
}

// enrichWithPrometheus runs three workload-level PromQL queries — memory
// working set over the last hour, CPU rate over the last hour, and the
// CPU throttling ratio for the last 5 minutes — and stitches their
// results into the response. Any failure (no client configured,
// Prometheus unreachable, malformed response) leaves PrometheusAvailable
// false and the enrichment fields empty, so the gauge degrades cleanly.
func (h *ResourceUsage) enrichWithPrometheus(ctx context.Context, namespace string, pods []corev1.Pod, resp *UsageResponse) {
	if h.PrometheusBaseURL == "" || namespace == "" || len(pods) == 0 {
		return
	}
	queryRange, queryInstant := h.prometheusClients()
	if queryRange == nil || queryInstant == nil {
		return
	}

	podRegex := buildPodRegex(pods)
	if podRegex == "" {
		return
	}

	now := time.Now()
	rangeStart := now.Add(-1 * time.Hour)
	step := time.Minute

	// 1h timeout cap so a slow Prometheus can't stall the whole handler.
	promCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	memQuery := fmt.Sprintf(`sum(container_memory_working_set_bytes{namespace=%q,pod=~%q,container!="",container!="POD"})`, namespace, podRegex)
	cpuQuery := fmt.Sprintf(`sum(rate(container_cpu_usage_seconds_total{namespace=%q,pod=~%q,container!="",container!="POD"}[1m])) * 1000`, namespace, podRegex)
	throttleQuery := fmt.Sprintf(`100 * sum(rate(container_cpu_cfs_throttled_periods_total{namespace=%q,pod=~%q,container!="",container!="POD"}[5m])) / clamp_min(sum(rate(container_cpu_cfs_periods_total{namespace=%q,pod=~%q,container!="",container!="POD"}[5m])), 1)`, namespace, podRegex, namespace, podRegex)

	memSeries, memErr := queryRange(promCtx, memQuery, rangeStart, now, step)
	cpuSeries, cpuErr := queryRange(promCtx, cpuQuery, rangeStart, now, step)
	throttle, throttleErr := queryInstant(promCtx, throttleQuery, now)

	// Need at least one signal back to call Prometheus "available". If
	// every query erroed, we treat it the same as "monitoring is off".
	if memErr != nil && cpuErr != nil && throttleErr != nil {
		return
	}
	resp.PrometheusAvailable = true
	if memErr == nil {
		resp.MemorySparkline = sparklineValues(memSeries)
	}
	if cpuErr == nil {
		resp.CPUSparkline = sparklineValues(cpuSeries)
	}
	if throttleErr == nil {
		resp.CPUThrottlingPct = throttle
	}
}

func (h *ResourceUsage) prometheusClients() (PromQueryRangeFunc, PromQueryInstantFunc) {
	qr := h.PromQueryRange
	qi := h.PromQueryInstant
	if qr == nil || qi == nil {
		client := &http.Client{Timeout: 5 * time.Second}
		base := h.PrometheusBaseURL
		if base == "" {
			base = defaultPromURL
		}
		if qr == nil {
			qr = realPromQueryRange(client, base)
		}
		if qi == nil {
			qi = realPromQueryInstant(client, base)
		}
	}
	return qr, qi
}

// buildPodRegex turns ["api-1", "api-2"] into "api-1|api-2", suitable
// for a PromQL =~ filter. Pod names are validated by Kubernetes
// (lowercase RFC 1123) so they need no escaping.
func buildPodRegex(pods []corev1.Pod) string {
	names := make([]string, 0, len(pods))
	for i := range pods {
		// Defensive: skip anything that contains a regex metacharacter.
		// In practice K8s names can't, but a corrupted fake-client
		// fixture could.
		if regexpMetacharacters.MatchString(pods[i].Name) {
			continue
		}
		names = append(names, pods[i].Name)
	}
	return strings.Join(names, "|")
}

var regexpMetacharacters = regexp.MustCompile(`[^a-z0-9.\-]`)

func sparklineValues(samples []PromSample) []float64 {
	out := make([]float64, len(samples))
	for i, s := range samples {
		out[i] = s.Value
	}
	return out
}

// ClusterSummaryBucket carries the memory + CPU footprint of one slice
// of the cluster (system or apps).
type ClusterSummaryBucket struct {
	MemoryBytes int64 `json:"memory_bytes"`
	CPUMillis   int64 `json:"cpu_millis"`
	PodCount    int   `json:"pod_count"`
}

// AllocatableBucket is the cluster-wide capacity, summed across nodes.
// It's the gauge denominator on the Dashboard.
type AllocatableBucket struct {
	MemoryBytes int64 `json:"memory_bytes"`
	CPUMillis   int64 `json:"cpu_millis"`
	NodeCount   int   `json:"node_count"`
}

// ClusterSummaryResponse is the wire shape for the dashboard's four
// gauges: it splits live usage into system vs apps and reports the total
// allocatable capacity. The frontend renders one ReadResourceControl per
// bucket using (system | apps).<dimension> over allocatable.<dimension>.
type ClusterSummaryResponse struct {
	MetricsAvailable bool                 `json:"metrics_available"`
	System           ClusterSummaryBucket `json:"system"`
	Apps             ClusterSummaryBucket `json:"apps"`
	Allocatable      AllocatableBucket    `json:"allocatable"`
}

// Summary serves GET /api/v1/resources/usage/summary. It buckets every
// pod into system or apps by namespace and returns aggregate usage
// alongside the cluster's allocatable capacity from node.Status.
// Metrics-server unavailable degrades to MetricsAvailable=false, leaving
// allocatable populated so the gauges still render with the right
// denominator even if usage shows 0%.
func (h *ResourceUsage) Summary(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	resp := ClusterSummaryResponse{}

	// Allocatable: sum across all nodes. Failure here is rare and would
	// signal a deeper API outage; surface as 500 so the dashboard can
	// retry rather than render with a zero denominator.
	nodes, err := h.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list nodes")
		return
	}
	for i := range nodes.Items {
		mem, hasMem := nodes.Items[i].Status.Allocatable[corev1.ResourceMemory]
		cpu, hasCPU := nodes.Items[i].Status.Allocatable[corev1.ResourceCPU]
		if hasMem {
			resp.Allocatable.MemoryBytes += mem.Value()
		}
		if hasCPU {
			resp.Allocatable.CPUMillis += cpu.MilliValue()
		}
		resp.Allocatable.NodeCount++
	}

	// Metrics: cluster-wide pod metrics from metrics-server. Each item
	// is a pod whose namespace tells us which bucket it belongs to.
	fetch := h.FetchPodMetricsRaw
	if fetch == nil {
		fetch = h.realFetchPodMetrics
	}
	raw, err := fetch(ctx, "", "")
	if err != nil || len(raw) == 0 {
		respondJSON(w, http.StatusOK, resp)
		return
	}
	items, err := parsePodMetricsItems(raw)
	if err != nil {
		respondJSON(w, http.StatusOK, resp)
		return
	}

	resp.MetricsAvailable = true
	for _, item := range items {
		var mem, cpu int64
		for _, c := range item.Containers {
			mem += parseMemoryToBytes(c.Memory)
			cpu += parseCPUToMillis(c.CPU)
		}
		if systemNamespaces[item.Namespace] {
			resp.System.MemoryBytes += mem
			resp.System.CPUMillis += cpu
			resp.System.PodCount++
		} else {
			// The app total only counts projects the caller belongs to;
			// admins see the whole cluster.
			if !canAccessNamespace(r, item.Namespace) {
				continue
			}
			resp.Apps.MemoryBytes += mem
			resp.Apps.CPUMillis += cpu
			resp.Apps.PodCount++
		}
	}
	respondJSON(w, http.StatusOK, resp)
}
