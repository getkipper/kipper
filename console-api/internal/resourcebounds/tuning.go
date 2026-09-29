package resourcebounds

import (
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/getkipper/kipper/controller/pkg/labels"
)

const (
	// ModeConfigMapName and ModeConfigMapNamespace locate the cluster-wide
	// tuning mode, "auto" or "expert".
	ModeConfigMapName      = "kipper-mode"
	ModeConfigMapNamespace = "kipper-system"
	// ModeExpert switches automatic resource changes off cluster-wide.
	ModeExpert = "expert"
)

// TuningName is the name of the ResourceTuning that belongs to an App,
// Service or Function.
func TuningName(kind, name string) string {
	return strings.ToLower(kind) + "-" + name
}

// TuningBelongsTo checks the controller UID so a recreated workload cannot
// inherit a previous owner's tuning record.
func TuningBelongsTo(record metav1.Object, ownerUID types.UID) bool {
	ref := metav1.GetControllerOf(record)
	return ref != nil && ref.UID == ownerUID
}

// TuningPaused reports whether a workload carries an unexpired tuning-pause
// annotation. A malformed timestamp counts as not paused, so a bad write
// cannot switch tuning off silently.
func TuningPaused(annotations map[string]string) bool {
	raw, ok := annotations[labels.AnnoTuningPausedUntil]
	if !ok {
		return false
	}
	until, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return false
	}
	return time.Now().Before(until)
}
