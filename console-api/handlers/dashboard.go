package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// oomCacheTTL is how long the cluster-wide OOM-kill scan is reused. It bounds
// the full pod list to once per interval no matter how many dashboards poll.
const oomCacheTTL = 15 * time.Second

// Dashboard provides the cluster health dashboard endpoint.
type Dashboard struct {
	Client kubernetes.Interface

	oomMu    sync.Mutex
	oomCache []oomKill
	oomAt    time.Time
	oomSF    singleflight.Group
}

type dashboardResponse struct {
	Components []componentStatus `json:"components"`
	Nodes      []dashboardNode   `json:"nodes"`
	OOMKills   []oomKill         `json:"oom_kills"`
}

type componentStatus struct {
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	Message string `json:"message"`
}

type dashboardNode struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	CPUUsage    string `json:"cpu_usage"`
	MemoryUsage string `json:"memory_usage"`
	DiskUsage   string `json:"disk_usage"`
}

type oomKill struct {
	Pod       string `json:"pod"`
	Namespace string `json:"namespace"`
	Time      string `json:"time"`
}

type componentCheck struct {
	namespace string
	name      string
	label     string
	kind      string // "deploy", "statefulset", "daemonset", or "" for k3s node check
}

// Status returns the full cluster health dashboard.
// GET /api/v1/dashboard
func (d *Dashboard) Status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// List nodes once and share the result across the k3s component check and
	// the node-usage table instead of listing them twice.
	nodeList, nodeErr := d.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	var nodes []corev1.Node
	if nodeErr == nil {
		nodes = nodeList.Items
	}

	components := d.checkComponents(ctx, nodes, nodeErr)
	dashNodes := d.getNodes(ctx, nodes)
	oomKills := d.getOOMKills(ctx, r)

	respondJSON(w, http.StatusOK, dashboardResponse{
		Components: components,
		Nodes:      dashNodes,
		OOMKills:   oomKills,
	})
}

func (d *Dashboard) checkComponents(ctx context.Context, nodes []corev1.Node, nodeErr error) []componentStatus {
	checks := []componentCheck{
		{"kube-system", "", "k3s", ""},
		{"traefik", "traefik", "Traefik", "deploy"},
		{"cert-manager", "cert-manager-webhook", "cert-manager", "deploy"},
		{"longhorn-system", "longhorn-driver-deployer", "Longhorn", "deploy"},
		{"keda", "keda-operator", "KEDA", "deploy"},
		{"monitoring", "loki", "Loki", "statefulset"},
		{"monitoring", "promtail", "Promtail", "daemonset"},
		{"monitoring", "prometheus-kube-prometheus-stack-prometheus", "Prometheus", "statefulset"},
		{"monitoring", "kube-prometheus-stack-grafana", "Grafana", "deploy"},
		{"velero", "velero", "Velero", "deploy"},
		{"dex", "dex", "Dex", "deploy"},
		{"kipper-system", "console-api", "Console API", "deploy"},
		{"kipper-system", "console", "Console", "deploy"},
	}

	// Each check is an independent read, so run them concurrently instead of
	// paying a serial round-trip per component. Results stay in check order.
	statuses := make([]componentStatus, len(checks))
	var wg sync.WaitGroup
	for i := range checks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = d.checkOne(ctx, checks[i], nodes, nodeErr)
		}(i)
	}
	wg.Wait()
	return statuses
}

func (d *Dashboard) checkOne(ctx context.Context, ch componentCheck, nodes []corev1.Node, nodeErr error) componentStatus {
	if ch.kind == "" {
		return checkK3s(ch.label, nodes, nodeErr)
	}

	switch ch.kind {
	case "deploy":
		return d.checkDeployment(ctx, ch)
	case "statefulset":
		return d.checkStatefulSet(ctx, ch)
	case "daemonset":
		return d.checkDaemonSet(ctx, ch)
	default:
		return componentStatus{Name: ch.label, Healthy: false, Message: "unknown kind"}
	}
}

func checkK3s(label string, nodes []corev1.Node, nodeErr error) componentStatus {
	if nodeErr != nil {
		return componentStatus{Name: label, Healthy: false, Message: nodeErr.Error()}
	}
	healthy := false
	for _, node := range nodes {
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				healthy = true
				break
			}
		}
	}
	return componentStatus{
		Name:    label,
		Healthy: healthy,
		Message: fmt.Sprintf("%d node(s)", len(nodes)),
	}
}

func (d *Dashboard) checkDeployment(ctx context.Context, ch componentCheck) componentStatus {
	deploy, err := d.Client.AppsV1().Deployments(ch.namespace).Get(ctx, ch.name, metav1.GetOptions{})
	if err != nil {
		return componentStatus{Name: ch.label, Healthy: false, Message: "not found"}
	}
	desired := int32(1)
	if deploy.Spec.Replicas != nil {
		desired = *deploy.Spec.Replicas
	}
	healthy := deploy.Status.AvailableReplicas > 0
	msg := fmt.Sprintf("%d/%d available", deploy.Status.AvailableReplicas, desired)
	return componentStatus{Name: ch.label, Healthy: healthy, Message: msg}
}

func (d *Dashboard) checkStatefulSet(ctx context.Context, ch componentCheck) componentStatus {
	ss, err := d.Client.AppsV1().StatefulSets(ch.namespace).Get(ctx, ch.name, metav1.GetOptions{})
	if err != nil {
		return componentStatus{Name: ch.label, Healthy: false, Message: "not found"}
	}
	desired := int32(1)
	if ss.Spec.Replicas != nil {
		desired = *ss.Spec.Replicas
	}
	healthy := ss.Status.ReadyReplicas >= desired
	msg := fmt.Sprintf("%d/%d ready", ss.Status.ReadyReplicas, desired)
	return componentStatus{Name: ch.label, Healthy: healthy, Message: msg}
}

func (d *Dashboard) checkDaemonSet(ctx context.Context, ch componentCheck) componentStatus {
	ds, err := d.Client.AppsV1().DaemonSets(ch.namespace).Get(ctx, ch.name, metav1.GetOptions{})
	if err != nil {
		return componentStatus{Name: ch.label, Healthy: false, Message: "not found"}
	}
	healthy := ds.Status.NumberReady > 0
	msg := fmt.Sprintf("%d/%d ready", ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)
	return componentStatus{Name: ch.label, Healthy: healthy, Message: msg}
}

type nodeUsage struct {
	cpu    string
	memory string
}

func (d *Dashboard) getNodes(ctx context.Context, nodes []corev1.Node) []dashboardNode {
	// Fetch usage for all nodes in one metrics call rather than one per node.
	usage := fetchNodeMetrics(d.Client, ctx)

	var result []dashboardNode
	for _, node := range nodes {
		status := "NotReady"
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				status = "Ready"
				break
			}
		}

		// Node resource usage requires metrics-server. When it is unavailable
		// the node is absent from the map, so we report "n/a" and let the
		// frontend handle that gracefully.
		cpu, mem := "n/a", "n/a"
		if u, ok := usage[node.Name]; ok {
			cpu, mem = u.cpu, u.memory
		}

		result = append(result, dashboardNode{
			Name:        node.Name,
			Status:      status,
			CPUUsage:    cpu,
			MemoryUsage: mem,
			// Disk usage is not exposed by metrics-server (CPU and memory
			// only), so it is reported as unavailable.
			DiskUsage: "n/a",
		})
	}
	return result
}

// fetchNodeMetrics returns per-node CPU and memory usage keyed by node name,
// read from metrics-server in a single list call. An empty map is returned
// when metrics-server is unavailable.
func fetchNodeMetrics(client kubernetes.Interface, ctx context.Context) (usage map[string]nodeUsage) {
	usage = make(map[string]nodeUsage)

	// The fake client's RESTClient() panics on use, so we recover gracefully.
	defer func() {
		if r := recover(); r != nil {
			usage = make(map[string]nodeUsage)
		}
	}()

	raw, err := client.CoreV1().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/nodes").
		Do(ctx).Raw()
	if err != nil {
		return usage
	}

	// Lightweight parsing without importing metrics types. The list shape is:
	// {"items": [{"metadata": {"name": "n1"}, "usage": {"cpu": "250m", "memory": "512Mi"}}]}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Usage struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			} `json:"usage"`
		} `json:"items"`
	}
	if err := decodeJSONBytes(raw, &list); err != nil {
		return usage
	}

	for _, item := range list.Items {
		if item.Metadata.Name == "" || item.Usage.CPU == "" {
			continue
		}
		usage[item.Metadata.Name] = nodeUsage{cpu: item.Usage.CPU, memory: item.Usage.Memory}
	}
	return usage
}

func (d *Dashboard) getOOMKills(ctx context.Context, r *http.Request) []oomKill {
	all := d.allOOMKills(ctx)

	// Filter to projects the caller belongs to. Monitoring and other platform
	// namespaces are kept for admins so component OOMs still surface.
	kills := make([]oomKill, 0, len(all))
	for _, k := range all {
		if canAccessNamespace(r, k.Namespace) {
			kills = append(kills, k)
		}
	}
	return kills
}

// allOOMKills returns every OOM kill in the last 24h across the cluster. The
// full pod list is expensive, so the result is cached briefly and shared by
// concurrent dashboard requests; access filtering happens per request in
// getOOMKills.
func (d *Dashboard) allOOMKills(ctx context.Context) []oomKill {
	d.oomMu.Lock()
	fresh := !d.oomAt.IsZero() && time.Since(d.oomAt) < oomCacheTTL
	cached := d.oomCache
	d.oomMu.Unlock()
	if fresh {
		return cached
	}

	// Collapse concurrent refreshes into a single pod List so a burst of
	// dashboard requests does not each scan the cluster, and never hold the
	// cache lock across that List — callers only block on it around the fast
	// cache read/write, not the I/O.
	v, _, _ := d.oomSF.Do("scan", func() (any, error) {
		cutoff := time.Now().Add(-24 * time.Hour)
		pods, err := d.Client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
		if err != nil {
			// Serve the last good scan rather than dropping the section on a
			// transient list error; oomAt is left stale so the next call retries.
			d.oomMu.Lock()
			last := d.oomCache
			d.oomMu.Unlock()
			return last, nil //nolint:nilerr // best-effort: serve the cached scan on a transient list error
		}

		var kills []oomKill
		for _, pod := range pods.Items {
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.LastTerminationState.Terminated == nil {
					continue
				}
				term := cs.LastTerminationState.Terminated
				if term.Reason != "OOMKilled" {
					continue
				}
				if term.FinishedAt.Time.Before(cutoff) {
					continue
				}
				kills = append(kills, oomKill{
					Pod:       pod.Name,
					Namespace: pod.Namespace,
					Time:      term.FinishedAt.Time.UTC().Format(time.RFC3339),
				})
			}
		}

		d.oomMu.Lock()
		d.oomCache = kills
		d.oomAt = time.Now()
		d.oomMu.Unlock()
		return kills, nil
	})
	return v.([]oomKill)
}

func decodeJSONBytes(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
