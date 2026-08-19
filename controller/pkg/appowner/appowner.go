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

// Take returns the owner references an object should carry once the named App
// owns it, and whether the App may own it at all.
//
// Only an object nothing owns, or one this same App already owns. Anything else
// is refused, including an object owned by an App of this name under a
// different UID: that is the incarnation before a delete and recreate, and
// garbage collection is already entitled to remove the object by that dangling
// reference. Installing a live owner does not recall a deletion it may have
// issued, so taking it over would report success on an object that can vanish
// straight afterwards. Refusing lets the collection finish, and the next
// attempt makes the object fresh.
//
// A reference that does not control counts as an owner too. Garbage collection
// follows every reference and removes a dependent once its owners are gone, so
// an object somebody else's lifetime governs is refused whether or not their
// reference claims to control it. Take and Unowned therefore draw the line in
// the same place, which is the point of them being one decision.
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

// Unowned reports whether a writer holding no App may use an object.
//
// Only an object nothing owns. Garbage collection follows every owner
// reference and removes a dependent once its owners are gone, and a writer with
// no App cannot add one in the same write to keep the object alive, so
// committing an App onto anything owned hands that owner the credential's
// lifetime.
//
// A reference to an App that no longer exists is refused too, which looks
// over-careful and is not: the object is one garbage collection is already
// entitled to remove, and stripping the reference does not recall a deletion it
// may have issued already. The deploy stops, the collection completes, and the
// next attempt creates the object fresh. Take is the path with a live App,
// because it can add that App beside whatever else names the object.
func Unowned(refs []metav1.OwnerReference) bool {
	return len(refs) == 0
}
