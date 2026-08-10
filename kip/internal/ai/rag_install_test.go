package ai

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// autoSucceedEmbeddingPullJob mirrors autoSucceedProbeJob from
// install_test.go but matches Jobs by the embedding-pull prefix
// instead of the model-probe prefix.
func autoSucceedEmbeddingPullJob(clientset *fake.Clientset) { //nolint:staticcheck
	fillGenerateNameForJobs(clientset)
	clientset.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		if !strings.HasPrefix(getAction.GetName(), embeddingPullJobNamePrefix) {
			return false, nil, nil
		}
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: getAction.GetName(), Namespace: Namespace},
			Status:     batchv1.JobStatus{Succeeded: 1},
		}, nil
	})
}

func TestWaitForEmbeddingModelLoadedSucceedsWhenJobSucceeded(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	autoSucceedEmbeddingPullJob(clientset)
	inst := &Installer{
		Clientset:    clientset,
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}
	require.NoError(t, inst.waitForEmbeddingModelLoaded(context.Background(), DefaultEmbeddingModel))
}

func TestWaitForEmbeddingModelLoadedFailsWhenJobFailed(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	fillGenerateNameForJobs(clientset)
	clientset.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		if !strings.HasPrefix(getAction.GetName(), embeddingPullJobNamePrefix) {
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
	err := inst.waitForEmbeddingModelLoaded(context.Background(), DefaultEmbeddingModel)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not load")
	assert.Contains(t, err.Error(), "kubectl logs",
		"failure message must point at the right diagnostic so a user knows where to look")
	assert.Contains(t, err.Error(), DefaultEmbeddingModel,
		"failure message must name the model that failed so a user with multiple bundles knows which to check")
}

func TestWaitForEmbeddingModelLoadedTimesOutWhenStuck(t *testing.T) {
	// No reactor: every Get returns NotFound (stays in initial state),
	// so the loop runs until the deadline.
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	inst := &Installer{
		Clientset:    clientset,
		PollInterval: time.Millisecond,
		PollTimeout:  20 * time.Millisecond,
	}
	err := inst.waitForEmbeddingModelLoaded(context.Background(), DefaultEmbeddingModel)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not ready within")
}

func TestWaitForEmbeddingModelLoadedRequiresModelName(t *testing.T) {
	inst := &Installer{Clientset: fake.NewSimpleClientset()} //nolint:staticcheck
	err := inst.waitForEmbeddingModelLoaded(context.Background(), "")
	require.Error(t, err)
}

// TestWaitForEmbeddingModelLoadedRetrySafe leaves a still-deleting
// predecessor Job in the cluster to simulate the AlreadyExists race
// the fixed-name approach hit. With GenerateName both runs get
// unique names and both succeed.
func TestWaitForEmbeddingModelLoadedRetrySafe(t *testing.T) {
	clientset := fake.NewSimpleClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      embeddingPullJobNamePrefix + "stuck",
			Namespace: Namespace,
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
	}) //nolint:staticcheck
	autoSucceedEmbeddingPullJob(clientset)
	inst := &Installer{
		Clientset:    clientset,
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}
	require.NoError(t, inst.waitForEmbeddingModelLoaded(context.Background(), DefaultEmbeddingModel))
	require.NoError(t, inst.waitForEmbeddingModelLoaded(context.Background(), DefaultEmbeddingModel))
}

// TestBuildEmbeddingPullJobShapeAndSafety locks in the Job shape
// invariants that matter for retry semantics, blast radius, and what
// the script actually does. Mirrors TestBuildModelProbeJobShapeAndSafety
// because the retry-collision class of bug we hit in Phase 1 applies
// here too.
func TestBuildEmbeddingPullJobShapeAndSafety(t *testing.T) {
	job := buildEmbeddingPullJob(DefaultEmbeddingModel)

	// GenerateName not Name: each install run gets a unique server-
	// assigned suffix so retries do not collide with a still-deleting
	// predecessor.
	assert.Empty(t, job.Name)
	assert.Equal(t, embeddingPullJobNamePrefix, job.GenerateName)
	assert.Equal(t, Namespace, job.Namespace)

	// Kipper ownership labels so kip ai rag uninstall can find and
	// clean up stuck Jobs by selector. component=rag distinguishes
	// from the Phase 1 model-probe Jobs (component=model-probe) so a
	// label-scoped uninstall does not touch Phase 1 artefacts.
	assert.Equal(t, "kipper", job.Labels[managedByLabel])
	assert.Equal(t, "rag", job.Labels["app.kubernetes.io/component"])
	assert.Equal(t, "kipper-ai", job.Labels["app.kubernetes.io/part-of"])

	// No chart-controller retries: the script polls itself and fails
	// the Job with a useful message rather than re-running the whole
	// pull on transient errors.
	require.NotNil(t, job.Spec.BackoffLimit)
	assert.Equal(t, int32(0), *job.Spec.BackoffLimit)

	// TTL cleanup so successful Jobs are reaped.
	require.NotNil(t, job.Spec.TTLSecondsAfterFinished)

	// In-pod deadline so a stuck pull cannot hang the install
	// indefinitely.
	require.NotNil(t, job.Spec.ActiveDeadlineSeconds)
	assert.Equal(t, int64(1800), *job.Spec.ActiveDeadlineSeconds, "30 minute upper bound matches the script's seq 1 360 * 5s = 30min loop")

	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	c := job.Spec.Template.Spec.Containers[0]
	assert.Contains(t, c.Image, "busybox", "matches Phase 1 model-probe Job image; small footprint, present in cluster cache")
	require.Len(t, c.Command, 3)
	assert.Equal(t, "sh", c.Command[0])

	// Script must POST to /api/pull (triggers the download) and
	// verify against /api/tags. Hardcoded URL points at the
	// in-cluster Ollama Service, NOT localhost; the Job runs in a
	// fresh Pod that has no Ollama running locally.
	script := c.Command[2]
	assert.Contains(t, script, "ollama.kipper-ai.svc.cluster.local:11434")
	assert.Contains(t, script, "/api/pull", "must POST to /api/pull to trigger the download")
	assert.Contains(t, script, "/api/tags", "must verify with /api/tags as the belt-and-braces check")
	assert.Contains(t, script, DefaultEmbeddingModel, "the model name must be embedded in the script")
	assert.Contains(t, script, "Content-Type: application/json", "Ollama's /api/pull rejects requests without the JSON content-type")

	// Pin the exact request field. Per ollama/ollama/docs/api.md the
	// /api/pull body uses `model`, not `name`. A wrong field name here
	// gets rejected by the server and the Job fails on the first run,
	// so locking this in stops a future "I'll just rename it" change
	// from silently regressing the bundle.
	// The script uses a $TARGET shell variable for the model so we
	// assert the field-and-variable form, not the substituted value.
	assert.Contains(t, script, `\"model\":\"$TARGET\"`,
		"request body must use `model` per Ollama's current /api/pull spec")
	assert.NotContains(t, script, `\"name\":\"$TARGET\"`,
		"the `name` field is rejected by Ollama's current /api/pull spec")
}

// =====================
// Step 4 helpers + tests
// =====================

// readyQdrantStatefulSet builds a StatefulSet that isStatefulSetReady
// accepts: matched generations, one updated+ready replica, current
// revision == update revision.
func readyQdrantStatefulSet(name string) *appsv1.StatefulSet {
	one := int32(1)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace, Generation: 1},
		Spec:       appsv1.StatefulSetSpec{Replicas: &one},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 1,
			UpdatedReplicas:    1,
			ReadyReplicas:      1,
			CurrentRevision:    "rev-1",
			UpdateRevision:     "rev-1",
		},
	}
}

// ragHelmChartUnstructured builds a minimal Kipper-owned HelmChart CR
// for the given chart name in kube-system. Used to fake a present RAG
// bundle for ragInstalled / Uninstall refusal tests.
func ragHelmChartUnstructured(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "helm.cattle.io/v1",
		"kind":       "HelmChart",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": "kube-system",
			"labels": map[string]interface{}{
				managedByLabel:                managedByValue,
				"app.kubernetes.io/component": componentValueRAG,
			},
		},
	}}
}

func phase1BundleStateConfigMap(model, host string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BundleStateConfigMapName,
			Namespace: Namespace,
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
		Data: map[string]string{"model": model, "host": host},
	}
}

// nodeWithFreeRAM builds a Ready Node with the given allocatable
// memory and an Ollama Pod requesting 6Gi, so DetectRAG's
// FreeMemory math matches what the test expects.
func nodeWithFreeRAM(name, allocatable string) (*corev1.Node, *corev1.Pod) {
	return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(allocatable)},
				Capacity:    corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(allocatable)},
				Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			},
		}, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "ollama-xyz", Namespace: Namespace},
			Spec: corev1.PodSpec{
				NodeName: name,
				Containers: []corev1.Container{{
					Name: "ollama",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("6Gi")},
					},
				}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
}

// ragInstallerWithReadyPhase1 returns a clientset/dynamic pair and an
// Installer wired with reactors so InstallRAG's preflight, helm-install
// Jobs, embedding-pull Job, and the two ready-resource gates all
// auto-succeed. Tests then layer their own state on top.
func ragInstallerWithReadyPhase1(t *testing.T, opts ...func(*fake.Clientset)) (*Installer, *bytes.Buffer) {
	t.Helper()
	node, ollamaPod := nodeWithFreeRAM("test-node", "16Gi")
	clientset := fake.NewSimpleClientset( //nolint:staticcheck
		node,
		ollamaPod,
		availableOllamaDeployment("test-node"),
		phase1BundleStateConfigMap("qwen2.5:3b-instruct-q4_K_M", "chat.example.com"),
		readyDeployment(AnythingLLMDeploymentName),
		readyQdrantStatefulSet(QdrantServiceName),
	)
	autoSucceedHelmInstallJob(clientset)
	autoSucceedEmbeddingPullJob(clientset)
	for _, o := range opts {
		o(clientset)
	}
	out := &bytes.Buffer{}
	inst := &Installer{
		Clientset:    clientset,
		Dynamic:      newDynamicFake(t),
		Out:          out,
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}
	return inst, out
}

// -------------------- InstallRAG --------------------

func TestInstallRAGRequiresHost(t *testing.T) {
	inst, _ := ragInstallerWithReadyPhase1(t)
	err := inst.InstallRAG(context.Background(), RAGOptions{Host: ""})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host is required")
}

func TestInstallRAGRefusesWhenPhase1Missing(t *testing.T) {
	clientset := fake.NewSimpleClientset(readyNode("box", "16Gi", nil, "")) //nolint:staticcheck
	inst := &Installer{
		Clientset: clientset,
		Dynamic:   newDynamicFake(t),
	}
	err := inst.InstallRAG(context.Background(), RAGOptions{Host: "ask.example.com"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "phase 1 not ready")
	assert.Contains(t, err.Error(), "kip ai install",
		"failure must point at the right next command, not just say 'no'")
}

func TestInstallRAGRefusesWhenInsufficientHeadroom(t *testing.T) {
	node, ollamaPod := nodeWithFreeRAM("tight", "10Gi") // 4 GiB free, exactly at the threshold...
	other := &corev1.Pod{                               // ...but a busy app eats 2 GiB more, leaving 2 GiB.
		ObjectMeta: metav1.ObjectMeta{Name: "busy", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "tight",
			Containers: []corev1.Container{{
				Name: "x",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	clientset := fake.NewSimpleClientset( //nolint:staticcheck
		node, ollamaPod, other,
		availableOllamaDeployment("tight"),
	)
	inst := &Installer{Clientset: clientset, Dynamic: newDynamicFake(t)}
	err := inst.InstallRAG(context.Background(), RAGOptions{Host: "ask.example.com"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "insufficient memory headroom")
}

func TestInstallRAGRefusesWhenChatModelNotRecorded(t *testing.T) {
	node, ollamaPod := nodeWithFreeRAM("test-node", "16Gi")
	clientset := fake.NewSimpleClientset( //nolint:staticcheck
		node, ollamaPod,
		availableOllamaDeployment("test-node"),
		// No bundle state ConfigMap.
	)
	inst := &Installer{Clientset: clientset, Dynamic: newDynamicFake(t)}
	err := inst.InstallRAG(context.Background(), RAGOptions{Host: "ask.example.com"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no chat model recorded")
	assert.Contains(t, err.Error(), "kip ai install")
}

func TestInstallRAGHappyPath(t *testing.T) {
	inst, out := ragInstallerWithReadyPhase1(t)

	err := inst.InstallRAG(context.Background(), RAGOptions{Host: "ask.example.com"})
	require.NoError(t, err)

	dyn := inst.Dynamic
	for _, chart := range []string{"qdrant", "anythingllm"} {
		_, err := dyn.Resource(helmChartGVR).Namespace("kube-system").Get(context.Background(), chart, metav1.GetOptions{})
		require.NoError(t, err, "helmchart %s should be applied", chart)
	}
	_, err = dyn.Resource(secretGVR).Namespace(Namespace).Get(context.Background(), AnythingLLMCredentialsSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	_, err = dyn.Resource(ingressGVR).Namespace(Namespace).Get(context.Background(), "anythingllm", metav1.GetOptions{})
	require.NoError(t, err)

	cms := inst.Clientset.CoreV1().ConfigMaps(Namespace)
	stateCM, err := cms.Get(context.Background(), RAGBundleStateConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, DefaultEmbeddingModel, stateCM.Data["embedding_model"])
	assert.Equal(t, "ask.example.com", stateCM.Data["host"])

	// Step ordering: Qdrant goes before AnythingLLM, embedding pull
	// before bundle-state recording.
	output := out.String()
	assert.Contains(t, output, "✔   Installing Qdrant")
	assert.Contains(t, output, "✔   Installing AnythingLLM")
	assert.Contains(t, output, "✔   Pulling embedding model into Ollama")
	assert.Contains(t, output, "✔   Recording RAG bundle state")
}

// -------------------- UninstallRAG --------------------

func TestUninstallRAGTolerantOfMissingResources(t *testing.T) {
	// Nothing applied beforehand: every delete is a no-op.
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	inst := &Installer{
		Clientset:    clientset,
		Dynamic:      newDynamicFake(t),
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}
	require.NoError(t, inst.UninstallRAG(context.Background(), RAGOptions{}))
}

func TestUninstallRAGRefusesForeignSecret(t *testing.T) {
	clientset := fake.NewSimpleClientset(&corev1.Secret{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{
			Name:      AnythingLLMCredentialsSecretName,
			Namespace: Namespace,
			// Intentionally no managed-by label.
		},
	})
	inst := &Installer{
		Clientset:    clientset,
		Dynamic:      newDynamicFake(t),
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}
	err := inst.UninstallRAG(context.Background(), RAGOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not managed by kipper")
}

// -------------------- Phase 1 Uninstall extension --------------------

func TestUninstallRefusesWhenRAGPresent(t *testing.T) {
	dyn := newDynamicFake(t)
	// Stage a qdrant HelmChart in kube-system so ragInstalled returns true.
	_, err := dyn.Resource(helmChartGVR).Namespace("kube-system").Create(
		context.Background(),
		ragHelmChartUnstructured("qdrant"),
		metav1.CreateOptions{},
	)
	require.NoError(t, err)
	inst := &Installer{
		Clientset:    fake.NewSimpleClientset(), //nolint:staticcheck
		Dynamic:      dyn,
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}
	err = inst.Uninstall(context.Background(), Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RAG bundle is installed")
	assert.Contains(t, err.Error(), "kip ai rag uninstall",
		"refusal must point at the right next command")
	assert.Contains(t, err.Error(), "destructive",
		"refusal must state that kip ai rag uninstall destroys data, not promise preservation")
	assert.Contains(t, err.Error(), "kip ai backup",
		"refusal must point at the actual data-preservation path")
}

// -------------------- credentials --------------------

func TestLoadOrGenerateAnythingLLMCredentialsCreatesFresh(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	inst := &Installer{Clientset: clientset}
	creds, err := inst.loadOrGenerateAnythingLLMCredentials(context.Background())
	require.NoError(t, err)
	assert.NotEmpty(t, creds.JWTSecret)
	assert.NotEmpty(t, creds.SigKey)
	assert.NotEmpty(t, creds.SigSalt)
	assert.NotEmpty(t, creds.AuthToken)
	// Length sanity-check against AnythingLLM's documented minimums.
	assert.GreaterOrEqual(t, len(creds.JWTSecret), 12, "JWT_SECRET >= 12 chars per AnythingLLM .env.example")
	assert.GreaterOrEqual(t, len(creds.SigKey), 32, "SIG_KEY >= 32 chars per AnythingLLM .env.example")
	assert.GreaterOrEqual(t, len(creds.SigSalt), 32, "SIG_SALT >= 32 chars per AnythingLLM .env.example")
}

func TestLoadOrGenerateAnythingLLMCredentialsReusesExisting(t *testing.T) {
	clientset := fake.NewSimpleClientset(&corev1.Secret{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{
			Name:      AnythingLLMCredentialsSecretName,
			Namespace: Namespace,
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
		Data: map[string][]byte{
			"JWT_SECRET": []byte("existing-jwt"),
			"SIG_KEY":    []byte("existing-sig-key-32-chars-long-x"),
			"SIG_SALT":   []byte("existing-sig-salt-32-chars-long-x"),
			"AUTH_TOKEN": []byte("existing-auth-token"),
		},
	})
	inst := &Installer{Clientset: clientset}
	creds, err := inst.loadOrGenerateAnythingLLMCredentials(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "existing-jwt", creds.JWTSecret)
	assert.Equal(t, "existing-auth-token", creds.AuthToken,
		"reusing existing AUTH_TOKEN avoids invalidating the operator's bootstrap login")
}

func TestLoadOrGenerateAnythingLLMCredentialsRegeneratesWhenPartial(t *testing.T) {
	// Existing Secret missing AUTH_TOKEN: treat as corrupt, regenerate.
	clientset := fake.NewSimpleClientset(&corev1.Secret{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{
			Name:      AnythingLLMCredentialsSecretName,
			Namespace: Namespace,
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
		Data: map[string][]byte{
			"JWT_SECRET": []byte("partial-only"),
			"SIG_KEY":    []byte("some-sig-key"),
			"SIG_SALT":   []byte("some-sig-salt"),
			// AUTH_TOKEN missing.
		},
	})
	inst := &Installer{Clientset: clientset}
	creds, err := inst.loadOrGenerateAnythingLLMCredentials(context.Background())
	require.NoError(t, err)
	assert.NotEqual(t, "partial-only", creds.JWTSecret, "partial Secret must trigger full regen")
	assert.NotEmpty(t, creds.AuthToken)
}

// TestAnythingLLMCredentialsReadsPersistedSecret pins the Step 5
// public reader the CLI uses to print the AUTH_TOKEN after install.
// Pure read: never generates fresh values, returns NotFound when the
// Secret is absent, refuses on foreign ownership.
func TestAnythingLLMCredentialsReadsPersistedSecret(t *testing.T) {
	clientset := fake.NewSimpleClientset(&corev1.Secret{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{
			Name:      AnythingLLMCredentialsSecretName,
			Namespace: Namespace,
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
		Data: map[string][]byte{
			"JWT_SECRET": []byte("jwt-value"),
			"SIG_KEY":    []byte("sig-key-value"),
			"SIG_SALT":   []byte("sig-salt-value"),
			"AUTH_TOKEN": []byte("bootstrap-token"),
		},
	})
	inst := &Installer{Clientset: clientset}
	creds, err := inst.AnythingLLMCredentials(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "jwt-value", creds.JWTSecret)
	assert.Equal(t, "bootstrap-token", creds.AuthToken,
		"the CLI prints AuthToken to the operator; this read must surface the persisted value verbatim")
}

func TestAnythingLLMCredentialsErrorsWhenSecretMissing(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	inst := &Installer{Clientset: clientset}
	_, err := inst.AnythingLLMCredentials(context.Background())
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "not found"),
		"missing Secret must surface as NotFound so the CLI prints a useful error")
}

func TestAnythingLLMCredentialsRefusesForeign(t *testing.T) {
	clientset := fake.NewSimpleClientset(&corev1.Secret{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{
			Name:      AnythingLLMCredentialsSecretName,
			Namespace: Namespace,
			// Intentionally no managed-by label.
		},
	})
	inst := &Installer{Clientset: clientset}
	_, err := inst.AnythingLLMCredentials(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not managed by kipper")
}

func TestLoadOrGenerateAnythingLLMCredentialsRefusesForeign(t *testing.T) {
	clientset := fake.NewSimpleClientset(&corev1.Secret{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{
			Name:      AnythingLLMCredentialsSecretName,
			Namespace: Namespace,
			// No managed-by label.
		},
	})
	inst := &Installer{Clientset: clientset}
	_, err := inst.loadOrGenerateAnythingLLMCredentials(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not managed by kipper")
}

// -------------------- bundle state --------------------

func TestReadPhase1ChatModelReturnsModel(t *testing.T) {
	clientset := fake.NewSimpleClientset(phase1BundleStateConfigMap("qwen2.5:7b", "chat.x")) //nolint:staticcheck
	inst := &Installer{Clientset: clientset}
	got, err := inst.readPhase1ChatModel(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "qwen2.5:7b", got)
}

func TestReadPhase1ChatModelReturnsEmptyWhenMissing(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	inst := &Installer{Clientset: clientset}
	got, err := inst.readPhase1ChatModel(context.Background())
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestWriteRAGBundleStateCreatesAndUpdates(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	inst := &Installer{Clientset: clientset}

	require.NoError(t, inst.writeRAGBundleState(context.Background(), "nomic-embed-text:latest", "docs.x"))
	cm, err := clientset.CoreV1().ConfigMaps(Namespace).Get(context.Background(), RAGBundleStateConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "nomic-embed-text:latest", cm.Data["embedding_model"])
	assert.Equal(t, "docs.x", cm.Data["host"])
	assert.Equal(t, "rag", cm.Labels["app.kubernetes.io/component"])

	// Re-write should update in place, not error on AlreadyExists.
	require.NoError(t, inst.writeRAGBundleState(context.Background(), "mxbai-embed-large", "docs.y"))
	cm, err = clientset.CoreV1().ConfigMaps(Namespace).Get(context.Background(), RAGBundleStateConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "mxbai-embed-large", cm.Data["embedding_model"])
	assert.Equal(t, "docs.y", cm.Data["host"])
}

func TestWriteRAGBundleStateRefusesForeign(t *testing.T) {
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{
			Name:      RAGBundleStateConfigMapName,
			Namespace: Namespace,
			// No managed-by label.
		},
	})
	inst := &Installer{Clientset: clientset}
	err := inst.writeRAGBundleState(context.Background(), "x", "y")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not managed by kipper")
}

// -------------------- StatefulSet wait --------------------

func TestWaitForStatefulSetReadyHappyPath(t *testing.T) {
	clientset := fake.NewSimpleClientset(readyQdrantStatefulSet(QdrantServiceName)) //nolint:staticcheck
	inst := &Installer{
		Clientset:    clientset,
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}
	require.NoError(t, inst.waitForStatefulSet(context.Background(), Namespace, QdrantServiceName))
}

func TestWaitForStatefulSetTimesOut(t *testing.T) {
	// Nothing in the cluster: poll never finds the StatefulSet.
	inst := &Installer{
		Clientset:    fake.NewSimpleClientset(), //nolint:staticcheck
		PollInterval: time.Millisecond,
		PollTimeout:  20 * time.Millisecond,
	}
	err := inst.waitForStatefulSet(context.Background(), Namespace, QdrantServiceName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not ready within")
}

func TestIsStatefulSetReadyMatrix(t *testing.T) {
	one := int32(1)
	tests := []struct {
		name string
		ss   *appsv1.StatefulSet
		want bool
	}{
		{
			name: "all-good",
			ss: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Spec:       appsv1.StatefulSetSpec{Replicas: &one},
				Status: appsv1.StatefulSetStatus{
					ObservedGeneration: 1, UpdatedReplicas: 1, ReadyReplicas: 1,
					CurrentRevision: "r", UpdateRevision: "r",
				},
			},
			want: true,
		},
		{
			name: "stale-observed-generation",
			ss: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Spec:       appsv1.StatefulSetSpec{Replicas: &one},
				Status: appsv1.StatefulSetStatus{
					ObservedGeneration: 1, UpdatedReplicas: 1, ReadyReplicas: 1,
					CurrentRevision: "r", UpdateRevision: "r",
				},
			},
			want: false,
		},
		{
			name: "mid-rollout-revisions-differ",
			ss: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Spec:       appsv1.StatefulSetSpec{Replicas: &one},
				Status: appsv1.StatefulSetStatus{
					ObservedGeneration: 1, UpdatedReplicas: 1, ReadyReplicas: 1,
					CurrentRevision: "old", UpdateRevision: "new",
				},
			},
			want: false,
		},
		{
			name: "not-yet-ready",
			ss: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Spec:       appsv1.StatefulSetSpec{Replicas: &one},
				Status: appsv1.StatefulSetStatus{
					ObservedGeneration: 1, UpdatedReplicas: 1, ReadyReplicas: 0,
					CurrentRevision: "r", UpdateRevision: "r",
				},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isStatefulSetReady(tt.ss))
		})
	}
}

// -------------------- delete helpers --------------------

func TestDeleteOwnedSecretRefusesForeign(t *testing.T) {
	clientset := fake.NewSimpleClientset(&corev1.Secret{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: Namespace},
	})
	inst := &Installer{Clientset: clientset}
	err := inst.deleteOwnedSecret(context.Background(), "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not managed by kipper")
}

func TestDeleteOwnedSecretNoOpWhenMissing(t *testing.T) {
	inst := &Installer{Clientset: fake.NewSimpleClientset()} //nolint:staticcheck
	require.NoError(t, inst.deleteOwnedSecret(context.Background(), "absent"))
}

func TestDeleteOwnedSecretDeletesOwned(t *testing.T) {
	clientset := fake.NewSimpleClientset(&corev1.Secret{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{
			Name:      "owned",
			Namespace: Namespace,
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
	})
	inst := &Installer{Clientset: clientset}
	require.NoError(t, inst.deleteOwnedSecret(context.Background(), "owned"))
	_, err := clientset.CoreV1().Secrets(Namespace).Get(context.Background(), "owned", metav1.GetOptions{})
	assert.True(t, kerrIsNotFound(err), "expected NotFound, got %v", err)
}

// kerrIsNotFound is a small helper to keep the test imports tight.
func kerrIsNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

func TestDeleteOwnedConfigMapRefusesForeign(t *testing.T) {
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: Namespace},
	})
	inst := &Installer{Clientset: clientset}
	err := inst.deleteOwnedConfigMap(context.Background(), Namespace, "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not managed by kipper")
}

func TestRagInstalledTrueWhenChartPresent(t *testing.T) {
	dyn := newDynamicFake(t)
	_, err := dyn.Resource(helmChartGVR).Namespace("kube-system").Create(
		context.Background(),
		ragHelmChartUnstructured("anythingllm"),
		metav1.CreateOptions{},
	)
	require.NoError(t, err)
	inst := &Installer{Dynamic: dyn}
	got, err := inst.ragInstalled(context.Background())
	require.NoError(t, err)
	assert.True(t, got)
}

func TestRagInstalledFalseWhenNoCharts(t *testing.T) {
	inst := &Installer{Dynamic: newDynamicFake(t)}
	got, err := inst.ragInstalled(context.Background())
	require.NoError(t, err)
	assert.False(t, got)
}

// TestRagInstalledIgnoresForeignChart verifies that a
// HelmChart named qdrant/anythingllm that does NOT carry the Kipper
// managed-by label belongs to someone else and must not be treated as
// "RAG is installed". Otherwise kip ai uninstall would refuse and the
// user would have no clean way out (deleteHelmChart also refuses
// foreign charts, so kip ai rag uninstall would also fail).
func TestRagInstalledIgnoresForeignChart(t *testing.T) {
	dyn := newDynamicFake(t)
	_, err := dyn.Resource(helmChartGVR).Namespace("kube-system").Create(
		context.Background(),
		&unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "helm.cattle.io/v1",
			"kind":       "HelmChart",
			"metadata": map[string]interface{}{
				"name":      "qdrant",
				"namespace": "kube-system",
				// No Kipper labels.
			},
		}},
		metav1.CreateOptions{},
	)
	require.NoError(t, err)
	inst := &Installer{Dynamic: dyn}
	got, err := inst.ragInstalled(context.Background())
	require.NoError(t, err)
	assert.False(t, got, "a non-Kipper chart sharing the name must not block kip ai uninstall")
}

// TestInstallRAGSkipsHeadroomOnReinstall verifies that
// once Qdrant and AnythingLLM are running, their pod requests count
// against the node's free memory. A re-apply on a node that just
// barely passed the first install should still succeed; otherwise the
// bundle's own footprint blocks every subsequent kip ai rag install.
// The fix mirrors Phase 1's "skip the tier check when the namespace
// is Kipper-owned" pattern.
func TestInstallRAGSkipsHeadroomOnReinstall(t *testing.T) {
	// Stage a node that would NOT pass a fresh headroom check.
	node, ollamaPod := nodeWithFreeRAM("tight", "10Gi")
	busy := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "busy", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "tight",
			Containers: []corev1.Container{{
				Name: "x",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	clientset := fake.NewSimpleClientset( //nolint:staticcheck
		node, ollamaPod, busy,
		availableOllamaDeployment("tight"),
		phase1BundleStateConfigMap("qwen2.5:3b-instruct-q4_K_M", "chat.example.com"),
		readyDeployment(AnythingLLMDeploymentName),
		readyQdrantStatefulSet(QdrantServiceName),
	)
	autoSucceedHelmInstallJob(clientset)
	autoSucceedEmbeddingPullJob(clientset)

	dyn := newDynamicFake(t)
	// Pre-stage Kipper-owned qdrant chart so ragInstalled returns true.
	_, err := dyn.Resource(helmChartGVR).Namespace("kube-system").Create(
		context.Background(),
		ragHelmChartUnstructured("qdrant"),
		metav1.CreateOptions{},
	)
	require.NoError(t, err)

	inst := &Installer{
		Clientset:    clientset,
		Dynamic:      dyn,
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}
	require.NoError(t, inst.InstallRAG(context.Background(), RAGOptions{Host: "ask.example.com"}),
		"re-install on a tight node must skip the headroom check; the bundle's own pods otherwise block every kip ai rag install after the first")
}
