package installer

import (
	"fmt"
	"regexp"
	"time"

	"github.com/getkipper/kipper/kip/internal/config"
)

const (
	FenceMethodKillall    = "killall"
	FenceMethodPoweredOff = "powered-off"

	BootIDUnknown = "unknown"

	FenceStopStale    = "stale"
	FenceStopOnlyCopy = "only-copy"
)

// fenceMethodPattern keeps the method an open set, so a later automatic
// fence can record its own method without a record format change.
var fenceMethodPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// kubeNamePattern is a DNS-1123 label, the form of namespaces and the
// StatefulSet names Kipper creates.
var kubeNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// Fenced reports whether state is fencing or fenced.
func Fenced(state string) bool {
	return state == NodeStateFencing || state == NodeStateFenced
}

func validTime(s string) bool {
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

func validateFence(r NodeRecord) error {
	f := r.Fence
	switch {
	case Fenced(r.State) && f == nil:
		return fmt.Errorf("node record %s is %s but holds no fence", r.Name, r.State)
	case f == nil:
		return nil
	case r.State == NodeStateJoining || r.State == NodeStateReady:
		return fmt.Errorf("node record %s is %s but still holds a fence", r.Name, r.State)
	}
	if !fenceMethodPattern.MatchString(f.Method) {
		return fmt.Errorf("node record %s has invalid fence method %q", r.Name, f.Method)
	}
	if f.BootID != BootIDUnknown && !bootIDPattern.MatchString(f.BootID) {
		return fmt.Errorf("node record %s has invalid fence boot ID %q", r.Name, f.BootID)
	}
	if f.RebootCheckpoint != "" && !bootIDPattern.MatchString(f.RebootCheckpoint) {
		return fmt.Errorf("node record %s has invalid reboot checkpoint %q", r.Name, f.RebootCheckpoint)
	}
	if !validTime(f.StartedAt) {
		return fmt.Errorf("node record %s has invalid fence start time %q", r.Name, f.StartedAt)
	}
	for _, t := range []string{f.FencedAt, f.VerifiedAt} {
		if t != "" && !validTime(t) {
			return fmt.Errorf("node record %s has invalid fence time %q", r.Name, t)
		}
	}
	for _, s := range f.Stopped {
		if err := validateFenceStop(r.Name, s); err != nil {
			return err
		}
	}
	return nil
}

func validateFenceStop(node string, s config.FenceStop) error {
	if !kubeNamePattern.MatchString(s.Namespace) || !kubeNamePattern.MatchString(s.StatefulSet) {
		return fmt.Errorf("node record %s lists an invalid stopped service %q/%q", node, s.Namespace, s.StatefulSet)
	}
	if s.Reason != FenceStopStale && s.Reason != FenceStopOnlyCopy {
		return fmt.Errorf("node record %s has invalid stop reason %q for %s/%s", node, s.Reason, s.Namespace, s.StatefulSet)
	}
	return nil
}

// ValidBootID reports whether id has the form of a kernel boot ID.
func ValidBootID(id string) bool { return bootIDPattern.MatchString(id) }
