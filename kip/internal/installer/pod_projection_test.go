package installer

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/jsonpath"
)

// testReadMarker replaces the random end marker in this package's tests, so
// stubs can match the exact pod read command and render its output.
const testReadMarker = "end-test"

func init() {
	newReadMarker = func() string { return testReadMarker }
}

// podsQuery returns the pod read command for args using testReadMarker.
func podsQuery(args string) string {
	return podsCommand(args, testReadMarker)
}

// projectedPods renders podTemplate over pods the way kubectl does, so stubs
// for the projected reads return what a cluster would.
func projectedPods(t *testing.T, pods ...corev1.Pod) string {
	t.Helper()
	raw, err := json.Marshal(corev1.PodList{Items: pods})
	require.NoError(t, err)
	return asProjected(string(raw))
}

// asProjected converts a pod list in kubectl's JSON form into podTemplate
// output, for stubs that keep their fixtures as JSON.
func asProjected(listJSON string) string {
	var list any
	if err := json.Unmarshal([]byte(listJSON), &list); err != nil {
		panic(err)
	}
	jp := jsonpath.New("pods").AllowMissingKeys(true)
	if err := jp.Parse(podTemplate(testReadMarker)); err != nil {
		panic(err)
	}
	var out bytes.Buffer
	if err := jp.Execute(&out, list); err != nil {
		panic(err)
	}
	return out.String()
}

// projectedPod converts one pod object in kubectl's JSON form into the output
// of a single-pod projected read.
func projectedPod(podJSON string) string {
	return asProjected(`{"items":[` + podJSON + `]}`)
}

func secretBearingPod() corev1.Pod {
	always := corev1.ContainerRestartPolicyAlways
	priority := int32(1000)
	deadline := int64(600)
	deleting := metav1.NewTime(time.Date(2026, 11, 23, 1, 2, 3, 0, time.UTC))
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-0", Namespace: "shop", UID: "uid-1",
			Labels:            map[string]string{"app": "db", "app.kubernetes.io/managed-by": "kipper"},
			Annotations:       map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "annotation-secret-7f3a"},
			OwnerReferences:   []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "db", UID: "sts-uid"}},
			DeletionTimestamp: &deleting,
		},
		Spec: corev1.PodSpec{
			NodeName:          "server-1",
			PriorityClassName: "kipper-app",
			Priority:          &priority,
			NodeSelector:      map[string]string{"kubernetes.io/hostname": "server-1"},
			Tolerations:       []corev1.Toleration{{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute}},
			Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}}}},
			}}},
			Overhead:              corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")},
			Resources:             &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")}},
			ActiveDeadlineSeconds: &deadline,
			InitContainers: []corev1.Container{{
				Name: "sidecar", RestartPolicy: &always,
				Env:       []corev1.EnvVar{{Name: "TOKEN", Value: "init-env-secret-91c2"}},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}},
			}},
			Containers: []corev1.Container{{
				Name:      "postgres",
				Image:     "postgres:17",
				Command:   []string{"sh", "-c", "command-secret-4d1e"},
				Args:      []string{"args-secret-b8f0"},
				Env:       []corev1.EnvVar{{Name: "POSTGRES_PASSWORD", Value: "env-secret-3c9d"}},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("1Gi")}, Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}},
			}},
			Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-db-0"}}},
				{Name: "creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "volume-secret-name-6a2b"}}},
			},
		},
		Status: corev1.PodStatus{
			Phase:   corev1.PodFailed,
			Reason:  "Evicted",
			Message: "The node was low on resource: memory.",
			Conditions: []corev1.PodCondition{
				{Type: "example.com/backend-reachable", Status: corev1.ConditionFalse, Message: "condition-secret-8e1f"},
				{Type: corev1.PodReady, Status: corev1.ConditionFalse, Message: "a condition message\twith a tab"},
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "postgres", Image: "postgres:17",
				State:                corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1, Message: "termination-secret-0e7d"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", Message: "last-state-secret-5b21"}},
			}},
			InitContainerStatuses: []corev1.ContainerStatus{{Name: "sidecar", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: "init-termination-secret-c4a8"}}}},
		},
	}
}

func TestPodProjectionLeavesSecretsOnTheServer(t *testing.T) {
	out := projectedPods(t, secretBearingPod())

	for _, secret := range []string{"env-secret-3c9d", "init-env-secret-91c2", "command-secret-4d1e", "args-secret-b8f0", "annotation-secret-7f3a", "volume-secret-name-6a2b", "POSTGRES_PASSWORD",
		"termination-secret-0e7d", "last-state-secret-5b21", "init-termination-secret-c4a8", "condition-secret-8e1f"} {
		assert.NotContains(t, out, secret)
	}
}

func TestPodProjectionKeepsTheFieldsKipReads(t *testing.T) {
	full := secretBearingPod()
	empty := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "kube-system", UID: "uid-2"}}

	pods, err := parsePods(projectedPods(t, full, empty), testReadMarker)
	require.NoError(t, err)
	require.Len(t, pods, 2)

	want := full
	want.Annotations = nil
	want.Spec.InitContainers = []corev1.Container{{Name: "sidecar", RestartPolicy: full.Spec.InitContainers[0].RestartPolicy, Resources: full.Spec.InitContainers[0].Resources}}
	want.Spec.Containers = []corev1.Container{{Name: "postgres", Image: "postgres:17", Resources: full.Spec.Containers[0].Resources}}
	want.Spec.Volumes = []corev1.Volume{full.Spec.Volumes[0], {Name: "creds"}}
	want.Status.InitContainerStatuses = nil
	want.Status.Message = ""
	want.Status.Conditions = full.Status.Conditions[1:]
	want.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "postgres", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}}}
	assert.Equal(t, mustJSON(t, want), mustJSON(t, pods[0]))
	assert.Equal(t, mustJSON(t, empty), mustJSON(t, pods[1]))
}

func TestParsePodsRefusesLinesThatAreNotRecords(t *testing.T) {
	web := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "shop"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx"}}}}
	both := projectedPods(t, secretBearingPod(), web)
	second := strings.Index(both, "\nP\t") + 1

	for name, out := range map[string]string{
		"a warning before the records":       "Warning: v1 ComponentStatus is deprecated\n" + both,
		"a warning inside a pod's header":    both[:second+1] + "Warning: deprecated\n" + both[second+1:],
		"a warning hiding a container's tag": strings.Replace(both, "C\tweb\t", "CWarning: x\n\tweb\t", 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parsePods(out, testReadMarker)

			assert.Error(t, err)
		})
	}
}

func TestParsePodsNamesAPodWithABrokenHeader(t *testing.T) {
	broken := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "shop"}, Status: corev1.PodStatus{Reason: "line one\nline two"}}

	_, err := parsePods(projectedPods(t, secretBearingPod(), broken), testReadMarker)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "pod shop/web-1 has a tab or newline")
}

func TestReadPodsKeepsStderrOutOfASuccessfulRead(t *testing.T) {
	cmd := podsCommand("-A", testReadMarker)

	assert.Contains(t, cmd, `2>"$e"`)
	assert.Contains(t, cmd, `[ "$rc" = 0 ] || cat "$e"`)
}

func TestParsePodsRefusesAContainerWithoutItsPod(t *testing.T) {
	_, err := parsePods("C\tpostgres\tpostgres:17\t{}\t"+testReadMarker+"\n", testReadMarker)

	require.Error(t, err)
}

func TestParsePodsReadsNothingFromEmptyOutput(t *testing.T) {
	pods, err := parsePods("", testReadMarker)

	require.NoError(t, err)
	assert.Empty(t, pods)
}

func TestParsePodsRefusesAnImageThatForgesALine(t *testing.T) {
	for name, image := range map[string]string{
		"a tab":                          "nginx\tbroken",
		"a newline":                      "nginx\nbroken",
		"a newline and a fake container": "nginx\t{}\tend-guess\nC\tfake\tnginx",
	} {
		t.Run(name, func(t *testing.T) {
			pod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "shop"},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: image}}},
			}

			_, err := parsePods(projectedPods(t, pod), testReadMarker)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "pod shop/web-1 has a tab or newline")
		})
	}
}

func TestReadMarkersDiffer(t *testing.T) {
	assert.NotEqual(t, randomReadMarker(), randomReadMarker())
}

func TestReadPodsRunsTheProjection(t *testing.T) {
	runner := &podReadRunner{out: projectedPods(t, secretBearingPod())}

	pods, err := readPods(runner, "the pods", "-A")

	require.NoError(t, err)
	require.Len(t, pods, 1)
	require.Len(t, runner.cmds, 1)
	assert.Contains(t, runner.cmds[0], "kubectl get pods -A -o jsonpath='")
	assert.NotContains(t, runner.cmds[0], "-o json ")
	assert.Contains(t, runner.cmds[0], testReadMarker)
}

type podReadRunner struct {
	out  string
	cmds []string
}

func (r *podReadRunner) Run(command string) (string, error) {
	r.cmds = append(r.cmds, command)
	return r.out, nil
}

func (r *podReadRunner) RunStdin(command string, _ io.Reader) (string, error) {
	return r.Run(command)
}

// TestParsePodsReadsAPodWithAMultilineStatusMessage verifies that cluster-wide
// pod reads succeed even when a controller writes a free-text status message
// containing tabs or line breaks.
func TestParsePodsReadsAPodWithAMultilineStatusMessage(t *testing.T) {
	odd := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "shop"}, Status: corev1.PodStatus{Phase: corev1.PodFailed, Message: "line one\nline two\twith a tab"}}

	pods, err := parsePods(projectedPods(t, secretBearingPod(), odd), testReadMarker)

	require.NoError(t, err)
	require.Len(t, pods, 2)
	assert.Equal(t, corev1.PodFailed, pods[1].Status.Phase)
}
