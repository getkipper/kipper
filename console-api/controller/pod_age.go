package controller

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// podAge measures time since the latest running workload container started,
// falling back to pod creation. This gives restarted containers a fresh
// startup grace period even in an older pod.
func podAge(pod *corev1.Pod, now time.Time) time.Duration {
	started := pod.CreationTimestamp.Time
	for _, s := range workloadStatuses(pod) {
		if r := s.State.Running; r != nil && r.StartedAt.After(started) {
			started = r.StartedAt.Time
		}
	}
	return now.Sub(started)
}

// restartedAt returns the latest start time among running workload containers
// with a positive restart count, or false if none has a start time.
func restartedAt(pod *corev1.Pod) (time.Time, bool) {
	var latest time.Time
	for _, s := range workloadStatuses(pod) {
		if r := s.State.Running; r != nil && s.RestartCount > 0 && r.StartedAt.After(latest) {
			latest = r.StartedAt.Time
		}
	}
	return latest, !latest.IsZero()
}

// workloadStatuses includes regular containers and native sidecars (init
// containers with restartPolicy Always).
func workloadStatuses(pod *corev1.Pod) []corev1.ContainerStatus {
	sidecars := make(map[string]bool)
	for _, c := range pod.Spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sidecars[c.Name] = true
		}
	}
	statuses := append([]corev1.ContainerStatus(nil), pod.Status.ContainerStatuses...)
	for _, s := range pod.Status.InitContainerStatuses {
		if sidecars[s.Name] {
			statuses = append(statuses, s)
		}
	}
	return statuses
}
