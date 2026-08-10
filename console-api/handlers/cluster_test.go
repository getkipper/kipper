package handlers

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestToNodeStatus(t *testing.T) {
	tests := []struct {
		name           string
		node           corev1.Node
		expectedName   string
		expectedStatus string
		expectedRole   string
		expectedIP     string
	}{
		{
			name: "ready master node with external IP",
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-1",
					Labels: map[string]string{
						"node-role.kubernetes.io/control-plane": "",
					},
				},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
					},
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
						{Type: corev1.NodeExternalIP, Address: "203.0.113.10"},
					},
					NodeInfo: corev1.NodeSystemInfo{
						KubeletVersion: "v1.28.3",
					},
				},
			},
			expectedName:   "master-1",
			expectedStatus: "Ready",
			expectedRole:   "master",
			expectedIP:     "203.0.113.10",
		},
		{
			name: "not ready worker node with only internal IP",
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker-1",
				},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
					},
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "10.0.0.2"},
					},
					NodeInfo: corev1.NodeSystemInfo{
						KubeletVersion: "v1.28.3",
					},
				},
			},
			expectedName:   "worker-1",
			expectedStatus: "NotReady",
			expectedRole:   "worker",
			expectedIP:     "10.0.0.2",
		},
		{
			name: "master role via legacy label",
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "old-master",
					Labels: map[string]string{
						"node-role.kubernetes.io/master": "",
					},
				},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
					},
				},
			},
			expectedName:   "old-master",
			expectedStatus: "Ready",
			expectedRole:   "master",
			expectedIP:     "",
		},
		{
			name: "node with no conditions",
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "bare"},
			},
			expectedName:   "bare",
			expectedStatus: "NotReady",
			expectedRole:   "worker",
			expectedIP:     "",
		},
		{
			name: "external IP preferred over internal",
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "multi-ip"},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
					},
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "10.0.0.3"},
						{Type: corev1.NodeExternalIP, Address: "203.0.113.20"},
					},
				},
			},
			expectedName:   "multi-ip",
			expectedStatus: "Ready",
			expectedRole:   "worker",
			expectedIP:     "203.0.113.20",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := toNodeStatus(tt.node)

			if result.Name != tt.expectedName {
				t.Errorf("expected name %q, got %q", tt.expectedName, result.Name)
			}
			if result.Status != tt.expectedStatus {
				t.Errorf("expected status %q, got %q", tt.expectedStatus, result.Status)
			}
			if result.Role != tt.expectedRole {
				t.Errorf("expected role %q, got %q", tt.expectedRole, result.Role)
			}
			if result.IP != tt.expectedIP {
				t.Errorf("expected IP %q, got %q", tt.expectedIP, result.IP)
			}
		})
	}
}
