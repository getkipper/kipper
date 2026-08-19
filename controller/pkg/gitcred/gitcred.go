// Package gitcred decides whether a git credential Secret already at a
// generated name may be used for the pair that name stands for.
package gitcred

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/controller/pkg/appowner"
	"github.com/getkipper/kipper/controller/pkg/labels"
)

// Claim prepares a credential Secret that already exists to be the one appName
// clones with, and reports why it cannot be. It lives here because kip and
// console-api both write credentials, and a check only one of them makes
// protects only the apps deployed through that one.
//
// A credential is named after a digest of the token and the clone host, so two
// writers of the same pair converge on one object rather than overwriting what
// an app is cloning with. That name is not a proof of what is at it: sixteen
// hex characters can collide, and anything able to write a Secret in the
// namespace can put something else there. So the contents are checked before an
// App is pointed at them, or the app clones with a token nobody supplied.
//
// owner is the App committing to the credential, and nil where the writer has
// no App yet and the reconciler binds one later. claimedAt is the writer's own
// clock, which holds the controller's sweep off an object a commit is still in
// flight for.
//
// live is prepared in place, and left as it was found when this returns an
// error.
func Claim(live *corev1.Secret, appName, token, authority string, owner *metav1.OwnerReference, claimedAt time.Time) error {
	if string(live.Data["token"]) != token {
		return refuse("the credential %s already exists and does not hold the token given, so it was not used. Remove that Secret if it is stale, or use a different token", live.Name)
	}
	if bound := live.Annotations[labels.AnnoGitAuthority]; bound != "" && bound != authority {
		return refuse("the credential %s already exists and is recorded for %s rather than %s, so it was not used. Remove that Secret if it is stale", live.Name, bound, authority)
	}

	// Something still running decides an owned object's lifetime, so pointing
	// the App at it would let that thing delete the credential the app clones
	// with. Refusing is the only honest answer: the name is derived from the
	// token, so there is no other object to write instead.
	refs := live.OwnerReferences
	if owner != nil && owner.UID != "" {
		taken, mayOwn := appowner.Take(live.OwnerReferences, *owner)
		if !mayOwn {
			return ownedElsewhere(live.Name)
		}
		refs = taken
	} else if !appowner.Unowned(live.OwnerReferences) {
		// No App to add as an owner, so an object anything already owns is one
		// this write cannot keep alive.
		return ownedElsewhere(live.Name)
	}

	live.OwnerReferences = refs
	if live.Annotations == nil {
		live.Annotations = map[string]string{}
	}
	if authority != "" {
		live.Annotations[labels.AnnoGitAuthority] = authority
	}
	live.Annotations[labels.AnnoGitCredentialClaimed] = claimedAt.UTC().Format(time.RFC3339)
	// The writer labels are ours and their absence only hurts: without them the
	// controller's sweeps cannot see the object to collect it, and a credential
	// the app later rotates off would stay in the namespace for good.
	if live.Labels == nil {
		live.Labels = map[string]string{}
	}
	live.Labels[labels.ManagedBy] = labels.Kipper
	live.Labels[labels.AppRef] = appName
	return nil
}

func ownedElsewhere(name string) error {
	return refuse("the credential %s belongs to something else in this namespace, so it cannot be used here. If an app of this name was just deleted, try again once its credential has been cleaned up; otherwise check what owns that Secret", name)
}

// Refusal is a claim no retry turns into a success: what is at the name is not
// the pair the name stands for, or it belongs to something else. It is its own
// type so a caller that answers with a status code can tell it from an
// apiserver failure, which is worth trying again.
type Refusal struct {
	Reason string
}

func (r *Refusal) Error() string { return r.Reason }

func refuse(format string, a ...any) error {
	return &Refusal{Reason: fmt.Sprintf(format, a...)}
}
