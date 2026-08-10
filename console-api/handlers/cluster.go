package handlers

import (
	"context"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Cluster provides handlers for cluster-level endpoints.
type Cluster struct {
	Client kubernetes.Interface
}

type clusterStatusResponse struct {
	Health string       `json:"health"`
	Nodes  []nodeStatus `json:"nodes"`
}

type nodeStatus struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Role    string `json:"role"`
	Version string `json:"version"`
	IP      string `json:"ip"`
}

// GetStatus returns the cluster health and node summary.
// GET /api/v1/cluster/status
func (c *Cluster) GetStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	nodes, err := c.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list nodes")
		return
	}

	health := "healthy"
	var nodeList []nodeStatus
	for _, node := range nodes.Items {
		ns := toNodeStatus(node)
		if ns.Status != "Ready" {
			health = "degraded"
		}
		nodeList = append(nodeList, ns)
	}

	if len(nodeList) == 0 {
		health = "unknown"
	}

	respondJSON(w, http.StatusOK, clusterStatusResponse{
		Health: health,
		Nodes:  nodeList,
	})
}

// GetNodes returns all cluster nodes.
// GET /api/v1/nodes
func (c *Cluster) GetNodes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	nodes, err := c.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list nodes")
		return
	}

	var result []nodeStatus
	for _, node := range nodes.Items {
		result = append(result, toNodeStatus(node))
	}

	respondJSON(w, http.StatusOK, result)
}

func toNodeStatus(node corev1.Node) nodeStatus {
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

	return nodeStatus{
		Name:    node.Name,
		Status:  status,
		Role:    role,
		Version: node.Status.NodeInfo.KubeletVersion,
		IP:      ip,
	}
}
