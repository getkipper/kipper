package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/kip/internal/installer"
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
			fleet := []k8s.NodeInfo{tc.node, {Name: "another", IP: "203.0.113.99"}}
			assert.Equal(t, tc.want, nodeIsHost(tc.node, tc.host, fleet))
		})
	}
}

// The stamp is only worth writing if something reads it. These pin that kip
// status turns the annotations into the list of nodes an operator has to go and
// repair.
func TestStorageRestartCoverageReadsTheStamp(t *testing.T) {
	stamped := func(version, machine string) map[string]string {
		return map[string]string{
			installer.StorageRestartsVersionAnnotation: version,
			installer.StorageRestartsMachineAnnotation: machine,
		}
	}

	nodes := []k8s.NodeInfo{
		{
			Name:        "control-plane",
			MachineID:   "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d",
			Annotations: stamped(installer.StorageRestartConfigVersion, "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d"),
		},
		{
			Name:      "worker-added-by-an-older-kip",
			MachineID: "0000ffff0000ffff0000ffff0000ffff",
		},
		{
			Name:        "worker-reimaged",
			MachineID:   "1111eeee1111eeee1111eeee1111eeee",
			Annotations: stamped(installer.StorageRestartConfigVersion, "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d"),
		},
	}

	var uncovered []string
	for _, node := range nodes {
		covered, _ := installer.StorageRestartCoverage(
			node.Annotations[installer.StorageRestartsVersionAnnotation],
			node.Annotations[installer.StorageRestartsMachineAnnotation],
			node.MachineID,
		)
		if !covered {
			uncovered = append(uncovered, node.Name)
		}
	}

	assert.Equal(t, []string{"worker-added-by-an-older-kip", "worker-reimaged"}, uncovered,
		"a node with no stamp and a node reimaged under its old stamp both need repairing")
}

// The node whose mounts were just read must not be listed as unchecked. A
// cluster configured by DNS name whose node registered under its hostname with
// an internal IP matched neither comparison, so status printed the control
// plane it had just inspected under "not checked on".
func TestNodeIsHostMatchesTheNodeItActuallyReached(t *testing.T) {
	tests := []struct {
		name string
		node k8s.NodeInfo
		host string
		want bool
	}{
		{
			name: "the configured host is a DNS name for the node's address",
			node: k8s.NodeInfo{Name: "node1", IP: "203.0.113.10"},
			host: "203.0.113.10",
			want: true,
		},
		{
			name: "the configured host is the node's own name with a domain on it",
			node: k8s.NodeInfo{Name: "node1", IP: "10.0.0.4"},
			host: "node1.example.com",
			want: true,
		},
		{
			name: "a single-node cluster is always the node it reached",
			node: k8s.NodeInfo{Name: "anything", IP: "10.0.0.9"},
			host: "cluster.example.com",
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nodes := []k8s.NodeInfo{tc.node}
			assert.Equal(t, tc.want, nodeIsHost(tc.node, tc.host, nodes))
		})
	}
}

// On a cluster with several nodes, a worker is still not the host.
func TestNodeIsHostStillExcludesAWorker(t *testing.T) {
	nodes := []k8s.NodeInfo{
		{Name: "control", IP: "203.0.113.10"},
		{Name: "worker-2", IP: "203.0.113.11"},
	}
	assert.True(t, nodeIsHost(nodes[0], "203.0.113.10", nodes))
	assert.False(t, nodeIsHost(nodes[1], "203.0.113.10", nodes))
}
