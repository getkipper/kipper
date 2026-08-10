// Package rollout answers whether a Deployment has finished rolling the spec it
// was last given. The console-api reconciler gates a host cutover on it and kip
// gates an upgrade step on it, and both have to mean the same thing by "ready":
// an upgrade that reported success on a weaker condition than the cutover
// requires would hand the operator a green tick over a cluster that never
// started the new code.
package rollout

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// Ready reports whether every replica of the Deployment's current generation is
// updated and available. It compares the observed generation so a status left
// over from the previous spec cannot be mistaken for a completed rollout.
func Ready(dep *appsv1.Deployment) bool {
	if dep == nil {
		return false
	}
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	st := dep.Status
	return st.ObservedGeneration >= dep.Generation &&
		st.UpdatedReplicas == want &&
		st.AvailableReplicas == want &&
		st.UnavailableReplicas == 0
}

// Failed reports whether Kubernetes has given up on the rollout. This is the
// signal that waiting longer is pointless: a pod that cannot be admitted or
// keeps crashing never becomes available, and with a surge-only strategy the old
// pod keeps serving, so nothing else makes the failure visible.
func Failed(dep *appsv1.Deployment) bool {
	if dep == nil {
		return false
	}
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse &&
			c.Reason == "ProgressDeadlineExceeded" {
			return true
		}
	}
	return false
}

// Message returns the Progressing condition's message, which carries the reason
// a rollout stalled.
func Message(dep *appsv1.Deployment) string {
	if dep == nil {
		return ""
	}
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing {
			return c.Message
		}
	}
	return ""
}
