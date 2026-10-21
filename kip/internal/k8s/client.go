package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/config"
)

// Client wraps a Kubernetes clientset configured from a kip cluster.
type Client struct {
	clientset     *kubernetes.Clientset
	dynamicClient dynamic.Interface
	restConfig    *rest.Config
}

// Clientset returns the underlying Kubernetes clientset.
func (c *Client) Clientset() *kubernetes.Clientset {
	return c.clientset
}

// Dynamic returns a dynamic client for working with CRDs.
func (c *Client) Dynamic() dynamic.Interface {
	return c.dynamicClient
}

// RESTConfig returns the rest.Config used to build this client. Needed
// by callers that build SPDY connections on top of the same cluster
// configuration (port-forward, pod exec, etc.).
func (c *Client) RESTConfig() *rest.Config {
	return c.restConfig
}

// NewFromCluster creates a Kubernetes client from a kip cluster config entry.
func NewFromCluster(cluster *config.Cluster) (*Client, error) {
	cfg, err := RESTConfig(cluster)
	if err != nil {
		return nil, err
	}
	// Keep the global timeout unset because log streaming needs unbounded
	// connections. Individual operations use context timeouts.

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic client: %w", err)
	}

	return &Client{clientset: clientset, dynamicClient: dynClient, restConfig: cfg}, nil
}

// NodeInfo holds the summary information for a single cluster node.
type NodeInfo struct {
	Name     string
	Status   string
	Cordoned bool
	Role     string
	Version  string
	IP       string

	MachineID   string
	OSImage     string
	Annotations map[string]string
	BootID      string
}

// Summary returns Status with ",SchedulingDisabled" appended for a cordoned
// node, matching kubectl's status format.
func (n NodeInfo) Summary() string {
	if n.Cordoned {
		return n.Status + ",SchedulingDisabled"
	}
	return n.Status
}

// ListNodes returns summary info for all nodes in the cluster.
func (c *Client) ListNodes(ctx context.Context) ([]NodeInfo, error) {
	nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}

	var result []NodeInfo
	for _, node := range nodes.Items {
		result = append(result, nodeToInfo(node))
	}
	return result, nil
}

// ComponentStatus holds a cluster component's health.
type ComponentStatus struct {
	Name     string
	Healthy  bool
	Starting bool
	Message  string
}

// healthCheck identifies a workload or the k3s node-readiness check.
type healthCheck struct {
	namespace string
	name      string
	label     string
	kind      string
	component string
}

var healthChecks = []healthCheck{
	{"kube-system", "", "k3s", "", ""},
	{"traefik", "traefik", "Traefik", "deploy", ""},
	{"cert-manager", "cert-manager-webhook", "cert-manager", "deploy", ""},
	{"longhorn-system", "longhorn-driver-deployer", "Longhorn", "deploy", platform.ComponentLonghorn},
	{"keda", "keda-operator", "KEDA", "deploy", ""},
	{"monitoring", "loki", "Loki", "statefulset", platform.ComponentLoki},
	{"monitoring", "promtail", "Promtail", "daemonset", platform.ComponentPromtail},
	{"monitoring", "prometheus-kube-prometheus-stack-prometheus", "Prometheus", "statefulset", platform.ComponentPrometheus},
	{"monitoring", "kube-prometheus-stack-grafana", "Grafana", "deploy", platform.ComponentGrafana},
	{"velero", "velero", "Velero", "deploy", platform.ComponentVelero},
	{"dex", "dex", "Dex", "deploy", ""},
	{"kipper-system", "console-api", "Console API", "deploy", ""},
	{"kipper-system", "console", "Console", "deploy", ""},
}

var platformConfigGVR = schema.GroupVersionResource{Group: "kipper.run", Version: "v1alpha1", Resource: "platformconfigs"}

// StorageMode returns the recorded storage mode, defaulting to Longhorn when
// PlatformConfig cannot be read or its storage field is empty or invalidly typed.
func (c *Client) StorageMode(ctx context.Context) string {
	return ReadStorageMode(ctx, c.dynamicClient)
}

// ReadStorageMode reads the recorded storage mode using a dynamic client.
// It defaults to Longhorn when PlatformConfig cannot be read or its storage
// field is empty or has the wrong type.
func ReadStorageMode(ctx context.Context, dyn dynamic.Interface) string {
	pc, err := dyn.Resource(platformConfigGVR).Get(ctx, "platform", metav1.GetOptions{})
	if err != nil {
		return storageModeOf(nil)
	}
	return storageModeOf(pc)
}

func storageModeOf(pc *unstructured.Unstructured) string {
	if pc == nil {
		return platform.StorageLonghorn
	}
	storage, _, _ := unstructured.NestedString(pc.Object, "spec", "storage")
	return platform.StorageMode(storage)
}

// installedChecks selects checks from the configured profile, component overrides
// and storage mode. A nil PlatformConfig includes all checks.
func installedChecks(pc *unstructured.Unstructured) []healthCheck {
	if pc == nil {
		return healthChecks
	}
	profile, _, _ := unstructured.NestedString(pc.Object, "spec", "profile")
	storage, _, _ := unstructured.NestedString(pc.Object, "spec", "storage")
	backup, _, _ := unstructured.NestedString(pc.Object, "spec", "backup")
	overrides := map[string]*bool{}
	components, _, _ := unstructured.NestedSlice(pc.Object, "spec", "components")
	for _, item := range components {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if enabled, ok := m["enabled"].(bool); ok && name != "" {
			overrides[name] = &enabled
		}
	}
	var checks []healthCheck
	for _, ch := range healthChecks {
		if ch.component == "" || platform.Installed(ch.component, overrides, profile, storage, backup) {
			checks = append(checks, ch)
		}
	}
	return checks
}

// ClusterHealth checks node readiness and core workloads, distinguishing
// unhealthy components from those whose pods appear to be starting.
// PlatformConfig selects the components to check. A failed configuration read
// includes all components. Check failures appear in the returned statuses,
// and the returned error is always nil.
func (c *Client) ClusterHealth(ctx context.Context) ([]ComponentStatus, error) {
	// Keep all checks on read failure so configuration errors cannot hide unhealthy components.
	pc, _ := c.dynamicClient.Resource(platformConfigGVR).Get(ctx, "platform", metav1.GetOptions{})
	checks := installedChecks(pc)

	readyNodes := c.readyNodeNames(ctx)

	var statuses []ComponentStatus
	for _, ch := range checks {
		if ch.kind == "" {
			// k3s health — check if any node is ready
			nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			if err != nil {
				statuses = append(statuses, ComponentStatus{
					Name: ch.label, Healthy: false, Message: err.Error(),
				})
				continue
			}
			healthy := false
			for _, node := range nodes.Items {
				for _, cond := range node.Status.Conditions {
					if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
						healthy = true
						break
					}
				}
			}
			msg := fmt.Sprintf("%d node(s)", len(nodes.Items))
			statuses = append(statuses, ComponentStatus{
				Name: ch.label, Healthy: healthy, Message: msg,
			})
			continue
		}

		switch ch.kind {
		case "deploy":
			deploy, err := c.clientset.AppsV1().Deployments(ch.namespace).Get(ctx, ch.name, metav1.GetOptions{})
			if err != nil {
				statuses = append(statuses, ComponentStatus{Name: ch.label, Healthy: false, Message: "not found"})
				continue
			}
			if deploy.Spec.Replicas != nil && *deploy.Spec.Replicas == 0 {
				statuses = append(statuses, ComponentStatus{Name: ch.label, Healthy: true, Message: "disabled"})
				continue
			}
			healthy := deploy.Status.AvailableReplicas > 0
			msg := fmt.Sprintf("%d/%d replicas available", deploy.Status.AvailableReplicas, *deploy.Spec.Replicas)
			statuses = append(statuses, ComponentStatus{Name: ch.label, Healthy: healthy, Message: msg,
				Starting: !healthy && c.selectedPodsStarting(ctx, readyNodes, ch.namespace, deploy.Spec.Selector)})

		case "statefulset":
			ss, err := c.clientset.AppsV1().StatefulSets(ch.namespace).Get(ctx, ch.name, metav1.GetOptions{})
			if err != nil {
				statuses = append(statuses, ComponentStatus{Name: ch.label, Healthy: false, Message: "not found"})
				continue
			}
			desired := int32(1)
			if ss.Spec.Replicas != nil {
				desired = *ss.Spec.Replicas
			}
			if desired == 0 {
				statuses = append(statuses, ComponentStatus{Name: ch.label, Healthy: true, Message: "disabled"})
				continue
			}
			healthy := ss.Status.ReadyReplicas >= desired
			msg := fmt.Sprintf("%d/%d ready", ss.Status.ReadyReplicas, desired)
			statuses = append(statuses, ComponentStatus{Name: ch.label, Healthy: healthy, Message: msg,
				Starting: ss.Status.ReadyReplicas == 0 && c.selectedPodsStarting(ctx, readyNodes, ch.namespace, ss.Spec.Selector)})

		case "daemonset":
			ds, err := c.clientset.AppsV1().DaemonSets(ch.namespace).Get(ctx, ch.name, metav1.GetOptions{})
			if err != nil {
				statuses = append(statuses, ComponentStatus{Name: ch.label, Healthy: false, Message: "not found"})
				continue
			}
			if ds.Status.DesiredNumberScheduled == 0 {
				statuses = append(statuses, ComponentStatus{Name: ch.label, Healthy: true, Message: "disabled"})
				continue
			}
			healthy := ds.Status.NumberReady > 0
			msg := fmt.Sprintf("%d/%d ready", ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)
			statuses = append(statuses, ComponentStatus{Name: ch.label, Healthy: healthy, Message: msg,
				Starting: !healthy && c.selectedPodsStarting(ctx, readyNodes, ch.namespace, ds.Spec.Selector)})
		}
	}

	return statuses, nil
}

// readyNodeNames returns nodes reporting Ready; lookup failures leave the set empty.
func (c *Client) readyNodeNames(ctx context.Context) map[string]bool {
	ready := map[string]bool{}
	nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return ready
	}
	for _, n := range nodes.Items {
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				ready[n.Name] = true
			}
		}
	}
	return ready
}

// selectedPodsStarting requires pod-state evidence before marking a workload as
// starting; selector and lookup errors preserve its unhealthy status.
func (c *Client) selectedPodsStarting(ctx context.Context, readyNodes map[string]bool, namespace string, selector *metav1.LabelSelector) bool {
	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil || sel.Empty() {
		return false
	}
	pods, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return false
	}
	return podsStarting(pods.Items, readyNodes)
}

func nodeToInfo(node corev1.Node) NodeInfo {
	status := "NotReady"
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
			status = "Ready"
			break
		}
	}

	role := "worker"
	if _, ok := node.Labels["node-role.kubernetes.io/master"]; ok {
		role = "master"
	}
	if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
		role = "master"
	}

	var ip string
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeExternalIP {
			ip = addr.Address
			break
		}
		if addr.Type == corev1.NodeInternalIP && ip == "" {
			ip = addr.Address
		}
	}

	return NodeInfo{
		Name:        node.Name,
		Status:      status,
		Cordoned:    node.Spec.Unschedulable,
		Role:        role,
		Version:     node.Status.NodeInfo.KubeletVersion,
		IP:          ip,
		MachineID:   node.Status.NodeInfo.MachineID,
		OSImage:     node.Status.NodeInfo.OSImage,
		BootID:      node.Status.NodeInfo.BootID,
		Annotations: node.Annotations,
	}
}
