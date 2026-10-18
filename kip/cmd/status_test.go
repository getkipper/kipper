package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/kip/internal/k8s"
)

func TestStatusPrintsNoUsageOnARuntimeError(t *testing.T) {
	assert.True(t, statusCmd.SilenceUsage, "an unreachable API is not a usage mistake")
}

func TestNodeMarkFlagsACordonedNode(t *testing.T) {
	assert.Equal(t, "✔", nodeMark(k8s.NodeInfo{Status: "Ready"}))
	assert.Equal(t, "⚠", nodeMark(k8s.NodeInfo{Status: "Ready", Cordoned: true}))
	assert.Equal(t, "✗", nodeMark(k8s.NodeInfo{Status: "NotReady", Cordoned: true}))
}
