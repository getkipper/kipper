package k8s

import corev1 "k8s.io/api/core/v1"

// podsStarting treats pending or running pods as starting when their reported
// states allow it. Finished and deleting pods are ignored; at least one pod must
// remain. Assigned nodes must be Ready, and explicit scheduling failures,
// container errors, and unsuccessful exits rule out startup.
func podsStarting(pods []corev1.Pod, readyNodes map[string]bool) bool {
	live := 0
	for _, p := range pods {
		if p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded {
			continue
		}
		live++
		if p.Status.Phase != corev1.PodPending && p.Status.Phase != corev1.PodRunning {
			return false
		}
		// Check node readiness because container states can outlive node contact.
		if p.Spec.NodeName != "" && !readyNodes[p.Spec.NodeName] {
			return false
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
				return false
			}
		}
		for _, s := range p.Status.InitContainerStatuses {
			if !initContainerStarting(s) {
				return false
			}
		}
		for _, s := range p.Status.ContainerStatuses {
			if !containerStarting(s) {
				return false
			}
		}
	}
	return live > 0
}

// initContainerStarting accepts successful completion even after retries.
func initContainerStarting(s corev1.ContainerStatus) bool {
	if s.State.Terminated != nil && s.State.Terminated.ExitCode == 0 {
		return true
	}
	return containerStarting(s)
}

// containerStarting rejects restarts with a recorded termination because
// Running can be a temporary state between crashes.
func containerStarting(s corev1.ContainerStatus) bool {
	if s.RestartCount > 0 && s.LastTerminationState.Terminated != nil {
		return false
	}
	state := s.State
	switch {
	case state.Waiting != nil:
		switch state.Waiting.Reason {
		case "", "ContainerCreating", "PodInitializing":
			return true
		}
		return false
	case state.Terminated != nil:
		return state.Terminated.ExitCode == 0
	default:
		return true
	}
}
