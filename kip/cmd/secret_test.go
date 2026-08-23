package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

func TestSaveSecret(t *testing.T) {
	ctx := context.Background()

	t.Run("creates the secret with kipper labels", func(t *testing.T) {
		cs := k8sfake.NewSimpleClientset()
		err := saveSecret(ctx, cs, secretname.KindApp, "blog-test", "api", map[string][]byte{"API_KEY": []byte("abc")})
		require.NoError(t, err)

		secret, err := cs.CoreV1().Secrets("blog-test").Get(ctx, "app-api-secrets", metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, []byte("abc"), secret.Data["API_KEY"])
		assert.Equal(t, "api", secret.Labels["app"])
		assert.Equal(t, "kipper", secret.Labels["app.kubernetes.io/managed-by"])
	})

	t.Run("preserves the previous value on overwrite", func(t *testing.T) {
		cs := k8sfake.NewSimpleClientset()
		require.NoError(t, saveSecret(ctx, cs, secretname.KindApp, "blog-test", "api", map[string][]byte{"API_KEY": []byte("old")}))
		require.NoError(t, saveSecret(ctx, cs, secretname.KindApp, "blog-test", "api", map[string][]byte{"API_KEY": []byte("new")}))

		secret, err := cs.CoreV1().Secrets("blog-test").Get(ctx, "app-api-secrets", metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, []byte("new"), secret.Data["API_KEY"])
		assert.Equal(t, []byte("old"), secret.Data["API_KEY"+previousSuffix])
	})

	t.Run("merges with keys it was not given", func(t *testing.T) {
		cs := k8sfake.NewSimpleClientset()
		require.NoError(t, saveSecret(ctx, cs, secretname.KindApp, "blog-test", "api", map[string][]byte{"API_KEY": []byte("abc")}))
		require.NoError(t, saveSecret(ctx, cs, secretname.KindApp, "blog-test", "api", map[string][]byte{"DB_PASSWORD": []byte("hunter2")}))

		secret, err := cs.CoreV1().Secrets("blog-test").Get(ctx, "app-api-secrets", metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, []byte("abc"), secret.Data["API_KEY"], "an untouched key must survive a later set")
		assert.Equal(t, []byte("hunter2"), secret.Data["DB_PASSWORD"])
	})

	t.Run("preserves ownerReferences on update", func(t *testing.T) {
		cs := k8sfake.NewSimpleClientset()
		require.NoError(t, saveSecret(ctx, cs, secretname.KindApp, "blog-test", "api", map[string][]byte{"API_KEY": []byte("old")}))

		// Simulate the reconciler adopting the Secret between two writes.
		secret, err := cs.CoreV1().Secrets("blog-test").Get(ctx, "app-api-secrets", metav1.GetOptions{})
		require.NoError(t, err)
		controller := true
		secret.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "kipper.run/v1alpha1", Kind: "App", Name: "api", UID: "app-uid", Controller: &controller,
		}}
		_, err = cs.CoreV1().Secrets("blog-test").Update(ctx, secret, metav1.UpdateOptions{})
		require.NoError(t, err)

		require.NoError(t, saveSecret(ctx, cs, secretname.KindApp, "blog-test", "api", map[string][]byte{"API_KEY": []byte("new")}))

		secret, err = cs.CoreV1().Secrets("blog-test").Get(ctx, "app-api-secrets", metav1.GetOptions{})
		require.NoError(t, err)
		require.Len(t, secret.OwnerReferences, 1, "a secret write must not detach the App's controller reference, or the Secret outlives the App")
		assert.Equal(t, []byte("new"), secret.Data["API_KEY"])
	})

	t.Run("unchanged value records no previous", func(t *testing.T) {
		cs := k8sfake.NewSimpleClientset()
		require.NoError(t, saveSecret(ctx, cs, secretname.KindApp, "blog-test", "api", map[string][]byte{"API_KEY": []byte("same")}))
		require.NoError(t, saveSecret(ctx, cs, secretname.KindApp, "blog-test", "api", map[string][]byte{"API_KEY": []byte("same")}))

		secret, err := cs.CoreV1().Secrets("blog-test").Get(ctx, "app-api-secrets", metav1.GetOptions{})
		require.NoError(t, err)
		_, hasPrevious := secret.Data["API_KEY"+previousSuffix]
		assert.False(t, hasPrevious, "setting the same value again must not create a bogus previous version")
	})
}

func appScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: "App"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: "AppList"},
		&unstructured.UnstructuredList{},
	)
	// Projects too, because the upgrade paths that read apps also read which
	// project holds the namespace each app is in.
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: "Project"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: "ProjectList"},
		&unstructured.UnstructuredList{},
	)
	return scheme
}

func appCR(name, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
	}}
}

func secretFixture(name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "blog-test"}}
}

func TestCleanupDeploySecrets(t *testing.T) {
	ctx := context.Background()

	t.Run("deletes secrets this deploy created when no App CR exists", func(t *testing.T) {
		cs := k8sfake.NewSimpleClientset(
			secretFixture("app-api-secrets"),
			secretFixture("api-git-credentials"),
		)
		d := &deployer.Deployer{Client: cs, Dynamic: dynamicfake.NewSimpleDynamicClient(appScheme())}

		cleanupDeploySecrets(ctx, cs, d, "blog-test", "api", "api-git-credentials", "", true, true)

		_, err := cs.CoreV1().Secrets("blog-test").Get(ctx, "app-api-secrets", metav1.GetOptions{})
		assert.True(t, apierrors.IsNotFound(err), "a failed deploy must not leave the secrets it created behind")
		_, err = cs.CoreV1().Secrets("blog-test").Get(ctx, "api-git-credentials", metav1.GetOptions{})
		assert.True(t, apierrors.IsNotFound(err), "a failed deploy must not leave the git credentials it created behind")
	})

	t.Run("keeps secrets when the App CR was created despite the error", func(t *testing.T) {
		cs := k8sfake.NewSimpleClientset(secretFixture("app-api-secrets"))
		d := &deployer.Deployer{Client: cs, Dynamic: dynamicfake.NewSimpleDynamicClient(appScheme(), appCR("api", "blog-test"))}

		cleanupDeploySecrets(ctx, cs, d, "blog-test", "api", "api-git-credentials", "", true, false)

		_, err := cs.CoreV1().Secrets("blog-test").Get(ctx, "app-api-secrets", metav1.GetOptions{})
		assert.NoError(t, err, "an existing App CR will adopt the Secret; it must not be deleted")
	})

	t.Run("keeps secrets that pre-existed the deploy", func(t *testing.T) {
		cs := k8sfake.NewSimpleClientset(secretFixture("app-api-secrets"))
		d := &deployer.Deployer{Client: cs, Dynamic: dynamicfake.NewSimpleDynamicClient(appScheme())}

		cleanupDeploySecrets(ctx, cs, d, "blog-test", "api", "api-git-credentials", "", false, false)

		_, err := cs.CoreV1().Secrets("blog-test").Get(ctx, "app-api-secrets", metav1.GetOptions{})
		assert.NoError(t, err, "secrets from an earlier deploy or another writer are not this invocation's to delete")
	})
}

// A secret change answers to the same rule as an env change: it is saved, and
// the pods keep what they started with until something restarts them. Rotating
// a credential and having every connection drop as a side effect is the
// surprise, and the console has never behaved that way.
func TestSecretSet_SavesWithoutRestartingByDefault(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "default", "api", nil)
	cs := k8sfake.NewSimpleClientset()
	withWorkloadClients(t, cs, dyn)

	out := captureStdout(t, func() {
		require.NoError(t, runSecretSet(secretSetCmd, []string{"api", "DATABASE_PASSWORD=hunter2"}))
	})

	secrets, err := cs.CoreV1().Secrets("default").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	saved := false
	for _, sec := range secrets.Items {
		if string(sec.Data["DATABASE_PASSWORD"]) == "hunter2" {
			saved = true
		}
	}
	assert.True(t, saved, "the secret is saved either way")

	_, _, annotations := specEnv(t, dyn, manifest.AppGVR)
	assert.NotContains(t, annotations, "kipper.run/restartedAt",
		"rotating a credential is not a request to drop every connection")
	assert.Contains(t, out, "--restart")
	assert.NotContains(t, out, "hunter2", "and the value it saved is never printed back")
}

func TestSecretSet_RestartsWhenAskedTo(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "default", "api", nil)
	cs := k8sfake.NewSimpleClientset()
	withWorkloadClients(t, cs, dyn)
	withRestartFlag(t, secretSetCmd)

	require.NoError(t, runSecretSet(secretSetCmd, []string{"api", "DATABASE_PASSWORD=hunter2"}))

	_, _, annotations := specEnv(t, dyn, manifest.AppGVR)
	assert.Contains(t, annotations, "kipper.run/restartedAt")
}

// An explicit --project resolves a namespace without looking anything up, so a
// mistyped name used to write the credential into <kind>-<name>-secrets and report
// success — leaving it in an object nothing owns, for a later workload of that
// name to read.
func TestSecretSet_RefusesAWorkloadThatIsNotThere(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "default", "resize", nil)
	cs := k8sfake.NewSimpleClientset()
	withWorkloadClients(t, cs, dyn)

	err := runSecretSet(secretSetCmd, []string{"reszie", "API_TOKEN=t0ken"})
	require.Error(t, err, "a name that names nothing must not take a credential")
	assert.Contains(t, err.Error(), "reszie")

	secrets, listErr := cs.CoreV1().Secrets("default").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, listErr)
	for _, sec := range secrets.Items {
		assert.NotContains(t, string(sec.Data["API_TOKEN"]), "t0ken",
			"and nothing is written for it, in %s", sec.Name)
	}
}

// A restart that was asked for and did not happen fails the command: exit 0
// would tell automation the new credential is live while the pods still run the
// old one.
func TestSecretSet_FailsWhenAnAskedForRestartDoesNot(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "default", "api", nil)
	cs := k8sfake.NewSimpleClientset()
	dyn.PrependReactor("update", "apps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "kipper.run", Resource: "apps"}, "api", context.DeadlineExceeded)
	})
	withWorkloadClients(t, cs, dyn)
	withRestartFlag(t, secretSetCmd)

	err := runSecretSet(secretSetCmd, []string{"api", "DATABASE_PASSWORD=hunter2"})
	require.Error(t, err, "--restart is an instruction, not a preference")
	assert.Contains(t, err.Error(), "saved", "and it says the value did land")
	assert.Contains(t, err.Error(), "kip app restart api", "and how to finish the job")
	assert.NotContains(t, err.Error(), "hunter2")
}

// Without the flag there is nothing to fail: the change is saved and the pods
// are left alone on purpose.
func TestSecretSet_ARestartFailureIsNotInventedWhenNoneWasAsked(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "default", "api", nil)
	cs := k8sfake.NewSimpleClientset()
	dyn.PrependReactor("update", "apps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "kipper.run", Resource: "apps"}, "api", context.DeadlineExceeded)
	})
	withWorkloadClients(t, cs, dyn)

	require.NoError(t, runSecretSet(secretSetCmd, []string{"api", "DATABASE_PASSWORD=hunter2"}),
		"nothing tried to restart, so nothing failed")
}

// A command aimed at a kind that is not there deletes nothing at all.
func TestSecretDelete_RefusesTheKindItWasAimedAt(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	// An App called api, and no Function of that name.
	seedWorkload(t, dyn, manifest.AppGVR, "App", "default", "api", nil)
	cs := k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-api-secrets", Namespace: "default"},
		Data:       map[string][]byte{"DATABASE_PASSWORD": []byte("current")},
	})
	withWorkloadClients(t, cs, dyn)

	err := runSecretDelete(functionSecretDeleteCmd, []string{"api", "DATABASE_PASSWORD"})
	require.Error(t, err, "there is no function called api, so there is nothing of its to delete")

	after, getErr := cs.CoreV1().Secrets("default").Get(context.Background(), "app-api-secrets", metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Equal(t, []byte("current"), after.Data["DATABASE_PASSWORD"],
		"and the app's credential is the app's")
}

// An App and a Function of one name are two workloads, so their credentials are
// two objects. Both exist here, which is the case the kind guard cannot catch:
// it passes, and before the names carried the kind the write went straight
// through to whichever object was there.
func TestSecretDelete_LeavesTheOtherKindsCredentialAlone(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "default", "api", nil)
	seedWorkload(t, dyn, manifest.FunctionGVR, "Function", "default", "api", nil)
	cs := k8sfake.NewSimpleClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "app-api-secrets", Namespace: "default"},
			Data:       map[string][]byte{"DATABASE_PASSWORD": []byte("the app's")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "function-api-secrets", Namespace: "default"},
			Data:       map[string][]byte{"DATABASE_PASSWORD": []byte("the function's")},
		},
	)
	withWorkloadClients(t, cs, dyn)

	require.NoError(t, runSecretDelete(functionSecretDeleteCmd, []string{"api", "DATABASE_PASSWORD"}))

	app, getErr := cs.CoreV1().Secrets("default").Get(context.Background(), "app-api-secrets", metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Equal(t, []byte("the app's"), app.Data["DATABASE_PASSWORD"],
		"deleting the function's password must not delete the app's")

	fn, getErr := cs.CoreV1().Secrets("default").Get(context.Background(), "function-api-secrets", metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.NotContains(t, fn.Data, "DATABASE_PASSWORD", "and the function's is the one that goes")
}

// Rollback writes too, and an orphan Secret is enough to let it proceed against
// a workload that is not there.
func TestSecretRollback_RefusesAWorkloadThatIsNotThere(t *testing.T) {
	dyn := fakeWorkloadDynamic() // nothing seeded: no App called api
	cs := k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-api-secrets", Namespace: "default"},
		// previousSuffix rather than a hand-written one: with the wrong key
		// there is no history to roll back to, so the command refuses for that
		// reason and the test passes with the guard removed.
		Data: map[string][]byte{
			"DATABASE_PASSWORD":                  []byte("current"),
			"DATABASE_PASSWORD" + previousSuffix: []byte("previous"),
		},
	})
	withWorkloadClients(t, cs, dyn)

	err := runSecretRollback(secretRollbackCmd, []string{"api", "DATABASE_PASSWORD"})
	require.Error(t, err, "an orphan Secret is not a workload")
	assert.Contains(t, err.Error(), "no app named", "and it refuses for that reason, not for want of history")

	after, getErr := cs.CoreV1().Secrets("default").Get(context.Background(), "app-api-secrets", metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Equal(t, []byte("current"), after.Data["DATABASE_PASSWORD"], "and nothing was swapped")
	assert.Equal(t, []byte("previous"), after.Data["DATABASE_PASSWORD"+previousSuffix])
}

// The remedy in a failed --restart has to be one that exists. Only Apps have a
// restart, in the CLI and in the console alike.
func TestApplyConfigChange_NamesNoRemedyAFunctionHasNot(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.FunctionGVR, "Function", "default", "resize", nil)
	dyn.PrependReactor("update", "functions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured)
		if _, restarting := obj.GetAnnotations()["kipper.run/restartedAt"]; !restarting {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "kipper.run", Resource: "functions"}, "resize", context.DeadlineExceeded)
	})
	withWorkloadClients(t, k8sfake.NewSimpleClientset(), dyn)
	withRestartFlag(t, functionEnvSetCmd)

	err := runEnvSet(functionEnvSetCmd, []string{"resize", "MODE=fast"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--restart", "the command itself is the whole of the remedy")
	assert.NotContains(t, err.Error(), "console", "there is no function restart in the console either")
	assert.NotContains(t, err.Error(), "kip function restart", "nor on the command line")
}

// A credential is named after the pair it holds, so two
// first deploys of the same token converge on one object. Deleting it because
// this run won the Create is the same fallacy the console abandoned: the other
// run may have claimed it and be about to name an App at it.
func TestCleanupDeploySecretsLeavesACredentialAnotherRunHasClaimed(t *testing.T) {
	ctx := context.Background()
	name := secretname.GitCredential("api", secretname.GitCredentialDigest("t", "git.example.com"))

	created := secretFixture(name)
	created.ResourceVersion = "1"
	cs := k8sfake.NewSimpleClientset(created)
	d := &deployer.Deployer{Client: cs, Dynamic: dynamicfake.NewSimpleDynamicClient(appScheme())}

	// The other run claims it after this one created it.
	claimed, err := cs.CoreV1().Secrets("blog-test").Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	claimed.Annotations = map[string]string{labels.AnnoGitCredentialClaimed: "2026-08-19T10:00:00Z"}
	// The apiserver bumps this on every write; the fake client does not, so the
	// test stands in for it. The version moving is the whole signal.
	claimed.ResourceVersion = "2"
	_, err = cs.CoreV1().Secrets("blog-test").Update(ctx, claimed, metav1.UpdateOptions{})
	require.NoError(t, err)

	cleanupDeploySecrets(ctx, cs, d, "blog-test", "api", name, "1", false, true)

	_, err = cs.CoreV1().Secrets("blog-test").Get(ctx, name, metav1.GetOptions{})
	assert.NoError(t, err,
		"a failed deploy removed a credential another run had claimed and is about to name an app at")
}
