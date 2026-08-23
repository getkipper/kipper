package handlers

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// Every fixture is a conflict recorded against the same project, whose own name
// is what the matching has to avoid reading as a refused namespace.
const refusedProject = "shop"

func conflicting(generation int64, message string) *kipperv1.Project {
	p := &kipperv1.Project{}
	p.Name = refusedProject
	p.Generation = generation
	p.Status.Conditions = []metav1.Condition{{
		Type:               "NamespaceConflict",
		Status:             metav1.ConditionTrue,
		Reason:             "NamespaceOwnedByAnotherProject",
		Message:            message,
		ObservedGeneration: generation,
	}}
	return p
}

// The message names the refused namespace and the project that wanted it. A
// project's default environment resolves to a namespace with the project's own
// name, so a match on any word in the message answers about the claimant
// instead of the namespace, and reports a refusal of something that was never
// refused.
func TestAConflictAboutOneNamespaceDoesNotRefuseTheProjectsOwnName(t *testing.T) {
	p := conflicting(3,
		`namespace "shop-prod" already belongs to project "grocer", so project "shop" cannot use it; `+
			`the two projects resolve to the same namespace name and one of them has to be renamed`)

	if refusal := namespaceRefusal(p, "shop"); refusal != "" {
		t.Errorf("adding the default environment was refused on the strength of a conflict about shop-prod: %q", refusal)
	}
}

// And the namespace the conflict is genuinely about is still reported, even
// when it happens to carry the project's own name.
func TestTheRefusedNamespaceIsStillReportedWhenItCarriesTheProjectName(t *testing.T) {
	p := conflicting(3,
		`namespace "shop" already belongs to project "grocer", so project "shop" cannot use it; `+
			`the two projects resolve to the same namespace name and one of them has to be renamed`)

	if namespaceRefusal(p, "shop") == "" {
		t.Error("a project was told nothing about a namespace the reconcile had refused it, so the request waits for a claim that will never come")
	}
}

// A longer name that merely starts with the refused one is a different
// namespace.
func TestAPrefixOfTheRefusedNamespaceIsNotRefused(t *testing.T) {
	p := conflicting(3,
		`namespace "shop-prod" already belongs to project "grocer", so project "shop" cannot use it`)

	if refusal := namespaceRefusal(p, "shop-prod-eu"); refusal != "" {
		t.Errorf("shop-prod-eu was refused on the strength of a conflict about shop-prod: %q", refusal)
	}
}

// The trailing list of other refused namespaces is part of the answer.
func TestTheOtherRefusedNamespacesAreMatchedToo(t *testing.T) {
	p := conflicting(3,
		`namespace "shop-prod" already belongs to project "grocer", so project "shop" cannot use it. `+
			`2 more namespaces are in the same position: "shop-eu", "shop-us"`)

	if namespaceRefusal(p, "shop-eu") == "" {
		t.Error("a namespace listed among the others was not reported as refused, so the caller waits out the timeout for an answer the reconcile already gave")
	}
}

// A condition from before the current spec describes a project that has since
// changed, and answering from it refuses an environment on stale grounds.
func TestAConditionFromAnOlderGenerationSaysNothing(t *testing.T) {
	p := conflicting(5,
		`namespace "shop-prod" already belongs to project "grocer", so project "shop" cannot use it`)
	p.Status.Conditions[0].ObservedGeneration = 4

	if refusal := namespaceRefusal(p, "shop-prod"); refusal != "" {
		t.Errorf("a conflict recorded against an older spec was reported as current: %q", refusal)
	}
}
