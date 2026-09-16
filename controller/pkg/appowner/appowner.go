// Package appowner decides whether a Kipper App may own an object it did not
// create, because more than one module has to agree about it.
//
// A git credential is named after the token and host it holds rather than after
// whoever asked for it, so two writers converge on one object and an App
// deleted and recreated under the same name meets the object its predecessor
// made. Whether that object may be taken over is the same question for the
// console, the CLI and the reconciler, and the answers have to match: one of
// them committing an App onto a Secret another controller owns means that
// controller can delete the credential the App is cloning with.
package appowner

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Kind is the owner kind an App writes.
const Kind = "App"

// Reference is the controller reference an App puts on an object it owns.
func Reference(apiVersion, name string, uid types.UID) metav1.OwnerReference {
	controller, block := true, true
	return metav1.OwnerReference{
		APIVersion:         apiVersion,
		Kind:               Kind,
		Name:               name,
		UID:                uid,
		Controller:         &controller,
		BlockOwnerDeletion: &block,
	}
}

// Take accepts unowned objects or references exclusively to want.UID, returning
// the controller reference to install. References to another UID, including
// non-controller references and earlier App incarnations, prevent adoption
// because garbage collection may already be deleting those objects.
func Take(refs []metav1.OwnerReference, want metav1.OwnerReference) ([]metav1.OwnerReference, bool) {
	ours := false
	for _, ref := range refs {
		if ref.UID != want.UID {
			return refs, false
		}
		// Scanning the rest matters: a foreign owner after ours governs
		// collection just the same, and stopping here would accept an object
		// the sweeps then refuse to collect.
		if ref.Controller != nil && *ref.Controller {
			ours = true
		}
	}
	if ours {
		// Already ours as written. Returned unchanged so a caller that compares
		// can tell there is nothing to write.
		return refs, true
	}
	// Either nothing owns it, or it lists us without controlling; the reference
	// replaces that rather than joining it, because the apiserver refuses two
	// references to one object.
	return []metav1.OwnerReference{want}, true
}

// OnlyOwnedBy reports whether every owner of an object is the given App, which
// includes an object nothing owns.
//
// Deleting is the same question as taking: an owner that does not control still
// governs collection, so an object anything else owns is not this App's to
// remove, whatever the controller reference says.
func OnlyOwnedBy(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, ref := range refs {
		if ref.UID != uid {
			return false
		}
	}
	return true
}

// Unowned permits use only when an object has no owner references. Even a
// stale reference can trigger garbage collection; removing it cannot recall
// a deletion already issued. Writers with a live App can use Take.
func Unowned(refs []metav1.OwnerReference) bool {
	return len(refs) == 0
}
