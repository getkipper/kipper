package handlers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func ingressTestNode(name string, controlPlane bool, addrs ...corev1.NodeAddress) *corev1.Node {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Addresses: addrs},
	}
	if controlPlane {
		node.Labels = map[string]string{"node-role.kubernetes.io/control-plane": "true"}
	}
	return node
}

func TestClusterIngressIPsLeavesOutWorkers(t *testing.T) {
	client := fake.NewClientset(
		ingressTestNode("server", true, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "203.0.113.10"}),
		ingressTestNode("worker", false, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "198.51.100.20"}),
	)

	ips, err := clusterIngressIPs(context.Background(), client)

	require.NoError(t, err)
	assert.Equal(t, []string{"203.0.113.10"}, ips, "only nodes that can run Traefik receive public traffic")
}

func TestClusterIngressIPsIgnoresAWorkerExternalIPWhenTheServerHasOnlyAnInternalOne(t *testing.T) {
	client := fake.NewClientset(
		ingressTestNode("server", true, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "192.0.2.10"}),
		ingressTestNode("worker", false, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "198.51.100.20"}),
	)

	ips, err := clusterIngressIPs(context.Background(), client)

	require.NoError(t, err)
	assert.Equal(t, []string{"192.0.2.10"}, ips)
}
