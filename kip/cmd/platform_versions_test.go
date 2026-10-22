package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/getkipper/kipper/controller/pkg/labels"
)

func podSpec(images ...string) corev1.PodTemplateSpec {
	var containers []corev1.Container
	for _, image := range images {
		containers = append(containers, corev1.Container{Name: "c", Image: image})
	}
	return corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: containers}}
}

func veleroCluster() *fake.Clientset {
	veleroTemplate := podSpec("velero/velero:v1.18.4")
	veleroTemplate.Spec.InitContainers = []corev1.Container{{Name: "aws", Image: "velero/velero-plugin-for-aws:v1.13.0"}}
	return fake.NewClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: "velero", Name: "velero"},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr.To[int32](1), Template: veleroTemplate},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 1, UpdatedReplicas: 1},
		},
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "velero", Name: "node-agent"},
			Spec:       appsv1.DaemonSetSpec{Template: podSpec("velero/velero:v1.18.4")},
			Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, NumberReady: 2, UpdatedNumberScheduled: 2},
		},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "monitoring", Name: "loki"},
			Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1), Template: podSpec("grafana/loki:3.5.0")},
			Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1, UpdatedReplicas: 0},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
			Spec:       appsv1.DeploymentSpec{Template: podSpec("registry.example.com/web:1")},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
			Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.33.4+k3s1"}},
		},
	)
}

func TestCollectPlatformWorkloadsReadsImagesFromPlatformNamespacesOnly(t *testing.T) {
	workloads, err := collectPlatformWorkloads(context.Background(), veleroCluster(), []string{"monitoring", "velero"})
	require.NoError(t, err)

	require.Len(t, workloads, 3)
	assert.Equal(t, platformWorkload{
		Namespace: "monitoring", Kind: "statefulset", Name: "loki",
		Ready: 1, Desired: 1, Updating: true,
		Images: []string{"grafana/loki:3.5.0"},
	}, workloads[0])
	assert.Equal(t, platformWorkload{
		Namespace: "velero", Kind: "deployment", Name: "velero",
		Ready: 1, Desired: 1,
		Images: []string{"velero/velero:v1.18.4", "velero/velero-plugin-for-aws:v1.13.0 (init)"},
	}, workloads[1])
	assert.Equal(t, platformWorkload{
		Namespace: "velero", Kind: "daemonset", Name: "node-agent",
		Ready: 2, Desired: 2,
		Images: []string{"velero/velero:v1.18.4"},
	}, workloads[2])
}

func TestCollectPlatformWorkloadsTreatsAMissingReplicaCountAsOne(t *testing.T) {
	cs := fake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "dex", Name: "dex"},
		Spec:       appsv1.DeploymentSpec{Template: podSpec("ghcr.io/dexidp/dex:v2.44.0")},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1, UpdatedReplicas: 1},
	})

	workloads, err := collectPlatformWorkloads(context.Background(), cs, []string{"dex"})
	require.NoError(t, err)

	require.Len(t, workloads, 1)
	assert.Equal(t, int32(1), workloads[0].Desired)
	assert.False(t, workloads[0].Updating)
}

func TestCollectPlatformWorkloadsMarksAStaleObservedGenerationAsUpdating(t *testing.T) {
	cs := fake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "dex", Name: "dex", Generation: 3},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To[int32](1), Template: podSpec("ghcr.io/dexidp/dex:v2.44.0")},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 2, ReadyReplicas: 1, UpdatedReplicas: 1},
	})

	workloads, err := collectPlatformWorkloads(context.Background(), cs, []string{"dex"})
	require.NoError(t, err)

	require.Len(t, workloads, 1)
	assert.True(t, workloads[0].Updating)
}

func TestCollectPlatformWorkloadsMarksADeploymentWithOldPodsLeftAsUpdating(t *testing.T) {
	cs := fake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "dex", Name: "dex"},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To[int32](1), Template: podSpec("ghcr.io/dexidp/dex:v2.44.0")},
		Status:     appsv1.DeploymentStatus{Replicas: 2, ReadyReplicas: 1, UpdatedReplicas: 1},
	})

	workloads, err := collectPlatformWorkloads(context.Background(), cs, []string{"dex"})
	require.NoError(t, err)

	require.Len(t, workloads, 1)
	assert.True(t, workloads[0].Updating)
}

func TestCollectPlatformWorkloadsMarksAStatefulSetWithOldPodsLeftAsUpdating(t *testing.T) {
	cs := fake.NewClientset(&appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "monitoring", Name: "loki"},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1), Template: podSpec("grafana/loki:3.6.7")},
		Status:     appsv1.StatefulSetStatus{Replicas: 2, ReadyReplicas: 1, UpdatedReplicas: 1},
	})

	workloads, err := collectPlatformWorkloads(context.Background(), cs, []string{"monitoring"})
	require.NoError(t, err)

	require.Len(t, workloads, 1)
	assert.True(t, workloads[0].Updating)
}

func TestCollectPlatformWorkloadsIgnoresStatefulSetRevisionsOnceEveryPodIsUpdated(t *testing.T) {
	// The controller only advances CurrentRevision once updated pods are ready,
	// and never under OnDelete, so the revisions can differ with no old pod left.
	cs := fake.NewClientset(&appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "monitoring", Name: "loki"},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1), Template: podSpec("grafana/loki:3.6.7")},
		Status: appsv1.StatefulSetStatus{
			Replicas: 1, ReadyReplicas: 0, UpdatedReplicas: 1,
			CurrentRevision: "loki-5d8f", UpdateRevision: "loki-7c2a",
		},
	})

	workloads, err := collectPlatformWorkloads(context.Background(), cs, []string{"monitoring"})
	require.NoError(t, err)

	require.Len(t, workloads, 1)
	assert.False(t, workloads[0].Updating)
}

func TestConsoleAPIBuildReadsTheStampOnItsNamespace(t *testing.T) {
	cs := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "kipper-system",
		Annotations: map[string]string{labels.AnnoConsoleAPIBuild: "b72f989d"},
	}})

	build, err := consoleAPIBuild(context.Background(), cs)
	require.NoError(t, err)
	assert.Equal(t, "b72f989d", build)
}

func TestConsoleAPIBuildIsUnknownWithoutTheNamespace(t *testing.T) {
	build, err := consoleAPIBuild(context.Background(), fake.NewClientset())
	require.NoError(t, err)
	assert.Equal(t, "", build)
}

func TestKubernetesVersionsSummarizesNodesThatAgree(t *testing.T) {
	assert.Equal(t, "v1.33.4+k3s1", kubernetesVersions([]corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.33.4+k3s1"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.33.4+k3s1"}}},
	}))
}

func TestKubernetesVersionsListsEachNodeWhenTheyDiffer(t *testing.T) {
	assert.Equal(t, "node-a v1.33.4+k3s1, node-b v1.32.9+k3s1", kubernetesVersions([]corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.32.9+k3s1"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.33.4+k3s1"}}},
	}))
}

func appsCRD(annotations map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": "apps.kipper.run", "annotations": annotations},
	}}
}

func TestKipperVersionOnClusterReadsTheCRDStamp(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), appsCRD(map[string]any{crdWrittenByAnnotation: "0.34.0"}))

	version, err := kipperVersionOnCluster(context.Background(), dyn)
	require.NoError(t, err)
	assert.Equal(t, "0.34.0", version)
}

func TestKipperVersionOnClusterIsUnknownWithoutAStamp(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), appsCRD(map[string]any{}))

	version, err := kipperVersionOnCluster(context.Background(), dyn)
	require.NoError(t, err)
	assert.Equal(t, "", version)
}

func TestKipperVersionOnClusterIsUnknownWithoutTheCRD(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())

	version, err := kipperVersionOnCluster(context.Background(), dyn)
	require.NoError(t, err)
	assert.Equal(t, "", version)
}

func TestPrintPlatformVersionsGroupsWorkloadsByNamespace(t *testing.T) {
	var out bytes.Buffer
	printPlatformVersions(&out, platformVersionReport{
		CRDs:       "0.34.0",
		ConsoleAPI: "b72f989d",
		Kubernetes: "v1.33.4+k3s1",
		CLI:        "0.34.0",
		Workloads: []platformWorkload{
			{Namespace: "monitoring", Kind: "statefulset", Name: "loki", Ready: 1, Desired: 1, Updating: true, Images: []string{"grafana/loki:3.5.0"}},
			{Namespace: "velero", Kind: "deployment", Name: "velero", Ready: 1, Desired: 1, Images: []string{"velero/velero:v1.18.4", "velero/velero-plugin-for-aws:v1.13.0 (init)"}},
			{Namespace: "velero", Kind: "daemonset", Name: "node-agent", Ready: 2, Desired: 2, Images: []string{"velero/velero:v1.18.4"}},
		},
	})

	assert.Equal(t, `
  Kipper CRDs:  written by kip 0.34.0
  Console API:  build b72f989d (recorded at its last start)
  Kubernetes:   v1.33.4+k3s1
  kip CLI:      0.34.0

  NAMESPACE   WORKLOAD              READY         IMAGES
  monitoring  statefulset/loki      1/1 updating  grafana/loki:3.5.0
  velero      deployment/velero     1/1           velero/velero:v1.18.4
                                                  velero/velero-plugin-for-aws:v1.13.0 (init)
              daemonset/node-agent  2/2           velero/velero:v1.18.4

`, out.String())
}

func TestPrintPlatformVersionsSaysWhenNoVersionIsRecorded(t *testing.T) {
	var out bytes.Buffer
	printPlatformVersions(&out, platformVersionReport{Kubernetes: "v1.33.4+k3s1", CLI: "dev"})

	assert.Contains(t, out.String(), "Kipper CRDs:  no version recorded\n")
	assert.Contains(t, out.String(), "Console API:  no build recorded\n")
	assert.Contains(t, out.String(), "No platform workloads found.")
}
