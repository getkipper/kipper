package controllers

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

type provisionCall struct {
	service, database, username string
}

func recordingProvisioner(calls *[]provisionCall, err error) DatabaseProvisioner {
	return func(_ context.Context, svc *kipperv1.Service, creds *corev1.Secret, database string) error {
		*calls = append(*calls, provisionCall{svc.Name, database, string(creds.Data["USERNAME"])})
		return err
	}
}

func boundPostgres(t *testing.T, app *kipperv1.App) crclient.Client {
	t.Helper()
	scheme := testScheme()
	creds := sharedCreds("db", map[string][]byte{"USERNAME": []byte("kipper"), "PASSWORD": []byte("s3cret"), "NAME": []byte("app")})
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, postgresService(), creds).Build()
	mustRenderBindings(t, context.Background(), c, scheme, app, secretname.KindApp, app.Spec.ServiceBindings)
	return c
}

func TestEnsureBindingDatabases_CreatesTheDatabaseOfABindingOnTheCR(t *testing.T) {
	app := boundApp(kipperv1.ServiceBinding{Name: "db", Database: "shop_prod"})
	c := boundPostgres(t, app)
	var calls []provisionCall

	declared, failures := ensureBindingDatabases(context.Background(), c, recordingProvisioner(&calls, nil), app, secretname.KindApp, app.Spec.ServiceBindings)

	assert.Equal(t, 1, declared)
	assert.Empty(t, failures)
	assert.Equal(t, []provisionCall{{"db", "shop_prod", "kipper"}}, calls)
}

func TestEnsureBindingDatabases_CreatesEachDatabaseOnce(t *testing.T) {
	app := boundApp(kipperv1.ServiceBinding{Name: "db", Database: "shop_prod"})
	c := boundPostgres(t, app)
	var calls []provisionCall
	provision := recordingProvisioner(&calls, nil)

	ensureBindingDatabases(context.Background(), c, provision, app, secretname.KindApp, app.Spec.ServiceBindings)
	ensureBindingDatabases(context.Background(), c, provision, app, secretname.KindApp, app.Spec.ServiceBindings)

	assert.Len(t, calls, 1, "a database already created must not be exec'd again on every reconcile")
}

func TestEnsureBindingDatabases_CreatesTheNewDatabaseWhenTheBindingChanges(t *testing.T) {
	app := boundApp(kipperv1.ServiceBinding{Name: "db", Database: "shop_prod"})
	c := boundPostgres(t, app)
	var calls []provisionCall
	provision := recordingProvisioner(&calls, nil)
	ensureBindingDatabases(context.Background(), c, provision, app, secretname.KindApp, app.Spec.ServiceBindings)

	app.Spec.ServiceBindings[0].Database = "shop_staging"
	mustRenderBindings(t, context.Background(), c, testScheme(), app, secretname.KindApp, app.Spec.ServiceBindings)
	ensureBindingDatabases(context.Background(), c, provision, app, secretname.KindApp, app.Spec.ServiceBindings)

	require.Len(t, calls, 2)
	assert.Equal(t, "shop_staging", calls[1].database)
}

func TestEnsureBindingDatabases_RetriesAFailedCreate(t *testing.T) {
	app := boundApp(kipperv1.ServiceBinding{Name: "db", Database: "shop_prod"})
	c := boundPostgres(t, app)
	var calls []provisionCall

	_, failures := ensureBindingDatabases(context.Background(), c, recordingProvisioner(&calls, errors.New("no running pod")), app, secretname.KindApp, app.Spec.ServiceBindings)
	require.Len(t, failures, 1)
	assert.Contains(t, failures[0], "shop_prod")
	assert.Contains(t, failures[0], "no running pod")

	_, failures = ensureBindingDatabases(context.Background(), c, recordingProvisioner(&calls, nil), app, secretname.KindApp, app.Spec.ServiceBindings)
	assert.Empty(t, failures)
	assert.Len(t, calls, 2)
}

func TestEnsureBindingDatabases_SkipsBindingsWithoutADatabase(t *testing.T) {
	app := boundApp(kipperv1.ServiceBinding{Name: "db"})
	c := boundPostgres(t, app)
	var calls []provisionCall

	declared, failures := ensureBindingDatabases(context.Background(), c, recordingProvisioner(&calls, nil), app, secretname.KindApp, app.Spec.ServiceBindings)

	assert.Zero(t, declared)
	assert.Empty(t, failures)
	assert.Empty(t, calls)
}

func TestApplyBindingDatabaseCondition(t *testing.T) {
	var conditions []metav1.Condition

	applyBindingDatabaseCondition(&conditions, 3, 1, []string{"binding \"db\": no running pod"})
	cond := apimeta.FindStatusCondition(conditions, kipperv1.ConditionBindingDatabasesReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Contains(t, cond.Message, "no running pod")

	applyBindingDatabaseCondition(&conditions, 3, 1, nil)
	cond = apimeta.FindStatusCondition(conditions, kipperv1.ConditionBindingDatabasesReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)

	applyBindingDatabaseCondition(&conditions, 3, 0, nil)
	assert.Nil(t, apimeta.FindStatusCondition(conditions, kipperv1.ConditionBindingDatabasesReady))
}

func TestAppReconcile_CreatesBindingDatabasesAndRetriesAFailure(t *testing.T) {
	scheme := testScheme()
	app := boundApp(kipperv1.ServiceBinding{Name: "db", Database: "shop_prod"})
	creds := sharedCreds("db", map[string][]byte{"USERNAME": []byte("kipper"), "PASSWORD": []byte("s3cret"), "NAME": []byte("app")})
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, postgresService(), creds).
		WithStatusSubresource(&kipperv1.App{}).Build()
	var calls []provisionCall
	r := &AppReconciler{Client: c, Scheme: scheme, Domain: "example.com",
		Provision: recordingProvisioner(&calls, errors.New("no running pod"))}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "api", Namespace: "shop-test"},
	})

	require.NoError(t, err, "a database that cannot be created yet must not stop the rest of the reconcile")
	assert.Equal(t, []provisionCall{{"db", "shop_prod", "kipper"}}, calls)
	assert.NotZero(t, result.RequeueAfter)
	assert.LessOrEqual(t, result.RequeueAfter, bindingDatabaseRetry)

	var got kipperv1.App
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "api", Namespace: "shop-test"}, &got))
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionBindingDatabasesReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
}

func TestFunctionReconcile_CreatesBindingDatabases(t *testing.T) {
	scheme := testScheme()
	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "shop-test", UID: types.UID("uid-resize")},
		Spec: kipperv1.FunctionSpec{
			Runtime:         "node20",
			Source:          &kipperv1.FunctionSource{Code: "module.exports = () => 'ok'"},
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Database: "resize_db"}},
		},
	}
	creds := sharedCreds("db", map[string][]byte{"USERNAME": []byte("kipper"), "PASSWORD": []byte("s3cret"), "NAME": []byte("app")})
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, postgresService(), creds).WithStatusSubresource(fn).Build()
	var calls []provisionCall
	r := &FunctionReconciler{Client: c, Scheme: scheme, Provision: recordingProvisioner(&calls, errors.New("no running pod"))}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "resize", Namespace: "shop-test"},
	})

	require.NoError(t, err)
	assert.Equal(t, []provisionCall{{"db", "resize_db", "kipper"}}, calls)
	assert.NotZero(t, result.RequeueAfter)
	assert.LessOrEqual(t, result.RequeueAfter, bindingDatabaseRetry)
}

// Bindings to the same service share a Secret, so it must retain both markers.
func TestEnsureBindingDatabases_TwoDatabasesOnOneServiceConverge(t *testing.T) {
	app := boundApp(
		kipperv1.ServiceBinding{Name: "db", Database: "orders", Prefix: "ORDERS_"},
		kipperv1.ServiceBinding{Name: "db", Database: "billing", Prefix: "BILLING_"},
	)
	c := boundPostgres(t, app)
	var calls []provisionCall
	provision := recordingProvisioner(&calls, nil)

	for range 3 {
		_, failures := ensureBindingDatabases(context.Background(), c, provision, app, secretname.KindApp, app.Spec.ServiceBindings)
		require.Empty(t, failures)
	}

	assert.Len(t, calls, 2, "each database is created once, however many passes run")
}
