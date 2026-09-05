package deployer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/fake"
)

// The app in the incident stayed green for fourteen days. Its own Deployment was
// healthy and its liveness probe never touched the database, so every surface an
// operator looks at agreed the app was fine while the service behind it had been
// crash-looping since the Tuesday before.
//
// The app really is running, so its status is not the thing to falsify. What was
// missing is any mention that the thing it depends on is broken.

func boundApp(namespace, name string, services ...string) *unstructured.Unstructured {
	bindings := make([]interface{}, 0, len(services))
	for _, s := range services {
		bindings = append(bindings, map[string]interface{}{"name": s})
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
		"spec": map[string]interface{}{
			"image":           "example.com/api:1",
			"replicas":        int64(1),
			"serviceBindings": bindings,
		},
		"status": map[string]interface{}{
			"phase":         "Running",
			"replicas":      int64(1),
			"readyReplicas": int64(1),
		},
	}}
}

func crashLoopingServicePod(namespace, service string, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      service + "-0",
			Namespace: namespace,
			Labels: map[string]string{
				"app":                     service,
				"kipper.run/service-type": "postgres",
			},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "postgres",
				RestartCount: restarts,
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
				},
			}},
		},
	}
}

func TestListReportsABrokenBoundService(t *testing.T) {
	d, dynClient := testDeployer()
	d.Client = fake.NewSimpleClientset(crashLoopingServicePod("default", "db", 996)) //nolint:staticcheck
	_, err := dynClient.Resource(AppGVR).Namespace("default").
		Create(context.Background(), boundApp("default", "api", "db"), metav1.CreateOptions{})
	require.NoError(t, err)

	apps, err := d.List(context.Background(), "default")
	require.NoError(t, err)
	require.Len(t, apps, 1)

	assert.Equal(t, "running", apps[0].Status,
		"the app's own status is true and must stay true")
	assert.Contains(t, apps[0].BrokenDependency, "db",
		"an app whose database is crash-looping must not read as simply healthy")
}

func TestListLeavesAnAppWithHealthyServicesAlone(t *testing.T) {
	d, dynClient := testDeployer()
	_, err := dynClient.Resource(AppGVR).Namespace("default").
		Create(context.Background(), boundApp("default", "api", "db"), metav1.CreateOptions{})
	require.NoError(t, err)

	apps, err := d.List(context.Background(), "default")
	require.NoError(t, err)
	require.Len(t, apps, 1)
	assert.Empty(t, apps[0].BrokenDependency)
}

// Only the services an app actually binds concern it. A broken service belonging
// to another app in the same namespace is that app's problem.
func TestListIgnoresACrashLoopInAServiceItDoesNotBind(t *testing.T) {
	d, dynClient := testDeployer()
	d.Client = fake.NewSimpleClientset(crashLoopingServicePod("default", "cache", 40)) //nolint:staticcheck
	_, err := dynClient.Resource(AppGVR).Namespace("default").
		Create(context.Background(), boundApp("default", "api", "db"), metav1.CreateOptions{})
	require.NoError(t, err)

	apps, err := d.List(context.Background(), "default")
	require.NoError(t, err)
	require.Len(t, apps, 1)
	assert.Empty(t, apps[0].BrokenDependency)
}
