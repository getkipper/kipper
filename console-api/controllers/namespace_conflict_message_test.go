package controllers

import (
	"slices"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// The add-environment flow reads the refused namespaces back out of this
// condition so it can answer a request straight away instead of waiting out the
// timeout on a claim that is never coming. The message is prose and the names
// are taken from its quoted slots, so rewording it here changes that answer
// without changing anything that looks like an interface: the reader finds no
// names, reports no refusal, and the request waits the full minute before
// failing on something else. That happened. This is the round trip that fails
// instead.
func conflictMessage(t *testing.T, conflicts ...*namespaceConflictError) string {
	t.Helper()
	project := &kipperv1.Project{}
	(&ProjectReconciler{}).setNamespaceConflictCondition(project, conflicts)
	cond := apimeta.FindStatusCondition(project.Status.Conditions, conditionNamespaceConflict)
	if cond == nil {
		t.Fatal("no NamespaceConflict condition was recorded for a pass that had conflicts")
	}
	return cond.Message
}

func TestTheRefusedNamespaceIsReadableBackOutOfTheConditionItIsReportedIn(t *testing.T) {
	message := conflictMessage(t, &namespaceConflictError{
		namespace: "shop-prod", owner: "grocer", claimant: "shop",
	})

	refused := RefusedNamespaces(message)

	if !slices.Contains(refused, "shop-prod") {
		t.Errorf("the refused namespace could not be read back out of %q; got %q", message, refused)
	}
	// The claimant and the holder are named in the same sentence, and a
	// project's default environment resolves to a namespace with the project's
	// own name. Reading either of them as refused refuses an environment the
	// reconcile never touched.
	for _, project := range []string{"shop", "grocer"} {
		if slices.Contains(refused, project) {
			t.Errorf("project %q was read as a refused namespace out of %q", project, message)
		}
	}
}

// The other kind of conflict, which leads with the same slot and a different
// sentence after it.
func TestAnUnlabelledNamespaceIsReadableBackOutOfItsConditionToo(t *testing.T) {
	message := conflictMessage(t, &namespaceConflictError{namespace: "shop-prod", claimant: "shop"})

	if refused := RefusedNamespaces(message); !slices.Contains(refused, "shop-prod") {
		t.Errorf("the refused namespace could not be read back out of %q; got %q", message, refused)
	}
}

// A pass that refuses several reports the first and lists the rest, and the
// caller asking about any one of them has to get an answer.
func TestEveryNamespaceARefusingPassNamesIsReadableBackOut(t *testing.T) {
	message := conflictMessage(t,
		&namespaceConflictError{namespace: "shop-prod", owner: "grocer", claimant: "shop"},
		&namespaceConflictError{namespace: "shop-eu", owner: "grocer", claimant: "shop"},
		&namespaceConflictError{namespace: "shop-us", claimant: "shop"},
	)

	refused := RefusedNamespaces(message)

	for _, ns := range []string{"shop-prod", "shop-eu", "shop-us"} {
		if !slices.Contains(refused, ns) {
			t.Errorf("namespace %q was refused and listed but could not be read back out of %q; got %q", ns, message, refused)
		}
	}
}
