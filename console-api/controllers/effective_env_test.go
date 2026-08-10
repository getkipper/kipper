package controllers

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// mustAppEnvSources builds an App's environment sources the way its reconciler
// does, so a test driving one half of the render drives the list production
// hands it rather than one the test made up.
func mustAppEnvSources(t *testing.T, c crclient.Client, app *kipperv1.App, links ...ResolvedLink) []envSource {
	t.Helper()
	sources, refused, err := appEnvSources(context.Background(), c, app, links, nil)
	require.NoError(t, err)
	require.Empty(t, refused, "the fixture must not refuse a binding unless the test is about refusal")
	return sources
}

// mustFunctionEnvSources resolves a Function's sources the way its reconciler
// does, once, so a test driving one pod shape drives what production hands it.
func mustFunctionEnvSources(t *testing.T, c crclient.Client, fn *kipperv1.Function) []envSource {
	t.Helper()
	sources, refused, err := functionEnvSources(context.Background(), c, fn, nil)
	require.NoError(t, err)
	require.Empty(t, refused, "the fixture must not refuse a binding unless the test is about refusal")
	return sources
}

// kubeletEnv is what a container's environment resolves to on the node, worked
// out from the pod object rather than from the sources the controller built.
//
// This is the whole point of the table test below. Asserting the table against
// the sources it came from proves only that the builder agrees with itself; the
// question is whether it agrees with the pod, so this reads the Deployment the
// controller actually wrote and applies Kubernetes' own rules: envFrom sources
// in order, each prefixed, then the container's own env, which beats all of
// them.
func kubeletEnv(t *testing.T, c crclient.Client, namespace string, container corev1.Container) map[string]string {
	t.Helper()
	env := map[string]string{}

	for _, from := range container.EnvFrom {
		require.NotNil(t, from.SecretRef, "every source Kipper builds is a Secret")
		var secret corev1.Secret
		err := c.Get(context.Background(),
			types.NamespacedName{Name: from.SecretRef.Name, Namespace: namespace}, &secret)
		if err != nil {
			// Optional envFrom: an absent Secret contributes nothing.
			require.True(t, from.SecretRef.Optional != nil && *from.SecretRef.Optional,
				"a Secret that may be absent must be referenced as optional")
			continue
		}
		for k, v := range secret.Data {
			env[from.Prefix+k] = string(v)
		}
	}

	for _, v := range container.Env {
		env[v.Name] = v.Value
	}
	return env
}

// boundEnvFixture is an App with something from every tier of its environment:
// its own spec.env, its writer Secret, two bindings whose prefixed names
// collide, and a link.
func boundEnvFixture(t *testing.T) (crclient.Client, *kipperv1.App) {
	t.Helper()
	scheme := testScheme()

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "docuseal", Namespace: "docuseal-test", UID: types.UID("uid-docuseal")},
		Spec: kipperv1.AppSpec{
			Image: "docuseal/docuseal:2.0",
			Port:  3000,
			Env: map[string]string{
				"RAILS_ENV": "production",
				"HOST":      "from-spec-env",
			},
			ServiceBindings: []kipperv1.ServiceBinding{
				{Name: "db", Prefix: "DB_"},
				{Name: "cache", Prefix: "DB_"},
			},
		},
	}

	dbSvc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "docuseal-test", UID: types.UID("uid-db")},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	cacheSvc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "docuseal-test", UID: types.UID("uid-cache")},
		Spec:       kipperv1.ServiceSpec{Type: "redis"},
	}

	controller := true
	ownedBy := func(name, service string, data map[string][]byte) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "docuseal-test",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
					Name: service, UID: types.UID("uid-" + service), Controller: &controller,
				}},
			},
			Data: data,
		}
	}

	objects := []crclient.Object{
		app, dbSvc, cacheSvc,
		ownedBy("db-credentials", "db", map[string][]byte{
			"HOST": []byte("db.docuseal-test.svc.cluster.local"), "PORT": []byte("5432"),
			"USERNAME": []byte("kipper"), "PASSWORD": []byte("p@ss:w/rd"), "NAME": []byte("docuseal"),
		}),
		ownedBy("cache-credentials", "cache", map[string][]byte{
			"HOST": []byte("cache.docuseal-test.svc.cluster.local"),
		}),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "app-docuseal-secrets", Namespace: "docuseal-test"},
			Data:       map[string][]byte{"MY_API_KEY": []byte("sk-live-1"), "RAILS_ENV": []byte("from-secrets")},
		},
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(app).Build()
	return c, app
}

// The table and the pod must agree, and the only way to know is to ask the pod.
// The precedence the plan writes down — spec.env, then the app's own secrets,
// then binding credentials, then link addresses — is exactly the kind of thing a
// test can agree with while the pod disagrees, so this reconciles for real and
// resolves the Deployment the controller wrote the way a node would.
func TestEffectiveEnv_MatchesWhatThePodResolvesTo(t *testing.T) {
	c, app := boundEnvFixture(t)
	ctx := context.Background()
	r := &AppReconciler{Client: c, Scheme: testScheme()}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: app.Name, Namespace: app.Namespace},
	})
	require.NoError(t, err)

	var deploy appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, &deploy))
	onTheNode := kubeletEnv(t, c, app.Namespace, deploy.Spec.Template.Spec.Containers[0])

	var stored kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, &stored))
	sources := mustAppEnvSources(t, c, &stored)
	table, err := effectiveEnv(ctx, c, app.Namespace, sources)
	require.NoError(t, err)

	// The table holds spec.env as written and the pod holds it resolved, so the
	// two agree on every key this fixture uses only because none of them is a
	// template. The template cases are covered below.
	for name, entry := range table {
		assert.Equal(t, onTheNode[name], entry.value, "table and pod disagree about %s", name)
	}
	for name, value := range onTheNode {
		entry, ok := table[name]
		require.True(t, ok, "the pod sees %s and the table does not", name)
		assert.Equal(t, value, entry.value)
	}

	// And the precedence itself, spelled out so a reordering is a failure and
	// not just a difference.
	assert.Equal(t, "from-secrets", table["RAILS_ENV"].value, "app-<app>-secrets beats spec.env")
	assert.Equal(t, originWriterSecret, table["RAILS_ENV"].origin)
	assert.Equal(t, "cache.docuseal-test.svc.cluster.local", table["DB_HOST"].value,
		"a later binding beats an earlier one under the same prefix")
	assert.Equal(t, originBinding, table["DB_HOST"].origin)
	assert.Equal(t, "from-spec-env", table["HOST"].value, "an unprefixed key is untouched by a prefixed binding")
}

// A second pass over an unchanged template must write nothing. Resolution runs
// on every reconcile, so a render that is not a function of the CR alone would
// rewrite the Secret each time — a write loop on every app on every cluster, and
// a restart banner in the console for a change nobody made.
func TestReconcile_ATemplateDoesNotChurnTheRender(t *testing.T) {
	c, app := boundEnvFixture(t)
	ctx := context.Background()
	stored := app.DeepCopy()
	stored.Spec.Env["DATABASE_URL"] = "postgres://${DB_USERNAME}:${DB_PASSWORD:urlencode}@${DB_HOST}/app"
	require.NoError(t, c.Update(ctx, stored))

	r := &AppReconciler{Client: c, Scheme: testScheme()}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: app.Name, Namespace: app.Namespace}}

	_, err := r.Reconcile(ctx, request)
	require.NoError(t, err)
	first := renderedEnvSecret(t, ctx, c, secretname.KindApp, "docuseal", app.Namespace)
	// DB_HOST is the cache binding's, which declares the same prefix and comes
	// second — the precedence the table reports, reaching the pod through the
	// rendered value as well.
	require.Equal(t, "postgres://kipper:p%40ss%3Aw%2Frd@cache.docuseal-test.svc.cluster.local/app",
		string(first.Data["DATABASE_URL"]))

	_, err = r.Reconcile(ctx, request)
	require.NoError(t, err)
	second := renderedEnvSecret(t, ctx, c, secretname.KindApp, "docuseal", app.Namespace)
	assert.Equal(t, first.ResourceVersion, second.ResourceVersion, "a pass that changed nothing must not write")
	assert.Equal(t, first.Annotations[kipperv1.DataUpdatedAtAnnotation], second.Annotations[kipperv1.DataUpdatedAtAnnotation])
}

// The pod is refused a binding whose credentials carry no proof Kipper made
// them, so a template must not resolve from one either. Reading every declared
// binding rather than every accepted one is the trap this whole ordering exists
// to avoid: the resolver would bake in a credential the pod never receives.
func TestAppEnvSources_RefusedBindingIsInNeitherTheTableNorThePod(t *testing.T) {
	scheme := testScheme()
	ctx := context.Background()

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-test", UID: types.UID("uid-api")},
		Spec: kipperv1.AppSpec{
			Image:           "api:1",
			Env:             map[string]string{"DATABASE_URL": "postgres://${DB_USERNAME}@${DB_HOST}/app"},
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_"}},
		},
	}
	db := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop-test", UID: types.UID("uid-db")},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	// Present under the name the binding maps to, owned by nothing that ties it
	// to this Service — the shape a hand-written CR uses to name someone else's
	// Secret.
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "shop-test"},
		Data:       map[string][]byte{"USERNAME": []byte("root"), "HOST": []byte("elsewhere")},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, db, foreign).WithStatusSubresource(app).Build()

	sources, refused, err := appEnvSources(ctx, c, app, nil, nil)
	require.NoError(t, err)
	require.Len(t, refused, 1, "the binding must be refused")

	table, err := effectiveEnv(ctx, c, app.Namespace, sources)
	require.NoError(t, err)
	assert.NotContains(t, table, "DB_USERNAME", "a refused binding must not reach the table")

	resolved, diag := renderEnv(app.Spec.Env, table)
	assert.Equal(t, "postgres://${DB_USERNAME}@${DB_HOST}/app", resolved["DATABASE_URL"],
		"with no credential in scope the reference must survive rather than resolve to something else")
	assert.Equal(t, []string{"DB_HOST", "DB_USERNAME"}, diag.unresolved)

	// And the reconcile stops rather than rolling the pod without it.
	r := &AppReconciler{Client: c, Scheme: scheme}
	_, err = r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: app.Name, Namespace: app.Namespace},
	})
	require.Error(t, err, "a refused binding must stop the pass, not render half an environment")
}

// A Function's env Secret is read by its HTTP pod and by every batch run, so a
// value resolved against a variable only one shape carries would be written once
// and read by the other. KIPPER_MODE is therefore absent from the shared table
// and present on the batch pod.
func TestFunctionEnvSources_ModesDifferAndTheSharedTableHoldsNeither(t *testing.T) {
	scheme := testScheme()
	ctx := context.Background()

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "media-test", UID: types.UID("uid-resize")},
		Spec: kipperv1.FunctionSpec{
			Runtime: "node",
			Source:  &kipperv1.FunctionSource{Code: "module.exports = () => 'ok'"},
			Env:     map[string]string{"NOTE": "mode is ${KIPPER_MODE}"},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn).WithStatusSubresource(fn).Build()

	// One resolution, three shapes derived from it — which is the point: the
	// cluster is asked once, so no shape can reach a different answer.
	resolved, _, err := functionEnvSources(ctx, c, fn, nil)
	require.NoError(t, err)
	serving := withFunctionRuntime(fn, resolved, functionServing, "")
	batch := withFunctionRuntime(fn, resolved, functionBatch, "cron")
	shared := withFunctionRuntime(fn, resolved, functionShared, "")

	servingEnv := directEnv(serving)
	batchEnv := directEnv(batch)
	assert.Equal(t, []corev1.EnvVar{{Name: "KIPPER_FUNCTION_PATH", Value: "/app/function/index.js"}}, servingEnv)
	assert.Equal(t, []corev1.EnvVar{
		{Name: "KIPPER_MODE", Value: "batch"},
		{Name: "KIPPER_TRIGGER", Value: "cron"},
		{Name: "KIPPER_FUNCTION_PATH", Value: "/app/function/index.js"},
	}, batchEnv)

	table, err := effectiveEnv(ctx, c, fn.Namespace, shared)
	require.NoError(t, err)
	assert.NotContains(t, table, "KIPPER_MODE", "one Secret serves both shapes, so neither shape's own variables may resolve")

	rendered, diag := renderEnv(fn.Spec.Env, table)
	assert.Equal(t, "mode is ${KIPPER_MODE}", rendered["NOTE"])
	assert.Equal(t, []string{"KIPPER_MODE"}, diag.unresolved)
}

// Emptying spec.env must clear the Secret rather than resolve against what the
// last pass wrote into it. Reading the env Secret back as a source would make
// the render depend on its own output — a resolved password would become the
// input of the next resolution and survive the template being deleted.
func TestEffectiveEnv_DoesNotReadBackTheSecretItRenders(t *testing.T) {
	scheme := testScheme()
	ctx := context.Background()

	app := newTestApp()
	app.Spec.Env = nil
	lastRender := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-my-app-env", Namespace: app.Namespace},
		Data:       map[string][]byte{"DATABASE_URL": []byte("postgres://kipper:s3cret@db/app")},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, lastRender).Build()

	table, err := effectiveEnv(ctx, c, app.Namespace, mustAppEnvSources(t, c, app))
	require.NoError(t, err)
	assert.NotContains(t, table, "DATABASE_URL", "the render's own output must never be one of its inputs")
}

// Kubernetes gives the container's own env precedence over every envFrom
// source, whatever order they were written in. Today every direct source is
// appended last, so following the list would give the same answer — and the day
// one is not, the table would report a value the pod overrides.
func TestEffectiveEnv_DirectEnvWinsWhereverItSitsInTheList(t *testing.T) {
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-api-secrets", Namespace: "shop-test"},
		Data:       map[string][]byte{"PORT": []byte("from-the-secret")},
	}).Build()

	table, err := effectiveEnv(context.Background(), c, "shop-test", []envSource{
		{origin: originLink, values: []corev1.EnvVar{{Name: "PORT", Value: "from-the-container"}}, held: true, direct: true},
		{origin: originWriterSecret, secret: "app-api-secrets"}, //nolint:gosec // G101: a Secret's name, not a credential
	})
	require.NoError(t, err)
	assert.Equal(t, "from-the-container", table["PORT"].value)
	assert.Equal(t, originLink, table["PORT"].origin)
}

func TestRenderEnv(t *testing.T) {
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "app-api-secrets", Namespace: "shop-test"},
			Data: map[string][]byte{
				"MY_API_KEY": []byte("sk-live-1"),
				// A value that looks like a template but is not one: only
				// spec.env is resolved, so this is literal text.
				"SPRING_URL": []byte("jdbc:postgresql://${DB_HOST}/shop"),
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "shop-test"},
			Data: map[string][]byte{
				"PASSWORD": []byte("p@ss:w/rd"),
				"HOST":     []byte("db.shop-test.svc.cluster.local"),
			},
		},
	).Build()

	// The table the render resolves against, built by the production flattener
	// from the sources an App's reconciler produces: spec.env lowest, then the
	// app's own secrets, then a prefixed binding.
	tableFor := func(t *testing.T, env map[string]string) map[string]envEntry {
		t.Helper()
		table, err := effectiveEnv(context.Background(), c, "shop-test", []envSource{
			{origin: originSpecEnv, secret: secretname.Env(secretname.KindApp, "api"), values: envVarsOf(env), held: true},
			{origin: originWriterSecret, secret: "app-api-secrets"}, //nolint:gosec // G101: a Secret's name, not a credential
			{origin: originBinding, secret: "db-credentials", prefix: "DB_", service: "db"},
		})
		require.NoError(t, err)
		return table
	}

	t.Run("a credential is composed into a URL and encoded for it", func(t *testing.T) {
		env := map[string]string{
			"DATABASE_URL": "postgres://kipper:${DB_PASSWORD:urlencode}@${DB_HOST}/app",
		}
		resolved, diag := renderEnv(env, tableFor(t, env))
		assert.Equal(t, "postgres://kipper:p%40ss%3Aw%2Frd@db.shop-test.svc.cluster.local/app", resolved["DATABASE_URL"])
		assert.True(t, diag.empty())
	})

	t.Run("a value from the app's own secrets resolves", func(t *testing.T) {
		env := map[string]string{"AUTH": "Bearer ${MY_API_KEY}"}
		resolved, _ := renderEnv(env, tableFor(t, env))
		assert.Equal(t, "Bearer sk-live-1", resolved["AUTH"])
	})

	t.Run("a typo survives verbatim and is reported", func(t *testing.T) {
		env := map[string]string{"URL": "postgres://${DB_HSOT}/app"}
		resolved, diag := renderEnv(env, tableFor(t, env))
		assert.Equal(t, "postgres://${DB_HSOT}/app", resolved["URL"],
			"an unknown name reaches the process as written, so the connection error names it")
		assert.Equal(t, []string{"DB_HSOT"}, diag.unresolved)
	})

	t.Run("a cycle terminates", func(t *testing.T) {
		// One pass, and lookups return the raw value, so A takes B's template
		// and stops there rather than following it back.
		env := map[string]string{"A": "${B}", "B": "${A}"}
		resolved, _ := renderEnv(env, tableFor(t, env))
		assert.Equal(t, "${A}", resolved["A"])
		assert.Equal(t, "${B}", resolved["B"])
	})

	t.Run("the escape yields a literal", func(t *testing.T) {
		env := map[string]string{"LOG_FORMAT": "$${DB_PASSWORD} at ${DB_HOST}"}
		resolved, diag := renderEnv(env, tableFor(t, env))
		assert.Equal(t, "${DB_PASSWORD} at db.shop-test.svc.cluster.local", resolved["LOG_FORMAT"])
		assert.True(t, diag.empty(), "an escaped reference is literal text, not an unresolved name")
	})

	t.Run("a shadowed key is reported", func(t *testing.T) {
		env := map[string]string{"DB_HOST": "localhost"}
		_, diag := renderEnv(env, tableFor(t, env))
		require.Len(t, diag.shadowed, 1)
		assert.Contains(t, diag.shadowed[0], "DB_HOST")
		assert.Contains(t, diag.shadowed[0], "db", "the report must name what overrides it")
	})

	t.Run("a reference to another template is reported", func(t *testing.T) {
		env := map[string]string{
			"HOST_TEMP": "${DB_HOST}",
			"URL":       "postgres://${HOST_TEMP}/app",
		}
		resolved, diag := renderEnv(env, tableFor(t, env))
		assert.Equal(t, "postgres://${DB_HOST}/app", resolved["URL"],
			"one pass: the reference inside the resolved value stays literal")
		require.Len(t, diag.transitive, 1)
		assert.Contains(t, diag.transitive[0], "URL references HOST_TEMP")
	})

	t.Run("a secret value that looks like a template is left alone", func(t *testing.T) {
		env := map[string]string{"COPY": "${SPRING_URL}"}
		resolved, diag := renderEnv(env, tableFor(t, env))
		assert.Equal(t, "jdbc:postgresql://${DB_HOST}/shop", resolved["COPY"],
			"a Secret value is resolved from, never resolved in")
		assert.Empty(t, diag.transitive,
			"only spec.env is resolved, so ${...} inside a Secret value is text nobody was going to resolve")
	})
}

// A template is a property of the CR, so the same CR applied to another
// environment resolves against that environment's credentials. Without this,
// `kip export` from staging and apply to production would carry staging's
// password in the exported YAML, which is the whole reason the template exists.
func TestReconcile_ATemplateReResolvesInAnotherEnvironment(t *testing.T) {
	scheme := testScheme()
	ctx := context.Background()

	build := func(namespace, password string) map[string][]byte {
		controller := true
		app := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "docuseal", Namespace: namespace, UID: types.UID("uid-" + namespace)},
			Spec: kipperv1.AppSpec{
				Image: "docuseal/docuseal:2.0", Port: 3000,
				Env:             map[string]string{"DATABASE_URL": "postgres://${DB_USERNAME}:${DB_PASSWORD:urlencode}@${DB_HOST}/docuseal"},
				ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_"}},
			},
		}
		svc := &kipperv1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: namespace, UID: types.UID("uid-db-" + namespace)},
			Spec:       kipperv1.ServiceSpec{Type: "postgres"},
		}
		creds := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "db-credentials", Namespace: namespace,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
					Name: "db", UID: types.UID("uid-db-" + namespace), Controller: &controller,
				}},
			},
			Data: map[string][]byte{
				"USERNAME": []byte("kipper"), "PASSWORD": []byte(password),
				"HOST": []byte("db." + namespace + ".svc.cluster.local"),
			},
		}
		c := crfake.NewClientBuilder().WithScheme(scheme).
			WithObjects(app, svc, creds).WithStatusSubresource(app).Build()
		r := &AppReconciler{Client: c, Scheme: scheme}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "docuseal", Namespace: namespace}})
		require.NoError(t, err)

		rendered := renderedEnvSecret(t, ctx, c, secretname.KindApp, "docuseal", namespace)

		var stored kipperv1.App
		require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "docuseal", Namespace: namespace}, &stored))
		assert.Equal(t, "postgres://${DB_USERNAME}:${DB_PASSWORD:urlencode}@${DB_HOST}/docuseal",
			stored.Spec.Env["DATABASE_URL"], "the CR keeps the template, which is what kip export copies")
		return rendered.Data
	}

	staging := build("docuseal-test", "staging-pw")
	production := build("docuseal-prod", "prod-pw")

	assert.Equal(t, "postgres://kipper:staging-pw@db.docuseal-test.svc.cluster.local/docuseal",
		string(staging["DATABASE_URL"]))
	assert.Equal(t, "postgres://kipper:prod-pw@db.docuseal-prod.svc.cluster.local/docuseal",
		string(production["DATABASE_URL"]))
}

// A link's address is one of the things worth composing a URL from, and it is
// the only source the pod carries as container.Env rather than envFrom.
func TestRenderEnv_ResolvesALinkAddress(t *testing.T) {
	scheme := testScheme()
	ctx := context.Background()

	caller := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "portal", Namespace: "acme-test", UID: types.UID("uid-portal")},
		Spec: kipperv1.AppSpec{
			Image: "portal:1", Port: 8080,
			Env:   map[string]string{"SIGN_CALLBACK": "${DOCUSEAL_URL}/callback"},
			Links: []kipperv1.AppLink{{App: "docuseal", Namespace: "acme-test"}},
		},
	}
	target := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "docuseal", Namespace: "acme-test"},
		Spec:       kipperv1.AppSpec{Image: "docuseal:2", Port: 3000},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(caller, target).Build()

	links, blocked, err := ResolveLinks(ctx, c, caller)
	require.NoError(t, err)
	require.Empty(t, blocked)

	sources := mustAppEnvSources(t, c, caller, links...)
	table, err := effectiveEnv(ctx, c, caller.Namespace, sources)
	require.NoError(t, err)

	resolved, diag := renderEnv(caller.Spec.Env, table)
	assert.Equal(t, "http://docuseal.acme-test.svc.cluster.local:3000/callback", resolved["SIGN_CALLBACK"])
	assert.True(t, diag.empty())
}

// Nothing renders a Job's env Secret in a way that could resolve a credential,
// because a Job binds no services. What it can do is compose from its own
// entries, and the condition says so when it cannot.
func TestJobEnv_ResolvesFromItsOwnEntriesAndReachesThePod(t *testing.T) {
	scheme := testScheme()
	ctx := context.Background()

	job := &kipperv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "migrate", Namespace: "shop-test", UID: types.UID("uid-migrate")},
		Spec: kipperv1.JobSpec{
			Image:   "migrate:1",
			Command: []string{"rails", "db:migrate"},
			Env: map[string]string{
				"DB_HOST":      "db.shop-test.svc.cluster.local",
				"DATABASE_URL": "postgres://${DB_HOST}/shop",
				"MISSING":      "${DB_PASSWORD}",
			},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(job).WithStatusSubresource(job).Build()
	r := &JobReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "migrate", Namespace: "shop-test"}})
	require.NoError(t, err)

	rendered := renderedEnvSecret(t, ctx, c, secretname.KindJob, "migrate", "shop-test")
	assert.Equal(t, "postgres://db.shop-test.svc.cluster.local/shop", string(rendered.Data["DATABASE_URL"]))
	assert.Equal(t, "${DB_PASSWORD}", string(rendered.Data["MISSING"]))

	var native batchv1.Job
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "migrate", Namespace: "shop-test"}, &native))
	published := podEnvGeneration(t, ctx, c, native.Spec.Template.Spec, "job-migrate-env-", "shop-test")
	assert.Equal(t, "postgres://db.shop-test.svc.cluster.local/shop", string(published.Data["DATABASE_URL"]),
		"the name the pod reads must be the name the controller wrote")

	var stored kipperv1.Job
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "migrate", Namespace: "shop-test"}, &stored))
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, kipperv1.ConditionEnvResolved)
	require.NotNil(t, cond, "a Job renders the same templates and carries the same condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Contains(t, cond.Message, "DB_PASSWORD")
}

func TestApplyEnvResolvedCondition(t *testing.T) {
	t.Run("a workload with no environment carries no condition", func(t *testing.T) {
		conditions := []metav1.Condition{{
			Type: kipperv1.ConditionEnvResolved, Status: metav1.ConditionFalse,
			Reason: "UnresolvedReferences", Message: "stale", LastTransitionTime: metav1.Now(),
		}}
		applyEnvResolvedCondition(&conditions, 1, 0, envDiagnostics{})
		assert.Nil(t, apimeta.FindStatusCondition(conditions, kipperv1.ConditionEnvResolved),
			"removing the last variable must clear the warning about it")
	})

	t.Run("the reason names the most serious problem", func(t *testing.T) {
		var conditions []metav1.Condition
		applyEnvResolvedCondition(&conditions, 1, 2, envDiagnostics{
			unresolved: []string{"DB_HSOT"},
			shadowed:   []string{"PORT (by db)"},
		})
		cond := apimeta.FindStatusCondition(conditions, kipperv1.ConditionEnvResolved)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, "UnresolvedReferences", cond.Reason)
		assert.Contains(t, cond.Message, "DB_HSOT")
		assert.Contains(t, cond.Message, "PORT", "the message carries every finding, not only the one the reason names")
	})

	t.Run("everything resolved", func(t *testing.T) {
		var conditions []metav1.Condition
		applyEnvResolvedCondition(&conditions, 3, 1, envDiagnostics{})
		cond := apimeta.FindStatusCondition(conditions, kipperv1.ConditionEnvResolved)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, int64(3), cond.ObservedGeneration)
	})
}

// A rotation must compose the new password in the same pass that writes it.
//
// The manager's client answers reads from an informer cache, so a Get issued
// moments after a write returns the object as it was before it. The pod-template
// hash, meanwhile, is computed from the data in hand. Reading the derived Secret
// back would therefore roll pods carrying the new password through envFrom and
// the old one composed into DATABASE_URL — and the corrective re-render on the
// next pass changes no hash, so nothing rolls again and the pod stays split.
//
// The fake client reads its own writes, so it cannot reproduce that miss. What
// this pins instead is the property that makes the miss impossible: the render
// takes its binding values from the snapshot, and the cluster is never asked.
func TestBindingSources_RenderFromTheSnapshotRatherThanARead(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-test", UID: types.UID("uid-api")},
		Spec: kipperv1.AppSpec{
			Image:           "api:1",
			Env:             map[string]string{"DATABASE_URL": "postgres://${DB_USERNAME}:${DB_PASSWORD}@${DB_HOST}/shop"},
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_"}},
		},
	}
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop-test", UID: types.UID("uid-db")},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	controller := true
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-credentials", Namespace: "shop-test",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
				Name: "db", UID: types.UID("uid-db"), Controller: &controller,
			}},
		},
		// What the cluster still holds: the password before the rotation.
		Data: map[string][]byte{
			"USERNAME": []byte("kipper"), "PASSWORD": []byte("old-password"),
			"HOST": []byte("db.shop-test.svc.cluster.local"),
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, svc, creds).Build()

	// What this pass rendered: the rotated password, not yet visible to a read.
	rendered := renderedBindings{}
	rendered.keep("db-credentials", map[string][]byte{
		"USERNAME": []byte("kipper"), "PASSWORD": []byte("new-password"),
		"HOST": []byte("db.shop-test.svc.cluster.local"),
	})

	sources, refused, err := appEnvSources(ctx, c, app, nil, rendered)
	require.NoError(t, err)
	require.Empty(t, refused)

	table, err := effectiveEnv(ctx, c, app.Namespace, sources)
	require.NoError(t, err)
	resolved, _ := renderEnv(app.Spec.Env, table)
	assert.Equal(t, "postgres://kipper:new-password@db.shop-test.svc.cluster.local/shop", resolved["DATABASE_URL"],
		"the composed value must carry what this pass rendered, not what a read of the cluster returns")
}

// The snapshot is a copy, so the render cannot be changed by whatever the
// caller does with its map afterwards.
func TestRenderedBindings_HoldsACopy(t *testing.T) {
	data := map[string][]byte{"PASSWORD": []byte("rendered")}
	rendered := renderedBindings{}
	rendered.keep("db-credentials", data)

	data["PASSWORD"] = []byte("changed underneath")
	delete(data, "PASSWORD")

	vars, ok := rendered.envVars("db-credentials")
	require.True(t, ok)
	require.Len(t, vars, 1)
	assert.Equal(t, "rendered", vars[0].Value)
}

// A binding this pass rendered needs no injection gate. Producing it required
// the Service's own controller-owner UID to match, which is what the gate
// checks and then some — and asking again would ask the cache, which is a pass
// behind the write that just happened.
func TestBindingSources_AcceptWhatThisPassRenderedWithoutReadingItBack(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-test", UID: types.UID("uid-api")},
		Spec: kipperv1.AppSpec{
			Image:           "api:1",
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_", Database: "api_db"}},
		},
	}
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop-test", UID: types.UID("uid-db")},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	// The derived Secret is deliberately absent from the cluster, standing in
	// for an informer that has not caught up with this pass's own write.
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, svc).Build()

	rendered := renderedBindings{}
	rendered.keep("db-app-api-credentials", map[string][]byte{"PASSWORD": []byte("s3cret"), "NAME": []byte("api_db")})

	sources, refused, err := appEnvSources(ctx, c, app, nil, rendered)
	require.NoError(t, err)
	assert.Empty(t, refused, "a binding this pass rendered must not be refused for not being readable yet")

	table, err := effectiveEnv(ctx, c, app.Namespace, sources)
	require.NoError(t, err)
	assert.Equal(t, "s3cret", table["DB_PASSWORD"].value)
	assert.Equal(t, "api_db", table["DB_NAME"].value)
}

// An unbound workload used to carry the SHA-256 of nothing as a pod annotation,
// which means nothing and costs it a restart to acquire.
func TestReconcileBindingSecrets_NoBindingsMeansNoHash(t *testing.T) {
	scheme := testScheme()
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-test", UID: types.UID("uid-api")},
		Spec:       kipperv1.AppSpec{Image: "api:1"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).Build()

	_, _, hash, err := reconcileBindingSecrets(context.Background(), c, scheme, app, secretname.KindApp, nil)
	require.NoError(t, err)
	assert.Empty(t, hash, "a workload with no credential sources has no credential generation to stamp")
	assert.Nil(t, podAnnotations(nil, hash), "and so carries no annotation at all")
}

// A Function used to skip a binding it could not read and render the
// environment without it, so ${DB_PASSWORD} reached the process as written and
// the pod failed on its first connection with nothing naming the binding. Both
// kinds now stop the pass and keep their last good template.
//
// The stop happens in reconcileBindingSecrets, which fails closed on
// credentials it cannot render and runs before anything reads them. The
// injection gate downstream is belt to that brace: since the render hands its
// own output to the table, a binding that reached the gate has already been
// verified. What this drives is the path that fires.
func TestReconcile_FunctionRefusesRatherThanRenderingWithoutABinding(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "media-test", UID: types.UID("uid-resize")},
		Spec: kipperv1.FunctionSpec{
			Runtime:         "node",
			Source:          &kipperv1.FunctionSource{Code: "module.exports = () => 'ok'"},
			Env:             map[string]string{"DATABASE_URL": "postgres://${DB_USERNAME}@${DB_HOST}/media"},
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_"}},
		},
	}
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "media-test", UID: types.UID("uid-db")},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	// Sitting under the name the binding maps to, owned by nothing that ties it
	// to this Service — the shape a hand-written CR uses to name an object it
	// was never given.
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "media-test"},
		Data:       map[string][]byte{"USERNAME": []byte("root"), "HOST": []byte("elsewhere")},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(fn, svc, foreign).WithStatusSubresource(fn).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "resize", Namespace: "media-test"}})
	require.Error(t, err, "credentials that cannot be rendered must stop the pass")

	err = c.Get(ctx, types.NamespacedName{Name: "function-resize-env", Namespace: "media-test"}, &corev1.Secret{})
	assert.True(t, kerrors.IsNotFound(err),
		"nothing may be rendered from a table missing a credential the function declares")

	var stored kipperv1.Function
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "resize", Namespace: "media-test"}, &stored))
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, kipperv1.ConditionServiceBindingsReady)
	require.NotNil(t, cond, "a Function reports it, as an App does — silence is what left the pod to fail on its first connection")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Contains(t, cond.Message, "db")
}

// A fixed cause must stop being reported as a live one, which means the
// condition has to transition rather than merely be written. Starting from a
// clean object would prove only that a good reconcile writes True.
func TestReconcile_FunctionBindingConditionClearsWhenUsable(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "media-test", UID: types.UID("uid-resize")},
		Spec: kipperv1.FunctionSpec{
			Runtime:         "node",
			Source:          &kipperv1.FunctionSource{Code: "module.exports = () => 'ok'"},
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_"}},
		},
		// The state a previous pass left behind when the credentials were not
		// yet usable.
		Status: kipperv1.FunctionStatus{Conditions: []metav1.Condition{{
			Type:               kipperv1.ConditionServiceBindingsReady,
			Status:             metav1.ConditionFalse,
			Reason:             "BindingSecretUnusable",
			Message:            "not injected: db (no usable db-credentials Secret)",
			LastTransitionTime: metav1.Now(),
		}}},
	}
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "media-test", UID: types.UID("uid-db")},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	controller := true
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-credentials", Namespace: "media-test",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
				Name: "db", UID: types.UID("uid-db"), Controller: &controller,
			}},
		},
		Data: map[string][]byte{"USERNAME": []byte("kipper"), "HOST": []byte("db.media-test.svc")},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(fn, svc, creds).WithStatusSubresource(fn).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "resize", Namespace: "media-test"}})
	require.NoError(t, err)

	var stored kipperv1.Function
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "resize", Namespace: "media-test"}, &stored))
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, kipperv1.ConditionServiceBindingsReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "a refusal that has been fixed must stop being reported")
	assert.Equal(t, "BindingsInjected", cond.Reason)
}

// A reconcile writes Secrets this controller owns, and it now watches the
// Secrets it owns. That is only safe because every write is skipped when
// nothing changed — otherwise each pass would wake the next one for ever.
func TestReconcile_FunctionSettlesRatherThanRewritingItself(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "media-test", UID: types.UID("uid-resize")},
		Spec: kipperv1.FunctionSpec{
			Runtime: "node",
			Source:  &kipperv1.FunctionSource{Code: "module.exports = () => 'ok'"},
			Env:     map[string]string{"LOG_LEVEL": "debug"},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn).WithStatusSubresource(fn).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "resize", Namespace: "media-test"}}

	_, err := r.Reconcile(ctx, request)
	require.NoError(t, err)
	first := renderedEnvSecret(t, ctx, c, secretname.KindFunction, "resize", "media-test")

	_, err = r.Reconcile(ctx, request)
	require.NoError(t, err)
	second := renderedEnvSecret(t, ctx, c, secretname.KindFunction, "resize", "media-test")

	assert.Equal(t, first.ResourceVersion, second.ResourceVersion,
		"a pass that changed nothing must not write, or watching what it writes never settles")
}

// A `database` on a service type that has no logical namespace — redis has no
// databases to carve up — is not a derived binding. The name, the render and
// the provenance gate must all say so: when only the name knew, the pod
// referenced a Secret nothing renders; when only the gate knew, the shared
// credentials were refused for not being owned by the workload. Either way the
// binding disappeared from the pod, and on the batch path there is no snapshot
// to paper over it.
func TestBindingSources_ADatabaseOnATypeWithoutOneStillReadsTheSharedCredentials(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "media-test", UID: types.UID("uid-resize")},
		Spec: kipperv1.FunctionSpec{
			Runtime:         "node",
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "cache", Prefix: "REDIS_", Database: "2"}},
		},
	}
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "media-test", UID: types.UID("uid-cache")},
		Spec:       kipperv1.ServiceSpec{Type: "redis"},
	}
	controller := true
	shared := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cache-credentials", Namespace: "media-test",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
				Name: "cache", UID: types.UID("uid-cache"), Controller: &controller,
			}},
		},
		Data: map[string][]byte{"HOST": []byte("cache.media-test.svc"), "PASSWORD": []byte("s3cret")},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, svc, shared).Build()

	// No snapshot: this is the path a cron or test run builds its pod through.
	sources, refused, err := functionEnvSources(ctx, c, fn, nil)
	require.NoError(t, err)
	require.Empty(t, refused, "the shared credentials are usable and must not be refused")

	names := contributingSecretNames(sources)
	assert.True(t, names["cache-credentials"], "the pod must read the Secret that actually holds the credentials")
	assert.False(t, names["cache-function-resize-credentials"], "nothing renders a derived Secret for a type with no logical namespace")

	table, err := effectiveEnv(ctx, c, fn.Namespace, sources)
	require.NoError(t, err)
	assert.Equal(t, "s3cret", table["REDIS_PASSWORD"].value)
}

// A batch run started without credentials it declares fails on its first
// connection, and a manual test run is exactly where that reads as the function
// being broken rather than as the binding being refused. The reconcile has
// failed closed on this since the Function joined the App in doing so; the
// batch spec used to build the pod anyway.
func TestBuildBatchPodSpec_RefusesABindingItMayNotRead(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "media-test", UID: types.UID("uid-resize")},
		Spec: kipperv1.FunctionSpec{
			Runtime:         "node",
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_"}},
		},
	}
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "media-test", UID: types.UID("uid-db")},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	// Under the name the binding maps to, owned by nothing that ties it to the
	// Service the binding declares.
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "media-test"},
		Data:       map[string][]byte{"USERNAME": []byte("root")},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, svc, foreign).Build()

	_, err := BuildBatchPodSpec(ctx, c, fn, "test", nil)
	require.Error(t, err, "a test run must not start without credentials the function declares")
	assert.Contains(t, err.Error(), "db")
}

// A Service read that fails for a reason other than "it is not there" must stop
// the pass, not fall back to what the binding says. The render resolved the same
// question from a Service it read successfully, so a fallback here names a
// different Secret than the one it produced — and since a batch run now fails
// closed on a refused binding, the disagreement takes the CronJob with it.
func TestBindingSources_ATransientServiceReadStopsRatherThanGuessing(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-test", UID: types.UID("uid-api")},
		Spec: kipperv1.AppSpec{
			Image:           "api:1",
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "cache", Prefix: "REDIS_", Database: "2"}},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
				if _, isService := obj.(*kipperv1.Service); isService {
					return kerrors.NewInternalError(errors.New("etcd unavailable"))
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()

	_, _, err := appEnvSources(ctx, c, app, nil, nil)
	require.Error(t, err, "a transient read must not be answered with a guess")
	assert.Contains(t, err.Error(), "cache")
}

// The Function resolves its sources once and derives every pod shape from that
// one answer. It used to resolve them per shape, and each of those calls read
// the Service again — so a Service deleted between the reconcile's refusal gate
// and the Deployment render flipped bindingIsDerived, moved the binding to a
// Secret name nothing had rendered, and the Deployment was written without it.
// Passing the gate and then failing open in the same pass is the worst of both.
func TestFunctionSources_OneResolutionSurvivesTheServiceDisappearing(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "media-test", UID: types.UID("uid-resize")},
		Spec: kipperv1.FunctionSpec{
			Runtime:         "node",
			Source:          &kipperv1.FunctionSource{Code: "module.exports = () => 'ok'"},
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "cache", Prefix: "REDIS_", Database: "2"}},
		},
	}
	cache := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "media-test", UID: types.UID("uid-cache")},
		Spec:       kipperv1.ServiceSpec{Type: "redis"},
	}
	controller := true
	shared := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cache-credentials", Namespace: "media-test",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
				Name: "cache", UID: types.UID("uid-cache"), Controller: &controller,
			}},
		},
		Data: map[string][]byte{"HOST": []byte("cache.media-test.svc")},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, cache, shared).Build()

	resolved, refused, err := functionEnvSources(ctx, c, fn, nil)
	require.NoError(t, err)
	require.Empty(t, refused)

	// The Service goes, exactly as it could between the gate and the render.
	require.NoError(t, c.Delete(ctx, cache))

	// The Deployment the reconciler writes must still name the Secret that one
	// resolution chose. Driving withFunctionRuntime alone would prove only that
	// the helper is pure; the defect was in the wiring that called it.
	r := &FunctionReconciler{Client: c, Scheme: scheme}
	gen := mustPublishFunctionEnv(t, r, ctx, fn, resolved)
	require.NoError(t, r.reconcileDeployment(ctx, fn, "", resolved, gen, nil))

	var deploy appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "resize", Namespace: "media-test"}, &deploy))
	published := podEnvGeneration(t, ctx, c, deploy.Spec.Template.Spec, "function-resize-env-", "media-test")

	// The value could only have come from cache-credentials, which is what the
	// one resolution chose. A later read would have picked the derived name for
	// a binding that pins a database, and nothing ever rendered that Secret.
	assert.Equal(t, []byte("cache.media-test.svc"), published.Data["REDIS_HOST"],
		"the pod must keep what the pass resolved, not what a later read would pick")
	assert.Equal(t, contributingSecretNames(resolved)["cache-credentials"], true,
		"a Service that went after the gate must not move the binding to a name nothing rendered")
}

// The digest names the Secret the pod template points at, so two environments
// sharing a name would mean a pod reading one while its template promised the
// other. Length-prefixing every field is what keeps the boundaries unambiguous.
func TestEnvDigest_SeparatesMapsAConcatenationWouldCollide(t *testing.T) {
	// Concatenated in sorted order these are the same stream: "A"+"1"+"B"+"2"
	// and "A1B"+"2" are both A1B2.
	a := map[string]string{"A": "1", "B": "2"}
	b := map[string]string{"A1B": "2"}
	assert.NotEqual(t, envDigest(a), envDigest(b),
		"two environments must not share a generation name")
}

func TestEnvDigest_IsStableAcrossMapOrder(t *testing.T) {
	first := envDigest(map[string]string{"B": "2", "A": "1", "C": "3"})
	for i := 0; i < 20; i++ {
		assert.Equal(t, first, envDigest(map[string]string{"C": "3", "A": "1", "B": "2"}),
			"the same content must always name the same generation, whatever order the map iterates")
	}
}

// The published generation is what the pod reads, so it must carry what the
// render produced. The resolution table holds spec.env raw on purpose, and
// publishing that table would put a literal ${DB_PASSWORD} in the container.
func TestPublishedEnv_CarriesTheRenderOutputRatherThanTheTemplate(t *testing.T) {
	ctx := context.Background()
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-app-api-credentials", Namespace: "project-test"},
		Data:       map[string][]byte{"PASSWORD": []byte("s3cret")},
	}).Build()

	sources := []envSource{
		//nolint:gosec // the name of a Secret, not a credential
		{origin: originBinding, secret: "db-app-api-credentials", prefix: "DB_", service: "db"},
		{origin: originSpecEnv, held: true, values: []corev1.EnvVar{
			{Name: "DATABASE_URL", Value: "postgres://u:${DB_PASSWORD}@db:5432/api"},
		}},
	}

	table, err := effectiveEnv(ctx, c, "project-test", sources)
	require.NoError(t, err)
	assert.Equal(t, "postgres://u:${DB_PASSWORD}@db:5432/api", table["DATABASE_URL"].value,
		"the resolution table holds the template, which is what renderEnv resolves against")

	resolved, _ := renderEnv(map[string]string{
		"DATABASE_URL": "postgres://u:${DB_PASSWORD}@db:5432/api",
	}, table)

	published, err := publishedEnv(ctx, c, "project-test", sources, resolved)
	require.NoError(t, err)
	assert.Equal(t, "postgres://u:s3cret@db:5432/api", published["DATABASE_URL"],
		"the generation must carry the resolved value the pod is meant to read")
	assert.Equal(t, "s3cret", published["DB_PASSWORD"],
		"and the discrete credential it was composed from")
}

// A pod's own env beats every envFrom source, so a runtime variable is already
// published atomically with the generation name and must not be duplicated
// inside it.
func TestPublishedEnv_LeavesOutWhatTravelsOnThePodTemplate(t *testing.T) {
	ctx := context.Background()
	c := crfake.NewClientBuilder().WithScheme(testScheme()).Build()

	sources := []envSource{
		{origin: originSpecEnv, held: true, values: []corev1.EnvVar{{Name: "MODE", Value: "live"}}},
		{origin: originRuntime, direct: true, values: []corev1.EnvVar{{Name: "PORT", Value: "8080"}}},
	}
	published, err := publishedEnv(ctx, c, "project-test", sources,
		map[string]string{"MODE": "live"})
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"MODE": "live"}, published,
		"a direct source belongs to the pod template, not to the generation")
}

// contributingSecretNames is the set of Secrets whose contents reach the pod.
// It used to be read off the container's EnvFrom entries; the container names
// one generation now, so the question is answered by the source list that
// generation is flattened from.
func contributingSecretNames(sources []envSource) map[string]bool {
	names := map[string]bool{}
	for _, s := range sources {
		if !s.direct && s.secret != "" {
			names[s.secret] = true
		}
	}
	return names
}
