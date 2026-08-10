package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// mustRenderBindings runs the render and fails the test on error, so the call
// sites stay readable and no test accidentally ignores a refusal.
func mustRenderBindings(t *testing.T, ctx context.Context, c crclient.Client, scheme *runtime.Scheme, owner crclient.Object, kind secretname.Kind, bindings []kipperv1.ServiceBinding) string {
	t.Helper()
	_, _, hash, err := reconcileBindingSecrets(ctx, c, scheme, owner, kind, bindings)
	require.NoError(t, err)
	return hash
}

func boundApp(bindings ...kipperv1.ServiceBinding) *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-test", UID: types.UID("uid-api")},
		Spec:       kipperv1.AppSpec{Image: "api:1", ServiceBindings: bindings},
	}
}

func postgresService() *kipperv1.Service {
	return &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop-test", UID: types.UID("uid-db")},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
}

func sharedCreds(service string, data map[string][]byte) *corev1.Secret {
	controller := true
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      service + "-credentials",
			Namespace: "shop-test",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(),
				Kind:       "Service",
				Name:       service,
				UID:        types.UID("uid-" + service),
				Controller: &controller,
			}},
		},
		Data: data,
	}
}

// The derived Secret is the service's shared credentials with the binding's own
// database substituted for the service default. Everything else, the password
// most of all, comes straight through.
func TestReconcileBindingSecrets_DerivesFromTheSharedCredentials(t *testing.T) {
	scheme := testScheme()
	app := boundApp(kipperv1.ServiceBinding{Name: "db", Database: "api_test"})
	svc := postgresService()
	creds := sharedCreds("db", map[string][]byte{
		"HOST": []byte("db.shop-test.svc"), "PORT": []byte("5432"),
		"USERNAME": []byte("kipper"), "PASSWORD": []byte("s3cret"), "NAME": []byte("app"),
	})
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, svc, creds).Build()

	mustRenderBindings(t, context.Background(), c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)

	var derived corev1.Secret
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "db-app-api-credentials", Namespace: "shop-test"}, &derived))
	assert.Equal(t, []byte("api_test"), derived.Data["NAME"], "the binding's database replaces the service default")
	assert.Equal(t, []byte("s3cret"), derived.Data["PASSWORD"])
	assert.Equal(t, []byte("db.shop-test.svc"), derived.Data["HOST"])

	require.Len(t, derived.OwnerReferences, 1, "the derived Secret must be garbage-collected with its workload")
	assert.Equal(t, "api", derived.OwnerReferences[0].Name)
}

// The reason the render moved out of the bind handler: it copied the shared
// credentials once, at bind time, and nothing revisited them. A rotated service
// password left every bound workload authenticating with the old one.
func TestReconcileBindingSecrets_FollowsARotatedServicePassword(t *testing.T) {
	scheme := testScheme()
	app := boundApp(kipperv1.ServiceBinding{Name: "db", Database: "api_test"})
	svc := postgresService()
	creds := sharedCreds("db", map[string][]byte{"PASSWORD": []byte("old"), "NAME": []byte("app")})
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, svc, creds).Build()
	ctx := context.Background()

	mustRenderBindings(t, ctx, c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)

	creds.Data["PASSWORD"] = []byte("rotated")
	require.NoError(t, c.Update(ctx, creds))
	mustRenderBindings(t, ctx, c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)

	var derived corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "db-app-api-credentials", Namespace: "shop-test"}, &derived))
	assert.Equal(t, []byte("rotated"), derived.Data["PASSWORD"],
		"a rotated service password must reach the binding without anyone re-binding")
	assert.Equal(t, []byte("api_test"), derived.Data["NAME"], "the binding keeps its own database across the rotation")
}

// An App and a Function of one name bound to one service must not share the
// derived Secret: the second render would otherwise overwrite the first's
// database and point that workload at the other's data.
func TestReconcileBindingSecrets_KindsDoNotShareADerivedSecret(t *testing.T) {
	scheme := testScheme()
	app := boundApp(kipperv1.ServiceBinding{Name: "db", Database: "app_db"})
	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-test", UID: types.UID("uid-fn-api")},
		Spec:       kipperv1.FunctionSpec{ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Database: "fn_db"}}},
	}
	svc := postgresService()
	creds := sharedCreds("db", map[string][]byte{"PASSWORD": []byte("s3cret"), "NAME": []byte("app")})
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, fn, svc, creds).Build()
	ctx := context.Background()

	mustRenderBindings(t, ctx, c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)
	mustRenderBindings(t, ctx, c, scheme, fn, secretname.KindFunction, fn.Spec.ServiceBindings)

	var appSecret, fnSecret corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "db-app-api-credentials", Namespace: "shop-test"}, &appSecret))
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "db-function-api-credentials", Namespace: "shop-test"}, &fnSecret))
	assert.Equal(t, []byte("app_db"), appSecret.Data["NAME"])
	assert.Equal(t, []byte("fn_db"), fnSecret.Data["NAME"], "the Function's render must not land on the App's Secret")
}

// The derived name is built from a field on the workload CR, which can be
// written directly, so the credentials it reads have to be provably the named
// Service's. Otherwise a binding could name any <x>-credentials in the
// namespace and have this render copy it somewhere the workload reads.
func TestReconcileBindingSecrets_RefusesCredentialsTheServiceDoesNotOwn(t *testing.T) {
	scheme := testScheme()
	app := boundApp(kipperv1.ServiceBinding{Name: "db", Database: "api_test"})
	svc := postgresService()
	// Named db-credentials, but owned by a different Service CR.
	foreign := sharedCreds("billing", map[string][]byte{"PASSWORD": []byte("not-yours")})
	foreign.Name = "db-credentials"
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, svc, foreign).Build()
	ctx := context.Background()

	_, _, _, err := reconcileBindingSecrets(ctx, c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)
	require.Error(t, err, "credentials the named Service does not own must not be copied into a workload's Secret")
	assert.Contains(t, err.Error(), "db", "the failure must name the binding an operator has to fix")

	var derived corev1.Secret
	assert.Error(t, c.Get(ctx, types.NamespacedName{Name: "db-app-api-credentials", Namespace: "shop-test"}, &derived),
		"nothing may be rendered from credentials that could not be proven")
}

// A no-op render must not restamp, or the console raises a restart banner on
// every reconcile for a credential nobody touched.
func TestReconcileBindingSecrets_StampsOnChangeOnly(t *testing.T) {
	scheme := testScheme()
	app := boundApp(kipperv1.ServiceBinding{Name: "db", Database: "api_test"})
	svc := postgresService()
	creds := sharedCreds("db", map[string][]byte{"PASSWORD": []byte("s3cret"), "NAME": []byte("app")})
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, svc, creds).Build()
	ctx := context.Background()
	name := types.NamespacedName{Name: "db-app-api-credentials", Namespace: "shop-test"}

	mustRenderBindings(t, ctx, c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)
	var first corev1.Secret
	require.NoError(t, c.Get(ctx, name, &first))
	stamp := first.Annotations[kipperv1.DataUpdatedAtAnnotation]
	require.NotEmpty(t, stamp, "the first render must stamp, so a pod already running is flagged stale")

	mustRenderBindings(t, ctx, c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)
	var second corev1.Secret
	require.NoError(t, c.Get(ctx, name, &second))
	assert.Equal(t, stamp, second.Annotations[kipperv1.DataUpdatedAtAnnotation], "a no-op render must not restamp")

	creds.Data["PASSWORD"] = []byte("rotated")
	require.NoError(t, c.Update(ctx, creds))
	mustRenderBindings(t, ctx, c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)
	var third corev1.Secret
	require.NoError(t, c.Get(ctx, name, &third))
	assert.NotEqual(t, stamp, third.Annotations[kipperv1.DataUpdatedAtAnnotation], "a real credential change must stamp")
}

// A binding without a database reads the service's shared credentials, so there
// is nothing to derive and no Secret to leave behind.
func TestReconcileBindingSecrets_SharedBindingDerivesNothing(t *testing.T) {
	scheme := testScheme()
	app := boundApp(kipperv1.ServiceBinding{Name: "db"})
	svc := postgresService()
	creds := sharedCreds("db", map[string][]byte{"PASSWORD": []byte("s3cret"), "NAME": []byte("app")})
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, svc, creds).Build()

	mustRenderBindings(t, context.Background(), c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)

	var derived corev1.Secret
	err := c.Get(context.Background(), types.NamespacedName{Name: "db-app-api-credentials", Namespace: "shop-test"}, &derived)
	assert.Error(t, err, "a binding on the shared credentials must not derive a Secret")
}

// A rotated password only propagates if the change wakes the workloads that
// derive from it. For a stable app nothing else ever would.
func TestEnqueueForServiceCredentials(t *testing.T) {
	scheme := testScheme()
	bound := boundApp(kipperv1.ServiceBinding{Name: "db", Database: "api_test"})
	shared := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-test"},
		Spec:       kipperv1.AppSpec{ServiceBindings: []kipperv1.ServiceBinding{{Name: "db"}}},
	}
	elsewhere := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Database: "other"}}},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(bound, shared, elsewhere).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	creds := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "shop-test"}}
	got := r.enqueueAppsForServiceCredentials(context.Background(), creds)

	// Both shapes put the rotated password into their pods: the one that pins a
	// database through a derived Secret, and the one that reads the shared
	// credentials directly.
	woken := map[string]string{}
	for _, req := range got {
		woken[req.Name] = req.Namespace
	}
	assert.Equal(t, map[string]string{"api": "shop-test", "web": "shop-test"}, woken,
		"every app bound to these credentials is woken, and one in another namespace is not")

	// The derived Secret is an output of the render. Treating it as an input
	// would have every render enqueue itself.
	derived := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db-app-api-credentials", Namespace: "shop-test"}}
	assert.Empty(t, r.enqueueAppsForServiceCredentials(context.Background(), derived),
		"a derived Secret must not be mistaken for the credentials it came from")

	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "api-env", Namespace: "shop-test"}}
	assert.Empty(t, r.enqueueAppsForServiceCredentials(context.Background(), other))
}

// The rollout blocker, end to end. A migrated service's credentials arrive from
// the handover that carried its Service CR, already owned by it, and a workload
// binds it with a database of its own. When those credentials were unowned the
// render was skipped, injection refused, and the same pass rolled the pod
// without its database credentials — the shape of a real failure. This drives
// both real reconcilers in the order the cluster does.
func TestMigratedServiceCredentials_StillCarryTheirBindingsAfterRollout(t *testing.T) {
	scheme := testScheme()
	ctx := context.Background()

	svc := postgresService()
	// As the migration receiver leaves it: the source cluster's password, under
	// the controller reference the receiver stamped on after creating the CR.
	migrated := sharedCreds("db", map[string][]byte{
		"HOST": []byte("db.shop-test.svc"), "PORT": []byte("5432"), "USERNAME": []byte("kipper"),
		"PASSWORD": []byte("migrated"), "NAME": []byte("app"),
	})
	migrated.Labels = map[string]string{
		"app":                     "db",
		kipperLabel:               kipperValue,
		"kipper.run/service-type": "postgres",
	}
	app := boundApp(kipperv1.ServiceBinding{Name: "db", Prefix: "DB_", Database: "api_test"})

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(svc, migrated, app).WithStatusSubresource(app).Build()

	// The Service reconciler accepts credentials it owns and leaves them alone.
	svcReconciler := &ServiceReconciler{Client: c, Scheme: scheme}
	require.NoError(t, svcReconciler.reconcileCredentialsSecret(ctx, svc))

	// Then the App reconciles, as it would on controller rollout.
	appReconciler := &AppReconciler{Client: c, Scheme: scheme}
	_, err := appReconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "api", Namespace: "shop-test"},
	})
	require.NoError(t, err)

	var derived corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "db-app-api-credentials", Namespace: "shop-test"}, &derived),
		"the derived Secret must be rendered from the adopted credentials")
	assert.Equal(t, []byte("migrated"), derived.Data["PASSWORD"], "the migrated password must carry through")
	assert.Equal(t, []byte("api_test"), derived.Data["NAME"])

	var deploy appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "api", Namespace: "shop-test"}, &deploy))
	published := podEnvGeneration(t, ctx, c, deploy.Spec.Template.Spec, "app-api-env-", "shop-test")
	assert.Equal(t, []byte("migrated"), published.Data["DB_PASSWORD"],
		"the pod must be rolled WITH its database credentials, not without them")
}

// The rabbitmq half of the render. The handler tests that carried this
// assertion were retargeted when the render moved, and the replacements covered
// postgres only — so hardcoding "NAME" at the render site left the whole suite
// green. A vhost written to NAME means the inherited VHOST=/ wins and the
// workload silently consumes another workload's queues.
func TestReconcileBindingSecrets_RabbitMQOverridesVhostNotName(t *testing.T) {
	scheme := testScheme()
	app := boundApp(kipperv1.ServiceBinding{Name: "rabbit", Database: "orders"})
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "rabbit", Namespace: "shop-test", UID: types.UID("uid-rabbit")},
		Spec:       kipperv1.ServiceSpec{Type: "rabbitmq"},
	}
	creds := sharedCreds("rabbit", map[string][]byte{
		"USERNAME": []byte("kipper"), "PASSWORD": []byte("s3cret"), "VHOST": []byte("/"),
	})
	creds.Name = "rabbit-credentials"
	creds.OwnerReferences[0].Name = "rabbit"
	creds.OwnerReferences[0].UID = types.UID("uid-rabbit")
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, svc, creds).Build()

	mustRenderBindings(t, context.Background(), c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)

	var derived corev1.Secret
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "rabbit-app-api-credentials", Namespace: "shop-test"}, &derived))
	assert.Equal(t, []byte("orders"), derived.Data["VHOST"], "a rabbitmq binding pins a vhost, not a database name")
	assert.NotContains(t, derived.Data, "NAME", "writing the vhost to NAME leaves the inherited VHOST=/ in force")
}

// A binding removed by a direct CR or GitOps edit never reaches the unbind
// handler, so the credential-bearing Secret would otherwise survive until the
// whole workload was deleted.
func TestReconcileBindingSecrets_PrunesAProjectionNoLongerWanted(t *testing.T) {
	scheme := testScheme()
	app := boundApp(kipperv1.ServiceBinding{Name: "db", Database: "api_test"})
	svc := postgresService()
	creds := sharedCreds("db", map[string][]byte{"PASSWORD": []byte("s3cret"), "NAME": []byte("app")})
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, svc, creds).Build()
	ctx := context.Background()
	name := types.NamespacedName{Name: "db-app-api-credentials", Namespace: "shop-test"}

	mustRenderBindings(t, ctx, c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)
	require.NoError(t, c.Get(ctx, name, &corev1.Secret{}))

	// The binding is edited away on the CR. The render stops wanting the
	// projection and says so; retirement is what removes it, under the grace
	// every other published object gets.
	_, keep, _, err := reconcileBindingSecrets(ctx, c, scheme, app, secretname.KindApp, nil)
	require.NoError(t, err)
	assert.False(t, keep[name.Name],
		"a projection no longer declared must stop being wanted")

	_, _, err = retireEnvSecrets(ctx, c, c, app, secretname.KindApp, "app-api-env-current", keep)
	require.NoError(t, err)

	var marked corev1.Secret
	require.NoError(t, c.Get(ctx, name, &marked))
	require.NotEmpty(t, marked.Annotations[unreferencedSinceAnnotation])
	marked.Annotations[unreferencedSinceAnnotation] =
		time.Now().Add(-2 * envRetirementGrace).UTC().Format(time.RFC3339Nano)
	require.NoError(t, c.Update(ctx, &marked))

	_, _, err = retireEnvSecrets(ctx, c, c, app, secretname.KindApp, "app-api-env-current", keep)
	require.NoError(t, err)
	assert.Error(t, c.Get(ctx, name, &corev1.Secret{}),
		"a projection no longer declared must not keep credentials alive")
}

// The digest is what rolls pods when a password rotates, so it has to change on
// a rotation and hold still otherwise — an unstable hash rolls the workload on
// every reconcile forever.
func TestReconcileBindingSecrets_HashTracksTheCredentials(t *testing.T) {
	scheme := testScheme()
	app := boundApp(
		kipperv1.ServiceBinding{Name: "zeta", Database: "z"},
		kipperv1.ServiceBinding{Name: "db", Database: "api_test"},
	)
	svc := postgresService()
	zeta := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "zeta", Namespace: "shop-test", UID: types.UID("uid-zeta")},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	creds := sharedCreds("db", map[string][]byte{"PASSWORD": []byte("old"), "NAME": []byte("app")})
	zetaCreds := sharedCreds("zeta", map[string][]byte{"PASSWORD": []byte("z"), "NAME": []byte("app")})
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, svc, zeta, creds, zetaCreds).Build()
	ctx := context.Background()

	first := mustRenderBindings(t, ctx, c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)
	again := mustRenderBindings(t, ctx, c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)
	require.NotEmpty(t, first)
	assert.Equal(t, first, again, "an unchanged render must produce the same digest, or the pods roll forever")

	creds.Data["PASSWORD"] = []byte("rotated")
	require.NoError(t, c.Update(ctx, creds))
	rotated := mustRenderBindings(t, ctx, c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)
	assert.NotEqual(t, first, rotated, "a rotated password must change the digest, or no pod is replaced")
}

// The Function reconciler's call to the render is wiring that survived its own
// revert with every test green: without it a Function with a pinned database
// never gets its derived Secret and rotation never reaches it.
func TestFunctionReconcile_RendersItsBindingSecrets(t *testing.T) {
	scheme := testScheme()
	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "shop-test", UID: types.UID("uid-resize")},
		Spec: kipperv1.FunctionSpec{
			Runtime:         "node20",
			Source:          &kipperv1.FunctionSource{Code: "module.exports = () => 'ok'"},
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_", Database: "resize_db"}},
		},
	}
	svc := postgresService()
	creds := sharedCreds("db", map[string][]byte{"PASSWORD": []byte("s3cret"), "NAME": []byte("app")})
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, svc, creds).WithStatusSubresource(fn).Build()

	r := &FunctionReconciler{Client: c, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "resize", Namespace: "shop-test"},
	})
	require.NoError(t, err)

	var derived corev1.Secret
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "db-function-resize-credentials", Namespace: "shop-test"}, &derived),
		"the Function reconciler must render its binding secrets, not only the App one")
	assert.Equal(t, []byte("resize_db"), derived.Data["NAME"])
}

// The derived name can land on an object that already exists — most plainly the
// shared credentials of a service actually called db-app-api, which produces
// the identical name. Overwriting it would make this render a way to clobber
// whatever sits there, so the render stops instead.
func TestReconcileBindingSecrets_RefusesToClobberAnObjectItDoesNotOwn(t *testing.T) {
	scheme := testScheme()
	app := boundApp(kipperv1.ServiceBinding{Name: "db", Database: "api_test"})
	svc := postgresService()
	creds := sharedCreds("db", map[string][]byte{"PASSWORD": []byte("s3cret"), "NAME": []byte("app")})

	controller := true
	squatter := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db-app-api-credentials",
			Namespace: "shop-test",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "App",
				Name: "someone-else", UID: types.UID("uid-someone-else"), Controller: &controller,
			}},
		},
		Data: map[string][]byte{"NOT": []byte("ours")},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, svc, creds, squatter).Build()
	ctx := context.Background()

	_, _, _, err := reconcileBindingSecrets(ctx, c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)
	require.Error(t, err, "the render must not overwrite an object owned by something else")

	var got corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "db-app-api-credentials", Namespace: "shop-test"}, &got))
	assert.Equal(t, []byte("ours"), got.Data["NOT"], "the other owner's data must survive untouched")
}

// The Function twin of the credentials mapper. Untested until now, so a
// rotation reaching bound Apps but not bound Functions would have looked
// identical to everything working.
func TestEnqueueFunctionsForServiceCredentials(t *testing.T) {
	scheme := testScheme()
	bound := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "shop-test"},
		Spec:       kipperv1.FunctionSpec{ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Database: "resize_db"}}},
	}
	shared := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "thumb", Namespace: "shop-test"},
		Spec:       kipperv1.FunctionSpec{ServiceBindings: []kipperv1.ServiceBinding{{Name: "db"}}},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(bound, shared).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	got := r.enqueueFunctionsForServiceCredentials(context.Background(),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "shop-test"}})

	woken := map[string]string{}
	for _, req := range got {
		woken[req.Name] = req.Namespace
	}
	assert.Equal(t, map[string]string{"resize": "shop-test", "thumb": "shop-test"}, woken,
		"a function reading the shared credentials holds the rotated password too")
}

// A service legitimately named my-app-db produces my-app-db-credentials, which
// the old substring heuristic read as derived — so its rotations woke nothing.
func TestEnqueueForServiceCredentials_ServiceNamedLikeAWorkloadKind(t *testing.T) {
	scheme := testScheme()
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-test"},
		Spec:       kipperv1.AppSpec{ServiceBindings: []kipperv1.ServiceBinding{{Name: "my-app-db", Database: "web_db"}}},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	got := r.enqueueAppsForServiceCredentials(context.Background(),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "my-app-db-credentials", Namespace: "shop-test"}})
	require.Len(t, got, 1, "a service whose name contains -app- is still a service")
	assert.Equal(t, "web", got[0].Name)
}

// Most bindings do not pin a database: they read the service's shared
// credentials straight through envFrom. Rotating that password has to roll them
// too, or the common case is exactly the one left authenticating with a
// password the service has stopped accepting.
func TestReconcileBindingSecrets_SharedBindingRotationChangesTheDigest(t *testing.T) {
	scheme := testScheme()
	app := boundApp(kipperv1.ServiceBinding{Name: "db", Prefix: "DB_"})
	svc := postgresService()
	creds := sharedCreds("db", map[string][]byte{"PASSWORD": []byte("old"), "NAME": []byte("app")})
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, svc, creds).Build()
	ctx := context.Background()

	before := mustRenderBindings(t, ctx, c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)
	require.NotEmpty(t, before, "a shared binding still contributes to the digest")

	creds.Data["PASSWORD"] = []byte("rotated")
	require.NoError(t, c.Update(ctx, creds))
	after := mustRenderBindings(t, ctx, c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)

	assert.NotEqual(t, before, after, "rotating the shared password must roll the pods reading it")

	// And nothing is derived for it — the pod reads the shared object directly.
	assert.Error(t, c.Get(ctx, types.NamespacedName{Name: "db-app-api-credentials", Namespace: "shop-test"}, &corev1.Secret{}),
		"a binding with no pinned database derives nothing")
}

// The binding label is what migration uses to decide a projection stays behind,
// so a derived Secret that lost it would travel, arrive owned by nobody, and
// wedge the workload that needs that name. The render re-asserts labels whether
// or not the credentials moved, because restoring it only on a password change
// would leave the window open until someone rotated.
func TestWriteDerivedBindingSecret_RestoresAStrippedBindingLabel(t *testing.T) {
	scheme := testScheme()
	ctx := context.Background()
	app := boundApp(kipperv1.ServiceBinding{Name: "db", Database: "api_test"})
	svc := postgresService()
	creds := sharedCreds("db", map[string][]byte{"PASSWORD": []byte("s3cret"), "NAME": []byte("app")})

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, creds, app).Build()
	_, _, _, err := reconcileBindingSecrets(ctx, c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)
	require.NoError(t, err)

	var derived corev1.Secret
	key := types.NamespacedName{Name: "db-app-api-credentials", Namespace: "shop-test"}
	require.NoError(t, c.Get(ctx, key, &derived))
	require.Equal(t, "true", derived.Labels[derivedBindingLabel])

	// Something strips the label out of band, leaving the credentials untouched.
	delete(derived.Labels, derivedBindingLabel)
	require.NoError(t, c.Update(ctx, &derived))

	_, _, _, err = reconcileBindingSecrets(ctx, c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)
	require.NoError(t, err)

	require.NoError(t, c.Get(ctx, key, &derived))
	assert.Equal(t, "true", derived.Labels[derivedBindingLabel],
		"an unchanged render must still put the label back, or the projection escapes into a migration")
}

// A restore gives the workload a new UID while its rendered projection keeps
// the old reference, or loses it. The object is still this workload's — nothing
// but this render stamps that label — and refusing to clean it up strands a
// credential nobody can clear: the render will not overwrite what it does not
// own, and rebinding needs that exact name.
func TestRetireEnvSecrets_ClearsARestoredProjection(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	controller := true

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-test", UID: types.UID("uid-after-restore")},
		Spec:       kipperv1.AppSpec{Image: "api:1"},
	}
	for _, tc := range []struct {
		name   string
		secret *corev1.Secret
		gone   bool
	}{
		{
			name: "the owner reference survived the restore but the UID did not",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: "db-app-api-credentials", Namespace: "shop-test",
				Labels: map[string]string{derivedBindingLabel: "true"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: kipperv1.GroupVersion.String(), Kind: "App",
					Name: "api", UID: types.UID("uid-before-restore"), Controller: &controller,
				}},
			}},
			gone: true,
		},
		{
			name: "the owner reference did not survive at all",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name:      "db-app-api-credentials",
				Namespace: "shop-test",
				Labels:    map[string]string{derivedBindingLabel: "true"},
			}},
			gone: true,
		},
		{
			name: "a stale reference naming a different workload is not ours",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: "db-app-api-credentials", Namespace: "shop-test",
				Labels: map[string]string{derivedBindingLabel: "true"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: kipperv1.GroupVersion.String(), Kind: "App",
					Name: "billing", UID: types.UID("uid-billing"), Controller: &controller,
				}},
			}},
			gone: false,
		},
		{
			name: "an object without the render's label is somebody else's",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: "db-app-api-credentials", Namespace: "shop-test",
			}},
			gone: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Marked long ago, so the grace is not what this is testing.
			if tc.secret.Annotations == nil {
				tc.secret.Annotations = map[string]string{}
			}
			tc.secret.Annotations[unreferencedSinceAnnotation] =
				time.Now().Add(-2 * envRetirementGrace).UTC().Format(time.RFC3339Nano)

			c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, tc.secret).Build()
			_, _, err := retireEnvSecrets(ctx, c, c, app, secretname.KindApp, "app-api-env-current", nil)
			require.NoError(t, err)

			err = c.Get(ctx, types.NamespacedName{Name: "db-app-api-credentials", Namespace: "shop-test"}, &corev1.Secret{})
			if tc.gone {
				assert.True(t, kerrors.IsNotFound(err), "a projection this workload rendered must be clearable")
			} else {
				assert.NoError(t, err, "an object this workload did not render is not its to delete")
			}
		})
	}
}

// The prefix used to live on the container's EnvFrom entry, so renaming it
// changed the pod template and rolled the workload. Once the environment is one
// flattened generation the prefix is only inside it, and a workload whose
// variables were renamed from DB_ to POSTGRES_ would otherwise keep running the
// old names with nothing on the template to say so.
func TestReconcileBindingSecrets_APrefixChangeChangesTheFingerprint(t *testing.T) {
	scheme := testScheme()
	ctx := context.Background()
	creds := sharedCreds("db", map[string][]byte{"PASSWORD": []byte("s3cret")})

	before := boundApp(kipperv1.ServiceBinding{Name: "db", Database: "api_test"})
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(before, postgresService(), creds).Build()
	first := mustRenderBindings(t, ctx, c, scheme, before, secretname.KindApp, before.Spec.ServiceBindings)

	after := boundApp(kipperv1.ServiceBinding{Name: "db", Database: "api_test", Prefix: "POSTGRES_"})
	second := mustRenderBindings(t, ctx, c, scheme, after, secretname.KindApp, after.Spec.ServiceBindings)

	assert.NotEqual(t, first, second,
		"renaming a binding's variables must reach the pod template")
}

// Two bindings can define the same variable, and the later one wins. Swapping
// them changes what the pod reads while every credential stays identical, so
// only the declared order distinguishes the two environments.
func TestReconcileBindingSecrets_ReorderingBindingsChangesTheFingerprint(t *testing.T) {
	scheme := testScheme()
	ctx := context.Background()

	primary := kipperv1.ServiceBinding{Name: "db", Database: "api_test", Prefix: "DB_"}
	replica := kipperv1.ServiceBinding{Name: "db-replica", Database: "api_test", Prefix: "DB_"}

	replicaSvc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-replica", Namespace: "shop-test", UID: types.UID("uid-db-replica"),
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
	}
	objs := []crclient.Object{
		postgresService(), replicaSvc,
		sharedCreds("db", map[string][]byte{"HOST": []byte("primary")}),
		sharedCreds("db-replica", map[string][]byte{"HOST": []byte("replica")}),
	}

	forward := boundApp(primary, replica)
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(append([]crclient.Object{forward}, objs...)...).Build()
	first := mustRenderBindings(t, ctx, c, scheme, forward, secretname.KindApp, forward.Spec.ServiceBindings)

	reversed := boundApp(replica, primary)
	second := mustRenderBindings(t, ctx, c, scheme, reversed, secretname.KindApp, reversed.Spec.ServiceBindings)

	assert.NotEqual(t, first, second,
		"swapping two bindings changes which one wins DB_HOST, so it must reach the pod template")
}

// A fingerprint that moved on its own would roll every bound workload on every
// pass, which is why the bindings were sorted before the walk in the first
// place. Reading the CR's own order has to keep that property.
func TestReconcileBindingSecrets_TheFingerprintIsStableAcrossPasses(t *testing.T) {
	scheme := testScheme()
	ctx := context.Background()
	app := boundApp(
		kipperv1.ServiceBinding{Name: "db", Database: "api_test"},
		kipperv1.ServiceBinding{Name: "cache", Database: "1"},
	)
	cacheSvc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cache", Namespace: "shop-test", UID: types.UID("uid-cache"),
		},
		Spec: kipperv1.ServiceSpec{Type: "redis"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, postgresService(), cacheSvc,
		sharedCreds("db", map[string][]byte{"PASSWORD": []byte("s3cret")}),
		sharedCreds("cache", map[string][]byte{"PASSWORD": []byte("r3dis")})).Build()

	first := mustRenderBindings(t, ctx, c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)
	for i := 0; i < 5; i++ {
		assert.Equal(t, first,
			mustRenderBindings(t, ctx, c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings),
			"an unchanged workload must not roll")
	}
}

// While a workload still has revisions from before it moved to published
// environments, those name a derived projection directly. Removing the binding
// used to delete it at once, taking an env source away from something that
// re-reads it on every container restart.
func TestRetireEnvSecrets_KeepsAProjectionARetainedRevisionNames(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := boundApp()
	derived := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-app-api-credentials", Namespace: "shop-test",
			Labels: map[string]string{derivedBindingLabel: "true", "app": "api"},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(app, app.GroupVersionKind()),
			},
		},
		Data: map[string][]byte{"PASSWORD": []byte("s3cret")},
	}
	oldRevision := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "api-7f9c", Namespace: "shop-test"},
		Spec: appsv1.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "api", Image: "api:1",
					EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "db-app-api-credentials"},
					}}},
				}},
				}},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, derived, oldRevision).Build()

	// The binding is gone, so the projection is no longer wanted.
	_, _, err := retireEnvSecrets(ctx, c, c, app, secretname.KindApp, "app-api-env-current", nil)
	require.NoError(t, err)

	assert.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: "db-app-api-credentials", Namespace: "shop-test"}, &corev1.Secret{}),
		"a revision that still names it can produce a pod that reads it")
}

// A projection nothing names waits out the same grace a generation does, and
// for the same reason: an hour of absence is what stands between a deletion and
// a controller that read an older template just before the scan.
func TestRetireEnvSecrets_MarksThenSweepsAnUnreadProjection(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := boundApp()
	derived := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-app-api-credentials", Namespace: "shop-test",
			Labels: map[string]string{derivedBindingLabel: "true", "app": "api"},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(app, app.GroupVersionKind()),
			},
		},
		Data: map[string][]byte{"PASSWORD": []byte("s3cret")},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, derived).Build()

	wait, _, err := retireEnvSecrets(ctx, c, c, app, secretname.KindApp, "app-api-env-current", nil)
	require.NoError(t, err)
	assert.Equal(t, envRetirementGrace, wait, "the pass that marks asks to be woken for the grace")

	var marked corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: "db-app-api-credentials", Namespace: "shop-test"}, &marked))
	require.NotEmpty(t, marked.Annotations[unreferencedSinceAnnotation],
		"a first look marks rather than deletes")

	marked.Annotations[unreferencedSinceAnnotation] =
		time.Now().Add(-2 * envRetirementGrace).UTC().Format(time.RFC3339Nano)
	require.NoError(t, c.Update(ctx, &marked))

	_, _, err = retireEnvSecrets(ctx, c, c, app, secretname.KindApp, "app-api-env-current", nil)
	require.NoError(t, err)
	assert.True(t, kerrors.IsNotFound(c.Get(ctx, types.NamespacedName{
		Name: "db-app-api-credentials", Namespace: "shop-test"}, &corev1.Secret{})),
		"and a matured mark with nothing reading it is swept")
}
