package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

// The CLI refuses what the console refuses, for the same reason and through the
// same predicate: an app named web on the pre-digest name keeps its git token in
// web-git-credentials, which is exactly where a service named web-git would keep
// its credentials.
func TestServiceAddRefusesANameThatSharesAnAppsGitCredential(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", secretname.LegacyGitCredential("web")))

	err := refuseServiceNameWhoseCredentialIsTaken(context.Background(), dyn, "shop-prod", "web-git")

	require.Error(t, err)
	assert.Contains(t, err.Error(), secretname.ServiceCredentials("web-git"))
	assert.Contains(t, err.Error(), "web")
}

func TestServiceAddAllowsTheNameWhenNoSuchAppExists(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme())

	require.NoError(t, refuseServiceNameWhoseCredentialIsTaken(context.Background(), dyn, "shop-prod", "web-git"))
}

func TestServiceAddAllowsANameWithNothingInItsWay(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", secretname.LegacyGitCredential("web")))

	for _, name := range []string{"database", "cache", "web", "web-gitlab"} {
		require.NoError(t, refuseServiceNameWhoseCredentialIsTaken(context.Background(), dyn, "shop-prod", name),
			"%q was refused", name)
	}
}

// A manifest is a create path too, and the comment in applyResource says so: it
// is the one place that reaches every kind, so a kipper.yaml must not be able to
// create what `kip service add` and the console both refuse.
func TestApplyRefusesAServiceNameThatSharesAnAppsGitCredential(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", secretname.LegacyGitCredential("web")))
	res := manifest.Resource{
		GVR: manifest.ServiceGVR,
		Object: &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": manifest.ServiceGVR.GroupVersion().String(),
			"kind":       "Service",
			"metadata":   map[string]any{"name": "web-git", "namespace": "shop-prod"},
			"spec":       map[string]any{"type": "postgres"},
		}},
	}

	_, err := applyResource(context.Background(), dyn, "shop-prod", res, false, nil, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), secretname.ServiceCredentials("web-git"))
}

// The app existing is not the collision. An app on a digest-named credential has
// nothing at the object the service would take, and no writer can put anything
// there, so the name is free.
func TestServiceAddAllowsTheNameWhenTheAppIsOnADigestCredential(t *testing.T) {
	digest := secretname.GitCredential("web", secretname.GitCredentialDigest("a-token", "git.example.com"))
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", digest))

	require.NoError(t, refuseServiceNameWhoseCredentialIsTaken(context.Background(), dyn, "shop-prod", "web-git"))
}

// And an app with no git source at all has no token to collide with.
func TestServiceAddAllowsTheNameWhenTheAppHasNoGitSource(t *testing.T) {
	app := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": deployer.AppGVR.GroupVersion().String(),
		"kind":       "App",
		"metadata":   map[string]any{"name": "web", "namespace": "shop-prod"},
		"spec":       map[string]any{"image": "nginx", "port": int64(80)},
	}}
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), app)

	require.NoError(t, refuseServiceNameWhoseCredentialIsTaken(context.Background(), dyn, "shop-prod", "web-git"))
}

// An app moving off the older credential name does not take the Secret with it:
// the sweep that removes it runs on a delay, and it leaves the object alone
// where something else has a claim on it. A service created onto that Secret
// reports SecretNotOwned and stays there, so the name is refused while there is
// still a choice of name.
func TestServiceAddRefusesANameWhoseCredentialIsStillThere(t *testing.T) {
	scheme := schemeWithSecrets()
	leftover := credentialSecret("web-git", holdingApp())
	// The app has rotated onto a digest name, so the reference check clears it.
	dyn := dynamicfake.NewSimpleDynamicClient(scheme,
		gitApp("shop-prod", secretname.GitCredential("web", secretname.GitCredentialDigest("t", "git.example.com"))),
		leftover, holdingApp())

	err := refuseServiceNameWhoseCredentialIsTaken(context.Background(), dyn, "shop-prod", "web-git")

	require.Error(t, err, "the service would have been created onto a Secret it can never own")
	assert.Contains(t, err.Error(), secretname.ServiceCredentials("web-git"))
}

func schemeWithSecrets() *runtime.Scheme {
	scheme := appScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Version: "v1", Kind: "SecretList"}, &unstructured.UnstructuredList{})
	return scheme
}

// A Secret a live controller holds is one no repair claims away, so the name is
// the only thing left to change. Without one it is what `kip service credentials
// --repair` gives back to its service.
func credentialSecret(service string, owner *unstructured.Unstructured) *unstructured.Unstructured {
	meta := map[string]interface{}{
		"name": secretname.ServiceCredentials(service), "namespace": "shop-prod",
	}
	if owner != nil {
		meta["ownerReferences"] = []interface{}{map[string]interface{}{
			"apiVersion": owner.GetAPIVersion(), "kind": owner.GetKind(),
			"name": owner.GetName(), "uid": string(owner.GetUID()), "controller": true,
		}}
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "Secret", "metadata": meta,
	}}
}

// The owner comes back with the Secret, because a reference alone proves
// nothing: it outlives the object it names.
func holdingApp() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata": map[string]interface{}{
			"name": "somebody-else", "namespace": "shop-prod", "uid": "another-object",
		},
	}}
}

// A reference outlives the object it names: garbage collection is not instant,
// and a restore brings a dependent back whose owner returned under a new UID.
// Reading the reference alone would take both for a live claim on the name.
func TestServiceAddAllowsANameWhoseCredentialHasAnOwnerThatIsGone(t *testing.T) {
	orphan := credentialSecret("cache", holdingApp())
	dyn := dynamicfake.NewSimpleDynamicClient(schemeWithSecrets(), orphan)

	require.NoError(t, refuseServiceNameWhoseCredentialIsTaken(context.Background(), dyn, "shop-prod", "cache"),
		"a reference to an object that is gone was read as a live claim on the name")
}

// The service losing a create race has its own Secret minted by the winner's
// reconciler before it looks. That is not another object holding the name.
func TestServiceAddAllowsANameWhoseCredentialItsOwnServiceOwns(t *testing.T) {
	live := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "Service",
		"metadata": map[string]interface{}{
			"name": "cache", "namespace": "shop-prod", "uid": "the-service",
		},
	}}
	dyn := dynamicfake.NewSimpleDynamicClient(schemeWithSecrets(), live, credentialSecret("cache", live))

	require.NoError(t, refuseServiceNameWhoseCredentialIsTaken(context.Background(), dyn, "shop-prod", "cache"),
		"the loser of a create race was told its own credentials belong to somebody else")
}

// Refusing an unowned Secret would block the repair path, and with it the only
// way to keep a password the volume was written under.
func TestServiceAddAllowsANameWhoseCredentialIsUnowned(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(schemeWithSecrets(), credentialSecret("cache", nil))

	require.NoError(t, refuseServiceNameWhoseCredentialIsTaken(context.Background(), dyn, "shop-prod", "cache"))
}

// The name an app once used is one way for the object to be occupied, not the
// only one. A restore, or a service deleted while its Secret stayed, leaves the
// same object in the way, and the reconciler refuses it for the same reason.
func TestServiceAddRefusesAnOrdinaryNameWhoseCredentialExists(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(schemeWithSecrets(), credentialSecret("cache", holdingApp()), holdingApp())

	err := refuseServiceNameWhoseCredentialIsTaken(context.Background(), dyn, "shop-prod", "cache")

	require.Error(t, err, "the service would have been created onto a Secret it can never own")
	assert.Contains(t, err.Error(), secretname.ServiceCredentials("cache"))
}

// The collision-shaped name with no app at all: the app check clears it, and the
// object is still there.
func TestServiceAddRefusesACollidingNameWhoseAppIsGone(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(schemeWithSecrets(), credentialSecret("web-git", holdingApp()), holdingApp())

	err := refuseServiceNameWhoseCredentialIsTaken(context.Background(), dyn, "shop-prod", "web-git")

	require.Error(t, err, "no app holds the name, but the Secret it left behind still does")
}

// Service is a core kind as well as one of ours, and a Secret held by a core one
// is held by something this check cannot look up. Matching on the kind alone
// would look for a kipper.run Service, get not-found, and read a live claim as a
// lapsed one.
func TestServiceAddRefusesANameHeldByAKindWeDoNotOwn(t *testing.T) {
	held := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]interface{}{
			"name": secretname.ServiceCredentials("cache"), "namespace": "shop-prod",
			"ownerReferences": []interface{}{map[string]interface{}{
				"apiVersion": "v1", "kind": "Service",
				"name": "cache", "uid": "somebody-elses-service", "controller": true,
			}},
		},
	}}
	dyn := dynamicfake.NewSimpleDynamicClient(schemeWithSecrets(), held)

	err := refuseServiceNameWhoseCredentialIsTaken(context.Background(), dyn, "shop-prod", "cache")

	require.Error(t, err, "a claim this check cannot disprove was treated as lapsed")
}

// The owner being there is not the owner still holding it. A restore brings both
// objects back and the owner comes back under a new UID, so the reference names
// something that exists and points at an identity that is gone.
func TestServiceAddAllowsANameWhoseOwnerCameBackWithANewIdentity(t *testing.T) {
	holder := holdingApp()
	restored := credentialSecret("cache", holder)
	holder.SetUID("came-back-different")
	dyn := dynamicfake.NewSimpleDynamicClient(schemeWithSecrets(), restored, holder)

	require.NoError(t, refuseServiceNameWhoseCredentialIsTaken(context.Background(), dyn, "shop-prod", "cache"),
		"a reference to an identity that is gone was read as a live claim on the name")
}
