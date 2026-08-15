package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

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
}
