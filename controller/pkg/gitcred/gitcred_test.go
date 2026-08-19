package gitcred

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/getkipper/kipper/controller/pkg/appowner"
	"github.com/getkipper/kipper/controller/pkg/labels"
)

var claimedAt = time.Date(2026, 8, 19, 9, 30, 0, 0, time.UTC)

func credential(token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-git-credentials-0123456789abcdef", Namespace: "acme-prod"},
		Data:       map[string][]byte{"token": []byte(token)},
	}
}

func appRef(uid string) *metav1.OwnerReference {
	ref := appowner.Reference("kipper.run/v1alpha1", "shop", types.UID(uid))
	return &ref
}

// The reason this package exists. The name is a digest of the pair, so an
// object at it that holds a different token is either a collision or something
// planted there, and committing the app onto it would clone with a token
// nobody supplied.
func TestClaimRefusesAnObjectHoldingADifferentToken(t *testing.T) {
	live := credential("someone-elses-token")

	err := Claim(live, "shop", "the-real-token", "github.com", appRef("app-uid"), claimedAt)

	if err == nil {
		t.Fatal("claimed a credential holding a different token")
	}
	if live.Labels != nil || live.Annotations != nil {
		t.Error("a refused claim must leave the object as it was found")
	}
}

// The clone host is half the pair. A credential recorded for one host must not
// be used to reach another, which is what stops a token stored for one forge
// being sent to a different one.
func TestClaimRefusesAnObjectRecordedForAnotherHost(t *testing.T) {
	live := credential("the-real-token")
	live.Annotations = map[string]string{labels.AnnoGitAuthority: "gitlab.example.com"}

	err := Claim(live, "shop", "the-real-token", "github.com", appRef("app-uid"), claimedAt)

	if err == nil {
		t.Fatal("claimed a credential recorded for another host")
	}
}

func TestClaimTakesAnUnownedObjectHoldingThePair(t *testing.T) {
	live := credential("the-real-token")

	if err := Claim(live, "shop", "the-real-token", "github.com", appRef("app-uid"), claimedAt); err != nil {
		t.Fatalf("refused a credential holding the pair: %v", err)
	}
	if len(live.OwnerReferences) != 1 || live.OwnerReferences[0].UID != types.UID("app-uid") {
		t.Errorf("owner references = %+v", live.OwnerReferences)
	}
	if live.Annotations[labels.AnnoGitCredentialClaimed] != "2026-08-19T09:30:00Z" {
		t.Errorf("claimed stamp = %q", live.Annotations[labels.AnnoGitCredentialClaimed])
	}
	if live.Annotations[labels.AnnoGitAuthority] != "github.com" {
		t.Errorf("recorded host = %q", live.Annotations[labels.AnnoGitAuthority])
	}
}

// Without the writer labels the controller's sweeps cannot list the object, so
// a credential the app rotates off stays in the namespace for the life of it.
// A writer that verified the contents and then left the labels off would trade
// one leak for another.
func TestClaimRepairsTheWriterLabels(t *testing.T) {
	live := credential("the-real-token")
	live.Labels = map[string]string{"kipper.run/app": "not-this-app"}

	if err := Claim(live, "shop", "the-real-token", "github.com", appRef("app-uid"), claimedAt); err != nil {
		t.Fatalf("refused a credential holding the pair: %v", err)
	}
	if live.Labels[labels.ManagedBy] != labels.Kipper {
		t.Errorf("managed-by = %q", live.Labels[labels.ManagedBy])
	}
	if live.Labels[labels.AppRef] != "shop" {
		t.Errorf("app label = %q", live.Labels[labels.AppRef])
	}
}

// An object something else owns dies when that thing does, and the name is
// derived from the token so there is no other object to write instead.
func TestClaimRefusesAnObjectOwnedBySomethingElse(t *testing.T) {
	live := credential("the-real-token")
	live.OwnerReferences = []metav1.OwnerReference{*appRef("a-dead-incarnation")}

	err := Claim(live, "shop", "the-real-token", "github.com", appRef("app-uid"), claimedAt)

	if err == nil {
		t.Fatal("claimed a credential owned by something else")
	}
	if len(live.OwnerReferences) != 1 || live.OwnerReferences[0].UID != types.UID("a-dead-incarnation") {
		t.Error("a refused claim must leave the owner references as they were found")
	}
}

// The first deploy of an app writes its credential before the App exists, so
// there is no owner to add. Nothing can then keep an already-owned object
// alive for this app, and taking it would point the app at a credential that
// vanishes with whatever owns it.
func TestClaimWithNoAppRefusesAnOwnedObject(t *testing.T) {
	live := credential("the-real-token")
	live.OwnerReferences = []metav1.OwnerReference{*appRef("another-app")}

	if err := Claim(live, "shop", "the-real-token", "github.com", nil, claimedAt); err == nil {
		t.Fatal("claimed an owned credential with no app to keep it alive")
	}
}

func TestClaimWithNoAppTakesAnUnownedObject(t *testing.T) {
	live := credential("the-real-token")

	if err := Claim(live, "shop", "the-real-token", "github.com", nil, claimedAt); err != nil {
		t.Fatalf("refused an unowned credential holding the pair: %v", err)
	}
	if len(live.OwnerReferences) != 0 {
		t.Errorf("owner references = %+v", live.OwnerReferences)
	}
}

// A credential written before the host was recorded carries no annotation, and
// refusing those would strand every app deployed before it existed.
func TestClaimRecordsTheHostOnAnObjectThatHasNone(t *testing.T) {
	live := credential("the-real-token")

	if err := Claim(live, "shop", "the-real-token", "github.com", appRef("app-uid"), claimedAt); err != nil {
		t.Fatalf("refused a credential with no recorded host: %v", err)
	}
	if live.Annotations[labels.AnnoGitAuthority] != "github.com" {
		t.Errorf("recorded host = %q", live.Annotations[labels.AnnoGitAuthority])
	}
}
