package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/kip/internal/k8s"
)

// kip status reads mounts over one SSH connection, to the cluster host. On a
// multi-node cluster that is one mount namespace out of several, so every other
// node has to be named as unchecked. Reporting "no read-only volumes" for a node
// nothing looked at is worse than reporting nothing at all.
func TestNodeIsHost(t *testing.T) {
	tests := []struct {
		name string
		node k8s.NodeInfo
		host string
		want bool
	}{
		{
			name: "the node name is the configured host",
			node: k8s.NodeInfo{Name: "node1.example.com"},
			host: "node1.example.com",
			want: true,
		},
		{
			name: "the cluster is configured by address and the node registered by hostname",
			node: k8s.NodeInfo{Name: "node1", IP: "203.0.113.10"},
			host: "203.0.113.10",
			want: true,
		},
		{
			name: "a worker is neither",
			node: k8s.NodeInfo{Name: "worker-2", IP: "203.0.113.11"},
			host: "203.0.113.10",
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, nodeIsHost(tc.node, tc.host))
		})
	}
}
