package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/domain"
)

func httpFunction() *kipperv1.Function {
	return &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name: "scheduler", Namespace: "blog-test", UID: types.UID("uid-scheduler"),
		},
		Spec: kipperv1.FunctionSpec{
			Runtime:  "node",
			Source:   &kipperv1.FunctionSource{Code: "module.exports = () => 'ok'"},
			Triggers: []kipperv1.FunctionTrigger{{Type: "http"}},
		},
	}
}

func httpScaledObject(name, namespace string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "http.keda.sh", Version: "v1alpha1", Kind: "HTTPScaledObject",
	})
	u.SetName(name)
	u.SetNamespace(namespace)
	u.Object["spec"] = map[string]interface{}{"hosts": []interface{}{"old"}}
	return u
}

// one HTTPScaledObject outlived its Function by seven hours in production,
// with no owner reference at all, and KEDA rebuilt the ScaledObject and HPA
// underneath it the whole time. Nothing cascaded because there was nothing to
// cascade from.
func TestReconcileHTTPScaledObject_AdoptsOneThatArrivedFirst(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	fn := httpFunction()

	orphan := httpScaledObject(fn.Name, fn.Namespace)
	orphan.SetLabels(functionLabels(fn))

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, orphan).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileHTTPScaledObject(ctx, fn))

	got := httpScaledObject(fn.Name, fn.Namespace)
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: fn.Name, Namespace: fn.Namespace}, got))
	assert.True(t, metav1.IsControlledBy(got, fn),
		"a function's HTTPScaledObject must die with the function, or KEDA keeps rebuilding under it")
}

func TestReconcileHTTPScaledObject_RefusesAForeignOne(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	fn := httpFunction()

	foreign := httpScaledObject(fn.Name, fn.Namespace)

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, foreign).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	err := r.reconcileHTTPScaledObject(ctx, fn)
	require.Error(t, err, "an object this function did not create must not become its child")
	assert.Contains(t, err.Error(), "was not created by Kipper")
}

// a cron child was left ownerless while its Function was alive, so
// deleting that Function today would leave a CronJob running on its schedule
// with nothing left to stop it.
func TestReconcileCronJob_AdoptsOneThatArrivedFirst(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := httpFunction()
	fn.Spec.Triggers = []kipperv1.FunctionTrigger{{Type: "cron", Config: map[string]string{
		"schedule": "*/5 * * * *",
	}}}

	orphan := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name: fn.Name + "-cron", Namespace: fn.Namespace, Labels: functionLabels(fn),
		},
		Spec: batchv1.CronJobSpec{Schedule: "0 0 * * *"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, orphan).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileCronJob(ctx, fn, &fn.Spec.Triggers[0], nil, ""))

	var got batchv1.CronJob
	require.NoError(t, c.Get(ctx,
		types.NamespacedName{Name: fn.Name + "-cron", Namespace: fn.Namespace}, &got))
	assert.True(t, metav1.IsControlledBy(&got, fn),
		"a cron that outlives its function keeps running with nothing left to reconcile it")
}

func jobWithSchedule() *kipperv1.Job {
	return &kipperv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nightly", Namespace: "shop-test", UID: types.UID("uid-nightly"),
		},
		Spec: kipperv1.JobSpec{Image: "batch:1", Schedule: "0 2 * * *"},
	}
}

func TestJobReconcileCronJob_AdoptsOneThatArrivedFirst(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	job := jobWithSchedule()

	orphan := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name: job.Name, Namespace: job.Namespace, Labels: jobLabels(job),
		},
		Spec: batchv1.CronJobSpec{Schedule: "0 0 * * *"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(job, orphan).Build()
	r := &JobReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileCronJob(ctx, job, ""))

	var got batchv1.CronJob
	require.NoError(t, c.Get(ctx,
		types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, &got))
	assert.True(t, metav1.IsControlledBy(&got, job),
		"deleting the Job must take its schedule with it")
}

// A Function and an App may share a name in one namespace and both create a
// Deployment. The App direction is already covered; this is the reverse, and it
// is the one that was open — an App's children carried no resource-type marker
// at all, so a Function would have accepted one as its own.
func TestReconcileDeployment_FunctionRefusesAnAppsDeployment(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := httpFunction()
	appDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: fn.Name, Namespace: fn.Namespace,
			Labels: map[string]string{
				"app": fn.Name, kipperLabel: kipperValue, resourceTypeLabel: "app",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": fn.Name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": fn.Name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: fn.Name, Image: "app:1"}},
				},
			},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, appDeploy).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	err := r.reconcileDeployment(ctx, fn, "", nil, "", nil)
	require.Error(t, err, "an app's deployment is not a same-named function's to claim")
	assert.Contains(t, err.Error(), "belongs to a app")
}

// Switching a trigger off must not destroy an object switching it on would have
// refused to overwrite.
func TestDeleteOwnedKEDAObject_LeavesAForeignObject(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	fn := httpFunction()

	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject",
	})
	foreign.SetName(fn.Name)
	foreign.SetNamespace(fn.Namespace)

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, foreign).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.deleteOwnedKEDAObject(ctx, fn, "ScaledObject", fn.Name))

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject",
	})
	assert.NoError(t, c.Get(ctx,
		types.NamespacedName{Name: fn.Name, Namespace: fn.Namespace}, got),
		"a scaled object this function does not own must survive its trigger being removed")
}

func kedaMiddleware(name, fnNamespace string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware",
	})
	u.SetName(name)
	u.SetNamespace("keda")
	if fnNamespace != "" {
		u.SetLabels(map[string]string{fnNamespaceLabel: fnNamespace})
	}
	u.Object["spec"] = map[string]interface{}{"headers": map[string]interface{}{
		"contentSecurityPolicy": "belongs to the other project",
	}}
	return u
}

func getKedaMiddleware(t *testing.T, ctx context.Context, c crclient.Client, name string) (*unstructured.Unstructured, error) {
	t.Helper()
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware",
	})
	err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "keda"}, got)
	return got, err
}

// A function's middleware lives in the shared keda namespace under fn-<name>,
// so two projects with a same-named function derive the same object. The
// security middleware is reconciled before the host is claimed, so the losing
// project reaches this code even though it will be refused the route.
func TestReconcileFunctionSecurityMiddleware_LeavesAnotherProjectsAlone(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	fn := httpFunction()

	theirs := kedaMiddleware("fn-"+fn.Name+"-security", "another-project")
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, theirs).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileFunctionSecurityMiddleware(ctx, fn))

	got, err := getKedaMiddleware(t, ctx, c, "fn-"+fn.Name+"-security")
	require.NoError(t, err)
	headers, _, _ := unstructured.NestedMap(got.Object, "spec", "headers")
	assert.Equal(t, "belongs to the other project", headers["contentSecurityPolicy"],
		"a same-named function in another project must not overwrite this one's middleware")
}

func TestReconcileFunctionSecurityMiddleware_DisablingLeavesAnotherProjectsAlone(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	fn := httpFunction()
	fn.Spec.NoSecurityHeaders = true

	theirs := kedaMiddleware("fn-"+fn.Name+"-security", "another-project")
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, theirs).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileFunctionSecurityMiddleware(ctx, fn))

	_, err := getKedaMiddleware(t, ctx, c, "fn-"+fn.Name+"-security")
	assert.NoError(t, err,
		"switching security headers off must not delete another project's middleware")
}

// Garbage collection cannot reach across namespaces, so nothing a function
// keeps in keda dies with it unless the finalizer deletes it.
func TestFunctionFinalizer_RemovesWhatItLeftInTheSharedNamespace(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	fn := httpFunction()

	ours := kedaMiddleware("fn-"+fn.Name+"-security", fn.Namespace)
	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fn-" + fn.Name, Namespace: "keda",
			Labels: map[string]string{fnNamespaceLabel: fn.Namespace},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, ours, ingress).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.cleanupSharedNamespaceObjects(ctx, fn))

	_, err := getKedaMiddleware(t, ctx, c, "fn-"+fn.Name+"-security")
	assert.True(t, errors.IsNotFound(err), "the middleware must go with its function")
	assert.True(t, errors.IsNotFound(c.Get(ctx,
		types.NamespacedName{Name: "fn-" + fn.Name, Namespace: "keda"}, &networkingv1.Ingress{})),
		"and so must the ingress")
}

// Switching the HTTP trigger off must not destroy an HTTPScaledObject that
// reconcileHTTPScaledObject would refuse to adopt.
func TestCleanupHTTPServing_LeavesAForeignScaledObject(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	fn := httpFunction()

	foreign := httpScaledObject(fn.Name, fn.Namespace)
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, foreign).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.cleanupHTTPServing(ctx, fn))

	got := httpScaledObject(fn.Name, fn.Namespace)
	assert.NoError(t, c.Get(ctx,
		types.NamespacedName{Name: fn.Name, Namespace: fn.Namespace}, got),
		"an object this function does not own must survive its trigger being removed")
}

// The middleware shares the keda namespace under a conventional name, so a
// project that is about to lose the host claim must not create it first and
// leave the winner unable to replace what it serves through.
func TestReconcileIngress_ALosingProjectDoesNotCreateTheSharedMiddleware(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	fn := httpFunction()

	host := domain.SubdomainFor("fn-"+fn.Name, "example.test")
	claim := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeClaimName(host),
			Namespace: routeClaimNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kipper",
				routeClaimLabel:                "true",
				routeOwnerNamespaceLabel:       "another-project",
			},
		},
		Data: map[string]string{"host": host, "owner": "another-project"},
	}
	// The claim is only honoured while its owning project still exists — a
	// stale claim is taken over, which is the documented behaviour.
	owner := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "another-project"}}

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(fn, claim, owner).WithStatusSubresource(fn).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme, Domain: "example.test"}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: fn.Name, Namespace: fn.Namespace,
	}})
	require.NoError(t, err)

	_, getErr := getKedaMiddleware(t, ctx, c, "fn-"+fn.Name+"-security")
	assert.True(t, errors.IsNotFound(getErr),
		"a function refused the host must not have created the middleware for it")
}

// A stale CronJob belonging to somebody else is left alone, and leaving it
// alone is not an error: the branch that skipped it used to fall through to a
// handler that wrapped a nil error and failed the pass for ever.
func TestReconcile_AForeignStaleCronJobIsNotAnError(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := httpFunction()
	foreign := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: fn.Name + "-cron", Namespace: fn.Namespace},
		Spec:       batchv1.CronJobSpec{Schedule: "0 0 * * *"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(fn, foreign).WithStatusSubresource(fn).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme, Domain: "example.test"}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: fn.Name, Namespace: fn.Namespace,
	}})
	require.NoError(t, err, "a cron job this function does not own must not fail the pass")

	assert.NoError(t, c.Get(ctx,
		types.NamespacedName{Name: fn.Name + "-cron", Namespace: fn.Namespace}, &batchv1.CronJob{}),
		"and must survive")
}

// Ownership is re-asserted on every pass, which only works if the object can
// still be recognised once its controller reference is gone. An unlabelled KEDA
// object is unrecognisable to both the adopt and the delete path.
func TestReconcileTriggerAuth_RecoversAfterItsOwnerReferenceIsLost(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := httpFunction()
	fn.Spec.Triggers = []kipperv1.FunctionTrigger{{
		Type:   "postgres",
		Config: map[string]string{"source": "db", "query": "SELECT 1"},
	}}

	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db", Namespace: fn.Namespace, UID: types.UID("uid-db"),
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
	}
	controller := true
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-credentials", Namespace: fn.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
				Name: "db", UID: types.UID("uid-db"), Controller: &controller,
			}},
		},
		Data: map[string][]byte{
			"HOST": []byte("db"), "PORT": []byte("5432"),
			"USERNAME": []byte("kipper"), "PASSWORD": []byte("s3cret"), "NAME": []byte("app"),
		},
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, svc, creds).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileTriggerAuth(ctx, fn, &fn.Spec.Triggers[0]))

	name := types.NamespacedName{Name: fn.Name + "-trigger-auth", Namespace: fn.Namespace}
	auth := &unstructured.Unstructured{}
	auth.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "keda.sh", Version: "v1alpha1", Kind: "TriggerAuthentication",
	})
	require.NoError(t, c.Get(ctx, name, auth))

	auth.SetOwnerReferences(nil)
	require.NoError(t, c.Update(ctx, auth))

	require.NoError(t, r.reconcileTriggerAuth(ctx, fn, &fn.Spec.Triggers[0]),
		"a child whose owner reference was lost must be adoptable again")

	repaired := &unstructured.Unstructured{}
	repaired.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "keda.sh", Version: "v1alpha1", Kind: "TriggerAuthentication",
	})
	require.NoError(t, c.Get(ctx, name, repaired))
	assert.True(t, metav1.IsControlledBy(repaired, fn),
		"the reference must be re-asserted rather than the object left unowned")
}
