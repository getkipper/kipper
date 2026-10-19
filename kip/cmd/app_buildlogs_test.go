package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/controller/pkg/labels"
)

func buildPod(name, sourceNS string, age time.Duration) *corev1.Pod {
	const app = "todo-app"
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         labels.BuildsNamespace,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
			Labels: map[string]string{
				labels.AppRef:          app,
				labels.Build:           labels.BuildTrue,
				labels.SourceNamespace: sourceNS,
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "kaniko"}}},
	}
}

// Builds run in their own namespace, never beside the app. Looking in the app's
// namespace finds nothing however well the build went, which is what reported
// "no build found" for an app that had built and was serving.
func TestLatestBuildPodLooksInTheBuildsNamespace(t *testing.T) {
	client := fake.NewSimpleClientset(buildPod("todo-app-build-1", "default", time.Minute))

	pod, err := latestBuildPod(context.Background(), client, "todo-app", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pod.Name != "todo-app-build-1" {
		t.Errorf("pod = %q, want todo-app-build-1", pod.Name)
	}
	if pod.Namespace != labels.BuildsNamespace {
		t.Errorf("namespace = %q, want %q", pod.Namespace, labels.BuildsNamespace)
	}
}

// Two projects may each hold an app of the same name, and their builds share one
// namespace. The source-namespace label is what keeps them apart; without it
// this streams another project's build.
func TestLatestBuildPodIsScopedToTheAppsNamespace(t *testing.T) {
	client := fake.NewSimpleClientset(
		buildPod("mine", "acme-prod", time.Minute),
		buildPod("theirs", "other-prod", time.Second),
	)

	pod, err := latestBuildPod(context.Background(), client, "todo-app", "acme-prod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pod.Name != "mine" {
		t.Errorf("pod = %q, want mine; a build from another project was selected", pod.Name)
	}
}

func TestLatestBuildPodPicksTheNewest(t *testing.T) {
	client := fake.NewSimpleClientset(
		buildPod("older", "default", time.Hour),
		buildPod("newest", "default", time.Second),
		buildPod("middle", "default", time.Minute),
	)

	pod, err := latestBuildPod(context.Background(), client, "todo-app", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pod.Name != "newest" {
		t.Errorf("pod = %q, want newest", pod.Name)
	}
}

// A build's Pod is removed an hour after it finishes, so "nothing here" is the
// normal state for a build that went fine. Saying only "no build found" sends
// the reader looking for a failure that never happened.
func TestLatestBuildPodExplainsAnEmptyResult(t *testing.T) {
	_, err := latestBuildPod(context.Background(), fake.NewSimpleClientset(), "todo-app", "default")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "todo-app") {
		t.Errorf("error does not name the app: %v", err)
	}
	for _, want := range []string{"finish", "hour"} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("error does not explain that logs expire (%q missing): %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "kip app rebuild todo-app") {
		t.Errorf("error does not name the command that starts a build: %v", err)
	}
}

// buildStages returns a build pod with a clone and a prepare init container
// and the kaniko main container, each in the given state.
func buildStages(phase corev1.PodPhase, clone, prepare, kaniko corev1.ContainerState) *corev1.Pod {
	p := buildPod("todo-app-build-1", "default", time.Minute)
	p.Spec.InitContainers = []corev1.Container{{Name: "clone"}, {Name: "prepare"}}
	p.Status.Phase = phase
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "clone", State: clone}, {Name: "prepare", State: prepare}}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "kaniko", State: kaniko}}
	return p
}

var (
	waiting  = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}
	running  = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	exitedOK = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
)

func exited(code int32, reason string) corev1.ContainerState {
	return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason}}
}

// podSequence serves successive pod snapshots for pod lookups, repeating the
// last one. It requires at least one pod and returns a pointer to the growing
// list of containers whose logs were requested before they started. Log requests
// succeed so tests can detect premature requests independently of error handling.
func podSequence(client *fake.Clientset, pods ...*corev1.Pod) *[]string {
	calls := 0
	var early []string
	client.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "log" {
			opts := action.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
			state := buildContainerState(pods[max(min(calls, len(pods))-1, 0)], opts.Container)
			if state.Running == nil && state.Terminated == nil {
				early = append(early, opts.Container)
			}
			return true, &corev1.Pod{}, nil
		}
		p := pods[min(calls, len(pods)-1)]
		calls++
		return true, p.DeepCopy(), nil
	})
	return &early
}

// Later stages may still be waiting when log streaming begins. Skipping their
// unavailable logs previously printed only the clone stage and reported success
// while the build continued.
func TestFollowBuildWaitsForEveryStage(t *testing.T) {
	client := fake.NewSimpleClientset()
	first := buildStages(corev1.PodPending, running, waiting, waiting)
	prepare := buildStages(corev1.PodPending, exitedOK, running, waiting)
	kaniko := buildStages(corev1.PodRunning, exitedOK, exitedOK, running)
	// Repeated snapshots simulate stages taking longer than a polling interval.
	early := podSequence(client, first, first, first, prepare, prepare, prepare, kaniko, kaniko, kaniko,
		buildStages(corev1.PodSucceeded, exitedOK, exitedOK, exitedOK))

	var out strings.Builder
	err := followBuild(context.Background(), client, first, &out, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*early) > 0 {
		t.Errorf("read the logs of %v before they started", *early)
	}
	for _, stage := range []string{"--- clone ---", "--- prepare ---", "--- kaniko ---"} {
		if !strings.Contains(out.String(), stage) {
			t.Errorf("output lacks %q:\n%s", stage, out.String())
		}
	}
}

func TestFollowBuildFailsWhenTheBuildFails(t *testing.T) {
	client := fake.NewSimpleClientset()
	first := buildStages(corev1.PodRunning, exitedOK, exitedOK, running)
	podSequence(client, first, buildStages(corev1.PodFailed, exitedOK, exitedOK, exited(1, "Error")))

	var out strings.Builder
	err := followBuild(context.Background(), client, first, &out, time.Millisecond)
	if err == nil {
		t.Fatal("a failed build must fail the command")
	}
	if !strings.Contains(err.Error(), "kaniko") || !strings.Contains(err.Error(), "code 1") {
		t.Errorf("error does not name the failed stage and its exit code: %v", err)
	}
}

// Build pods use RestartPolicyNever, so a failed init container ends the pod
// before later stages can start.
func TestFollowBuildStopsAtAFailedStage(t *testing.T) {
	client := fake.NewSimpleClientset()
	first := buildStages(corev1.PodFailed, exited(128, "Error"), waiting, waiting)
	podSequence(client, first)

	var out strings.Builder
	err := followBuild(context.Background(), client, first, &out, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "clone") {
		t.Fatalf("error does not name the failed clone stage: %v", err)
	}
	if strings.Contains(out.String(), "--- kaniko ---") {
		t.Errorf("printed a stage that never ran:\n%s", out.String())
	}
}
