// Package provenance classifies resource values by field ownership.
// console-api and kip share these rules.
package provenance

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/controller/pkg/fieldowners"
)

// Source says who set a resource quantity, which decides whether the
// auto-sizer may move it.
type Source int

const (
	Unset Source = iota
	User
	Automatic
	Held
)

func (s Source) String() string {
	switch s {
	case Unset:
		return "unset"
	case User:
		return "user"
	case Automatic:
		return "automatic"
	case Held:
		return "held"
	default:
		return "unknown"
	}
}

// automaticManager is the field manager the pre-upgrade auto-sizer wrote
// under. The pre-upgrade console handlers shared it, so their values are
// treated as automatic too.
const automaticManager = "console-api"

// HeldManager is the field manager copy and migration use to carry a value
// whose author is unknown, so the target keeps it held.
const HeldManager = "kipper-held"

// provenanceManagers own fields without saying anything about a person's
// choice: the API server assigns before-first-apply on the first apply to an
// object without managedFields, and Velero creates restored objects.
var provenanceManagers = map[string]bool{
	"before-first-apply": true,
	"velero":             true,
	HeldManager:          true,
}

// ClassifyApp decides who set an App quantity, given its value and
// the object's managedFields. field is the quantity's name under
// spec.resources, such as "memoryRequest".
func ClassifyApp(value string, managedFields []metav1.ManagedFieldsEntry, field string) Source {
	if value == "" {
		return Unset
	}
	owners := fieldowners.Owners(managedFields, "spec", "resources", field)
	automatic, provenance := false, false
	for _, owner := range owners {
		switch {
		case owner == automaticManager:
			automatic = true
		case provenanceManagers[owner]:
			provenance = true
		default:
			return User
		}
	}
	if automatic && !provenance {
		return Automatic
	}
	return Held
}

// ClassifyOwned decides who set a Service or Function quantity. The
// auto-sizer never wrote those specs, so any value is the user's.
func ClassifyOwned(value string) Source {
	if value == "" {
		return Unset
	}
	return User
}

// Mode says how Kipper sizes a resource, given who set its request and limit.
type Mode int

const (
	ModeAutomatic Mode = iota
	ModeBounded
	ModeFixed
	ModeHeld
)

func (m Mode) String() string {
	switch m {
	case ModeAutomatic:
		return "automatic"
	case ModeBounded:
		return "bounded"
	case ModeFixed:
		return "fixed"
	case ModeHeld:
		return "held"
	default:
		return "unknown"
	}
}

// ModeOf classifies sizing by field ownership and whether the request is below
// the limit. Held ownership takes precedence over user settings.
func ModeOf(request, limit Source, requestBelowLimit bool) Mode {
	reqIsUser, limIsUser := request == User, limit == User
	switch {
	case request == Held || limit == Held:
		return ModeHeld
	case reqIsUser && limIsUser && requestBelowLimit:
		return ModeBounded
	case reqIsUser || limIsUser:
		return ModeFixed
	default:
		return ModeAutomatic
	}
}
