package handlers

import (
	"context"
	"strings"
	"testing"

	"net/http"
	"net/http/httptest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// An app named web on the name generated before digests keeps its git token in
// web-git-credentials, and a service named web-git would keep its credentials in
// the same object. Whichever exists, the other kind reads it and finds the wrong
// keys. Both names are published, so the only move left is refusing the second.
func TestServiceNameThatWouldShareAnAppsGitCredential(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod"},
		Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{
			URL: "https://git.example.com/acme/web.git", Branch: "main",
			CredentialsSecret: secretname.LegacyGitCredential("web"),
		}},
	}
	c := testCRClient(app)

	err := refuseServiceNameWhoseCredentialIsTaken(context.Background(), c, "shop-prod", "web-git")

	if err == nil {
		t.Fatal("a service was allowed to take the object an app keeps its token in")
	}
	if got := secretname.ServiceCredentials("web-git"); !strings.Contains(err.Error(), got) {
		t.Errorf("the refusal does not name the object at stake: %v", err)
	}
}

// The same name in a namespace where no such app exists is just a name.
func TestServiceNameIsFreeWhenNoSuchAppExists(t *testing.T) {
	c := testCRClient()

	if err := refuseServiceNameWhoseCredentialIsTaken(context.Background(), c, "shop-prod", "web-git"); err != nil {
		t.Errorf("a name nothing collides with was refused: %v", err)
	}
}

// Nearly every service name cannot share an app's credential at all, and none of
// them should cost a read of that app to find out. They do cost one read of the
// object they would take, which is the question that applies to every name.
func TestOrdinaryServiceNamesSkipTheAppLookup(t *testing.T) {
	c := testCRClient(&kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod"},
		Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{
			CredentialsSecret: secretname.LegacyGitCredential("web"),
		}},
	})

	for _, name := range []string{"database", "cache", "web", "web-gitlab"} {
		if err := refuseServiceNameWhoseCredentialIsTaken(context.Background(), c, "shop-prod", name); err != nil {
			t.Errorf("%q was refused: %v", name, err)
		}
	}
}

// The app existing is not the collision. An app on a digest-named credential has
// nothing at the object the service would take, and no writer can put anything
// there, so the name is free.
func TestServiceNameIsFreeWhenTheAppIsOnADigestCredential(t *testing.T) {
	digest := secretname.GitCredential("web", secretname.GitCredentialDigest("a-token", "git.example.com"))
	c := testCRClient(&kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Git: &kipperv1.AppGitSource{CredentialsSecret: digest}},
	})

	if err := refuseServiceNameWhoseCredentialIsTaken(context.Background(), c, "shop-prod", "web-git"); err != nil {
		t.Errorf("a name nothing collides with was refused: %v", err)
	}
}

func TestServiceNameIsFreeWhenTheAppHasNoGitSource(t *testing.T) {
	c := testCRClient(&kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod"}})

	if err := refuseServiceNameWhoseCredentialIsTaken(context.Background(), c, "shop-prod", "web-git"); err != nil {
		t.Errorf("an app with no token was treated as owning the object: %v", err)
	}
}

// The app rotating onto a digest name frees the reference, not the object. The
// sweep that removes the Secret runs on a delay and leaves it alone entirely
// where something else has a claim on it, and the service reconciler adopts
// nothing on the strength of a name, so a service created here reports
// SecretNotOwned for good.
func TestServiceNameWhoseCredentialIsStillThere(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod"},
		Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{
			URL: "https://git.example.com/acme/web.git", Branch: "main",
			CredentialsSecret: secretname.GitCredential("web", secretname.GitCredentialDigest("t", "git.example.com")),
		}},
	}
	leftover, holder := heldSecret(secretname.ServiceCredentials("web-git"))
	c := testCRClient(app, leftover, holder)

	err := refuseServiceNameWhoseCredentialIsTaken(context.Background(), c, "shop-prod", "web-git")

	var taken *serviceNameTakenError
	require.ErrorAs(t, err, &taken, "the service would have been created onto a Secret it can never own")
	assert.Contains(t, err.Error(), secretname.ServiceCredentials("web-git"))
}

// The name an app once used is one way for the object to be occupied, not the
// only one. A restore, or a service deleted while its Secret stayed, leaves the
// same object in the way, whatever the service is called.
func TestServiceNameWhoseCredentialExistsAlready(t *testing.T) {
	leftover, holder := heldSecret(secretname.ServiceCredentials("cache"))
	c := testCRClient(leftover, holder)

	err := refuseServiceNameWhoseCredentialIsTaken(context.Background(), c, "shop-prod", "cache")

	var taken *serviceNameTakenError
	require.ErrorAs(t, err, &taken, "the service would have been created onto a Secret it can never own")
	assert.Contains(t, err.Error(), secretname.ServiceCredentials("cache"))
}

// The collision-shaped name with no app at all: the app check clears it, and the
// object it left behind is still in the way.
func TestServiceNameWhoseAppIsGoneButCredentialIsNot(t *testing.T) {
	leftover, holder := heldSecret(secretname.ServiceCredentials("web-git"))
	c := testCRClient(leftover, holder)

	err := refuseServiceNameWhoseCredentialIsTaken(context.Background(), c, "shop-prod", "web-git")

	require.Error(t, err, "no app holds the name, but the Secret it left behind still does")
}

// A Secret a live controller holds is one no repair claims away, so the name is
// the only thing left to change. The owner comes back with it: a reference alone
// proves nothing, since it outlives the object it names.
func heldSecret(name string) (*corev1.Secret, *kipperv1.App) {
	const holder, uid = "somebody-else", "another-object"
	owner := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: holder, Namespace: "shop-prod", UID: uid},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "shop-prod",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "App",
				Name: holder, UID: uid, Controller: ptr.To(true),
			}},
		},
	}
	return secret, owner
}

// A reference outlives the object it names: garbage collection is not instant,
// and a restore brings a dependent back whose owner returned under a new UID.
// Reading the reference alone would take both for a live claim, and the service
// could not be created to point that Secret at, which is what the reconciler
// asks for and what keeps the password the volume was written under.
func TestServiceNameWhoseCredentialHasAnOwnerThatIsGone(t *testing.T) {
	orphan, _ := heldSecret(secretname.ServiceCredentials("cache"))
	c := testCRClient(orphan)

	require.NoError(t, refuseServiceNameWhoseCredentialIsTaken(context.Background(), c, "shop-prod", "cache"),
		"a reference to an object that is gone was read as a live claim on the name")
}

// The service losing a create race has its own Secret minted by the winner's
// reconciler before it looks. That is not another object holding the name: the
// service exists, and saying so is the existence check's job.
func TestServiceNameWhoseCredentialItsOwnServiceOwns(t *testing.T) {
	live := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "shop-prod", UID: "the-service"},
		Spec:       kipperv1.ServiceSpec{Type: "redis"},
	}
	own := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretname.ServiceCredentials("cache"), Namespace: "shop-prod",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
				Name: "cache", UID: "the-service", Controller: ptr.To(true),
			}},
		},
	}
	c := testCRClient(live, own)

	require.NoError(t, refuseServiceNameWhoseCredentialIsTaken(context.Background(), c, "shop-prod", "cache"),
		"the loser of a create race was told its own credentials belong to somebody else")
}

// An unowned Secret is the one `kip service credentials --repair` gives back to
// its service, which is how a password reaches the volume it was written under.
// Refusing the name would leave deleting the Secret as the only way through.
func TestServiceNameWhoseCredentialIsUnowned(t *testing.T) {
	orphan := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretname.ServiceCredentials("cache"), Namespace: "shop-prod",
		},
	}
	c := testCRClient(orphan)

	require.NoError(t, refuseServiceNameWhoseCredentialIsTaken(context.Background(), c, "shop-prod", "cache"),
		"the repair path was blocked, and with it the only way to keep the password")
}

// Creating a service that is already there is answered by saying so. The
// credential guard's advice is about an object somebody else holds, and a
// healthy service owns its own credentials Secret, so running the guard first
// answered a duplicate create by naming a running service's credentials and
// suggesting they be removed. The CLI has always asked in this order.
func TestServiceCreate_SaysTheServiceExistsRatherThanBlamingItsCredentials(t *testing.T) {
	live := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-git", Namespace: "shop-prod", UID: "the-service"},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "shop-prod"},
		Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{
			URL: "https://git.example.com/acme/shop.git", Branch: "main",
			CredentialsSecret: secretname.GitCredential("shop", secretname.GitCredentialDigest("t", "git.example.com")),
		}},
	}
	handler := &Services{
		Client:   k8sfake.NewClientset(),
		CRClient: testCRClient(live, app, ownSecret("shop-git", live.UID)),
	}

	rec := httptest.NewRecorder()
	handler.Create(rec, httptest.NewRequest(http.MethodPost, "/services",
		strings.NewReader(`{"name":"shop-git","type":"postgres","namespace":"shop-prod"}`)))

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "already exists")
	assert.NotContains(t, rec.Body.String(), secretname.ServiceCredentials("shop-git"),
		"an operator was pointed at a running service's credentials and told to remove them")
}

// The credentials Secret a live service already holds.
func ownSecret(service string, uid types.UID) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretname.ServiceCredentials(service), Namespace: "shop-prod",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
				Name: service, UID: uid, Controller: ptr.To(true),
			}},
		},
	}
}

// Service is a core kind as well as one of ours, and a Secret held by a core one
// is held by something this check cannot look up. Matching on the kind alone
// would look for a kipper.run Service, get not-found, and read a live claim as a
// lapsed one, which is how a service gets created onto a name it can never have.
func TestServiceNameWhoseCredentialAKindWeDoNotOwnHolds(t *testing.T) {
	held := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretname.ServiceCredentials("cache"), Namespace: "shop-prod",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1", Kind: "Service",
				Name: "cache", UID: "somebody-elses-service", Controller: ptr.To(true),
			}},
		},
	}
	c := testCRClient(held)

	err := refuseServiceNameWhoseCredentialIsTaken(context.Background(), c, "shop-prod", "cache")

	require.Error(t, err, "a claim this check cannot disprove was treated as lapsed")
}

// The owner being there is not the owner still holding it. A restore brings both
// objects back and the owner comes back under a new UID, so the reference names
// something that exists and points at an identity that is gone. Garbage
// collection reads it the same way and will remove the Secret; refusing the name
// on it would take away the window an operator has to act.
func TestServiceNameWhoseCredentialsOwnerCameBackWithANewIdentity(t *testing.T) {
	restored, holder := heldSecret(secretname.ServiceCredentials("cache"))
	holder.UID = "came-back-different"
	c := testCRClient(restored, holder)

	require.NoError(t, refuseServiceNameWhoseCredentialIsTaken(context.Background(), c, "shop-prod", "cache"),
		"a reference to an identity that is gone was read as a live claim on the name")
}
