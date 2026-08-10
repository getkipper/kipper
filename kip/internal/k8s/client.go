package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

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
	cfg, err := clientcmd.BuildConfigFromFlags("", cluster.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %s: %w", cluster.Kubeconfig, err)
	}
	// No global timeout — long-running operations like log streaming need
	// unbounded connections. Individual operations use context timeouts.

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
	Name    string
	Status  string
	Role    string
	Version string
	IP      string
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
	Name    string
	Healthy bool
	Message string
}

// ClusterHealth checks the health of core cluster components by
// verifying that key deployments are available.
func (c *Client) ClusterHealth(ctx context.Context) ([]ComponentStatus, error) {
	type check struct {
		namespace string
		name      string
		label     string
		kind      string // "deploy", "statefulset", "daemonset", or "" for k3s
	}

	checks := []check{
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
			statuses = append(statuses, ComponentStatus{Name: ch.label, Healthy: healthy, Message: msg})

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
			statuses = append(statuses, ComponentStatus{Name: ch.label, Healthy: healthy, Message: msg})

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
			statuses = append(statuses, ComponentStatus{Name: ch.label, Healthy: healthy, Message: msg})
		}
	}

	return statuses, nil
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
		Name:    node.Name,
		Status:  status,
		Role:    role,
		Version: node.Status.NodeInfo.KubeletVersion,
		IP:      ip,
	}
}
