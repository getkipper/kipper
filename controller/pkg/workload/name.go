// Package workload holds rules that apply to every kind of Kipper workload
// rather than to one of them.
package workload

import (
	"fmt"
	"time"
)

// Kinds are the workload kinds that compete for one name in a namespace.
//
// An App and a Function both reconcile a Deployment named after themselves, and
// a Kubernetes object has one controller, so the second of the two to claim a
// name is refused by its controller on every pass, for ever, while the CR that
// caused it looks healthy. A Job's children are named differently and do not
// contend that way, but all three label their children with the workload's name
// and are told apart only by kipper.run/resource-type, so a shared name still
// leaves an object whose owner is decided by one label. The Kubernetes API
// cannot catch any of this, because the collision is across kinds and each
// kind's own name index is free.
var Kinds = []string{"app", "function", "job"}

// ClaimUnavailableError says the reservation could not be made because the
// cluster has no WorkloadName resource to make it in.
//
// kip talks to clusters it did not install and that may predate the CRD. A
// caller that sees this falls back to reading the other kinds, which is what
// every caller did before claims existed: weaker, because two creates racing
// each other both read first, and no worse than the behaviour that cluster
// already had.
type ClaimUnavailableError struct {
	Err error
}

func (e ClaimUnavailableError) Error() string {
	return fmt.Sprintf("this cluster does not support workload name reservations: %v", e.Err)
}

func (e ClaimUnavailableError) Unwrap() error { return e.Err }

// NameTakenError says which kind of workload already holds a name.
//
// It is a type rather than a string so a caller can tell a taken name from a
// failure to find out, which are different things to whoever reads them: one
// says rename it, the other says try again.
type NameTakenError struct {
	Name string
	Kind string
}

func (e NameTakenError) Error() string {
	return fmt.Sprintf("the name %q is already used by %s %s in this environment; an app, a function and a job cannot share a name",
		e.Name, article(e.Kind), e.Kind)
}

// Holder names the kind holding the name, with its article, for a sentence that
// has to read as English wherever it is quoted.
func (e NameTakenError) Holder() string {
	return article(e.Kind) + " " + e.Kind
}

// Incumbent reports whether the workload of kind other, created at theirs, has
// the better claim to a contested name than the workload of kind mineKind
// created at mine.
//
// Age decides, because the name belongs to whoever had it, and on a cluster
// that upgraded into an existing collision no reservation says who that was.
// Ties break on the order in Kinds, so two workloads created within the same
// second still reach opposite answers and one of them proceeds; both stopping
// would need a human to break, which is the worse outcome. Every path that can
// establish the first reservation has to use this, or a client request can
// award a name the controllers would award elsewhere.
func Incumbent(other string, theirs time.Time, mineKind string, mine time.Time) bool {
	if theirs.Before(mine) {
		return true
	}
	return theirs.Equal(mine) && kindOrder(other) < kindOrder(mineKind)
}

// kindOrder is a kind's position in Kinds, which is the tiebreak when two
// workloads share a creation second.
func kindOrder(kind string) int {
	for i, k := range Kinds {
		if k == kind {
			return i
		}
	}
	return len(Kinds)
}

// article returns the indefinite article for a workload kind.
func article(kind string) string {
	if kind == "app" {
		return "an"
	}
	return "a"
}
