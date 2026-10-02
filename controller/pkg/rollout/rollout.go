// Package rollout shares Deployment readiness and progress checks between
// the CLI and controllers.
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

// Settled requires Ready and a total replica count matching the desired
// count, so old replicas still reported during scale-down keep it false.
func Settled(dep *appsv1.Deployment) bool {
	if !Ready(dep) {
		return false
	}
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	return dep.Status.Replicas == want
}

// Finished accepts a settled Deployment or a current generation with the
// desired replica counts and a NewReplicaSetAvailable condition. This keeps
// later pod failures separate from rollout progress.
func Finished(dep *appsv1.Deployment) bool {
	if Settled(dep) {
		return true
	}
	if dep == nil || dep.Status.ObservedGeneration < dep.Generation {
		return false
	}
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	if dep.Status.UpdatedReplicas != want || dep.Status.Replicas != want {
		return false
	}
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing {
			return c.Reason == "NewReplicaSetAvailable"
		}
	}
	return false
}

// Failed reports whether Kubernetes recorded ProgressDeadlineExceeded.
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
