package platform

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// servedSinceFailure reports whether lastFailedAt is empty, or both are valid
// RFC 3339 timestamps and lastHealthyAt is strictly after lastFailedAt.
func servedSinceFailure(lastHealthyAt, lastFailedAt string) bool {
	if lastFailedAt == "" {
		return true
	}
	failed, err := time.Parse(time.RFC3339, lastFailedAt)
	if err != nil {
		return false
	}
	healthy, err := time.Parse(time.RFC3339, lastHealthyAt)
	return err == nil && healthy.After(failed)
}

// CopyMarkers are a Longhorn replica's spec.healthyAt, failedAt, lastFailedAt
// and lastHealthyAt.
type CopyMarkers struct {
	HealthyAt, FailedAt, LastFailedAt, LastHealthyAt string
}

// CopyVolume supplies volume context to HealthyCopies. State is status.state.
// Seeded indicates data from a backup, another volume, a snapshot or a backing
// image, including data whose restore or clone is still pending.
type CopyVolume struct {
	State  string
	Seeded bool
}

// CopyVolumeOf reads a CopyVolume from a Longhorn v1beta2 Volume object.
// Seeded is true when spec.fromBackup, spec.dataSource, spec.backingImage or
// status.cloneStatus.sourceVolume is non-empty, or status.restoreRequired is true,
// including while the data is still pending. Missing or mistyped fields are
// treated as empty strings or false.
func CopyVolumeOf(volume map[string]interface{}) CopyVolume {
	str := func(path ...string) string {
		v, _, _ := unstructured.NestedString(volume, path...)
		return v
	}
	restore, _, _ := unstructured.NestedBool(volume, "status", "restoreRequired")
	return CopyVolume{
		State: str("status", "state"),
		Seeded: restore || str("spec", "fromBackup") != "" || str("spec", "dataSource") != "" ||
			str("spec", "backingImage") != "" || str("status", "cloneStatus", "sourceVolume") != "",
	}
}

// HealthyCopies reports whether each replica's markers indicate a complete copy
// of one Longhorn volume. Results preserve the input order and length.
// An active failure returns false. Otherwise, a replica with non-empty healthyAt
// counts when ReplicaCurrent accepts its markers or valid RFC 3339 timestamps
// show lastHealthyAt strictly after a non-empty lastFailedAt. The latter accepts
// a salvaged copy that served writes again after its last failure.
// A detached, unseeded volume with no health or failure markers on any replica
// is treated as empty, so all its copies count. Failure history on a replica
// reused for a rebuild prevents it from being treated as fresh.
// Callers check node availability separately. These markers alone cannot rule
// out newer writes on another copy.
func HealthyCopies(volume CopyVolume, replicas []CopyMarkers) []bool {
	used := volume.Seeded
	for _, r := range replicas {
		used = used || r.HealthyAt != "" || r.FailedAt != "" || r.LastFailedAt != "" || r.LastHealthyAt != ""
	}
	out := make([]bool, len(replicas))
	for i, r := range replicas {
		switch {
		case r.FailedAt != "":
		case !used:
			out[i] = volume.State == "detached"
		case r.HealthyAt != "":
			out[i] = ReplicaCurrent(r.HealthyAt, r.FailedAt, r.LastFailedAt) ||
				(r.LastFailedAt != "" && servedSinceFailure(r.LastHealthyAt, r.LastFailedAt))
		}
	}
	return out
}
