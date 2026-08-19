package controllers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func appWithoutGit() *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test", UID: types.UID("app-uid")},
		Spec:       kipperv1.AppSpec{Image: "registry.example.com/shop/checkout:9f2c1a", Port: 8080},
	}
}

func gitCredential(labels map[string]string, owner *metav1.OwnerReference) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "checkout-git-credentials",
			Namespace: "shop-test",
			Labels:    labels,
		},
		Data: map[string][]byte{"token": []byte("placeholder")},
	}
	if owner != nil {
		secret.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return secret
}

func kipperOwned() *metav1.OwnerReference {
	controller := true
	return &metav1.OwnerReference{
		APIVersion: "kipper.run/v1alpha1", Kind: "App", Name: "checkout",
		UID: types.UID("app-uid"), Controller: &controller,
	}
}

func writerLabels() map[string]string {
	return map[string]string{kipperLabel: kipperValue, "kipper.run/app": "checkout"}
}

func gitCredentialSurvives(t *testing.T, r *AppReconciler) bool {
	t.Helper()
	const name = "checkout-git-credentials"
	var secret corev1.Secret
	err := r.Get(t.Context(), types.NamespacedName{Name: name, Namespace: "shop-test"}, &secret)
	if errors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return true
}

// Removing a git source leaves the token behind otherwise: adoption ties the
// Secret's life to the App rather than to the source, so it would sit in the
// namespace in plaintext with nothing referencing it.
func TestDetachingGitRemovesTheCredentialItAdopted(t *testing.T) {
	app := appWithoutGit()
	secret := gitCredential(writerLabels(), kipperOwned())
	scheme := testScheme()
	r := &AppReconciler{Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, secret).Build(), Scheme: scheme}

	if err := r.sweepDetachedGitCredential(t.Context(), app); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if gitCredentialSurvives(t, r) {
		t.Error("the token outlived the source it belonged to")
	}
}

// An app still building from git keeps its credential, however many times the
// reconciler runs.
func TestAnAppStillBuildingFromGitKeepsItsCredential(t *testing.T) {
	app := appWithoutGit()
	app.Spec.Git = &kipperv1.AppGitSource{ //nolint:gosec // G101 false positive: credentialsSecret is a K8s Secret name, not a credential value
		URL:               "https://git.example.com/shop/checkout.git",
		CredentialsSecret: "checkout-git-credentials",
	}
	secret := gitCredential(writerLabels(), kipperOwned())
	scheme := testScheme()
	r := &AppReconciler{Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, secret).Build(), Scheme: scheme}

	if err := r.sweepDetachedGitCredential(t.Context(), app); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if !gitCredentialSurvives(t, r) {
		t.Error("a configured git source lost the credential it builds with")
	}
}

// A Secret under the conventional name that nobody labelled is somebody
// else's: names are namespace-global, so a collision must not turn another
// object into this app's to delete.
func TestDetachingGitLeavesAnUnlabelledSecretAlone(t *testing.T) {
	app := appWithoutGit()
	secret := gitCredential(nil, nil)
	scheme := testScheme()
	r := &AppReconciler{Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, secret).Build(), Scheme: scheme}

	if err := r.sweepDetachedGitCredential(t.Context(), app); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if !gitCredentialSurvives(t, r) {
		t.Error("a foreign object sharing the conventional name was deleted")
	}
}

// A Secret another controller owns is not this app's to remove, whatever it is
// called and whatever labels it carries.
func TestDetachingGitLeavesASecretAnotherOwnerControls(t *testing.T) {
	app := appWithoutGit()
	controller := true
	foreign := &metav1.OwnerReference{
		APIVersion: "apps/v1", Kind: "Deployment", Name: "other",
		UID: types.UID("other-uid"), Controller: &controller,
	}
	secret := gitCredential(writerLabels(), foreign)
	scheme := testScheme()
	r := &AppReconciler{Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, secret).Build(), Scheme: scheme}

	if err := r.sweepDetachedGitCredential(t.Context(), app); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if !gitCredentialSurvives(t, r) {
		t.Error("a Secret owned by something else was deleted")
	}
}

// The sweep runs on every pass, so it has to be safe when there is nothing to
// remove — a partial failure is retried by the next reconcile rather than
// leaving the app wedged.
func TestDetachingGitIsSafeWhenThereIsNoCredential(t *testing.T) {
	app := appWithoutGit()
	scheme := testScheme()
	r := &AppReconciler{Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).Build(), Scheme: scheme}

	for range 3 {
		if err := r.sweepDetachedGitCredential(t.Context(), app); err != nil {
			t.Fatalf("sweep: %v", err)
		}
	}
}

// A build status left over from the source that has gone reports a failure for
// an app that no longer builds, which is what the console showed after the
// live incident.
func TestDetachingGitClearsTheBuildStatusItLeftBehind(t *testing.T) {
	app := appWithoutGit()
	app.Status.Build = &kipperv1.AppBuildStatus{Phase: "Failed", Commit: "9f2c1a", Message: "Job has reached the specified backoff limit"}

	if !detachedBuildStatus(app) {
		t.Fatal("a build result belonging to a source that is gone was not recognised as stale")
	}
}

func TestABuildStatusSurvivesWhileTheSourceDoes(t *testing.T) {
	app := appWithoutGit()
	app.Spec.Git = &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git"}
	app.Status.Build = &kipperv1.AppBuildStatus{Phase: "Succeeded", Commit: "9f2c1a"}

	if detachedBuildStatus(app) {
		t.Error("a live source's build status was treated as stale")
	}
}

// The seam, not the helper. Both cleanup calls could be deleted from Reconcile
// and every test above would stay green, which is the same gap that let the
// original defects ship: a test and the production path being separate
// consumers of one helper. This drives Reconcile, so removing either line
// fails here.
func TestReconcileSweepsAfterAGitSourceIsDetached(t *testing.T) {
	scheme := testScheme()
	app := appWithoutGit()
	app.Status.Build = &kipperv1.AppBuildStatus{
		Phase: "Failed", Commit: "9f2c1a", Message: "Job has reached the specified backoff limit",
	}
	secret := gitCredential(writerLabels(), kipperOwned())

	client := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(withWorld(app, secret)...).
		WithStatusSubresource(app).
		Build()
	r := &AppReconciler{Client: client, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "checkout", Namespace: "shop-test"},
	})
	require.NoError(t, err)

	var stranded corev1.Secret
	getErr := r.Get(context.Background(),
		types.NamespacedName{Name: "checkout-git-credentials", Namespace: "shop-test"}, &stranded)
	assert.True(t, errors.IsNotFound(getErr),
		"a reconcile of a detached app leaves the plaintext token in the namespace")

	var after kipperv1.App
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "checkout", Namespace: "shop-test"}, &after))
	assert.Nil(t, after.Status.Build,
		"a reconcile of a detached app keeps reporting a build that no longer exists")
}

// The window between detaching a source and the sweep running is real: the
// controller can be restarted in it. Deleting the app then took the only
// reference to the token with it, and the sweep that runs on deletion skipped
// the Secret because there was no longer a source naming it — leaving plaintext
// in the namespace with no owner, forever.
func TestDeletingADetachedAppStillSweepsItsToken(t *testing.T) {
	scheme := testScheme()
	app := appWithoutGit()
	secret := gitCredential(writerLabels(), kipperOwned())
	r := &AppReconciler{Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, secret).Build(), Scheme: scheme}

	if err := r.sweepWriterSecrets(t.Context(), app); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if gitCredentialSurvives(t, r) {
		t.Error("the token outlived the app that owned it, with nothing left to reference or remove it")
	}
}

// The guards that make the unconditional sweep safe: a Secret under the same
// name that nobody labelled is somebody else's.
func TestDeletingAnAppLeavesAnUnlabelledSecretOfTheSameName(t *testing.T) {
	scheme := testScheme()
	app := appWithoutGit()
	secret := gitCredential(nil, nil)
	r := &AppReconciler{Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, secret).Build(), Scheme: scheme}

	if err := r.sweepWriterSecrets(t.Context(), app); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if !gitCredentialSurvives(t, r) {
		t.Error("a foreign object sharing the conventional name was deleted")
	}
}

// The ordering that matters. An app with a fault of its own fails the same
// step on every pass, so cleanup placed after that step is never reached: the
// token whose source was removed would sit in the namespace in plaintext for
// as long as the unrelated fault lasts, which can be indefinitely.
func TestCleanupSurvivesAnUnrelatedReconcileFailure(t *testing.T) {
	scheme := testScheme()
	app := appWithoutGit()
	// A service binding to a service that does not exist fails this app's
	// reconcile every time, well before the old position of the sweep.
	app.Spec.ServiceBindings = []kipperv1.ServiceBinding{{Name: "nowhere", Prefix: "DB_"}}
	app.Status.Build = &kipperv1.AppBuildStatus{Phase: "Failed", Commit: "9f2c1a"}
	secret := gitCredential(writerLabels(), kipperOwned())

	client := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(withWorld(app, secret)...).
		WithStatusSubresource(app).
		Build()
	r := &AppReconciler{Client: client, Scheme: scheme}

	// The pass is expected to fail. What matters is what it did before failing.
	_, _ = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "checkout", Namespace: "shop-test"},
	})

	var stranded corev1.Secret
	getErr := r.Get(context.Background(),
		types.NamespacedName{Name: "checkout-git-credentials", Namespace: "shop-test"}, &stranded)
	assert.True(t, errors.IsNotFound(getErr),
		"an app with an unrelated fault keeps its detached token forever, because cleanup never runs")

	var after kipperv1.App
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "checkout", Namespace: "shop-test"}, &after))
	assert.Nil(t, after.Status.Build, "and goes on reporting a build it no longer has")
}

// The name claim is the earliest step that can fail persistently, and an App
// written straight to the Kubernetes API can arrive holding a name another
// workload already owns. Cleanup ordered after it would never run for as long
// as the collision lasted.
func TestCleanupSurvivesANameHeldByAnotherWorkload(t *testing.T) {
	scheme := testScheme()
	app := appWithoutGit()
	app.Status.Build = &kipperv1.AppBuildStatus{Phase: "Failed", Commit: "9f2c1a"}
	secret := gitCredential(writerLabels(), kipperOwned())
	// A Function of the same name in the same namespace is what the claim
	// refuses against.
	rival := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test", UID: types.UID("fn-uid")},
	}

	client := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(withWorld(app, secret, rival)...).
		WithStatusSubresource(app).
		Build()
	r := &AppReconciler{Client: client, Scheme: scheme}

	_, _ = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "checkout", Namespace: "shop-test"},
	})

	var stranded corev1.Secret
	getErr := r.Get(context.Background(),
		types.NamespacedName{Name: "checkout-git-credentials", Namespace: "shop-test"}, &stranded)
	assert.True(t, errors.IsNotFound(getErr),
		"an app blocked at the name claim keeps its detached token for as long as the collision lasts")
}
