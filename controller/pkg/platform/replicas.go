package platform

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// OutOfServiceTaint is Kubernetes' non-graceful node shutdown taint, which
// kip node fence applies to a dead worker.
const OutOfServiceTaint = "node.kubernetes.io/out-of-service"

// NodeAvailable reports whether node has a True Ready condition and no
// out-of-service taint. A fenced host can report Ready after booting, so the
// taint wins. A nil node is not available.
func NodeAvailable(node *corev1.Node) bool {
	if node == nil {
		return false
	}
	for _, t := range node.Spec.Taints {
		if t.Key == OutOfServiceTaint {
			return false
		}
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// ReplicaCurrent reports whether healthyAt is a valid RFC 3339 timestamp,
// failedAt is empty, and lastFailedAt is either empty or a valid RFC 3339
// timestamp strictly before healthyAt. Missing healthyAt or invalid timestamps
// return false. Requiring recovery after the last failure prevents a replica
// salvaged by clearing failedAt alone from counting as current.
func ReplicaCurrent(healthyAt, failedAt, lastFailedAt string) bool {
	if failedAt != "" || healthyAt == "" {
		return false
	}
	healthy, err := time.Parse(time.RFC3339, healthyAt)
	if err != nil {
		return false
	}
	if lastFailedAt == "" {
		return true
	}
	failed, err := time.Parse(time.RFC3339, lastFailedAt)
	if err != nil {
		return false
	}
	return healthy.After(failed)
}
