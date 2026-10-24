package service

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func kubeNodeObject(name, ready string, taints ...string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "Node",
		"metadata": map[string]interface{}{"name": name},
		"status": map[string]interface{}{"conditions": []interface{}{
			map[string]interface{}{"type": "Ready", "status": ready},
		}},
	}}
	var list []interface{}
	for _, key := range taints {
		list = append(list, map[string]interface{}{"key": key, "effect": "NoExecute"})
	}
	if len(list) > 0 {
		_ = unstructured.SetNestedSlice(obj.Object, list, "spec", "taints")
	}
	return obj
}

func TestReplicaNodesCountsTaggedNodesThatTakeReplicas(t *testing.T) {
	notReady := healthyNode("w3", []string{"kipper-replica"})
	notReady.ready = "False"
	noScheduling := healthyNode("w4", []string{"kipper-replica"})
	noScheduling.allowScheduling = false
	objects := defaultSettings()
	for _, n := range []testNode{
		healthyNode("server", []string{"kipper-server", "kipper-replica"}),
		healthyNode("w1", []string{"kipper-replica"}),
		healthyNode("w2", nil),
		notReady,
		noScheduling,
		healthyNode("w5", []string{"kipper-replica"}),
		healthyNode("w6", []string{"kipper-replica"}),
		healthyNode("w7", []string{"kipper-replica"}),
	} {
		objects = append(objects, longhornNodeObject(n))
	}
	for _, name := range []string{"server", "w1", "w2", "w3", "w4"} {
		objects = append(objects, kubeNodeObject(name, "True"))
	}
	objects = append(objects,
		kubeNodeObject("w5", "False"),
		kubeNodeObject("w6", "True", "node.kubernetes.io/out-of-service"),
	)
	m := storageManager(kipperClasses(), objects...)

	n, err := m.ReplicaNodes(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, n, "Longhorn-healthy nodes count only when their Kubernetes node is there, Ready and not fenced")
}

func TestReplicaNodesWithNoLonghornNodesIsZero(t *testing.T) {
	n, err := storageManager(kipperClasses()).ReplicaNodes(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)
}

func copiesCR(spec *int64, storage map[string]interface{}, condition map[string]interface{}) map[string]interface{} {
	obj := map[string]interface{}{"spec": map[string]interface{}{}, "status": map[string]interface{}{}}
	if spec != nil {
		obj["spec"].(map[string]interface{})["copies"] = *spec
	}
	if storage != nil {
		obj["status"].(map[string]interface{})["storage"] = storage
	}
	if condition != nil {
		condition["type"] = "StorageCopies"
		obj["status"].(map[string]interface{})["conditions"] = []interface{}{condition}
	}
	return obj
}

func TestCopiesSummaryOfAHealthyManagedVolume(t *testing.T) {
	two := int64(2)
	c := CopiesOf(copiesCR(&two,
		map[string]interface{}{"copies": int64(2), "healthyCopies": int64(2), "nodes": []interface{}{"node-a", "node-b"}},
		map[string]interface{}{"status": "True", "reason": "Healthy", "message": "2 of 2 copies are healthy."}))
	assert.Equal(t, "2", c.Summary())
	assert.Empty(t, c.Problem())
}

func TestCopiesSummaryOfAnUnmanagedVolume(t *testing.T) {
	c := CopiesOf(copiesCR(nil,
		map[string]interface{}{"copies": int64(3), "healthyCopies": int64(3), "nodes": []interface{}{"node-a", "node-b", "node-c"}},
		map[string]interface{}{"status": "Unknown", "reason": "NotSet", "message": "Copies are not set. Set them with 'kip service update db --copies 1|2'."}))
	assert.Equal(t, "not set", c.Summary())
	assert.Contains(t, c.Problem(), "kip service update db --copies")
}

func TestCopiesSummaryOfAHeldBackVolume(t *testing.T) {
	two := int64(2)
	c := CopiesOf(copiesCR(&two, nil,
		map[string]interface{}{"status": "False", "reason": "NotEnoughReplicaNodes", "message": "Two copies need two ready nodes."}))
	assert.Equal(t, "2, not created yet", c.Summary())
	assert.Equal(t, "Two copies need two ready nodes.", c.Problem())
}

func TestCopiesSummaryOnAClusterOlderThanCopies(t *testing.T) {
	assert.Empty(t, CopiesOf(copiesCR(nil, nil, nil)).Summary())
}

func TestASummaryShowsARunningConversion(t *testing.T) {
	two := int64(2)
	for _, reason := range []string{"Converting", "ConversionPending"} {
		c := Copies{Wanted: &two, Live: 1, LiveKnown: true, Status: "False", Reason: reason}
		assert.Equal(t, "2, converting", c.Summary())
	}
	blocked := Copies{Wanted: &two, Live: 1, LiveKnown: true, Status: "False", Reason: "Blocked", Message: "No room."}
	assert.Equal(t, "2, blocked", blocked.Summary(), "a blocked change has not reached the requested count")
	assert.Equal(t, "No room.", blocked.Problem())
}
