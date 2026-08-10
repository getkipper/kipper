package handlers

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// clusterIngressIPs returns the deduped set of IPs that public traffic
// reaches the cluster on. We prefer ExternalIPs from each node and fall
// back to InternalIPs only when no node exposes an external one — useful
// for single-node lab clusters where the install script binds Traefik to
// the only address the host has.
func clusterIngressIPs(ctx context.Context, client kubernetes.Interface) ([]string, error) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	var external, internal []string
	for _, node := range nodes.Items {
		for _, addr := range node.Status.Addresses {
			if _, dup := seen[addr.Address]; dup {
				continue
			}
			switch addr.Type {
			case corev1.NodeExternalIP:
				seen[addr.Address] = struct{}{}
				external = append(external, addr.Address)
			case corev1.NodeInternalIP:
				seen[addr.Address] = struct{}{}
				internal = append(internal, addr.Address)
			}
		}
	}

	if len(external) > 0 {
		return external, nil
	}
	return internal, nil
}
