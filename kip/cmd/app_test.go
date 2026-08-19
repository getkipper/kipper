package cmd

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/controller/pkg/appowner"
	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

func TestReadEnvFile(t *testing.T) {
	write := func(t *testing.T, content string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("parses pairs and skips comments and blanks", func(t *testing.T) {
		env, err := readEnvFile(write(t, "# a comment\n\nLOG_LEVEL=info\nREGION=eu-west-1\n"))
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"LOG_LEVEL": "info", "REGION": "eu-west-1"}, env)
	})

	// The silent-drop bug on the --from-file path: a line with no '=' must fail
	// the whole read, not vanish.
	t.Run("rejects a line with no equals sign", func(t *testing.T) {
		_, err := readEnvFile(write(t, "GOOD=1\nTRUSTED_PROXIES\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "TRUSTED_PROXIES")
	})

	t.Run("rejects an empty key", func(t *testing.T) {
		_, err := readEnvFile(write(t, "=orphan\n"))
		require.Error(t, err)
	})
}

func TestParseEnvVars(t *testing.T) {
	t.Run("parses valid pairs", func(t *testing.T) {
		env, err := parseEnvVars([]string{"LOG_LEVEL=info", "REGION=eu-west-1"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"LOG_LEVEL": "info", "REGION": "eu-west-1"}, env)
	})

	t.Run("keeps a value that contains equals signs", func(t *testing.T) {
		env, err := parseEnvVars([]string{"DSN=postgres://u:p@h/db?sslmode=require"})
		require.NoError(t, err)
		assert.Equal(t, "postgres://u:p@h/db?sslmode=require", env["DSN"])
	})

	t.Run("allows an explicitly empty value", func(t *testing.T) {
		env, err := parseEnvVars([]string{"FEATURE_FLAG="})
		require.NoError(t, err)
		v, ok := env["FEATURE_FLAG"]
		assert.True(t, ok, "an explicit KEY= must set the key to an empty value, not drop it")
		assert.Equal(t, "", v)
	})

	// The silent-drop bug: a pair with no '=' used to vanish without a word, so a
	// typo like `kip app env set app TRUSTED_PROXIES` looked like it worked
	// but set nothing.
	t.Run("rejects a pair with no equals sign", func(t *testing.T) {
		_, err := parseEnvVars([]string{"TRUSTED_PROXIES"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "TRUSTED_PROXIES")
		assert.Contains(t, err.Error(), "KEY=VALUE")
	})

	t.Run("rejects an empty key", func(t *testing.T) {
		_, err := parseEnvVars([]string{"=orphan"})
		require.Error(t, err)
	})

	// Atomic: one malformed entry fails the whole parse so a partial set never
	// reaches the cluster.
	t.Run("rejects the whole batch if any pair is malformed", func(t *testing.T) {
		_, err := parseEnvVars([]string{"GOOD=1", "BAD", "ALSO_GOOD=2"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "BAD")
	})
}

func TestCollectDeploySecrets(t *testing.T) {
	// Swap the interactive prompt so tests never touch stdin.
	prompted := []string{}
	promptSecretValue = func(prompt string) (string, error) {
		prompted = append(prompted, prompt)
		return "prompted-value", nil
	}
	t.Cleanup(func() { promptSecretValue = promptHidden })

	t.Run("parses inline pairs", func(t *testing.T) {
		secrets, err := collectDeploySecrets([]string{"API_KEY=abc123", "DB_PASSWORD=hunter2"}, nil)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"API_KEY": "abc123", "DB_PASSWORD": "hunter2"}, secrets)
	})

	t.Run("keeps a value that contains equals signs", func(t *testing.T) {
		secrets, err := collectDeploySecrets([]string{"DATABASE_URL=postgres://u:p@h/db?sslmode=require"}, nil)
		require.NoError(t, err)
		assert.Equal(t, "postgres://u:p@h/db?sslmode=require", secrets["DATABASE_URL"])
	})

	t.Run("prompts for bare keys", func(t *testing.T) {
		prompted = nil
		secrets, err := collectDeploySecrets([]string{"SECRET_KEY_BASE"}, nil)
		require.NoError(t, err)
		assert.Equal(t, "prompted-value", secrets["SECRET_KEY_BASE"])
		require.Len(t, prompted, 1)
		assert.Contains(t, prompted[0], "SECRET_KEY_BASE")
	})

	t.Run("does not prompt for a key already given inline", func(t *testing.T) {
		prompted = nil
		secrets, err := collectDeploySecrets([]string{"API_KEY=inline", "API_KEY"}, nil)
		require.NoError(t, err)
		assert.Equal(t, "inline", secrets["API_KEY"])
		assert.Empty(t, prompted)
	})

	t.Run("rejects a key passed as both env and secret", func(t *testing.T) {
		env := map[string]string{"API_KEY": "x", "LOG_LEVEL": "info"}
		_, err := collectDeploySecrets([]string{"API_KEY=y"}, env)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "API_KEY")
		assert.Contains(t, err.Error(), "--env and --secret")
	})

	// The overlap check must fire before any prompting, so a doomed deploy never
	// asks the user to type secret values first.
	t.Run("rejects a bare key that overlaps env without prompting", func(t *testing.T) {
		prompted = nil
		_, err := collectDeploySecrets([]string{"DB_PASSWORD"}, map[string]string{"DB_PASSWORD": "x"})
		require.Error(t, err)
		assert.Empty(t, prompted)
	})

	t.Run("rejects an empty key", func(t *testing.T) {
		_, err := collectDeploySecrets([]string{"=orphan"}, nil)
		require.Error(t, err)
	})

	t.Run("empty flag yields empty map", func(t *testing.T) {
		secrets, err := collectDeploySecrets(nil, map[string]string{"LOG_LEVEL": "info"})
		require.NoError(t, err)
		assert.Empty(t, secrets)
	})
}

// pflag's StringSlice type CSV-splits each occurrence, so a value containing a
// comma (a DATABASE_URL host list, JSON, certificate material) would be torn
// into separate entries and the tail mistaken for a bare key to prompt for.
// StringArray keeps each occurrence verbatim. This locks the flag types so a
// refactor back to StringSlice fails loudly.
func TestDeployEnvAndSecretFlagsPreserveCommas(t *testing.T) {
	for _, flag := range []string{"env", "secret"} {
		f := appDeployCmd.Flags().Lookup(flag)
		require.NotNil(t, f, flag)
		assert.Equalf(t, "stringArray", f.Value.Type(), "--%s must not CSV-split values on commas", flag)
	}
}

// An App and a Function may share a name in different namespaces, and both
// their Deployments carry app=<name> with the same managed-by label. Resolving
// on that label alone returns whichever the apiserver lists first, so a
// function command could act in the App's namespace — and now that each kind
// reads its own Secret, write one the Function never sees.
func TestFindWorkloadNamespace_ResolvesTheRequestedKind(t *testing.T) {
	ctx := context.Background()
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "shop-prod", "api", nil)
	seedWorkload(t, dyn, manifest.FunctionGVR, "Function", "tools-test", "api", nil)
	cs := k8sfake.NewSimpleClientset(
		kipperDeployment("api", "shop-prod", nil),
		kipperDeployment("api", "tools-test", map[string]string{"kipper.run/resource-type": "function"}),
	)

	ns, err := findWorkloadNamespace(ctx, cs, dyn, secretname.KindFunction, "api")
	require.NoError(t, err)
	assert.Equal(t, "tools-test", ns, "a function command must resolve to the Function's namespace")

	ns, err = findWorkloadNamespace(ctx, cs, dyn, secretname.KindApp, "api")
	require.NoError(t, err)
	assert.Equal(t, "shop-prod", ns, "an app command must resolve to the App's namespace")
}

// `kip app promote` builds a Deployment with no App CR behind it, so the CR
// lookup finds nothing and the label lookup is the only way to place the app.
func TestFindWorkloadNamespace_FallsBackToTheDeploymentForACRlessApp(t *testing.T) {
	dyn := fakeWorkloadDynamic() // nothing promoted has a CR
	cs := k8sfake.NewSimpleClientset(kipperDeployment("web", "shop-prod", nil))

	ns, err := findWorkloadNamespace(context.Background(), cs, dyn, secretname.KindApp, "web")
	require.NoError(t, err)
	assert.Equal(t, "shop-prod", ns)
}

// That fallback must not answer with a Function's Deployment, which carries the
// same app=<name> label. Otherwise an app command on a name only a Function
// holds resolves to the Function's namespace and writes an App Secret there.
func TestFindWorkloadNamespace_FunctionDeploymentIsNotAnApp(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.FunctionGVR, "Function", "tools-test", "resize", nil)
	cs := k8sfake.NewSimpleClientset(
		kipperDeployment("resize", "tools-test", map[string]string{"kipper.run/resource-type": "function"}),
	)

	_, err := findWorkloadNamespace(context.Background(), cs, dyn, secretname.KindApp, "resize")
	require.Error(t, err, "no App called resize exists; its Function's Deployment must not stand in for one")
	assert.Contains(t, err.Error(), `app "resize" not found`)
}

// kipperDeployment builds a Kipper-managed Deployment carrying the labels
// namespace resolution matches on.
func kipperDeployment(name, ns string, extra map[string]string) *appsv1.Deployment {
	labels := map[string]string{"app": name, "app.kubernetes.io/managed-by": "kipper"}
	for k, v := range extra {
		labels[k] = v
	}
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}}
}

// One project's environments each get their own namespace, so the same app name
// in blog-test and blog-prod is ordinary rather than exotic. Returning the first
// match made `kip app env set blog DATABASE_URL=…` write to whichever the
// apiserver listed first and restart it, which is how you change prod while
// meaning to change test.
func TestFindWorkloadNamespace_RefusesANameInSeveralNamespaces(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "blog-prod", "api", nil)
	seedWorkload(t, dyn, manifest.AppGVR, "App", "blog-test", "api", nil)
	cs := k8sfake.NewSimpleClientset()

	_, err := findWorkloadNamespace(context.Background(), cs, dyn, secretname.KindApp, "api")
	require.Error(t, err, "an ambiguous name must not resolve to one of its matches by list order")
	assert.Contains(t, err.Error(), "blog-prod")
	assert.Contains(t, err.Error(), "blog-test")
	assert.Contains(t, err.Error(), "--project", "the error has to say how to disambiguate")

	var ambiguous *ambiguousWorkloadError
	require.True(t, stderrors.As(err, &ambiguous), "callers that default on not-found must be able to tell this apart")
}

// The same name as two different kinds is not ambiguous: the kind picks one.
func TestFindWorkloadNamespace_SameNameDifferentKindsIsNotAmbiguous(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.AppGVR, "App", "blog-prod", "api", nil)
	seedWorkload(t, dyn, manifest.FunctionGVR, "Function", "tools-test", "api", nil)
	cs := k8sfake.NewSimpleClientset()

	ns, err := findWorkloadNamespace(context.Background(), cs, dyn, secretname.KindApp, "api")
	require.NoError(t, err)
	assert.Equal(t, "blog-prod", ns)
}

// The App fallback covers CR-less promoted apps, so it needs the same guard: two
// promoted Deployments of one name must not resolve to whichever listed first.
func TestFindWorkloadNamespace_RefusesAnAmbiguousPromotedApp(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	cs := k8sfake.NewSimpleClientset(
		kipperDeployment("web", "shop-prod", nil),
		kipperDeployment("web", "shop-test", nil),
	)

	_, err := findWorkloadNamespace(context.Background(), cs, dyn, secretname.KindApp, "web")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shop-prod")
}

// `kip function create` defaults to the default namespace for a function that
// does not exist yet. It must not do that for an ambiguous one: creating in
// "default" would add a third function beside the two already there.
func TestFindFunctionNamespaceOrDefault_DefaultsOnMissingButNotOnAmbiguous(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewSimpleClientset()

	ns, err := findFunctionNamespaceOrDefault(ctx, cs, fakeWorkloadDynamic(), "brand-new")
	require.NoError(t, err)
	assert.Equal(t, "default", ns, "a function that does not exist yet is created in the default namespace")

	dyn := fakeWorkloadDynamic()
	seedWorkload(t, dyn, manifest.FunctionGVR, "Function", "tools-prod", "resize", nil)
	seedWorkload(t, dyn, manifest.FunctionGVR, "Function", "tools-test", "resize", nil)

	_, err = findFunctionNamespaceOrDefault(ctx, cs, dyn, "resize")
	require.Error(t, err, "falling back to default here would act on a namespace holding neither function")
	assert.Contains(t, err.Error(), "tools-prod")
}

// A Deployment name is unique per namespace while a workload name is unique only
// per kind, so the name alone does not identify one. A cron-only Function has no
// Deployment and an App of the same name does, so fetching by name handed
// `kip function env set api` the App's Deployment — to describe as the
// function's, and to edit when the conflict prompt was accepted.
func TestWorkloadDeployment_RefusesAnotherKindsDeployment(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewSimpleClientset(
		kipperDeployment("api", "shop-test", nil),
		kipperDeployment("resize", "shop-test", map[string]string{"kipper.run/resource-type": "function"}),
	)

	assert.Nil(t, workloadDeployment(ctx, cs, secretname.KindFunction, "shop-test", "api"),
		"a function command must not be handed the App's Deployment")
	assert.Nil(t, workloadDeployment(ctx, cs, secretname.KindApp, "shop-test", "resize"),
		"an app command must not be handed the Function's Deployment")

	require.NotNil(t, workloadDeployment(ctx, cs, secretname.KindApp, "shop-test", "api"))
	require.NotNil(t, workloadDeployment(ctx, cs, secretname.KindFunction, "shop-test", "resize"))
	assert.Nil(t, workloadDeployment(ctx, cs, secretname.KindApp, "shop-test", "absent"))
}

// The CR list is the authoritative lookup, so a failure to run it is the answer.
// Reported as "not found" it becomes a command that quietly acts elsewhere —
// and for function create/bind/unbind, that elsewhere is the default namespace.
func TestFindWorkloadNamespace_ReportsALookupFailureRatherThanNotFound(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	dyn.PrependReactor("list", "functions", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "kipper.run", Resource: "functions"}, "", nil)
	})
	cs := k8sfake.NewSimpleClientset()

	_, err := findWorkloadNamespace(context.Background(), cs, dyn, secretname.KindFunction, "resize")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden", "the RBAC failure has to reach the user")

	var notFound *workloadNotFoundError
	assert.False(t, stderrors.As(err, &notFound), "a failed lookup is not an absent workload")

	ns, err := findFunctionNamespaceOrDefault(context.Background(), cs, dyn, "resize")
	require.Error(t, err, "an unreadable API must not send the command to the default namespace")
	assert.Empty(t, ns)
}

// The credential Secret carries the host its token was stored for, and every
// path that would send the token refuses a pair that disagrees. kip is a writer
// of that Secret, so a kip-written credential with no host recorded sits
// permanently in the class the compatibility rule keeps for clusters that
// predate the binding — and a kip-written credential that keeps a previous
// host's annotation is refused everywhere, with a remedy the operator cannot
// reach through kip.
func TestDeployRecordsTheHostAGitTokenWasStoredFor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing []runtime.Object
	}{
		{"create", nil},
		{"replace one stored for another host", []runtime.Object{&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "web-git-credentials", Namespace: "shop-test",
				Annotations: map[string]string{labels.AnnoGitAuthority: "git.old.example.com"},
			},
			Data: map[string][]byte{"token": []byte("the-old-token")},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientset := k8sfake.NewClientset(tc.existing...)

			name, _, _, err := storeGitCredential(context.Background(), clientset, "shop-test", "web",
				"a-token", "https://git.example.com/acme/web.git", nil)
			require.NoError(t, err)
			assert.True(t, secretname.IsGitCredentialOf("web", name),
				"kip wrote a credential the readers will not recognise as this app's: %s", name)

			secret, getErr := clientset.CoreV1().Secrets("shop-test").Get(
				context.Background(), name, metav1.GetOptions{})
			require.NoError(t, getErr)
			assert.Equal(t, "a-token", string(secret.Data["token"]))
			assert.Equal(t, "git.example.com", secret.Annotations[labels.AnnoGitAuthority],
				"the token was stored with no record of the host it is for, or with a stale one")

			// The credential the app was cloning with is left for the sweep
			// rather than replaced, so a rotation cannot damage the live pair.
			if len(tc.existing) > 0 {
				previous, prevErr := clientset.CoreV1().Secrets("shop-test").Get(
					context.Background(), "web-git-credentials", metav1.GetOptions{})
				require.NoError(t, prevErr)
				assert.Equal(t, "the-old-token", string(previous.Data["token"]),
					"kip rewrote the credential the app is still cloning with")
			}
		})
	}
}

// A token names no repository on its own, and the writer records the host from
// the clone URL. Given a token with no --git, it stored the token and removed
// the host binding, dropping a credential written after the binding existed
// into the class trusted for clusters that predate it. It ran before the deploy
// itself was refused, so the refusal did not undo it.
func TestDeployRefusesAGitTokenWithNoRepository(t *testing.T) {
	cmd := appDeployCmd
	require.NoError(t, cmd.Flags().Set("image", "nginx"))
	require.NoError(t, cmd.Flags().Set("git-token", "a-token"))
	t.Cleanup(func() {
		_ = cmd.Flags().Set("image", "")
		_ = cmd.Flags().Set("git-token", "")
	})

	err := runAppDeploy(cmd, []string{"web"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--git")
}

// kip's reuse branch had no test at all, so its
// ownership decision could be deleted and the suite stayed green. It also kept
// a reference left by an App of this name that is gone, which garbage
// collection removes the object by, and it re-implemented the decision the
// console and the reconciler share rather than using it.
func TestStoreGitCredentialReusingAnExistingObject(t *testing.T) {
	name := secretname.GitCredential("web", secretname.GitCredentialDigest("a-token", "git.example.com"))
	controls := true
	existing := func(owner *metav1.OwnerReference) *corev1.Secret {
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop-test"},
			Data:       map[string][]byte{"token": []byte("a-token")},
		}
		if owner != nil {
			s.OwnerReferences = []metav1.OwnerReference{*owner}
		}
		return s
	}

	t.Run("reuses an object nothing owns", func(t *testing.T) {
		clientset := k8sfake.NewClientset(existing(nil))

		got, _, fresh, err := storeGitCredential(context.Background(), clientset, "shop-test", "web",
			"a-token", "https://git.example.com/acme/web.git", nil)
		require.NoError(t, err)
		assert.Equal(t, name, got)
		assert.False(t, fresh, "an object that was already there was reported as created")

		live, getErr := clientset.CoreV1().Secrets("shop-test").Get(context.Background(), name, metav1.GetOptions{})
		require.NoError(t, getErr)
		assert.NotEmpty(t, live.Annotations[labels.AnnoGitCredentialClaimed],
			"reuse did not record that a deploy is committing onto it")
	})

	// A first deploy has no App to keep the object alive, and stripping a dead
	// app's reference does not recall a collection already issued against it.
	t.Run("refuses an object left owned by an app that is gone", func(t *testing.T) {
		clientset := k8sfake.NewClientset(existing(&metav1.OwnerReference{
			APIVersion: "kipper.run/v1alpha1", Kind: "App",
			Name: "web", UID: "the-app-that-was-deleted", Controller: &controls,
		}))

		_, _, _, err := storeGitCredential(context.Background(), clientset, "shop-test", "web",
			"a-token", "https://git.example.com/acme/web.git", nil)
		require.Error(t, err, "a deploy committed onto an object garbage collection is entitled to remove")
	})

	// The name is a digest of the pair, and a digest is not a proof: sixteen
	// hex characters can collide, and anything that can write a Secret in the
	// namespace can put something else at the address. A deploy that claimed
	// without reading it would clone with a token nobody supplied.
	t.Run("refuses an object holding a different token", func(t *testing.T) {
		planted := existing(nil)
		planted.Data = map[string][]byte{"token": []byte("someone-elses-token")}
		clientset := k8sfake.NewClientset(planted)

		_, _, _, err := storeGitCredential(context.Background(), clientset, "shop-test", "web",
			"a-token", "https://git.example.com/acme/web.git", nil)
		require.Error(t, err, "the deploy committed onto an object that does not hold the token supplied")
		assert.Contains(t, err.Error(), "does not hold the token given")
	})

	t.Run("refuses an object recorded for another clone host", func(t *testing.T) {
		elsewhere := existing(nil)
		elsewhere.Annotations = map[string]string{labels.AnnoGitAuthority: "git.internal.example"}
		clientset := k8sfake.NewClientset(elsewhere)

		_, _, _, err := storeGitCredential(context.Background(), clientset, "shop-test", "web",
			"a-token", "https://git.example.com/acme/web.git", nil)
		require.Error(t, err, "the deploy sent a token to a host it was not stored for")
	})

	// Without the writer labels the controller's sweeps cannot list the object,
	// so a credential the app later rotates off stays in the namespace with the
	// token in it for good.
	t.Run("repairs the writer labels on an object that lost them", func(t *testing.T) {
		stripped := existing(nil)
		stripped.Labels = nil
		clientset := k8sfake.NewClientset(stripped)

		_, _, _, err := storeGitCredential(context.Background(), clientset, "shop-test", "web",
			"a-token", "https://git.example.com/acme/web.git", nil)
		require.NoError(t, err)

		live, getErr := clientset.CoreV1().Secrets("shop-test").Get(context.Background(), name, metav1.GetOptions{})
		require.NoError(t, getErr)
		assert.Equal(t, labels.Kipper, live.Labels[labels.ManagedBy],
			"the sweeps list by these labels, so a credential without them is never collected")
		assert.Equal(t, "web", live.Labels[labels.AppRef])
	})

	t.Run("refuses an object another controller owns", func(t *testing.T) {
		clientset := k8sfake.NewClientset(existing(&metav1.OwnerReference{
			APIVersion: "serving.example.com/v1", Kind: "Service",
			Name: "something-else", UID: "a-service", Controller: &controls,
		}))

		_, _, _, err := storeGitCredential(context.Background(), clientset, "shop-test", "web",
			"a-token", "https://git.example.com/acme/web.git", nil)
		require.Error(t, err, "the deploy committed onto a credential another controller owns")
		assert.Contains(t, err.Error(), "belongs to something else")
		assert.Contains(t, err.Error(), "check what owns that Secret", "the refusal gives the operator nothing to do")
	})
}

// Reusing a credential for an App that already exists used
// to strip that App's own ownership, because the writer had no UID to tell a
// live owner from a dead one. Every same-pair redeploy churned the reference
// the reconciler had just set.
func TestStoreGitCredentialBindsToAnAppThatAlreadyExists(t *testing.T) {
	name := secretname.GitCredential("web", secretname.GitCredentialDigest("a-token", "git.example.com"))
	controls := true
	clientset := k8sfake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "shop-test",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "kipper.run/v1alpha1", Kind: "App",
				Name: "web", UID: "the-live-app", Controller: &controls,
			}},
		},
		Data: map[string][]byte{"token": []byte("a-token")},
	})
	live := appowner.Reference("kipper.run/v1alpha1", "web", "the-live-app")

	_, _, _, err := storeGitCredential(context.Background(), clientset, "shop-test", "web",
		"a-token", "https://git.example.com/acme/web.git", &live)
	require.NoError(t, err)

	got, getErr := clientset.CoreV1().Secrets("shop-test").Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, getErr)
	require.Len(t, got.OwnerReferences, 1,
		"a redeploy stripped the ownership the reconciler had set, leaving the credential unowned")
	assert.Equal(t, "the-live-app", string(got.OwnerReferences[0].UID))
}

// liveAppOwner decides whether a deploy can bind the credential itself or must
// leave it for the reconciler. Only NotFound means there is no App: anything
// else is unknown rather than absent, and treating the two alike sent a
// redeploy down the no-App path.
func TestLiveAppOwner(t *testing.T) {
	app := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "App",
		"metadata": map[string]interface{}{"name": "web", "namespace": "shop-test", "uid": "the-live-app"},
	}}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(deployer.AppGVR.GroupVersion().WithKind("App"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(deployer.AppGVR.GroupVersion().WithKind("AppList"), &unstructured.UnstructuredList{})

	owner, err := liveAppOwner(context.Background(), dynamicfake.NewSimpleDynamicClient(scheme, app), "shop-test", "web")
	require.NoError(t, err)
	require.NotNil(t, owner, "an app that exists was treated as absent, so its credential is left unbound")
	assert.Equal(t, "the-live-app", string(owner.UID))
	assert.Equal(t, "App", owner.Kind)

	owner, err = liveAppOwner(context.Background(), dynamicfake.NewSimpleDynamicClient(scheme), "shop-test", "web")
	require.NoError(t, err)
	assert.Nil(t, owner, "a first deploy has no app to bind to and must say so")

	// An unreadable App is unknown rather than absent. Treating it as absent
	// would send a redeploy down the no-owner path and strip the ownership the
	// reconciler had set.
	refusing := dynamicfake.NewSimpleDynamicClient(scheme)
	refusing.PrependReactor("get", "apps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("the apiserver is busy")
	})
	_, err = liveAppOwner(context.Background(), refusing, "shop-test", "web")
	assert.Error(t, err, "an unreadable app was treated as one that does not exist")
}

// kip left a freshly created credential unowned even
// when it had just looked the App up, so the object stayed collectable by
// nothing until a reconcile adopted it. The console binds at creation; this now
// does too, and the unowned path is only a first deploy, where there is no App.
func TestStoreGitCredentialBindsANewCredentialToALiveApp(t *testing.T) {
	clientset := k8sfake.NewClientset()
	live := appowner.Reference("kipper.run/v1alpha1", "web", "the-live-app")

	name, _, fresh, err := storeGitCredential(context.Background(), clientset, "shop-test", "web",
		"a-token", "https://git.example.com/acme/web.git", &live)
	require.NoError(t, err)
	require.True(t, fresh)

	got, getErr := clientset.CoreV1().Secrets("shop-test").Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, getErr)
	require.Len(t, got.OwnerReferences, 1,
		"a credential written for an app that already exists was left for the reconciler to find")
	assert.Equal(t, "the-live-app", string(got.OwnerReferences[0].UID))
}

// A first deploy has no App, so the credential is written unowned and the
// reconciler binds it once the App is there.
func TestStoreGitCredentialLeavesAFirstDeploysCredentialForTheReconciler(t *testing.T) {
	clientset := k8sfake.NewClientset()

	name, _, _, err := storeGitCredential(context.Background(), clientset, "shop-test", "web",
		"a-token", "https://git.example.com/acme/web.git", nil)
	require.NoError(t, err)

	got, getErr := clientset.CoreV1().Secrets("shop-test").Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Empty(t, got.OwnerReferences)
}
