package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func podCreatedAt(created time.Time, statuses ...corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(created)},
		Status:     corev1.PodStatus{ContainerStatuses: statuses},
	}
}

func runningSince(started time.Time, restarts int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		RestartCount: restarts,
		State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(started)}},
	}
}

func TestPodAgeCountsFromTheLatestContainerStart(t *testing.T) {
	now := time.Date(2026, 10, 10, 21, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		pod  *corev1.Pod
		want time.Duration
	}{
		{"a container restarted after a node reboot", podCreatedAt(now.Add(-time.Hour), runningSince(now.Add(-time.Minute), 1)), time.Minute},
		{"a new pod", podCreatedAt(now.Add(-3*time.Minute), runningSince(now.Add(-2*time.Minute), 0)), 2 * time.Minute},
		{"the latest of several containers", podCreatedAt(now.Add(-time.Hour),
			runningSince(now.Add(-50*time.Minute), 0), runningSince(now.Add(-4*time.Minute), 2)), 4 * time.Minute},
		{"no running container falls back to creation", podCreatedAt(now.Add(-10 * time.Minute)), 10 * time.Minute},
	}
	for _, c := range cases {
		if got := podAge(c.pod, now); got != c.want {
			t.Errorf("%s: podAge = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRestartedAtIgnoresNewPods(t *testing.T) {
	now := time.Date(2026, 10, 10, 21, 0, 0, 0, time.UTC)

	if _, ok := restartedAt(podCreatedAt(now.Add(-2*time.Minute), runningSince(now.Add(-time.Minute), 0))); ok {
		t.Error("a new pod from a scale-out counted as restarted")
	}
	at, ok := restartedAt(podCreatedAt(now.Add(-time.Hour), runningSince(now.Add(-time.Minute), 1)))
	if !ok || !at.Equal(now.Add(-time.Minute)) {
		t.Errorf("restartedAt = %v, %v; want the container's start after its restart", at, ok)
	}
}

func withNativeSidecar(pod *corev1.Pod, status corev1.ContainerStatus) *corev1.Pod {
	always := corev1.ContainerRestartPolicyAlways
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{Name: "proxy", RestartPolicy: &always})
	status.Name = "proxy"
	pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses, status)
	return pod
}

func TestNativeSidecarsCountAsContainers(t *testing.T) {
	now := time.Date(2026, 10, 10, 21, 0, 0, 0, time.UTC)
	pod := withNativeSidecar(podCreatedAt(now.Add(-time.Hour), runningSince(now.Add(-time.Hour), 0)), runningSince(now.Add(-time.Minute), 1))

	if got := podAge(pod, now); got != time.Minute {
		t.Errorf("podAge = %v, want 1m: the restarted native sidecar's CPU counts in the pod's metrics", got)
	}
	if _, ok := restartedAt(pod); !ok {
		t.Error("a restarted native sidecar did not count as a restart")
	}
}

func TestCompletedInitContainersDoNotCount(t *testing.T) {
	now := time.Date(2026, 10, 10, 21, 0, 0, 0, time.UTC)
	pod := podCreatedAt(now.Add(-time.Hour), runningSince(now.Add(-time.Hour), 0))
	pod.Spec.InitContainers = []corev1.Container{{Name: "migrate"}}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "migrate", RestartCount: 2,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-time.Minute))}},
	}}

	if got := podAge(pod, now); got != time.Hour {
		t.Errorf("podAge = %v, want 1h: an ordinary init container is not part of the running workload", got)
	}
	if _, ok := restartedAt(pod); ok {
		t.Error("an ordinary init container counted as a restart")
	}
}
