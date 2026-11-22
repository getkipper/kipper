package ai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"
)

// fillGenerateNameForJobs installs a reactor that fills in a unique Name
// for each Job Create call carrying GenerateName. The fake clientset does
// not do this on its own; a real apiserver would. Without it, every test
// run after the first hits AlreadyExists.
func fillGenerateNameForJobs(clientset *fake.Clientset) { //nolint:staticcheck
	var counter int
	clientset.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		obj, ok := createAction.GetObject().(*batchv1.Job)
		if !ok || obj.Name != "" || obj.GenerateName == "" {
			return false, nil, nil
		}
		counter++
		obj.Name = fmt.Sprintf("%s%d", obj.GenerateName, counter)
		return false, nil, nil
	})
}

// autoSucceedProbeJob installs reactors so the probe Job appears
// succeeded on Get. The Get reactor matches by prefix because the real
// Job uses GenerateName.
func autoSucceedProbeJob(clientset *fake.Clientset) { //nolint:staticcheck
	fillGenerateNameForJobs(clientset)
	clientset.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		if !strings.HasPrefix(getAction.GetName(), modelProbeNamePrefix) {
			return false, nil, nil
		}
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: getAction.GetName(), Namespace: Namespace},
			Status:     batchv1.JobStatus{Succeeded: 1},
		}, nil
	})
}

// autoSucceedHelmInstallJob installs a Get reactor that returns a
// Complete helm-install-<chartname> Job in kube-system for any chart
// name. Mimics what k3s helm-controller would do once it has finished
// reconciling the HelmChart in a real cluster — the fake apiserver
// has no controller running, so without this the new
// waitForHelmChartJob gate blocks every install test forever.
func autoSucceedHelmInstallJob(clientset *fake.Clientset) { //nolint:staticcheck
	clientset.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		if !strings.HasPrefix(getAction.GetName(), "helm-install-") || getAction.GetNamespace() != "kube-system" {
			return false, nil, nil
		}
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      getAction.GetName(),
				Namespace: "kube-system",
				UID:       k8stypes.UID(getAction.GetName() + "-uid"),
			},
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
				},
			},
		}, nil
	})
}

func newDynamicFake(t *testing.T) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		namespaceGVR: "NamespaceList",
		secretGVR:    "SecretList",
		ingressGVR:   "IngressList",
		helmChartGVR: "HelmChartList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
}

func readyDeployment(name string) *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace, Generation: 1},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			Replicas:           1,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
			ReadyReplicas:      1,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: "True"},
			},
		},
	}
}

func TestInstallRefusesBelowMinimum(t *testing.T) {
	inst := &Installer{
		Clientset: fake.NewSimpleClientset(), //nolint:staticcheck
		Dynamic:   newDynamicFake(t),
	}
	err := inst.Install(context.Background(), TierNone, Options{Host: "chat.example.com", NodeName: "test-node"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "below tier 1")
}

func TestInstallRequiresHost(t *testing.T) {
	inst := &Installer{
		Clientset: fake.NewSimpleClientset(), //nolint:staticcheck
		Dynamic:   newDynamicFake(t),
	}
	err := inst.Install(context.Background(), TierOne, Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host is required")
}

func TestInstallHappyPath(t *testing.T) {
	clientset := fake.NewSimpleClientset( //nolint:staticcheck
		readyDeployment("ollama"),
		readyDeployment("librechat-librechat"),
	)
	autoSucceedProbeJob(clientset)
	autoSucceedHelmInstallJob(clientset)
	dyn := newDynamicFake(t)
	out := &bytes.Buffer{}
	inst := &Installer{
		Clientset:    clientset,
		Dynamic:      dyn,
		Metadata:     deployedReleases(t),
		Out:          out,
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}

	err := inst.Install(context.Background(), TierOne, Options{Host: "chat.example.com", NodeName: "test-node"})
	require.NoError(t, err)

	// All four types of resource were created via the dynamic client.
	ns, err := dyn.Resource(namespaceGVR).Get(context.Background(), Namespace, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "Namespace", ns.GetKind())

	for _, chart := range []string{"ollama", "librechat"} {
		_, err := dyn.Resource(helmChartGVR).Namespace("kube-system").Get(context.Background(), chart, metav1.GetOptions{})
		require.NoError(t, err, "helmchart %s should exist", chart)
	}

	_, err = dyn.Resource(secretGVR).Namespace(Namespace).Get(context.Background(), "librechat-credentials", metav1.GetOptions{})
	require.NoError(t, err)

	_, err = dyn.Resource(ingressGVR).Namespace(Namespace).Get(context.Background(), "librechat", metav1.GetOptions{})
	require.NoError(t, err)

	assert.Contains(t, out.String(), "✔   Installing Ollama")
	assert.Contains(t, out.String(), "✔   Waiting for LibreChat to be ready")
}

func TestInstallTier3EnablesGPUValue(t *testing.T) {
	clientset := fake.NewSimpleClientset( //nolint:staticcheck
		readyDeployment("ollama"),
		readyDeployment("librechat-librechat"),
	)
	autoSucceedProbeJob(clientset)
	autoSucceedHelmInstallJob(clientset)
	dyn := newDynamicFake(t)
	inst := &Installer{
		Clientset:    clientset,
		Dynamic:      dyn,
		Metadata:     deployedReleases(t),
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}
	err := inst.Install(context.Background(), TierThree, Options{Host: "chat.example.com", NodeName: "test-node"})
	require.NoError(t, err)

	chart, err := dyn.Resource(helmChartGVR).Namespace("kube-system").Get(context.Background(), "ollama", metav1.GetOptions{})
	require.NoError(t, err)
	values, _, _ := unstructured.NestedString(chart.Object, "spec", "valuesContent")
	assert.Contains(t, values, "enabled: true")
}

func TestInstallUsesDefaultModelForTier(t *testing.T) {
	clientset := fake.NewSimpleClientset( //nolint:staticcheck
		readyDeployment("ollama"),
		readyDeployment("librechat-librechat"),
	)
	autoSucceedProbeJob(clientset)
	autoSucceedHelmInstallJob(clientset)
	dyn := newDynamicFake(t)
	inst := &Installer{
		Clientset:    clientset,
		Dynamic:      dyn,
		Metadata:     deployedReleases(t),
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}
	require.NoError(t, inst.Install(context.Background(), TierTwo, Options{Host: "chat.example.com", NodeName: "test-node"}))

	chart, err := dyn.Resource(helmChartGVR).Namespace("kube-system").Get(context.Background(), "ollama", metav1.GetOptions{})
	require.NoError(t, err)
	values, _, _ := unstructured.NestedString(chart.Object, "spec", "valuesContent")
	assert.Contains(t, values, "qwen2.5:7b-instruct-q4_K_M")
}

func TestInstallTimesOutWhenNothingProgresses(t *testing.T) {
	// No reactors staged: helm-install Job never appears, so the
	// chart-job wait times out first.
	inst := &Installer{
		Clientset:    fake.NewSimpleClientset(), //nolint:staticcheck
		Dynamic:      newDynamicFake(t),
		Metadata:     deployedReleases(t),
		PollInterval: time.Millisecond,
		PollTimeout:  20 * time.Millisecond,
	}
	err := inst.Install(context.Background(), TierOne, Options{Host: "chat.example.com", NodeName: "test-node"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not finish within")
}

func TestInstallPropagatesApplyError(t *testing.T) {
	dyn := newDynamicFake(t)
	dyn.PrependReactor("create", "namespaces", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api server is angry")
	})
	inst := &Installer{
		Clientset:    fake.NewSimpleClientset(), //nolint:staticcheck
		Dynamic:      dyn,
		Metadata:     deployedReleases(t),
		PollInterval: time.Millisecond,
		PollTimeout:  20 * time.Millisecond,
	}
	err := inst.Install(context.Background(), TierOne, Options{Host: "chat.example.com", NodeName: "test-node"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Creating namespace")
	assert.Contains(t, err.Error(), "api server is angry")
}

func TestApplyDocumentUpdatesExistingResource(t *testing.T) {
	dyn := newDynamicFake(t)
	inst := &Installer{Dynamic: dyn}

	// Pre-create the namespace so the apply path goes through Update. The
	// pre-existing resource carries the managed-by label, simulating a
	// prior Kipper install.
	pre := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]interface{}{
			"name":            Namespace,
			"resourceVersion": "1",
			"labels": map[string]interface{}{
				"app.kubernetes.io/managed-by": "kipper",
			},
		},
	}}
	_, err := dyn.Resource(namespaceGVR).Create(context.Background(), pre, metav1.CreateOptions{})
	require.NoError(t, err)

	require.NoError(t, inst.applyManifest(context.Background(), NamespaceManifest()))

	got, err := dyn.Resource(namespaceGVR).Get(context.Background(), Namespace, metav1.GetOptions{})
	require.NoError(t, err)
	labels := got.GetLabels()
	assert.Equal(t, "kipper", labels["app.kubernetes.io/managed-by"])
}

func TestApplyDocumentRefusesToOverwriteForeignResource(t *testing.T) {
	dyn := newDynamicFake(t)
	inst := &Installer{Dynamic: dyn}

	// Some user has a HelmChart called "ollama" already, with no Kipper
	// ownership label. Re-applying the bundle must not stomp it.
	foreign := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "helm.cattle.io/v1",
		"kind":       "HelmChart",
		"metadata": map[string]interface{}{
			"name":      "ollama",
			"namespace": "kube-system",
		},
		"spec": map[string]interface{}{"chart": "user-owned"},
	}}
	_, err := dyn.Resource(helmChartGVR).Namespace("kube-system").Create(context.Background(), foreign, metav1.CreateOptions{})
	require.NoError(t, err)

	doc := OllamaManifest(OllamaConfig{Model: "qwen2.5:3b-instruct-q4_K_M", PVCSize: "10Gi"})
	err = inst.applyDocument(context.Background(), doc)
	require.Error(t, err)

	var conflict *errNotOwnedByKipper
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "HelmChart", conflict.kind)
	assert.Equal(t, "ollama", conflict.name)

	// The user's chart spec must still be intact.
	got, err := dyn.Resource(helmChartGVR).Namespace("kube-system").Get(context.Background(), "ollama", metav1.GetOptions{})
	require.NoError(t, err)
	chart, _, _ := unstructured.NestedString(got.Object, "spec", "chart")
	assert.Equal(t, "user-owned", chart)
}

func TestWaitForHelmChartGoneTimesOutWhenFinalizersStuck(t *testing.T) {
	dyn := newDynamicFake(t)
	// Stage a chart and intercept Delete so the resource is never actually
	// removed, simulating a stuck helm-controller finalizer.
	_, err := dyn.Resource(helmChartGVR).Namespace("kube-system").Create(context.Background(),
		&unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "helm.cattle.io/v1",
			"kind":       "HelmChart",
			"metadata":   map[string]interface{}{"name": "ollama", "namespace": "kube-system"},
		}}, metav1.CreateOptions{})
	require.NoError(t, err)

	inst := &Installer{
		Dynamic:      dyn,
		PollInterval: time.Millisecond,
		PollTimeout:  20 * time.Millisecond,
	}
	err = inst.waitForHelmChartGone(context.Background(), "ollama")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "still present after")
}

func TestDeleteHelmChartRefusesToDeleteForeignResource(t *testing.T) {
	dyn := newDynamicFake(t)
	foreign := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "helm.cattle.io/v1",
		"kind":       "HelmChart",
		"metadata": map[string]interface{}{
			"name":      "ollama",
			"namespace": "kube-system",
		},
	}}
	_, err := dyn.Resource(helmChartGVR).Namespace("kube-system").Create(context.Background(), foreign, metav1.CreateOptions{})
	require.NoError(t, err)

	inst := &Installer{Dynamic: dyn}
	err = inst.deleteHelmChart(context.Background(), "ollama")
	require.Error(t, err)
	var conflict *errNotOwnedByKipper
	require.ErrorAs(t, err, &conflict)

	_, err = dyn.Resource(helmChartGVR).Namespace("kube-system").Get(context.Background(), "ollama", metav1.GetOptions{})
	require.NoError(t, err, "foreign chart must survive a refused delete")
}

func TestUninstallRemovesChartsAndNamespace(t *testing.T) {
	dyn := newDynamicFake(t)
	for _, name := range []string{"ollama", "librechat"} {
		_, err := dyn.Resource(helmChartGVR).Namespace("kube-system").Create(context.Background(),
			&unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "helm.cattle.io/v1",
				"kind":       "HelmChart",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": "kube-system",
					"labels":    map[string]interface{}{"app.kubernetes.io/managed-by": "kipper"},
				},
			}}, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	_, err := dyn.Resource(namespaceGVR).Create(context.Background(),
		&unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]interface{}{
				"name":   Namespace,
				"labels": map[string]interface{}{"app.kubernetes.io/managed-by": "kipper"},
			},
		}}, metav1.CreateOptions{})
	require.NoError(t, err)

	inst := &Installer{
		Clientset: fake.NewSimpleClientset(), //nolint:staticcheck
		Dynamic:   dyn,
	}
	require.NoError(t, inst.Uninstall(context.Background(), Options{}))

	for _, name := range []string{"ollama", "librechat"} {
		_, err := dyn.Resource(helmChartGVR).Namespace("kube-system").Get(context.Background(), name, metav1.GetOptions{})
		assert.True(t, apierrors.IsNotFound(err), "helmchart %s should be gone", name)
	}
	_, err = dyn.Resource(namespaceGVR).Get(context.Background(), Namespace, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "namespace must be deleted (data and all)")
}

func TestUninstallRefusesToDeleteForeignNamespace(t *testing.T) {
	dyn := newDynamicFake(t)
	// Pre-existing namespace WITHOUT the managed-by label means a user
	// (or some other tool) created it. Uninstall must refuse to wipe it.
	_, err := dyn.Resource(namespaceGVR).Create(context.Background(),
		&unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata":   map[string]interface{}{"name": Namespace},
		}}, metav1.CreateOptions{})
	require.NoError(t, err)

	inst := &Installer{
		Clientset: fake.NewSimpleClientset(), //nolint:staticcheck
		Dynamic:   dyn,
	}
	err = inst.Uninstall(context.Background(), Options{})
	require.Error(t, err)
	var conflict *errNotOwnedByKipper
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "Namespace", conflict.kind)

	// Foreign namespace must survive.
	_, err = dyn.Resource(namespaceGVR).Get(context.Background(), Namespace, metav1.GetOptions{})
	require.NoError(t, err)
}

func TestUninstallTolerantOfMissingResources(t *testing.T) {
	inst := &Installer{
		Clientset: fake.NewSimpleClientset(), //nolint:staticcheck
		Dynamic:   newDynamicFake(t),
	}
	require.NoError(t, inst.Uninstall(context.Background(), Options{}))
}

func TestWaitForDeploymentSucceedsWhenAlreadyAvailable(t *testing.T) {
	clientset := fake.NewSimpleClientset(readyDeployment("ollama")) //nolint:staticcheck
	inst := &Installer{
		Clientset:    clientset,
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}
	require.NoError(t, inst.waitForDeployment(context.Background(), "ollama"))
}

func TestWaitForDeploymentRejectsStaleObservedGeneration(t *testing.T) {
	// Deployment was updated (generation 2) but the deployment controller
	// has only observed generation 1. A naive Available=True check would
	// pass on the stale state; the rollout-progress checks must reject it.
	one := int32(1)
	stale := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "ollama", Namespace: Namespace, Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			Replicas:           1,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
			ReadyReplicas:      1,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: "True"},
			},
		},
	}
	clientset := fake.NewSimpleClientset(stale) //nolint:staticcheck
	inst := &Installer{
		Clientset:    clientset,
		PollInterval: time.Millisecond,
		PollTimeout:  20 * time.Millisecond,
	}
	err := inst.waitForDeployment(context.Background(), "ollama")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not ready within")
}

func TestWaitForDeploymentRejectsMidRollout(t *testing.T) {
	// 3 replicas desired, only 1 of the new template ready while old ones
	// terminate. Available=True but rollout incomplete.
	three := int32(3)
	mid := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "librechat", Namespace: Namespace, Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: &three},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2,
			Replicas:           3,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
			ReadyReplicas:      1,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: "True"},
			},
		},
	}
	clientset := fake.NewSimpleClientset(mid) //nolint:staticcheck
	inst := &Installer{
		Clientset:    clientset,
		PollInterval: time.Millisecond,
		PollTimeout:  20 * time.Millisecond,
	}
	err := inst.waitForDeployment(context.Background(), "librechat")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not ready within")
}

func TestWaitForModelLoadedSucceedsWhenJobSucceeded(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	autoSucceedProbeJob(clientset)
	autoSucceedHelmInstallJob(clientset)
	inst := &Installer{
		Clientset:    clientset,
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}
	require.NoError(t, inst.waitForModelLoaded(context.Background(), "qwen2.5:3b-instruct-q4_K_M"))
}

func TestWaitForModelLoadedFailsWhenJobFailed(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	fillGenerateNameForJobs(clientset)
	clientset.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		if !strings.HasPrefix(getAction.GetName(), modelProbeNamePrefix) {
			return false, nil, nil
		}
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: getAction.GetName(), Namespace: Namespace},
			Status:     batchv1.JobStatus{Failed: 1},
		}, nil
	})
	inst := &Installer{
		Clientset:    clientset,
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}
	err := inst.waitForModelLoaded(context.Background(), "qwen2.5:3b-instruct-q4_K_M")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not load")
	assert.Contains(t, err.Error(), "kubectl logs")
}

func TestWaitForModelLoadedRetrySafe(t *testing.T) {
	// First call: leave a still-deleting "predecessor" Job in the cluster
	// to simulate the AlreadyExists race the fixed-name approach hit.
	// With GenerateName, both runs get unique names and both succeed.
	clientset := fake.NewSimpleClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      modelProbeNamePrefix + "abcde",
			Namespace: Namespace,
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
	}) //nolint:staticcheck
	autoSucceedProbeJob(clientset)
	autoSucceedHelmInstallJob(clientset)
	inst := &Installer{
		Clientset:    clientset,
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}
	require.NoError(t, inst.waitForModelLoaded(context.Background(), "qwen2.5:3b-instruct-q4_K_M"))
	require.NoError(t, inst.waitForModelLoaded(context.Background(), "qwen2.5:3b-instruct-q4_K_M"))
}

func TestWaitForModelLoadedTimesOutWhenStuck(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	// No reactor: every Get returns the Job in its initial pending state.
	inst := &Installer{
		Clientset:    clientset,
		PollInterval: time.Millisecond,
		PollTimeout:  20 * time.Millisecond,
	}
	err := inst.waitForModelLoaded(context.Background(), "qwen2.5:3b-instruct-q4_K_M")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not ready within")
}

func TestWaitForModelLoadedRequiresModelName(t *testing.T) {
	inst := &Installer{Clientset: fake.NewSimpleClientset()} //nolint:staticcheck
	err := inst.waitForModelLoaded(context.Background(), "")
	require.Error(t, err)
}

func TestBuildModelProbeJobShapeAndSafety(t *testing.T) {
	job := buildModelProbeJob("qwen2.5:3b-instruct-q4_K_M")
	// GenerateName, not Name, so each install run gets a unique name and
	// retries don't collide with a still-deleting predecessor.
	assert.Empty(t, job.Name)
	assert.Equal(t, modelProbeNamePrefix, job.GenerateName)
	assert.Equal(t, Namespace, job.Namespace)
	assert.Equal(t, "kipper", job.Labels[managedByLabel])
	require.NotNil(t, job.Spec.BackoffLimit)
	assert.Equal(t, int32(0), *job.Spec.BackoffLimit, "no chart-controller retry; the script polls itself")
	require.NotNil(t, job.Spec.TTLSecondsAfterFinished)
	require.NotNil(t, job.Spec.ActiveDeadlineSeconds)
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	c := job.Spec.Template.Spec.Containers[0]
	assert.Contains(t, c.Image, "busybox")
	require.Len(t, c.Command, 3)
	assert.Equal(t, "sh", c.Command[0])
	// The probe URL is inside the cluster, not localhost.
	assert.Contains(t, c.Command[2], "ollama.kipper-ai.svc.cluster.local:11434")
	assert.Contains(t, c.Command[2], "qwen2.5:3b-instruct-q4_K_M")
}

func TestWaitForDeploymentRespectsCancellation(t *testing.T) {
	inst := &Installer{
		Clientset:    fake.NewSimpleClientset(), //nolint:staticcheck
		PollInterval: 50 * time.Millisecond,
		PollTimeout:  time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := inst.waitForDeployment(ctx, "ollama")
	require.Error(t, err)
}

func TestGVRForKnownKinds(t *testing.T) {
	cases := []struct {
		apiVersion string
		kind       string
		want       schema.GroupVersionResource
	}{
		{"v1", "Namespace", namespaceGVR},
		{"v1", "Secret", secretGVR},
		{"networking.k8s.io/v1", "Ingress", ingressGVR},
		{"helm.cattle.io/v1", "HelmChart", helmChartGVR},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			obj := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": tc.apiVersion,
				"kind":       tc.kind,
			}}
			got, err := gvrFor(obj)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestGVRForUnsupportedKindFails(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
	}}
	_, err := gvrFor(obj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported resource kind")
}

func TestDefaultPVCByTier(t *testing.T) {
	assert.Equal(t, defaultPVCTier1, DefaultPVCFor(TierOne))
	assert.Equal(t, defaultPVCTier2, DefaultPVCFor(TierTwo))
	assert.Equal(t, defaultPVCTier3, DefaultPVCFor(TierThree))
	assert.Equal(t, defaultPVCTier3, DefaultPVCFor(TierFour))
}

func TestGenerateLibreChatCredentialsAreHexAndUnique(t *testing.T) {
	a, err := generateLibreChatCredentials()
	require.NoError(t, err)
	b, err := generateLibreChatCredentials()
	require.NoError(t, err)
	// Lengths track the chart's example values: 32-byte hex (64 chars) for
	// CREDS_KEY, JWT pair, and MEILI_MASTER_KEY; 16-byte hex (32 chars)
	// for CREDS_IV.
	assert.Len(t, a.CredsKey, 64)
	assert.Len(t, a.CredsIV, 32)
	assert.Len(t, a.JWTSecret, 64)
	assert.Len(t, a.JWTRefreshSecret, 64)
	assert.Len(t, a.MeiliMasterKey, 64)
	assert.NotEqual(t, a.CredsKey, b.CredsKey)
	assert.NotEqual(t, a.JWTSecret, b.JWTSecret)
	assert.NotEqual(t, a.JWTRefreshSecret, b.JWTRefreshSecret)
	assert.NotEqual(t, a.MeiliMasterKey, b.MeiliMasterKey)
}

func TestWriteBundleStateCreatesConfigMap(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	inst := &Installer{Clientset: clientset}

	require.NoError(t, inst.writeBundleState(context.Background(), "qwen2.5:7b-instruct-q4_K_M", "chat.example.com"))

	cm, err := clientset.CoreV1().ConfigMaps(Namespace).Get(context.Background(), BundleStateConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "qwen2.5:7b-instruct-q4_K_M", cm.Data["model"])
	assert.Equal(t, "chat.example.com", cm.Data["host"])
	assert.Equal(t, managedByValue, cm.Labels[managedByLabel])
}

func TestWriteBundleStateUpdatesExisting(t *testing.T) {
	pre := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BundleStateConfigMapName,
			Namespace: Namespace,
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
		Data: map[string]string{"model": "old", "host": "old"},
	}
	clientset := fake.NewSimpleClientset(pre) //nolint:staticcheck
	inst := &Installer{Clientset: clientset}

	require.NoError(t, inst.writeBundleState(context.Background(), "new-model", "chat.new.example.com"))

	cm, err := clientset.CoreV1().ConfigMaps(Namespace).Get(context.Background(), BundleStateConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "new-model", cm.Data["model"])
	assert.Equal(t, "chat.new.example.com", cm.Data["host"])
}

func TestWriteBundleStateRefusesForeignConfigMap(t *testing.T) {
	pre := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BundleStateConfigMapName,
			Namespace: Namespace,
		},
		Data: map[string]string{"user": "data"},
	}
	clientset := fake.NewSimpleClientset(pre) //nolint:staticcheck
	inst := &Installer{Clientset: clientset}

	err := inst.writeBundleState(context.Background(), "qwen2.5:3b", "chat.example.com")
	require.Error(t, err)
	var conflict *errNotOwnedByKipper
	require.ErrorAs(t, err, &conflict)
}

func TestInstallerDefaultsForIntervalAndTimeout(t *testing.T) {
	// Zero values must fall back to the production defaults so callers
	// that omit the polling fields still get a sane wait policy. The
	// 30-minute default covers cold-start cases (Mongo image pull plus
	// init plus crash-loop recovery) that the prior 10-minute default
	// did not. Healthy reruns short-circuit at each wait step.
	inst := &Installer{}
	assert.Equal(t, 5*time.Second, inst.interval())
	assert.Equal(t, 30*time.Minute, inst.timeout())

	custom := &Installer{PollInterval: 200 * time.Millisecond, PollTimeout: 7 * time.Second}
	assert.Equal(t, 200*time.Millisecond, custom.interval())
	assert.Equal(t, 7*time.Second, custom.timeout())
}

func TestApplyDocumentRejectsMalformedYAML(t *testing.T) {
	inst := &Installer{Dynamic: newDynamicFake(t)}
	err := inst.applyDocument(context.Background(), "this is: not\n  valid: [yaml")
	require.Error(t, err)
}

func TestApplyDocumentRejectsBadAPIVersion(t *testing.T) {
	inst := &Installer{Dynamic: newDynamicFake(t)}
	doc := "apiVersion: \"a/b/c/d\"\nkind: Garbage\nmetadata:\n  name: x\n"
	err := inst.applyDocument(context.Background(), doc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing apiVersion")
}

// helmChart returns an unstructured Ollama HelmChart with the given
// generation and (optionally) status.jobName, ready for the dynamic
// fake's tracker. Hardcoded to "ollama" since every test in this file
// targets that chart.
func helmChart(generation int64, statusJobName string) *unstructured.Unstructured {
	obj := map[string]interface{}{
		"apiVersion": "helm.cattle.io/v1",
		"kind":       "HelmChart",
		"metadata": map[string]interface{}{
			"name":       "ollama",
			"namespace":  "kube-system",
			"generation": generation,
		},
		"spec": map[string]interface{}{},
	}
	if statusJobName != "" {
		obj["status"] = map[string]interface{}{"jobName": statusJobName}
	}
	return &unstructured.Unstructured{Object: obj}
}

// helmInstallJob returns a typed Job suitable for seeding into a fake
// clientset's tracker. The chart name is hardcoded to "ollama" because
// every test in this file targets the Ollama chart job.
//
// terminalCond is one of "Complete", "Failed", or "" (still running).
// We read terminal state from the Job's conditions, not Succeeded /
// Failed counters, so per-pod retry noise doesn't get confused with
// end state.
func helmInstallJob(uid string, terminalCond batchv1.JobConditionType) *batchv1.Job {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "helm-install-ollama",
			Namespace: "kube-system",
			UID:       k8stypes.UID(uid),
		},
	}
	if terminalCond != "" {
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: terminalCond, Status: corev1.ConditionTrue},
		}
	}
	return job
}

func TestWaitForHelmChartJobFirstInstallCompletes(t *testing.T) {
	dyn := newDynamicFake(t)
	require.NoError(t, dyn.Tracker().Create(helmChartGVR, helmChart(1, "helm-install-ollama"), "kube-system"))
	clientset := fake.NewSimpleClientset(helmInstallJob("job-1", batchv1.JobComplete)) //nolint:staticcheck
	inst := &Installer{
		Clientset:    clientset,
		Dynamic:      dyn,
		Metadata:     deployedReleases(t),
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}
	require.NoError(t, inst.waitForHelmChartJob(context.Background(), "ollama", "", true))
}

func TestWaitForHelmChartJobNoOpReApplyAcceptsExistingSucceeded(t *testing.T) {
	dyn := newDynamicFake(t)
	require.NoError(t, dyn.Tracker().Create(helmChartGVR, helmChart(1, "helm-install-ollama"), "kube-system"))
	clientset := fake.NewSimpleClientset(helmInstallJob("job-1", batchv1.JobComplete)) //nolint:staticcheck
	inst := &Installer{
		Clientset:    clientset,
		Dynamic:      dyn,
		Metadata:     deployedReleases(t),
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}
	// No-op apply: prevJobUID matches the existing Job, expectNewJob
	// is false. Existing Succeeded should satisfy us immediately.
	require.NoError(t, inst.waitForHelmChartJob(context.Background(), "ollama", "job-1", false))
}

func TestWaitForHelmChartJobUpgradeSkipsStaleSucceededJob(t *testing.T) {
	// Upgrade race: prevJobUID points at the previous Succeeded Job,
	// helm-controller hasn't replaced it yet. Initially the wait must
	// not declare success on the stale Job. Once helm-controller
	// recreates the Job (different UID), the wait succeeds.
	dyn := newDynamicFake(t)
	require.NoError(t, dyn.Tracker().Create(helmChartGVR, helmChart(2, "helm-install-ollama"), "kube-system"))
	clientset := fake.NewSimpleClientset() //nolint:staticcheck

	// Return the stale Job for two polls before replacing it to emulate
	// helm-controller deleting and recreating the Job.
	calls := 0
	clientset.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		if getAction.GetName() != "helm-install-ollama" {
			return false, nil, nil
		}
		calls++
		if calls < 3 {
			// Stale job: same UID as prevJobUID.
			return true, helmInstallJob("old-uid", batchv1.JobComplete), nil
		}
		return true, helmInstallJob("new-uid", batchv1.JobComplete), nil
	})

	inst := &Installer{
		Clientset:    clientset,
		Dynamic:      dyn,
		Metadata:     deployedReleases(t),
		PollInterval: 5 * time.Millisecond,
		PollTimeout:  500 * time.Millisecond,
	}
	require.NoError(t, inst.waitForHelmChartJob(context.Background(), "ollama", "old-uid", true))
	assert.GreaterOrEqual(t, calls, 3, "wait should keep polling past the stale Job until the new one appears")
}

func TestWaitForHelmChartJobReportsFailedJob(t *testing.T) {
	dyn := newDynamicFake(t)
	require.NoError(t, dyn.Tracker().Create(helmChartGVR, helmChart(1, "helm-install-ollama"), "kube-system"))
	clientset := fake.NewSimpleClientset(helmInstallJob("job-1", batchv1.JobFailed)) //nolint:staticcheck
	inst := &Installer{
		Clientset:    clientset,
		Dynamic:      dyn,
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}
	err := inst.waitForHelmChartJob(context.Background(), "ollama", "", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed")
	assert.Contains(t, err.Error(), "kubectl logs")
}

// TestLibreChatConfigHashRollsChartGeneration: changing the model
// (or any other input that flows into our ConfigMap or Secret) must
// produce a different rendered LibreChat HelmChart spec, so a re-apply
// bumps the HelmChart's generation and helm-controller spawns a new
// install Job. Without the podAnnotations hash, a model-only change
// would leave the chart spec identical and the upgrade would race the
// Deployment readiness check.
func TestLibreChatConfigHashRollsChartGeneration(t *testing.T) {
	creds := LibreChatCredentials{
		CredsKey:         "k",
		CredsIV:          "iv",
		JWTSecret:        "j",
		JWTRefreshSecret: "r",
		MeiliMasterKey:   "m",
	}
	out1 := LibreChatManifest(LibreChatConfig{Host: "chat.example.com", Model: "qwen2.5:3b", Credentials: creds})
	out2 := LibreChatManifest(LibreChatConfig{Host: "chat.example.com", Model: "qwen2.5:7b", Credentials: creds})
	assert.NotEqual(t, out1, out2, "different model must produce different chart spec so HelmChart generation changes")
	assert.Contains(t, out1, "kipper.run/config-hash:")
	assert.Contains(t, out2, "kipper.run/config-hash:")
}

func TestLoadOrGenerateLibreChatCredentialsReusesExisting(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      LibreChatCredentialsSecretName,
			Namespace: Namespace,
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
		Data: map[string][]byte{
			"CREDS_KEY":          []byte("existing-creds-key"),
			"CREDS_IV":           []byte("existing-creds-iv"),
			"JWT_SECRET":         []byte("existing-jwt"),
			"JWT_REFRESH_SECRET": []byte("existing-refresh"),
			"MEILI_MASTER_KEY":   []byte("existing-meili"),
		},
	}
	inst := &Installer{
		Clientset: fake.NewSimpleClientset(existing), //nolint:staticcheck
		Dynamic:   newDynamicFake(t),
	}
	got, err := inst.loadOrGenerateLibreChatCredentials(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "existing-creds-key", got.CredsKey, "must reuse existing credentials, not regenerate them")
	assert.Equal(t, "existing-meili", got.MeiliMasterKey)
}

func TestLoadOrGenerateLibreChatCredentialsGeneratesOnFirstInstall(t *testing.T) {
	inst := &Installer{
		Clientset: fake.NewSimpleClientset(), //nolint:staticcheck
		Dynamic:   newDynamicFake(t),
	}
	got, err := inst.loadOrGenerateLibreChatCredentials(context.Background())
	require.NoError(t, err)
	assert.NotEmpty(t, got.CredsKey, "first install must generate credentials")
	assert.NotEmpty(t, got.MeiliMasterKey)
}

func TestLoadOrGenerateLibreChatCredentialsRefusesUnowned(t *testing.T) {
	unowned := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      LibreChatCredentialsSecretName,
			Namespace: Namespace,
			// No managed-by=kipper label.
		},
		Data: map[string][]byte{"CREDS_KEY": []byte("x")},
	}
	inst := &Installer{
		Clientset: fake.NewSimpleClientset(unowned), //nolint:staticcheck
		Dynamic:   newDynamicFake(t),
	}
	_, err := inst.loadOrGenerateLibreChatCredentials(context.Background())
	require.Error(t, err)
	var notOwned *errNotOwnedByKipper
	require.True(t, errors.As(err, &notOwned), "expected errNotOwnedByKipper, got %T", err)
}

// TestWaitForHelmChartJobToleratesTransientPodFailures: helm-controller
// Jobs have a high backoffLimit (~1000 in k3s). A failed pod ticks
// Status.Failed but the Job stays running until either it Completes
// or its backoff is exhausted (Failed condition true). The wait must
// not declare failure on the counter alone — a transient image pull
// error during chart install would otherwise abort the user's
// command while helm-controller would have recovered.
func TestWaitForHelmChartJobToleratesTransientPodFailures(t *testing.T) {
	dyn := newDynamicFake(t)
	require.NoError(t, dyn.Tracker().Create(helmChartGVR, helmChart(1, "helm-install-ollama"), "kube-system"))
	clientset := fake.NewSimpleClientset() //nolint:staticcheck

	calls := 0
	clientset.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		if getAction.GetName() != "helm-install-ollama" {
			return false, nil, nil
		}
		calls++
		if calls < 3 {
			// Transient pod failure: Failed counter > 0 but no
			// terminal Failed condition. Job is still being retried
			// by the controller.
			return true, &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "helm-install-ollama",
					Namespace: "kube-system",
					UID:       "job-1",
				},
				Status: batchv1.JobStatus{Active: 1, Failed: 1},
			}, nil
		}
		// Eventually the job Completes.
		return true, helmInstallJob("job-1", batchv1.JobComplete), nil
	})

	inst := &Installer{
		Clientset:    clientset,
		Dynamic:      dyn,
		Metadata:     deployedReleases(t),
		PollInterval: 5 * time.Millisecond,
		PollTimeout:  500 * time.Millisecond,
	}
	require.NoError(t, inst.waitForHelmChartJob(context.Background(), "ollama", "", true), "transient Failed counter must not abort the wait while the controller is still retrying")
	assert.GreaterOrEqual(t, calls, 3)
}

func TestWaitForHelmChartJobTolersJobNotYetCreated(t *testing.T) {
	// helm-controller hasn't yet created the Job. The wait must keep
	// polling rather than erroring on NotFound.
	dyn := newDynamicFake(t)
	require.NoError(t, dyn.Tracker().Create(helmChartGVR, helmChart(1, ""), "kube-system"))
	clientset := fake.NewSimpleClientset() //nolint:staticcheck

	calls := 0
	clientset.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		if getAction.GetName() != "helm-install-ollama" {
			return false, nil, nil
		}
		calls++
		if calls < 3 {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "jobs"}, "helm-install-ollama")
		}
		return true, helmInstallJob("new-uid", batchv1.JobComplete), nil
	})

	inst := &Installer{
		Clientset:    clientset,
		Dynamic:      dyn,
		Metadata:     deployedReleases(t),
		PollInterval: 5 * time.Millisecond,
		PollTimeout:  500 * time.Millisecond,
	}
	require.NoError(t, inst.waitForHelmChartJob(context.Background(), "ollama", "", true))
	assert.GreaterOrEqual(t, calls, 3)
}

var _ dynamic.Interface = (*dynamicfake.FakeDynamicClient)(nil)

func helmRelease(name, namespace string, version int, status string) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("sh.helm.release.v1.%s.v%d", name, version),
			Namespace: namespace,
			Labels:    map[string]string{"owner": "helm", "name": name, "version": fmt.Sprint(version), "status": status},
		},
	}
}

func releaseMetadata(t *testing.T, releases ...*metav1.PartialObjectMetadata) *metadatafake.FakeMetadataClient {
	t.Helper()
	scheme := metadatafake.NewTestScheme()
	require.NoError(t, metav1.AddMetaToScheme(scheme))
	objects := make([]runtime.Object, len(releases))
	for i, r := range releases {
		objects[i] = r
	}
	return metadatafake.NewSimpleMetadataClient(scheme, objects...)
}

// deployedReleases seeds revision 1 as deployed for all four AI charts in
// the AI namespace and kube-system. Both namespaces support fixtures with
// an explicit target namespace and those that fall back to the chart namespace.
func deployedReleases(t *testing.T) *metadatafake.FakeMetadataClient {
	t.Helper()
	var releases []*metav1.PartialObjectMetadata
	for _, name := range []string{"ollama", "librechat", "qdrant", "anythingllm"} {
		for _, namespace := range []string{Namespace, "kube-system"} {
			releases = append(releases, helmRelease(name, namespace, 1, "deployed"))
		}
	}
	return releaseMetadata(t, releases...)
}

func completedOllamaInstaller(t *testing.T, releases ...*metav1.PartialObjectMetadata) *Installer {
	t.Helper()
	dyn := newDynamicFake(t)
	chart := helmChart(2, "helm-install-ollama")
	require.NoError(t, unstructured.SetNestedField(chart.Object, Namespace, "spec", "targetNamespace"))
	require.NoError(t, dyn.Tracker().Create(helmChartGVR, chart, "kube-system"))
	return &Installer{
		Clientset:    fake.NewSimpleClientset(helmInstallJob("job-2", batchv1.JobComplete)), //nolint:staticcheck
		Dynamic:      dyn,
		Metadata:     releaseMetadata(t, releases...),
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}
}

func TestWaitForHelmChartJobReportsAReleaseHelmLeftFailed(t *testing.T) {
	// A failed upgrade can advance the release revision, causing the retry
	// to exit successfully while the release remains failed.
	inst := completedOllamaInstaller(t,
		helmRelease("ollama", Namespace, 1, "deployed"),
		helmRelease("ollama", Namespace, 2, "failed"))

	err := inst.waitForHelmChartJob(context.Background(), "ollama", "", true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `"ollama"`)
	assert.Contains(t, err.Error(), "failed")
	assert.Contains(t, err.Error(), "kip ai backup --wait")
	assert.Contains(t, err.Error(), "--reinstall-failed")
}

func TestWaitForHelmChartJobReadsTheNewestRevisionNumerically(t *testing.T) {
	inst := completedOllamaInstaller(t,
		helmRelease("ollama", Namespace, 9, "failed"),
		helmRelease("ollama", Namespace, 10, "deployed"))

	require.NoError(t, inst.waitForHelmChartJob(context.Background(), "ollama", "", true))
}

func TestWaitForHelmChartJobIgnoresOtherReleases(t *testing.T) {
	inst := completedOllamaInstaller(t,
		helmRelease("ollama", Namespace, 1, "deployed"),
		helmRelease("librechat", Namespace, 3, "failed"),
		helmRelease("ollama", "elsewhere", 4, "failed"))

	require.NoError(t, inst.waitForHelmChartJob(context.Background(), "ollama", "", true))
}

func TestWaitForHelmChartJobRefusesWithoutAReleaseReader(t *testing.T) {
	inst := completedOllamaInstaller(t, helmRelease("ollama", Namespace, 1, "deployed"))
	inst.Metadata = nil

	require.Error(t, inst.waitForHelmChartJob(context.Background(), "ollama", "", true))
}

func TestWaitForHelmChartJobRefusesWithoutAReadableReleaseRecord(t *testing.T) {
	for name, releases := range map[string][]*metav1.PartialObjectMetadata{
		"none":             nil,
		"unreadable label": {unreadableRelease()},
		"unreadable newer": {helmRelease("ollama", Namespace, 1, "deployed"), unreadableRelease()},
	} {
		t.Run(name, func(t *testing.T) {
			inst := completedOllamaInstaller(t, releases...)
			err := inst.waitForHelmChartJob(context.Background(), "ollama", "", true)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "cannot tell whether the chart was applied")
		})
	}
}

func unreadableRelease() *metav1.PartialObjectMetadata {
	r := helmRelease("ollama", Namespace, 2, "failed")
	r.Labels["version"] = "x"
	return r
}

func TestWaitForHelmChartJobReadsTheReleaseInTheChartNamespaceWithoutATarget(t *testing.T) {
	dyn := newDynamicFake(t)
	require.NoError(t, dyn.Tracker().Create(helmChartGVR, helmChart(1, "helm-install-ollama"), "kube-system"))
	inst := &Installer{
		Clientset:    fake.NewSimpleClientset(helmInstallJob("job-1", batchv1.JobComplete)), //nolint:staticcheck
		Dynamic:      dyn,
		Metadata:     releaseMetadata(t, helmRelease("ollama", "kube-system", 1, "failed")),
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}

	err := inst.waitForHelmChartJob(context.Background(), "ollama", "", true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed")
}

func helmOwnedImagesClaim() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:      LibreChatImagesClaimName,
		Namespace: Namespace,
		UID:       "images-uid",
		Labels:    map[string]string{"app.kubernetes.io/managed-by": "Helm"},
		Annotations: map[string]string{
			"meta.helm.sh/release-name":      "librechat",
			"meta.helm.sh/release-namespace": Namespace,
		},
	}}
}

func TestEnsureLibreChatImagesClaimCreatesAKeptClaim(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	inst := &Installer{Clientset: clientset}

	require.NoError(t, inst.ensureLibreChatImagesClaim(context.Background()))

	claim, err := clientset.CoreV1().PersistentVolumeClaims(Namespace).Get(context.Background(), LibreChatImagesClaimName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "kipper", claim.Labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, "kipper-ai", claim.Labels["app.kubernetes.io/part-of"])
	assert.Equal(t, "keep", claim.Annotations["helm.sh/resource-policy"])
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, claim.Spec.AccessModes)
	size := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	assert.Equal(t, "10G", size.String(), "the chart's default size, so new and adopted claims match")
	assert.Nil(t, claim.Spec.StorageClassName, "the cluster's default class, as the chart used")
}

// libreChatChartFake holds the librechat HelmChart, labeled as kip's when
// owned is true.
func libreChatChartFake(t *testing.T, owned bool) *dynamicfake.FakeDynamicClient {
	t.Helper()
	dyn := newDynamicFake(t)
	chart := ragHelmChartUnstructured("librechat")
	if !owned {
		chart.SetLabels(nil)
	}
	_, err := dyn.Resource(helmChartGVR).Namespace("kube-system").Create(context.Background(), chart, metav1.CreateOptions{})
	require.NoError(t, err)
	return dyn
}

func TestEnsureLibreChatImagesClaimTakesOverTheChartClaim(t *testing.T) {
	clientset := fake.NewSimpleClientset(helmOwnedImagesClaim()) //nolint:staticcheck
	inst := &Installer{Clientset: clientset, Dynamic: libreChatChartFake(t, true)}

	require.NoError(t, inst.ensureLibreChatImagesClaim(context.Background()))

	claim, err := clientset.CoreV1().PersistentVolumeClaims(Namespace).Get(context.Background(), LibreChatImagesClaimName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, k8stypes.UID("images-uid"), claim.UID, "the claim holding the uploads stays")
	assert.Equal(t, "kipper", claim.Labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, "kipper-ai", claim.Labels["app.kubernetes.io/part-of"])
	assert.Equal(t, "keep", claim.Annotations["helm.sh/resource-policy"],
		"Helm skips deleting a claim marked keep when the chart upgrade drops it")
	assert.NotContains(t, claim.Annotations, "meta.helm.sh/release-name")
	assert.NotContains(t, claim.Annotations, "meta.helm.sh/release-namespace")
}

func TestEnsureLibreChatImagesClaimRefusesAForeignClaim(t *testing.T) {
	foreign := helmOwnedImagesClaim()
	foreign.Annotations["meta.helm.sh/release-name"] = "someone-else"
	for name, tc := range map[string]struct {
		claim *corev1.PersistentVolumeClaim
		dyn   *dynamicfake.FakeDynamicClient
	}{
		"another release's claim":            {foreign, libreChatChartFake(t, true)},
		"a librechat chart kip does not own": {helmOwnedImagesClaim(), libreChatChartFake(t, false)},
		"no librechat chart":                 {helmOwnedImagesClaim(), newDynamicFake(t)},
	} {
		t.Run(name, func(t *testing.T) {
			clientset := fake.NewSimpleClientset(tc.claim) //nolint:staticcheck
			inst := &Installer{Clientset: clientset, Dynamic: tc.dyn}

			err := inst.ensureLibreChatImagesClaim(context.Background())

			require.Error(t, err)
			assert.Contains(t, err.Error(), "not managed by kipper")
			claim, getErr := clientset.CoreV1().PersistentVolumeClaims(Namespace).Get(context.Background(), LibreChatImagesClaimName, metav1.GetOptions{})
			require.NoError(t, getErr)
			assert.Equal(t, tc.claim.Annotations, claim.Annotations, "the claim is left untouched")
			assert.Equal(t, tc.claim.Labels, claim.Labels)
		})
	}
}

func TestEnsureLibreChatImagesClaimRefusesAClaimBeingDeleted(t *testing.T) {
	// A reinstall from a release recorded before kip owned the claim deletes
	// it, so kip must not count a terminating claim as present.
	terminating := helmOwnedImagesClaim()
	terminating.Labels = map[string]string{managedByLabel: managedByValue}
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	inst := &Installer{Clientset: fake.NewSimpleClientset(terminating)} //nolint:staticcheck

	err := inst.ensureLibreChatImagesClaim(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "being deleted")
}

func TestInstallRecreatesAnImagesClaimTheChartJobDeleted(t *testing.T) {
	clientset := fake.NewSimpleClientset(readyDeployment("ollama"), readyDeployment("librechat-librechat")) //nolint:staticcheck
	autoSucceedProbeJob(clientset)
	autoSucceedHelmInstallJob(clientset)
	// The chart job's reinstall deletes the claim of a release recorded with it.
	clientset.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.GetAction).GetName() == "helm-install-librechat" {
			_ = clientset.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), Namespace, LibreChatImagesClaimName)
		}
		return false, nil, nil
	})
	inst := &Installer{
		Clientset:    clientset,
		Dynamic:      newDynamicFake(t),
		Metadata:     deployedReleases(t),
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}

	require.NoError(t, inst.Install(context.Background(), TierOne, Options{Host: "chat.example.com", NodeName: "test-node"}))

	_, err := clientset.CoreV1().PersistentVolumeClaims(Namespace).Get(context.Background(), LibreChatImagesClaimName, metav1.GetOptions{})
	require.NoError(t, err, "LibreChat must not be left waiting for a claim that is gone")
}

func TestInstallKeepsTheImagesClaimBeforeApplyingTheLibreChatChart(t *testing.T) {
	clientset := fake.NewSimpleClientset( //nolint:staticcheck
		readyDeployment("ollama"),
		readyDeployment("librechat-librechat"),
		helmOwnedImagesClaim(),
	)
	autoSucceedProbeJob(clientset)
	autoSucceedHelmInstallJob(clientset)
	dyn := libreChatChartFake(t, true)
	keptAtApply := false
	dyn.PrependReactor("update", "helmcharts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured)
		if obj.GetName() == "librechat" {
			claim, err := clientset.CoreV1().PersistentVolumeClaims(Namespace).Get(context.Background(), LibreChatImagesClaimName, metav1.GetOptions{})
			keptAtApply = err == nil && claim.Annotations["helm.sh/resource-policy"] == "keep"
		}
		return false, nil, nil
	})
	inst := &Installer{
		Clientset:    clientset,
		Dynamic:      dyn,
		Metadata:     deployedReleases(t),
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}

	require.NoError(t, inst.Install(context.Background(), TierOne, Options{Host: "chat.example.com", NodeName: "test-node"}))
	assert.True(t, keptAtApply, "the claim must be marked keep before the chart upgrade that drops it")
}

func TestRefuseUndeployedReleasesStopsAReinstall(t *testing.T) {
	// Applying the chart of a release in any of these states can make the
	// Helm controller reinstall it or retry an upgrade that may still be running.
	for _, status := range []string{"failed", "pending-install", "pending-upgrade", "pending-rollback", "uninstalling", "unknown"} {
		t.Run(status, func(t *testing.T) {
			inst := &Installer{Metadata: releaseMetadata(t,
				helmRelease("librechat", Namespace, 1, "deployed"),
				helmRelease("librechat", Namespace, 2, status))}

			err := inst.refuseUndeployedReleases(context.Background(), false, "ollama", "librechat")

			require.Error(t, err)
			assert.Contains(t, err.Error(), `"librechat"`)
			assert.Contains(t, err.Error(), status)
			assert.Contains(t, err.Error(), "kip ai backup --wait")
			assert.Contains(t, err.Error(), "--reinstall-failed")
		})
	}
}

func TestRefuseUndeployedReleasesAllowsDeployedAndNewReleases(t *testing.T) {
	inst := &Installer{Metadata: releaseMetadata(t, helmRelease("ollama", Namespace, 3, "deployed"))}

	require.NoError(t, inst.refuseUndeployedReleases(context.Background(), false, "ollama", "librechat"))
}

func TestRefuseUndeployedReleasesWarnsWhenAReinstallIsAllowed(t *testing.T) {
	out := &bytes.Buffer{}
	inst := &Installer{
		Metadata: releaseMetadata(t, helmRelease("librechat", Namespace, 2, "failed")),
		Out:      out,
	}

	require.NoError(t, inst.refuseUndeployedReleases(context.Background(), true, "librechat"))
	assert.Contains(t, out.String(), `"librechat"`)
	assert.Contains(t, out.String(), "reinstall")
}

func TestRefuseUndeployedReleasesRefusesWithoutAReadableRecord(t *testing.T) {
	inst := &Installer{Metadata: releaseMetadata(t, unreadableRelease())}
	require.Error(t, inst.refuseUndeployedReleases(context.Background(), false, "ollama"))

	inst.Metadata = nil
	require.Error(t, inst.refuseUndeployedReleases(context.Background(), false, "ollama"))
}

// writeActions lists the create, update, patch and delete calls a fake client saw.
func writeActions(actions []k8stesting.Action) []string {
	var writes []string
	for _, a := range actions {
		switch a.GetVerb() {
		case "create", "update", "patch", "delete", "delete-collection":
			writes = append(writes, a.GetVerb()+" "+a.GetResource().Resource)
		}
	}
	return writes
}

func TestInstallAppliesNoChartOverAFailedRelease(t *testing.T) {
	clientset := fake.NewSimpleClientset( //nolint:staticcheck
		readyDeployment("ollama"), readyDeployment("librechat-librechat"), helmOwnedImagesClaim())
	dyn := libreChatChartFake(t, true)
	dyn.ClearActions()
	inst := &Installer{
		Clientset:    clientset,
		Dynamic:      dyn,
		Metadata:     releaseMetadata(t, helmRelease("librechat", Namespace, 2, "failed")),
		PollInterval: time.Millisecond,
		PollTimeout:  20 * time.Millisecond,
	}

	err := inst.Install(context.Background(), TierOne, Options{Host: "chat.example.com", NodeName: "test-node"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--reinstall-failed")
	assert.Empty(t, writeActions(clientset.Actions()), "nothing is changed before the refusal")
	assert.Empty(t, writeActions(dyn.Actions()), "no chart is applied, so the Helm controller starts no reinstall")
}

func TestInstallAppliesTheChartsOverAFailedReleaseWhenAsked(t *testing.T) {
	clientset := fake.NewSimpleClientset(readyDeployment("ollama"), readyDeployment("librechat-librechat")) //nolint:staticcheck
	autoSucceedProbeJob(clientset)
	autoSucceedHelmInstallJob(clientset)
	dyn := newDynamicFake(t)
	inst := &Installer{
		Clientset:    clientset,
		Dynamic:      dyn,
		Metadata:     releaseMetadata(t, helmRelease("ollama", Namespace, 1, "deployed"), helmRelease("librechat", Namespace, 2, "failed")),
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}

	// The fake release records never change, so the chart job check still
	// reports the failed release after the apply.
	err := inst.Install(context.Background(), TierOne, Options{Host: "chat.example.com", NodeName: "test-node", ReinstallFailed: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `helm left the "librechat" release in status failed`)

	_, err = dyn.Resource(helmChartGVR).Namespace("kube-system").Get(context.Background(), "librechat", metav1.GetOptions{})
	require.NoError(t, err, "the chart is applied")
}
