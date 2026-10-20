package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func workloadPod(name, workload, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: map[string]string{"app": workload}},
		Spec:       corev1.PodSpec{NodeName: node},
	}
}

func clusterNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func TestWorkloadNodesNamesEachNodeWithItsPods(t *testing.T) {
	client := fake.NewSimpleClientset(
		clusterNode("server-1"), clusterNode("worker-1"),
		workloadPod("api-a", "api", "worker-1"),
		workloadPod("api-b", "api", "worker-1"),
		workloadPod("api-c", "api", "server-1"),
		workloadPod("api-d", "api", ""),
		workloadPod("web-a", "web", "server-1"),
	)

	got, err := workloadNodes(context.Background(), client, "shop", "api")
	require.NoError(t, err)
	assert.Equal(t, "server-1 (1 pod), worker-1 (2 pods), 1 pod not scheduled", got)
}

func TestWorkloadNodesWithoutPods(t *testing.T) {
	client := fake.NewSimpleClientset(clusterNode("server-1"), clusterNode("worker-1"))

	got, err := workloadNodes(context.Background(), client, "shop", "api")
	require.NoError(t, err)
	assert.Equal(t, "no pods running", got)
}

func TestWorkloadNodesOnASingleNodeClusterSaysNothing(t *testing.T) {
	objects := []runtime.Object{clusterNode("server-1"), workloadPod("api-a", "api", "server-1")}
	got, err := workloadNodes(context.Background(), fake.NewSimpleClientset(objects...), "shop", "api")
	require.NoError(t, err)
	assert.Empty(t, got)
}

// Finished pods, such as the runs a cron function keeps, are history and not
// where the workload runs now.
func TestWorkloadNodesLeavesOutFinishedPods(t *testing.T) {
	done := workloadPod("nightly-1", "nightly", "worker-1")
	done.Status.Phase = corev1.PodSucceeded
	failed := workloadPod("nightly-2", "nightly", "worker-1")
	failed.Status.Phase = corev1.PodFailed
	client := fake.NewSimpleClientset(clusterNode("server-1"), clusterNode("worker-1"), done, failed)

	got, err := workloadNodes(context.Background(), client, "shop", "nightly")
	require.NoError(t, err)
	assert.Equal(t, "no pods running", got)

	running := workloadPod("nightly-3", "nightly", "server-1")
	running.Status.Phase = corev1.PodRunning
	client = fake.NewSimpleClientset(clusterNode("server-1"), clusterNode("worker-1"), done, running)
	got, err = workloadNodes(context.Background(), client, "shop", "nightly")
	require.NoError(t, err)
	assert.Equal(t, "server-1 (1 pod)", got)
}
