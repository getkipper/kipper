package cmd

import (
	"context"
	"os"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

// fakeWorkloadDynamic knows both App and Function CRs, because the collisions
// these tests cover only appear when the same name exists as both kinds.
func fakeWorkloadDynamic() *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	for _, kind := range []string{"App", "Function", "Service"} {
		scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: kind}, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: kind + "List"}, &unstructured.UnstructuredList{})
	}
	// The scan reads a kind's CRD to learn which fields the cluster defaults, so
	// the fake has to be able to hold one.
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinitionList"}, &unstructured.UnstructuredList{})
	return dynamicfake.NewSimpleDynamicClient(scheme)
}

func seedWorkload(t *testing.T, dyn *dynamicfake.FakeDynamicClient, gvr schema.GroupVersionResource, kind, ns, name string, env map[string]interface{}) {
	t.Helper()
	spec := map[string]interface{}{"image": "nginx"}
	if env != nil {
		spec["env"] = env
	}
	_, err := dyn.Resource(gvr).Namespace(ns).Create(context.Background(), &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       kind,
			"metadata":   map[string]interface{}{"name": name, "namespace": ns},
			"spec":       spec,
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func seedEnvApp(t *testing.T, dyn *dynamicfake.FakeDynamicClient, env map[string]interface{}) {
	t.Helper()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "default", "api", env)
}

// specEnv reads spec.env off the workload called api in the tests' one
// namespace, reporting whether the field is there at all — an emptied env has
// to remove it, not leave an empty map — and the CR's annotations, which carry
// the restart stamp.
func specEnv(t *testing.T, dyn *dynamicfake.FakeDynamicClient, gvr schema.GroupVersionResource) (map[string]string, bool, map[string]string) {
	t.Helper()
	obj, err := dyn.Resource(gvr).Namespace("default").Get(context.Background(), "api", metav1.GetOptions{})
	require.NoError(t, err)
	env, found, _ := unstructured.NestedStringMap(obj.Object, "spec", "env")
	return env, found, obj.GetAnnotations()
}

// withWorkloadClients points the env handlers at fake clients so a test can run
// the real command path instead of a copy of it. Every workload these tests
// build lives in the default namespace.
func withWorkloadClients(t *testing.T, clientset kubernetes.Interface, dyn dynamic.Interface) {
	t.Helper()
	original := workloadClients
	workloadClients = func(*cobra.Command, string) (string, kubernetes.Interface, dynamic.Interface, error) {
		return "default", clientset, dyn, nil
	}
	t.Cleanup(func() { workloadClients = original })
}

// withRestartFlag opts one test into --restart and puts it back afterwards. The
// cobra commands are package-level singletons, so a flag left set would quietly
// change what a later test exercises.
func withRestartFlag(t *testing.T, cmd *cobra.Command) {
	t.Helper()
	require.NoError(t, cmd.Flags().Set("restart", "true"))
	t.Cleanup(func() { require.NoError(t, cmd.Flags().Set("restart", "false")) })
}

func TestWriteWorkloadEnv_RemovesDeletedKey(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	cs := k8sfake.NewSimpleClientset()
	seedEnvApp(t, dyn, map[string]interface{}{"FOO": "bar", "BAZ": "qux"})

	require.NoError(t, writeWorkloadEnv(context.Background(), dyn, cs, secretname.KindApp, "default", "api",
		map[string]string{"FOO": "bar"}, envOnCR))

	env, _, _ := specEnv(t, dyn, manifest.AppGVR)
	assert.Equal(t, map[string]string{"FOO": "bar"}, env, "a deleted key must be removed from spec.env so the reconciler cannot restore it")
}

func TestWriteWorkloadEnv_RemovesEnvWhenEmpty(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	cs := k8sfake.NewSimpleClientset()
	seedEnvApp(t, dyn, map[string]interface{}{"FOO": "bar"})

	require.NoError(t, writeWorkloadEnv(context.Background(), dyn, cs, secretname.KindApp, "default", "api",
		map[string]string{}, envOnCR))

	_, found, _ := specEnv(t, dyn, manifest.AppGVR)
	assert.False(t, found, "removing the last env var must drop spec.env entirely")
}

// `kip app promote` builds a Deployment with no CR behind it, so nothing
// renders that app's env Secret and the Secret is the only place its
// environment exists.
func TestWriteWorkloadEnv_NoCRWritesTheSecret(t *testing.T) {
	dyn := fakeWorkloadDynamic() // no App CR exists
	cs := k8sfake.NewSimpleClientset()

	require.NoError(t, writeWorkloadEnv(context.Background(), dyn, cs, secretname.KindApp, "default", "api",
		map[string]string{"FOO": "bar"}, envOnSecretNamed("app-api-env")))

	secret, err := cs.CoreV1().Secrets("default").Get(context.Background(), "app-api-env", metav1.GetOptions{})
	require.NoError(t, err, "an app with no CR has nowhere else for its environment to live")
	assert.Equal(t, []byte("bar"), secret.Data["FOO"])
}

// The Secret is the reconciler's output, and since values became templates it
// is not the same thing as what was written: it holds the resolved password
// where the CR holds ${DB_PASSWORD}. A command that read it back and wrote what
// it read would replace the template with the credential — on the CR, which is
// exactly what `kip export` copies.
func TestEnvSet_LeavesATemplateOnTheCRAndTheRenderToTheReconciler(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "default", "api", map[string]interface{}{
		"DATABASE_URL": "postgres://${DB_USERNAME}:${DB_PASSWORD}@${DB_HOST}/app",
	})
	rendered := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-api-env", Namespace: "default"},
		Data:       map[string][]byte{"DATABASE_URL": []byte("postgres://kipper:hunter2@db.default.svc/app")},
	}
	cs := k8sfake.NewSimpleClientset(rendered)
	withWorkloadClients(t, cs, dyn)

	require.NoError(t, runEnvSet(envSetCmd, []string{"api", "LOG_LEVEL=debug"}))

	env, _, _ := specEnv(t, dyn, manifest.AppGVR)
	assert.Equal(t, "postgres://${DB_USERNAME}:${DB_PASSWORD}@${DB_HOST}/app", env["DATABASE_URL"],
		"setting an unrelated variable must not replace the template with the password it resolved to")
	assert.Equal(t, "debug", env["LOG_LEVEL"])

	live, err := cs.CoreV1().Secrets("default").Get(context.Background(), "app-api-env", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, map[string][]byte{"DATABASE_URL": []byte("postgres://kipper:hunter2@db.default.svc/app")}, live.Data,
		"the render belongs to the reconciler; the command writes the CR and lets the next pass rebuild it")
}

// The same for reading: what an operator set is what they should be shown.
func TestEnvList_ShowsTheTemplateRatherThanTheCredential(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "default", "api", map[string]interface{}{
		"DATABASE_URL": "postgres://${DB_USERNAME}:${DB_PASSWORD}@${DB_HOST}/app",
	})
	cs := k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-api-env", Namespace: "default"},
		Data:       map[string][]byte{"DATABASE_URL": []byte("postgres://kipper:hunter2@db.default.svc/app")},
	})
	withWorkloadClients(t, cs, dyn)

	env, home, err := readWorkloadEnv(context.Background(), dyn, cs, secretname.KindApp, "default", "api")
	require.NoError(t, err)
	assert.Equal(t, envOnCR, home)
	assert.Equal(t, "postgres://${DB_USERNAME}:${DB_PASSWORD}@${DB_HOST}/app", env["DATABASE_URL"],
		"listing must show what was set, not the password it resolved to")
}

// An App and a Function may both be called api in one namespace. Naming the
// Secret after the kind stops them sharing an object, but the CR write and the
// restart have to follow the same kind or the collision simply moves: the
// Function's values land in the App's spec.env, the App is restarted, and the
// Function reconciler reverts the Secret on its next pass. These two tests run
// the real handlers, because the defect was in the wiring rather than in any
// helper each one calls.
func TestEnvSet_WritesOnlyTheInvokedKind(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cmd        *cobra.Command
		wrote      schema.GroupVersionResource
		leftAlone  schema.GroupVersionResource
		otherKind  string
		writtenGVR string
	}{
		{"function", functionEnvSetCmd, manifest.FunctionGVR, manifest.AppGVR, "App", "Function"},
		{"app", envSetCmd, manifest.AppGVR, manifest.FunctionGVR, "Function", "App"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dyn := fakeWorkloadDynamic()
			seedWorkload(t, dyn, manifest.AppGVR, "App", "default", "api", nil)
			seedWorkload(t, dyn, manifest.FunctionGVR, "Function", "default", "api", nil)
			cs := k8sfake.NewSimpleClientset()
			withWorkloadClients(t, cs, dyn)
			withRestartFlag(t, tc.cmd)

			require.NoError(t, runEnvSet(tc.cmd, []string{"api", "DB_HOST=db.internal"}))

			env, _, annotations := specEnv(t, dyn, tc.wrote)
			assert.Equal(t, map[string]string{"DB_HOST": "db.internal"}, env,
				"spec.env on the %s CR is what the reconciler rebuilds the Secret from; without it the next reconcile reverts the command", tc.writtenGVR)
			assert.Contains(t, annotations, "kipper.run/restartedAt",
				"the %s must be restarted, or a pod already running keeps serving the old values", tc.writtenGVR)

			otherEnv, found, otherAnnotations := specEnv(t, dyn, tc.leftAlone)
			assert.Falsef(t, found, "the %s of the same name must not receive the other workload's env: got %v", tc.otherKind, otherEnv)
			assert.NotContainsf(t, otherAnnotations, "kipper.run/restartedAt", "the %s of the same name must not be restarted", tc.otherKind)
		})
	}
}

// `kip function env delete` has the same split: the key goes from the Function's
// Secret, and spec.env on the Function CR has to follow or the reconciler puts
// it straight back.
func TestFunctionEnvDelete_RemovesFromTheFunctionCR(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "default", "api", map[string]interface{}{"KEEP": "app-value"})
	seedWorkload(t, dyn, manifest.FunctionGVR, "Function", "default", "api", map[string]interface{}{"KEEP": "yes", "DROP": "no"})
	cs := k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "function-api-env", Namespace: "default"},
		Data:       map[string][]byte{"KEEP": []byte("yes"), "DROP": []byte("no")},
	})
	withWorkloadClients(t, cs, dyn)

	require.NoError(t, runEnvDelete(functionEnvDeleteCmd, []string{"api", "DROP"}))

	env, _, _ := specEnv(t, dyn, manifest.FunctionGVR)
	assert.Equal(t, map[string]string{"KEEP": "yes"}, env, "the key must go from the Function's spec.env, or the reconciler restores it")

	appEnv, _, _ := specEnv(t, dyn, manifest.AppGVR)
	assert.Equal(t, map[string]string{"KEEP": "app-value"}, appEnv, "the App of the same name must be untouched")
}

func TestSaveEnvSecret_PreservesOwnerReferenceOnUpdate(t *testing.T) {
	// A reconciler-created env Secret owned by the App CR.
	owned := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretname.Env(secretname.KindApp, "api"),
			Namespace: "default",
			Labels:    map[string]string{"app": "api", "app.kubernetes.io/managed-by": "kipper"},
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "kipper.run/v1alpha1", Kind: "App", Name: "api", UID: "abc123"},
			},
		},
		Data: map[string][]byte{"FOO": []byte("bar")},
	}
	cs := k8sfake.NewSimpleClientset(owned)

	require.NoError(t, saveEnvSecret(context.Background(), cs, "default", "api", secretname.Env(secretname.KindApp, "api"), map[string]string{"FOO": "bar", "BAZ": "qux"}))

	got, err := cs.CoreV1().Secrets("default").Get(context.Background(), secretname.Env(secretname.KindApp, "api"), metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, got.OwnerReferences, 1, "the write must not strip the App's ownerReference, or the Secret orphans on app delete")
	assert.Equal(t, "api", got.OwnerReferences[0].Name)
	assert.Equal(t, "qux", string(got.Data["BAZ"]), "the new key must be written")
}

// promotedDeploy is what `kip app promote` leaves behind: a Deployment owned by
// nothing, carrying the promote stamp on its pod template, with no CR.
func promotedDeploy() *appsv1.Deployment {
	const name = "api"
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{"app": name, "app.kubernetes.io/managed-by": "kipper"},
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"kipper.run/promoted-from": "test"},
				},
			},
		},
	}
}

// controllerDeploy is an ordinary app's Deployment: the App CR owns it, so it
// outlives a deleted CR only for as long as garbage collection takes.
func controllerDeploy() *appsv1.Deployment {
	const name = "api"
	controller := true
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{"app": name, "app.kubernetes.io/managed-by": "kipper"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "kipper.run/v1alpha1", Kind: "App", Name: name, UID: types.UID("uid-" + name), Controller: &controller,
			}},
		},
	}
}

// The Secret is read for one shape only, and "the CR is gone" does not identify
// it. An ordinary app's Deployment and rendered Secret outlive its App CR by
// however long garbage collection takes, and a stranded one outlives it for
// good — reading the Secret there would print the resolved credentials this
// whole feature exists to keep off the surfaces an operator looks at.
func TestReadWorkloadEnv_RefusesTheRenderedSecretOfAnAppWhoseCRIsGone(t *testing.T) {
	dyn := fakeWorkloadDynamic() // the App CR is already gone
	cs := k8sfake.NewSimpleClientset(
		controllerDeploy(),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "app-api-env", Namespace: "default"},
			Data:       map[string][]byte{"DATABASE_URL": []byte("postgres://kipper:hunter2@db.default.svc/app")},
		},
	)

	_, _, err := readWorkloadEnv(context.Background(), dyn, cs, secretname.KindApp, "default", "api")
	require.Error(t, err, "an app mid-deletion is not a promoted app, and its rendered Secret must not be read back")
	assert.NotContains(t, err.Error(), "hunter2")
}

func TestReadWorkloadEnv_ReturnsErrorOnNonNotFound(t *testing.T) {
	dyn := fakeWorkloadDynamic() // no CR, so the read falls through to the Secret
	cs := k8sfake.NewSimpleClientset(promotedDeploy())
	// Simulate a transient apiserver failure rather than a clean NotFound.
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver is down")
	})

	_, _, err := readWorkloadEnv(context.Background(), dyn, cs, secretname.KindApp, "default", "api")
	require.Error(t, err, "a transient read error must not be swallowed as an empty env, or env set would wipe existing vars")
}

// A promoted app has no CR to annotate, so the restart every other workload gets
// through its reconciler has to reach the pod template directly. Without it the
// command writes new values, says so, and every running pod keeps the old ones:
// a container reads envFrom once, at start.
func TestEnvSet_RollsAPromotedAppsPods(t *testing.T) {
	dyn := fakeWorkloadDynamic() // promoted apps have no CR
	cs := k8sfake.NewSimpleClientset(promotedDeploy())
	withWorkloadClients(t, cs, dyn)
	withRestartFlag(t, envSetCmd)

	require.NoError(t, runEnvSet(envSetCmd, []string{"api", "LOG_LEVEL=debug"}))

	secret, err := cs.CoreV1().Secrets("default").Get(context.Background(), "app-api-env", metav1.GetOptions{})
	require.NoError(t, err, "with no CR to render from, the Secret is where a promoted app's env lives")
	assert.Equal(t, []byte("debug"), secret.Data["LOG_LEVEL"])

	deploy, err := cs.AppsV1().Deployments("default").Get(context.Background(), "api", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, deploy.Spec.Template.Annotations, "kipper.run/restartedAt",
		"nothing reconciles this Deployment, so the pod template is the only place a restart can be asked for")
	assert.Equal(t, "test", deploy.Spec.Template.Annotations["kipper.run/promoted-from"],
		"the stamp that identifies the shape must survive the restart, or the next command stops recognising it")
}

func TestReadWorkloadEnv_MissingEverythingIsAnError(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	cs := k8sfake.NewSimpleClientset()
	_, _, err := readWorkloadEnv(context.Background(), dyn, cs, secretname.KindApp, "default", "api")
	require.Error(t, err, "a name that is neither a CR nor a promoted Deployment is not a workload")
}

func TestLooksLikeSecret(t *testing.T) {
	sensitive := []string{"DB_PASSWORD", "API_KEY", "STRIPE_SECRET", "GITHUB_TOKEN", "AWS_ACCESS_KEY", "TLS_PRIVATE_KEY", "SMTP_PASSWD", "DB_CREDENTIAL", "GPG_PASSPHRASE", "apikey"}
	for _, k := range sensitive {
		assert.Truef(t, looksLikeSecret(k), "%q should be flagged as secret-like", k)
	}

	benign := []string{"LOG_LEVEL", "API_URL", "REGION", "PORT", "FEATURE_FLAGS", "PUBLIC_HOST"}
	for _, k := range benign {
		assert.Falsef(t, looksLikeSecret(k), "%q should not be flagged", k)
	}
}

// The name heuristic alone missed the case that prompted this check: a real
// DocuSeal deployment stored its database password inside DATABASE_URL, whose
// name matches none of the tokens above, so nothing warned.
func TestFlagSensitiveEnv_InspectsValuesNotOnlyNames(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		flagged bool
	}{
		{"literal password in a benign key", "DATABASE_URL", "postgresql://kipper:s3cr3t@db:5432/app", true},
		{"literal password, no scheme", "DSN", "user:hunter2@tcp(db:3306)/app", false},
		{"url without credentials", "DATABASE_URL", "postgresql://db:5432/app", false},
		{"fully templated url", "DATABASE_URL", "postgresql://${DB_USERNAME}:${DB_PASSWORD}@${DB_HOST}:${DB_PORT}/${DB_NAME}", false},
		{"templated password, literal user", "DATABASE_URL", "postgresql://kipper:${DB_PASSWORD}@db:5432/app", false},
		{"templated user, literal password", "DATABASE_URL", "postgresql://${DB_USERNAME}:s3cr3t@db:5432/app", true},
		{"jdbc url carries no credential", "SPRING_DATASOURCE_URL", "jdbc:postgresql://${DB_HOST}:${DB_PORT}/orders", false},
		{"secret-like name still flagged on any value", "API_KEY", "abc123", true},
		{"urlencode modifier is a placeholder", "DATABASE_URL", "postgresql://${DB_USERNAME:urlencode}:${DB_PASSWORD:urlencode}@db:5432/app", false},

		// A placeholder-shaped run that the resolver would not resolve must not
		// erase the URL's own delimiters. Here the password really is `pass}`.
		{"malformed placeholder cannot suppress the warning", "DATABASE_URL", "postgresql://user${:pass}@db:5432/app", true},
		{"name starting with a digit is not a placeholder", "DATABASE_URL", "postgresql://user:${1pass}@db:5432/app", true},
		{"unknown modifier is not a placeholder", "DATABASE_URL", "postgresql://user:${PW:base64}@db:5432/app", true},
		{"empty braces are not a placeholder", "DATABASE_URL", "postgresql://user:${}pw@db:5432/app", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			flagged := flagSensitiveEnv(map[string]string{tc.key: tc.value})
			if tc.flagged {
				assert.Equal(t, []string{tc.key}, flagged)
				return
			}
			assert.Empty(t, flagged)
		})
	}
}

// runEnvSet, runEnvList and runEnvDelete are registered under both `kip app env`
// and `kip function env`, so the Secret they name depends entirely on which tree
// invoked them. Before the kind was threaded through, `kip function env set api`
// wrote the same Secret as `kip app env set api`. This drives the real command
// tree that init() builds rather than a stand-in, because the wiring is the part
// that can break.
func TestWorkloadKind_FollowsTheCommandTree(t *testing.T) {
	for _, tc := range []struct {
		cmd  *cobra.Command
		want secretname.Kind
	}{
		{envSetCmd, secretname.KindApp},
		{envListCmd, secretname.KindApp},
		{envDeleteCmd, secretname.KindApp},
		{functionEnvSetCmd, secretname.KindFunction},
		{functionEnvListCmd, secretname.KindFunction},
		{functionEnvDeleteCmd, secretname.KindFunction},
	} {
		got := workloadKind(tc.cmd)
		assert.Equalf(t, tc.want, got, "%s: got %s", tc.cmd.CommandPath(), got)
	}

	assert.NotEqual(t,
		secretname.Env(workloadKind(envSetCmd), "api"),
		secretname.Env(workloadKind(functionEnvSetCmd), "api"),
		"an App and a Function called api must not share an env Secret")
}

func TestFlagSensitiveEnv_SortsForStableOutput(t *testing.T) {
	//nolint:gosec // G101 fires on the fixture, which must carry an embedded password because that is the shape the detector exists to find.
	flagged := flagSensitiveEnv(map[string]string{
		"REDIS_URL":    "redis://:hunter2@cache:6379/0",
		"API_KEY":      "abc123",
		"DATABASE_URL": "postgresql://kipper:s3cr3t@db:5432/app",
		"LOG_LEVEL":    "debug",
	})
	assert.Equal(t, []string{"API_KEY", "DATABASE_URL", "REDIS_URL"}, flagged)
}

// Every kind maps to its own CR. A kind falling through to the App GVR is how
// `kip function env set` came to write the App's spec.env, so a new kind must
// not inherit that default silently.
func TestWorkloadGVR_EveryKindAddressesItsOwnCR(t *testing.T) {
	assert.Equal(t, manifest.AppGVR, workloadGVR(secretname.KindApp))
	assert.Equal(t, manifest.FunctionGVR, workloadGVR(secretname.KindFunction))
	assert.Equal(t, manifest.JobGVR, workloadGVR(secretname.KindJob))
}

// withStdin answers an interactive prompt, so a test can take the branch that
// writes rather than stopping at the question.
func withStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	_, err = w.WriteString(input)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	original := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = original; _ = r.Close() })
}

// The conflict prompt is the last place a `kip function` command could still
// write an App. A cron-only Function has no Deployment while a same-named App
// has one, so looking it up by name alone found the App's, described it as the
// function's, and edited it when the prompt was accepted.
//
// This drives the real setters, because the defect is which kind they hand to
// the lookup: a test of the lookup alone stays green if either passes KindApp.
// Both answer "y", so a setter that found a Deployment would go on to write it.
func TestFunctionSetters_LeaveASameNamedAppsDeploymentAlone(t *testing.T) {
	// The App's Deployment, carrying a direct env entry that collides with the
	// key the function command sets. No resource-type label: Apps have none.
	appDeployment := func() *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "api",
				Namespace: "default",
				Labels:    map[string]string{"app": "api", "app.kubernetes.io/managed-by": "kipper"},
			},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: "api",
						Env:  []corev1.EnvVar{{Name: "DB_HOST", Value: "db.app.internal"}},
					}}},
				},
			},
		}
	}

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"env set", func() error { return runEnvSet(functionEnvSetCmd, []string{"api", "DB_HOST=db.fn.internal"}) }},
		{"secret set", func() error { return runSecretSet(functionSecretSetCmd, []string{"api", "DB_HOST=db.fn.internal"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dyn := fakeWorkloadDynamic()
			seedWorkload(t, dyn, manifest.AppGVR, "App", "default", "api", nil)
			// Cron-only: a Function CR with no Deployment of its own.
			seedWorkload(t, dyn, manifest.FunctionGVR, "Function", "default", "api", nil)
			cs := k8sfake.NewSimpleClientset(appDeployment())
			withWorkloadClients(t, cs, dyn)
			withStdin(t, "y\n")

			require.NoError(t, tc.run())

			deploy, err := cs.AppsV1().Deployments("default").Get(context.Background(), "api", metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t,
				[]corev1.EnvVar{{Name: "DB_HOST", Value: "db.app.internal"}},
				deploy.Spec.Template.Spec.Containers[0].Env,
				"a function command must not strip the App's direct env, whatever the user answers")
		})
	}
}

// A promoted Deployment built before the env Secret was kind-qualified still
// names <app>-env. Writing app-<app>-env would create an object beside the one
// its pods read, restart them, and report success while nothing changed —
// silent env loss on the one -env Secret nothing rebuilds.
func TestEnvSet_WritesTheSecretThePromotedDeploymentReads(t *testing.T) {
	dyn := fakeWorkloadDynamic() // promoted apps have no CR
	deploy := promotedDeploy()
	optional := true
	deploy.Spec.Template.Spec.Containers = []corev1.Container{{
		Name: "api",
		EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: "api-env"},
			Optional:             &optional,
		}}},
	}}
	cs := k8sfake.NewSimpleClientset(deploy, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-env", Namespace: "default",
			Labels: map[string]string{"app": "api", "app.kubernetes.io/managed-by": "kipper"},
		},
		Data: map[string][]byte{"LOG_LEVEL": []byte("info")},
	})
	withWorkloadClients(t, cs, dyn)

	require.NoError(t, runEnvSet(envSetCmd, []string{"api", "LOG_LEVEL=debug"}))

	read, err := cs.CoreV1().Secrets("default").Get(context.Background(), "api-env", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []byte("debug"), read.Data["LOG_LEVEL"], "the write must land in the Secret the pod reads")

	_, err = cs.CoreV1().Secrets("default").Get(context.Background(), "app-api-env", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "and must not create a second Secret nothing reads")
}

// envSecretOf decides which object gets overwritten, so it accepts only the two
// names Kipper has ever written: the current one and the one it replaced. A
// container may reference several Secrets, and another `*-env` in that list
// belongs to somebody else.
func TestEnvSecretOf(t *testing.T) {
	optional := true
	withEnvFrom := func(names ...string) *appsv1.Deployment {
		d := promotedDeploy()
		var from []corev1.EnvFromSource
		for _, n := range names {
			from = append(from, corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: n},
				Optional:             &optional,
			}})
		}
		d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "api", EnvFrom: from}}
		return d
	}

	t.Run("the name it replaced, still referenced", func(t *testing.T) {
		got := envSecretOf(withEnvFrom("api-env", "api-secrets"), secretname.KindApp, "api")
		assert.Equal(t, "api-env", got)
	})

	t.Run("the current name wins wherever it sits", func(t *testing.T) {
		got := envSecretOf(withEnvFrom("api-secrets", "app-api-env"), secretname.KindApp, "api")
		assert.Equal(t, "app-api-env", got)
	})

	t.Run("somebody else's -env is not ours to overwrite", func(t *testing.T) {
		got := envSecretOf(withEnvFrom("shared-platform-env", "api-secrets"), secretname.KindApp, "api")
		assert.Equal(t, "app-api-env", got,
			"an unrecognised Secret must not be written to; the current name at worst creates one nothing reads")
	})

	t.Run("no deployment", func(t *testing.T) {
		assert.Equal(t, "app-api-env", envSecretOf(nil, secretname.KindApp, "api"))
	})
}

// The name comes from a pod template or from convention, and neither proves the
// object under it is Kipper's. Replacing the data of a Secret somebody else
// created would destroy it, so the write stops rather than assuming the name.
func TestSaveEnvSecret_RefusesASecretKipperDidNotCreate(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-api-env", Namespace: "default"},
		Data:       map[string][]byte{"SOMEONE_ELSES": []byte("data")},
	})

	err := saveEnvSecret(context.Background(), cs, "default", "api", "app-api-env",
		map[string]string{"LOG_LEVEL": "debug"})
	require.Error(t, err, "a Secret without Kipper's label is not ours to overwrite")
	assert.Contains(t, err.Error(), "not created by Kipper")

	live, getErr := cs.CoreV1().Secrets("default").Get(context.Background(), "app-api-env", metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Equal(t, map[string][]byte{"SOMEONE_ELSES": []byte("data")}, live.Data, "and its data must survive")
}

// Restarting is the destructive half of a configuration change: it drops every
// connection the workload is serving. Setting a variable does not ask for that,
// and the console has never done it — it writes the change and raises a banner.
// The CLI matches now, which is what makes one surface's behaviour predict the
// other's.
func TestEnvSet_SavesWithoutRestartingByDefault(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "default", "api", nil)
	cs := k8sfake.NewSimpleClientset()
	withWorkloadClients(t, cs, dyn)

	out := captureStdout(t, func() {
		require.NoError(t, runEnvSet(envSetCmd, []string{"api", "LOG_LEVEL=debug"}))
	})

	env, _, annotations := specEnv(t, dyn, manifest.AppGVR)
	assert.Equal(t, map[string]string{"LOG_LEVEL": "debug"}, env, "the change is saved either way")
	assert.NotContains(t, annotations, "kipper.run/restartedAt",
		"a variable set is not a restart requested")

	// Saying nothing would be worse than restarting: a container reads envFrom
	// once, at start, so silence leaves someone believing it took effect.
	assert.Contains(t, out, "keep the values they started with")
	assert.Contains(t, out, "--restart")
	assert.Contains(t, out, "kip app restart api")
}

// And the flag does what it says.
func TestEnvSet_RestartsWhenAskedTo(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "default", "api", nil)
	cs := k8sfake.NewSimpleClientset()
	withWorkloadClients(t, cs, dyn)
	withRestartFlag(t, envSetCmd)

	out := captureStdout(t, func() {
		require.NoError(t, runEnvSet(envSetCmd, []string{"api", "LOG_LEVEL=debug"}))
	})

	_, _, annotations := specEnv(t, dyn, manifest.AppGVR)
	assert.Contains(t, annotations, "kipper.run/restartedAt")
	assert.NotContains(t, out, "--restart", "there is nothing left to suggest")
}

// A function has no `kip function restart`, so naming one would send its
// operator to a command that is not there. --restart exists for every kind.
func TestEnvSet_DoesNotNameARestartCommandAFunctionHasNot(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.FunctionGVR, "Function", "default", "resize", nil)
	cs := k8sfake.NewSimpleClientset()
	withWorkloadClients(t, cs, dyn)

	out := captureStdout(t, func() {
		require.NoError(t, runEnvSet(functionEnvSetCmd, []string{"resize", "MODE=fast"}))
	})

	assert.Contains(t, out, "--restart")
	assert.NotContains(t, out, "kip function restart", "that command does not exist")
	assert.NotContains(t, out, "kip app restart", "and this is not an app")
}

// Deleting a variable is the same kind of change and answers to the same rule.
func TestEnvDelete_SavesWithoutRestartingByDefault(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "default", "api",
		map[string]interface{}{"LOG_LEVEL": "debug", "KEEP": "yes"})
	cs := k8sfake.NewSimpleClientset()
	withWorkloadClients(t, cs, dyn)

	require.NoError(t, runEnvDelete(envDeleteCmd, []string{"api", "LOG_LEVEL"}))

	env, _, annotations := specEnv(t, dyn, manifest.AppGVR)
	assert.Equal(t, map[string]string{"KEEP": "yes"}, env)
	assert.NotContains(t, annotations, "kipper.run/restartedAt")
}

// The error from a failed --restart is read by someone already stuck, so it must
// not name a command that does not exist. There is no `kip function restart`.
func TestEnvSet_AFailedRestartNamesOnlyCommandsThatExist(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cmd      *cobra.Command
		gvr      schema.GroupVersionResource
		kind     string
		resource string
		wants    string
		rejects  string
	}{
		{"app", envSetCmd, manifest.AppGVR, "App", "apps", "kip app restart api", "kip function restart"},
		{"function", functionEnvSetCmd, manifest.FunctionGVR, "Function", "functions", "--restart", "kip function restart"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dyn := fakeWorkloadDynamic()
			seedWorkload(t, dyn, tc.gvr, tc.kind, "default", "api", nil)
			// Only the restart fails. The env write is an update on the same
			// resource, so the restart is told apart by the annotation it bumps.
			dyn.PrependReactor("update", tc.resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
				obj := action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured)
				if _, restarting := obj.GetAnnotations()["kipper.run/restartedAt"]; !restarting {
					return false, nil, nil
				}
				return true, nil, apierrors.NewForbidden(
					schema.GroupResource{Group: "kipper.run", Resource: tc.resource}, "api", context.DeadlineExceeded)
			})
			withWorkloadClients(t, k8sfake.NewSimpleClientset(), dyn)
			withRestartFlag(t, tc.cmd)

			err := runEnvSet(tc.cmd, []string{"api", "LOG_LEVEL=debug"})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wants)
			assert.NotContains(t, err.Error(), tc.rejects, "that command does not exist")
		})
	}
}

// The restart stamp is what makes the pod template differ, so two restarts
// inside one second have to produce two values. With second granularity the
// second command reported success while the controller, seeing an unchanged
// template, held the workload where it was.
func TestRestartWorkload_ConsecutiveRestartsDiffer(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "default", "api", nil)
	cs := k8sfake.NewSimpleClientset()
	withWorkloadClients(t, cs, dyn)
	withRestartFlag(t, envSetCmd)

	require.NoError(t, runEnvSet(envSetCmd, []string{"api", "MODE=first"}))
	_, _, first := specEnv(t, dyn, manifest.AppGVR)
	stampOne := first["kipper.run/restartedAt"]
	require.NotEmpty(t, stampOne)

	require.NoError(t, runEnvSet(envSetCmd, []string{"api", "MODE=second"}))
	_, _, second := specEnv(t, dyn, manifest.AppGVR)

	assert.NotEqual(t, stampOne, second["kipper.run/restartedAt"],
		"two restarts in the same second must still be two restarts")
}

// Removing a direct env entry changes the pod template, so the workload rolls.
// The operator agreed to that at the prompt, but not by the word "restart" —
// and the command must not then tell them their pods kept the old values.
func TestEnvSet_SaysSoWhenTheConflictCleanupRestarted(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "default", "api", nil)
	cs := k8sfake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "api",
					Env:  []corev1.EnvVar{{Name: "LOG_LEVEL", Value: "info"}},
				}}},
			},
		},
	})
	withWorkloadClients(t, cs, dyn)

	// Answer the prompt with y.
	stdin := os.Stdin
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = stdin })
	_, _ = w.WriteString("y\n")
	require.NoError(t, w.Close())

	out := captureStdout(t, func() {
		require.NoError(t, runEnvSet(envSetCmd, []string{"api", "LOG_LEVEL=debug"}))
	})

	assert.Contains(t, out, "restarted api", "the cleanup rolled it, and the output says so")
	assert.NotContains(t, out, "keep the values they started with",
		"they did not keep them, so this must not be said")
}
