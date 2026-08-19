package appowner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const api = "kipper.run/v1alpha1"

func ref(apiVersion, kind, name, uid string, controls bool) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: apiVersion, Kind: kind, Name: name,
		UID: types.UID(uid), Controller: &controls,
	}
}

func TestTakeDecidesWhoMayOwnTheObject(t *testing.T) {
	want := Reference(api, "web", types.UID("live"))

	t.Run("nothing controls it", func(t *testing.T) {
		out, ok := Take(nil, want)
		assert.True(t, ok)
		assert.Equal(t, []metav1.OwnerReference{want}, out)
	})

	// The incarnation before a delete and recreate. Garbage collection is
	// already entitled to remove the object by that dangling reference, and
	// installing a live owner does not recall a deletion it may have issued, so
	// taking it over would report success on an object that can vanish.
	t.Run("an app of this name that no longer exists", func(t *testing.T) {
		in := []metav1.OwnerReference{ref(api, "App", "web", "dead", true)}
		out, ok := Take(in, want)
		assert.False(t, ok, "an object garbage collection may already be removing was taken over")
		assert.Equal(t, in, out)
	})

	t.Run("already ours", func(t *testing.T) {
		mine := []metav1.OwnerReference{ref(api, "App", "web", "live", true)}
		out, ok := Take(mine, want)
		assert.True(t, ok)
		assert.Equal(t, mine, out, "an object already ours was rewritten")
	})

	t.Run("controlled by something else", func(t *testing.T) {
		for _, other := range []metav1.OwnerReference{
			ref(api, "Service", "something", "x", true),
			ref(api, "App", "another-app", "x", true),
			ref("example.com/v1", "App", "web", "x", true),
			ref(api, "App", "web", "dead", true),
			ref(api, "Project", "shop", "p", false),
		} {
			in := []metav1.OwnerReference{other}
			out, ok := Take(in, want)
			assert.False(t, ok, "an object controlled by %s/%s %q was taken", other.APIVersion, other.Kind, other.Name)
			assert.Equal(t, in, out, "the refusal changed the references")
		}
	})

	// An owner that does not control is still an owner: collection follows
	// every reference. Refusing here is what makes Take and Unowned one rule.
	t.Run("an owner that does not control is still an owner", func(t *testing.T) {
		in := []metav1.OwnerReference{ref(api, "Project", "shop", "p", false)}
		out, ok := Take(in, want)
		assert.False(t, ok, "an object another actor's lifetime governs was accepted")
		assert.Equal(t, in, out)
	})
}

// A writer with no App cannot keep an object alive, so it may only use one
// nothing owns. A reference to an App that is gone is refused with the rest:
// the object is one garbage collection is entitled to remove, and dropping the
// reference does not recall a deletion already issued.
func TestUnownedAcceptsOnlyAnObjectNothingOwns(t *testing.T) {
	assert.True(t, Unowned(nil))
	assert.True(t, Unowned([]metav1.OwnerReference{}))

	for _, owner := range []metav1.OwnerReference{
		ref(api, "App", "web", "dead", true),
		ref(api, "Service", "something", "s", true),
		ref(api, "Project", "shop", "p", false),
	} {
		assert.False(t, Unowned([]metav1.OwnerReference{owner}),
			"an object owned by %s %q was accepted by a writer that cannot keep it alive", owner.Kind, owner.Name)
	}
}

// A reference written under an earlier version of the same CRD still names the
// same App, so an upgrade that adds a version must not turn every takeover into
// a refusal.
// The same App under an earlier CRD version is still ours, and is recognised by
// UID rather than by the version its reference was written under.
func TestTakeRecognisesItselfWhateverVersionWroteTheReference(t *testing.T) {
	want := Reference(api, "web", types.UID("live"))

	out, ok := Take([]metav1.OwnerReference{ref("kipper.run/v1beta1", "App", "web", "live", true)}, want)
	assert.True(t, ok, "an app did not recognise its own reference written under another version")
	assert.Equal(t, []metav1.OwnerReference{ref("kipper.run/v1beta1", "App", "web", "live", true)}, out)
}

// Two references to one object are rejected by the apiserver, so an App already
// listed as a plain owner must not be listed again as the controller.
func TestTakeDoesNotListTheSameOwnerTwice(t *testing.T) {
	want := Reference(api, "web", types.UID("live"))

	out, ok := Take([]metav1.OwnerReference{ref(api, "App", "web", "live", false)}, want)

	assert.True(t, ok)
	assert.Equal(t, []metav1.OwnerReference{want}, out,
		"the app was listed twice, which the apiserver refuses")
}

// Deleting asks the same question as taking. A credential something else owns
// is not this App's to remove, whatever marks the controller.
func TestOnlyOwnedByCountsEveryOwner(t *testing.T) {
	const mine = types.UID("mine")

	assert.True(t, OnlyOwnedBy(nil, mine), "an object nothing owns is this app's to remove")
	assert.True(t, OnlyOwnedBy([]metav1.OwnerReference{ref(api, "App", "web", "mine", true)}, mine))

	assert.False(t, OnlyOwnedBy([]metav1.OwnerReference{ref(api, "Project", "shop", "p", false)}, mine),
		"an owner that does not control still governs collection")
	assert.False(t, OnlyOwnedBy([]metav1.OwnerReference{
		ref(api, "App", "web", "mine", true), ref(api, "Project", "shop", "p", false),
	}, mine), "an object co-owned with another actor was treated as this app's alone")
}

// Scanning must not stop at our own reference. A foreign owner listed after it
// governs collection just the same, and accepting the object would leave the
// sweeps refusing to collect what this write committed to.
func TestTakeScansPastItsOwnReference(t *testing.T) {
	want := Reference(api, "web", types.UID("live"))
	mine := ref(api, "App", "web", "live", true)
	foreign := ref(api, "Project", "shop", "p", false)

	for _, order := range [][]metav1.OwnerReference{{mine, foreign}, {foreign, mine}} {
		out, ok := Take(order, want)
		assert.False(t, ok, "a co-owned object was accepted in one order and not the other")
		assert.Equal(t, order, out)
	}
}
