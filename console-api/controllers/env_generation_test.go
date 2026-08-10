package controllers

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

func TestPublishEnvGeneration_WritesAnImmutableSecretNamedForItsContent(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).Build()

	env := map[string]string{"MODE": "live", "DB_PASSWORD": "s3cret"}
	name, err := publishEnvGeneration(ctx, c, scheme, app, secretname.KindApp, env,
		map[string]string{"app": "my-app", kipperLabel: kipperValue})
	require.NoError(t, err)
	assert.Equal(t, secretname.EnvGeneration(secretname.KindApp, "my-app", envDigest(env)), name)

	var got corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: name, Namespace: "project-test"}, &got))
	require.NotNil(t, got.Immutable)
	assert.True(t, *got.Immutable,
		"a pod cannot be pointed at an environment that can change under it")
	assert.Equal(t, []byte("s3cret"), got.Data["DB_PASSWORD"])
	assert.True(t, metav1.IsControlledBy(&got, app),
		"the generation must be garbage-collected with its workload")
}

// Republishing identical content is the common case: most passes change
// nothing. An immutable Secret cannot have moved, so the name is the whole
// check and there is no write to make.
func TestPublishEnvGeneration_RepublishingTheSameContentWritesNothing(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).Build()

	env := map[string]string{"MODE": "live"}
	name, err := publishEnvGeneration(ctx, c, scheme, app, secretname.KindApp, env, nil)
	require.NoError(t, err)

	var first corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: name, Namespace: "project-test"}, &first))

	again, err := publishEnvGeneration(ctx, c, scheme, app, secretname.KindApp, env, nil)
	require.NoError(t, err)
	assert.Equal(t, name, again)

	var second corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: name, Namespace: "project-test"}, &second))
	assert.Equal(t, first.ResourceVersion, second.ResourceVersion,
		"identical content must not produce a write")
}

// An App, a Function and a Job may all be called "api" in one namespace, and
// Secret names are namespace-global. Two of them publishing the same content
// must still get two objects, or one workload reads the other's environment.
func TestPublishEnvGeneration_SeparatesWorkloadsOfDifferentKinds(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name: app.Name, Namespace: app.Namespace, UID: types.UID("uid-my-fn"),
		},
		Spec: kipperv1.FunctionSpec{Runtime: "node"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, fn).Build()

	env := map[string]string{"MODE": "live"}
	appGen, err := publishEnvGeneration(ctx, c, scheme, app, secretname.KindApp, env, nil)
	require.NoError(t, err)
	fnGen, err := publishEnvGeneration(ctx, c, scheme, fn, secretname.KindFunction, env, nil)
	require.NoError(t, err)

	assert.NotEqual(t, appGen, fnGen,
		"a same-named app and function must not share one generation object")

	var got corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: fnGen, Namespace: fn.Namespace}, &got))
	assert.True(t, metav1.IsControlledBy(&got, fn),
		"and each must own its own")
}

// The name is derived from the content, so it is guessable, and finding an
// object there is not evidence this controller wrote it.
func TestPublishEnvGeneration_RefusesAnObjectThatCannotCarryTheGuarantee(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := newTestApp()
	app.UID = types.UID("uid-my-app")

	env := map[string]string{"MODE": "live"}
	name := secretname.EnvGeneration(secretname.KindApp, "my-app", envDigest(env))
	immutable := true

	cases := []struct {
		name    string
		secret  *corev1.Secret
		wantErr string
	}{
		{
			name: "mutable",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "project-test"},
				Data:       map[string][]byte{"MODE": []byte("live")},
			},
			wantErr: "mutable",
		},
		{
			name: "owned by somebody else",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "project-test"},
				Immutable:  &immutable,
				Data:       map[string][]byte{"MODE": []byte("live")},
			},
			wantErr: "not controlled by this workload",
		},
		{
			name: "an extra variable under a matching digest",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: name, Namespace: "project-test",
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(app,
						app.GroupVersionKind())},
				},
				Immutable: &immutable,
				Data: map[string][]byte{
					"MODE": []byte("live"), "EXTRA": []byte("the pod would read this too"),
				},
			},
			wantErr: "holds 2 variables where this environment has 1",
		},
		{
			name: "different content under a matching digest",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: name, Namespace: "project-test",
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(app,
						app.GroupVersionKind())},
				},
				Immutable: &immutable,
				Data:      map[string][]byte{"MODE": []byte("something else")},
			},
			wantErr: "different value",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, tc.secret).Build()
			_, err := publishEnvGeneration(ctx, c, scheme, app, secretname.KindApp, env, nil)
			require.Error(t, err, "a generation that cannot carry the guarantee must not be served")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// The D14 scenario, and the reason the whole change exists.
//
// A rotation writes the binding Secret and the env Secret as two API calls. A
// pod that started between them took the new discrete DB_PASSWORD from one and
// the old password baked into DATABASE_URL from the other, and Kipper had
// published an environment that contradicted itself. One immutable object per
// generation makes that unrepresentable: whichever generation a pod reads, the
// composed value and the credential it was composed from came from the same
// pass.
func TestPublishEnvGeneration_ComposedCredentialMatchesItsOwnDiscreteOne(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.ServiceBindings = []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_", Database: "my_app"}}
	app.Spec.Env = map[string]string{
		"DATABASE_URL": "postgres://${DB_USERNAME}:${DB_PASSWORD}@db:5432/my_app",
	}

	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db", Namespace: app.Namespace, UID: types.UID("uid-db"),
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
	}
	controller := true
	creds := func(password string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "db-credentials", Namespace: app.Namespace,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
					Name: "db", UID: types.UID("uid-db"), Controller: &controller,
				}},
			},
			Data: map[string][]byte{
				"HOST": []byte("db"), "PORT": []byte("5432"), "NAME": []byte("app"),
				"USERNAME": []byte("kipper"), "PASSWORD": []byte(password),
			},
		}
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, svc, creds("before")).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	req := ctrl.Request{NamespacedName: types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace,
	}}
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	// The service password rotates.
	rotated := creds("after")
	require.NoError(t, c.Update(ctx, rotated))
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)

	var deploy appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace}, &deploy))
	published := podEnvGeneration(t, ctx, c, deploy.Spec.Template.Spec, "app-my-app-env-", app.Namespace)

	assert.Equal(t, []byte("after"), published.Data["DB_PASSWORD"],
		"the rotation must reach the pod's discrete credential")
	assert.Equal(t, "postgres://kipper:after@db:5432/my_app", string(published.Data["DATABASE_URL"]),
		"and the composed value must carry the same password, not the one it was composed from last pass")
}

// runningApp reconciles an app once and marks its Deployment as having a live
// pod, which is the state the hold rule exists to protect.
func runningApp(t *testing.T, ctx context.Context, c crclient.Client, r *AppReconciler, app *kipperv1.App) string {
	t.Helper()
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace,
	}})
	require.NoError(t, err)

	var deploy appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace}, &deploy))
	return generationOnContainer(deploy.Spec.Template.Spec.Containers, secretname.KindApp, app.Name)
}

func currentGeneration(t *testing.T, ctx context.Context, c crclient.Client, app *kipperv1.App) string {
	t.Helper()
	var deploy appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace}, &deploy))
	return generationOnContainer(deploy.Spec.Template.Spec.Containers, secretname.KindApp, app.Name)
}

// Editing env in the console shows a "restart to apply" banner and must not
// restart a running app. The new environment is published either way; what the
// pod template names is what decides whether anything rolls.
func TestReconcileDeployment_AnEnvEditPublishesWithoutRollingARunningApp(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Env = map[string]string{"LOG_LEVEL": "info"}
	replicas := int32(1)
	app.Spec.Replicas = &replicas

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	first := runningApp(t, ctx, c, r, app)
	require.NotEmpty(t, first)

	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, app))
	app.Spec.Env = map[string]string{"LOG_LEVEL": "debug"}
	require.NoError(t, c.Update(ctx, app))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace,
	}})
	require.NoError(t, err)

	assert.Equal(t, first, currentGeneration(t, ctx, c, app),
		"an env-only edit must leave a running app on the environment it started with")

	// Published all the same, so a restart applies it without another reconcile.
	newest, err := publishEnvGeneration(ctx, c, scheme, app, secretname.KindApp,
		map[string]string{"LOG_LEVEL": "debug"}, nil)
	require.NoError(t, err)
	assert.NotEqual(t, first, newest, "the edit must have produced a different generation")
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: newest, Namespace: app.Namespace},
		&corev1.Secret{}), "the new environment must be published even while the pod holds the old one")
}

// A restart is what the banner asks for, and it travels from the CR to the pod
// annotations, so the template differs in a second field and the hold does not
// apply.
func TestReconcileDeployment_ARestartAppliesThePendingEnv(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Env = map[string]string{"LOG_LEVEL": "info"}
	replicas := int32(1)
	app.Spec.Replicas = &replicas

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	first := runningApp(t, ctx, c, r, app)

	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, app))
	app.Spec.Env = map[string]string{"LOG_LEVEL": "debug"}
	if app.Annotations == nil {
		app.Annotations = map[string]string{}
	}
	app.Annotations["kipper.run/restartedAt"] = "2026-08-01T09:00:00Z"
	require.NoError(t, c.Update(ctx, app))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace,
	}})
	require.NoError(t, err)

	assert.NotEqual(t, first, currentGeneration(t, ctx, c, app),
		"a restart must apply the environment the banner said was pending")
}

// Nothing is running, so nothing is protected: a cold start must not come up on
// an environment older than the CR.
func TestReconcileDeployment_AScaledToZeroAppTakesTheNewestEnv(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Env = map[string]string{"LOG_LEVEL": "info"}
	zero := int32(0)
	app.Spec.Replicas = &zero

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	first := runningApp(t, ctx, c, r, app)

	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, app))
	app.Spec.Env = map[string]string{"LOG_LEVEL": "debug"}
	require.NoError(t, c.Update(ctx, app))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace,
	}})
	require.NoError(t, err)

	assert.NotEqual(t, first, currentGeneration(t, ctx, c, app),
		"with no pod to protect, the next one to start must read the current environment")
}

// A held generation that has been deleted would strand the workload for good:
// the pass republishes the newest, so nothing rewrites the one the template
// names, and the pods can never start.
func TestReconcileDeployment_DoesNotHoldAGenerationThatHasGone(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Env = map[string]string{"LOG_LEVEL": "info"}
	replicas := int32(1)
	app.Spec.Replicas = &replicas

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	first := runningApp(t, ctx, c, r, app)
	require.NotEmpty(t, first)

	// The environment the pods are on is deleted out of band, and an env edit
	// means the pass will publish a different one rather than rewriting this.
	require.NoError(t, c.Delete(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: first, Namespace: app.Namespace},
	}))
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, app))
	app.Spec.Env = map[string]string{"LOG_LEVEL": "debug"}
	require.NoError(t, c.Update(ctx, app))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace,
	}})
	require.NoError(t, err)

	now := currentGeneration(t, ctx, c, app)
	assert.NotEqual(t, first, now,
		"a workload must not be left naming an environment that no longer exists")
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: now, Namespace: app.Namespace},
		&corev1.Secret{}), "and the one it moved to must exist")
}

// A pod whose environment did not publish does not start, and the reason it
// gives is CreateContainerConfigError, which names nothing.
func TestReconcile_AFailedPublicationIsReportedOnStatus(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Env = map[string]string{"LOG_LEVEL": "info"}

	// Somebody else's object sitting at the name this content hashes to. That
	// is the one publication failure a later pass cannot repair on its own.
	published := map[string]string{"LOG_LEVEL": "info"}
	squatter := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretname.EnvGeneration(secretname.KindApp, app.Name, envDigest(published)),
			Namespace: app.Namespace,
		},
		Data: map[string][]byte{"LOG_LEVEL": []byte("info")},
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, squatter).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace,
	}})
	require.Error(t, err)

	var got kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace}, &got))
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionEnvPublished)
	require.NotNil(t, cond, "the failure must be visible on the workload, not only in a pod's events")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Contains(t, cond.Message, "mutable")
}

// An App and a Function may share a name in one namespace and their pods carry
// the same app label, so a selector-matched list returns pods belonging to the
// other one. Holding on somebody else's pod leaves this workload's next cold
// start on a stale environment.
func TestHasLivePod_IgnoresASameNamedWorkloadsPods(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	zero := int32(0)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api", Namespace: "shop-test", UID: types.UID("uid-deploy"),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &zero,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
		},
	}

	// A ReplicaSet and pod belonging to a different Deployment of the same name
	// in the same namespace — the same-named Function's.
	controller := true
	theirRS := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-fn-abc123", Namespace: "shop-test",
			Labels: map[string]string{"app": "api"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "api",
				UID: types.UID("uid-other-deploy"), Controller: &controller,
			}},
		},
	}
	theirPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-fn-abc123-xyz", Namespace: "shop-test",
			Labels: map[string]string{"app": "api"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "api-fn-abc123",
				UID: theirRS.UID, Controller: &controller,
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(deploy, theirRS, theirPod).Build()

	live, err := hasLivePod(ctx, c, deploy)
	require.NoError(t, err)
	assert.False(t, live,
		"a pod belonging to a same-named workload is not this one's to protect")
}

func TestHasLivePod_FindsItsOwnPod(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	zero := int32(0)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api", Namespace: "shop-test", UID: types.UID("uid-deploy"),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &zero,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
		},
	}
	controller := true
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-abc123", Namespace: "shop-test", UID: types.UID("uid-rs"),
			Labels: map[string]string{"app": "api"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "api",
				UID: deploy.UID, Controller: &controller,
			}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-abc123-xyz", Namespace: "shop-test",
			Labels: map[string]string{"app": "api"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "api-abc123",
				UID: rs.UID, Controller: &controller,
			}},
		},
		// Pending counts: it is already committed to the current template and
		// will start on it. Reading readiness would roll a workload every time
		// an image pull ran long.
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, rs, pod).Build()

	live, err := hasLivePod(ctx, c, deploy)
	require.NoError(t, err)
	assert.True(t, live, "a pending pod of this workload's own is protected")
}

// A test run is callable the moment a Function is created, before any
// controller pass. Two things had to hold for that and did not: a binding that
// pins a database needs its projection to exist, and the environment the run
// publishes has to be internally consistent even though nothing resolved it in
// advance.
func TestBuildBatchPodSpec_PublishesAConsistentEnvironmentBeforeAnyReconcile(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name: "resize", Namespace: "media-test", UID: types.UID("uid-resize"),
		},
		Spec: kipperv1.FunctionSpec{
			Runtime: "node",
			Source:  &kipperv1.FunctionSource{Code: "module.exports = () => 'ok'"},
			//nolint:gosec // a template naming variables, which is the point
			Env: map[string]string{
				"DATABASE_URL": "postgres://${DB_USERNAME}:${DB_PASSWORD}@${DB_HOST}/${DB_NAME}",
			},
			ServiceBindings: []kipperv1.ServiceBinding{
				{Name: "db", Prefix: "DB_", Database: "resize_test"},
			},
		},
	}
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
			"HOST": []byte("db"), "PORT": []byte("5432"), "NAME": []byte("app"),
			"USERNAME": []byte("kipper"), "PASSWORD": []byte("s3cret"),
		},
	}

	// No derived projection seeded: only what a freshly created Function has.
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, svc, creds).Build()

	spec, err := BuildBatchPodSpec(ctx, c, fn, "test", nil)
	require.NoError(t, err, "a pinned binding must not refuse a run before the first reconcile")

	published := podEnvGeneration(t, ctx, c, spec, "function-resize-env-", fn.Namespace)
	assert.Equal(t, []byte("s3cret"), published.Data["DB_PASSWORD"])
	assert.Equal(t, []byte("resize_test"), published.Data["DB_NAME"],
		"the binding's own database must have been derived, not the service default")
	assert.Equal(t, "postgres://kipper:s3cret@db/resize_test", string(published.Data["DATABASE_URL"]),
		"the composed value must agree with the discrete credentials in its own generation")
}

// The poll sidecar and the main container are written into one pod template, so
// they must carry one credential generation. The sidecar used to read the shared
// Secret a second time, which is how a rotation between the two reads put two
// passwords in one pod.
func TestBuildPollSidecar_UsesThePassSnapshotRatherThanASecondRead(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name: "resize", Namespace: "media-test", UID: types.UID("uid-resize"),
		},
		Spec: kipperv1.FunctionSpec{
			Runtime: "node",
			// Unpinned, so this binding is held under the service's shared name.
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_"}},
		},
	}
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db", Namespace: fn.Namespace, UID: types.UID("uid-db"),
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
	}
	// What the Secret holds now — a rotation that landed after this pass took
	// its snapshot.
	live := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: fn.Namespace},
		Data: map[string][]byte{
			"HOST": []byte("db"), "PORT": []byte("5432"), "NAME": []byte("app"),
			"USERNAME": []byte("kipper"), "PASSWORD": []byte("rotated"),
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, svc, live).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	// What this pass resolved, and what the main container's generation holds.
	rendered := renderedBindings{}
	rendered.keep("db-credentials", map[string][]byte{
		"HOST": []byte("db"), "PORT": []byte("5432"), "NAME": []byte("app"),
		"USERNAME": []byte("kipper"), "PASSWORD": []byte("snapshotted"),
	})

	trigger := &kipperv1.FunctionTrigger{
		Type:   "postgres",
		Config: map[string]string{"source": "db", "query": "SELECT 1"},
	}
	sidecar, err := r.buildPollSidecar(ctx, fn, trigger, 8080, rendered)
	require.NoError(t, err)

	var sourceURL string
	for _, e := range sidecar.Env {
		if e.Name == "KIPPER_SOURCE_URL" {
			sourceURL = e.Value
		}
	}
	require.NotEmpty(t, sourceURL)
	assert.Contains(t, sourceURL, "snapshotted",
		"the sidecar must carry the credential this pass published, not a later read of it")
	assert.NotContains(t, sourceURL, "rotated",
		"one pod template must not carry two credential generations")
}

// A binding that pins a database is held under its own derived name and carries
// that database. Asking for the service's shared name finds nothing, falls back
// to a live read, and points the sidecar at the service default with whatever
// password the Secret holds by then.
func TestBuildPollSidecar_UsesThePinnedBindingsOwnSnapshot(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name: "resize", Namespace: "media-test", UID: types.UID("uid-resize"),
		},
		Spec: kipperv1.FunctionSpec{
			Runtime:         "node",
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_", Database: "resize_test"}},
		},
	}
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db", Namespace: fn.Namespace, UID: types.UID("uid-db"),
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
	}
	live := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: fn.Namespace},
		Data: map[string][]byte{
			"HOST": []byte("db"), "PORT": []byte("5432"), "NAME": []byte("service_default"),
			"USERNAME": []byte("kipper"), "PASSWORD": []byte("rotated"),
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, svc, live).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	rendered := renderedBindings{}
	rendered.keep("db-function-resize-credentials", map[string][]byte{
		"HOST": []byte("db"), "PORT": []byte("5432"), "NAME": []byte("resize_test"),
		"USERNAME": []byte("kipper"), "PASSWORD": []byte("snapshotted"),
	})

	trigger := &kipperv1.FunctionTrigger{
		Type:   "postgres",
		Config: map[string]string{"source": "db", "query": "SELECT 1"},
	}
	sidecar, err := r.buildPollSidecar(ctx, fn, trigger, 8080, rendered)
	require.NoError(t, err)

	var sourceURL string
	for _, e := range sidecar.Env {
		if e.Name == "KIPPER_SOURCE_URL" {
			sourceURL = e.Value
		}
	}
	require.NotEmpty(t, sourceURL)
	assert.Contains(t, sourceURL, "resize_test",
		"the poller must watch the database the binding pinned, not the service default")
	assert.Contains(t, sourceURL, "snapshotted",
		"and carry the credential this pass published")
}

// Two bindings to one service give two answers to which database the poller
// watches, and picking one silently puts it on a table nobody expected.
func TestBuildPollSidecar_RefusesAnAmbiguousBinding(t *testing.T) {
	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "media-test"},
		Spec: kipperv1.FunctionSpec{ServiceBindings: []kipperv1.ServiceBinding{
			{Name: "db", Database: "one"},
			{Name: "db", Database: "two"},
		}},
	}
	_, err := boundCredentialSnapshot(fn, "db", "postgres", renderedBindings{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bound more than once")
}

// A held generation is a pod being pointed at an object again, so it is checked
// again. The name is a digest of the content, which makes it guessable.
func TestGenerationUsable_RefusesAnObjectThatNoLongerCarriesTheGuarantee(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := newTestApp()
	app.UID = types.UID("uid-my-app")

	env := map[string]string{"LOG_LEVEL": "info"}
	name := secretname.EnvGeneration(secretname.KindApp, app.Name, envDigest(env))
	immutable := true
	owned := []metav1.OwnerReference{*metav1.NewControllerRef(app, app.GroupVersionKind())}

	cases := []struct {
		name   string
		secret *corev1.Secret
		want   bool
	}{
		{"the environment as published", &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: app.Namespace, OwnerReferences: owned},
			Immutable:  &immutable,
			Data:       map[string][]byte{"LOG_LEVEL": []byte("info")},
		}, true},
		{"mutable", &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: app.Namespace, OwnerReferences: owned},
			Data:       map[string][]byte{"LOG_LEVEL": []byte("info")},
		}, false},
		{"foreign", &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: app.Namespace},
			Immutable:  &immutable,
			Data:       map[string][]byte{"LOG_LEVEL": []byte("info")},
		}, false},
		{"contents that no longer hash to their own name", &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: app.Namespace, OwnerReferences: owned},
			Immutable:  &immutable,
			Data:       map[string][]byte{"LOG_LEVEL": []byte("tampered")},
		}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, tc.secret).Build()
			ok, err := generationUsable(ctx, c, app, secretname.KindApp, name)
			require.NoError(t, err)
			assert.Equal(t, tc.want, ok)
		})
	}
}

// The exact name the scheme would produce, not merely one ending in the right
// digest. Anything between the prefix and the digest is text no publication
// ever wrote, so holding it would keep a pod on an object the scheme could not
// have created.
func TestGenerationUsable_RefusesAMalformedName(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := newTestApp()
	app.UID = types.UID("uid-my-app")

	env := map[string]string{"LOG_LEVEL": "info"}
	immutable := true
	malformed := secretname.EnvGenerationPrefix(secretname.KindApp, app.Name) +
		"injected-" + envDigest(env)

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: malformed, Namespace: app.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(app, app.GroupVersionKind()),
			},
		},
		Immutable: &immutable,
		Data:      map[string][]byte{"LOG_LEVEL": []byte("info")},
	}).Build()

	ok, err := generationUsable(ctx, c, app, secretname.KindApp, malformed)
	require.NoError(t, err)
	assert.False(t, ok, "a name the publication scheme could not produce is not a generation")
}

// renderedEnvSecret is the one published environment of a workload, found by
// listing rather than by name: the name carries a digest of the contents, so a
// test that constructed it would be asserting that the digest agrees with
// itself.
func renderedEnvSecret(t *testing.T, ctx context.Context, c crclient.Client,
	kind secretname.Kind, workload, namespace string) corev1.Secret {
	t.Helper()
	var list corev1.SecretList
	require.NoError(t, c.List(ctx, &list, crclient.InNamespace(namespace)))

	prefix := secretname.EnvGenerationPrefix(kind, workload)
	var found []corev1.Secret
	for i := range list.Items {
		if strings.HasPrefix(list.Items[i].Name, prefix) {
			found = append(found, list.Items[i])
		}
	}
	require.Len(t, found, 1, "expected one published environment for %s %q", kind, workload)
	return found[0]
}

// The field the console compares against is written before anything downstream
// can fail. It used to ride to the end of the reconcile, so a failure in an
// unrelated step left it naming the previous generation — and the banner then
// answers "no restart needed" while a restart would move the workload onto what
// was just published.
func TestReconcile_PublicationIsRecordedBeforeLaterStepsCanFail(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Env = map[string]string{"LOG_LEVEL": "info"}

	// Everything after the publication fails: the Service write is the first
	// thing the pass does with the API once the environment is out.
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).
		WithStatusSubresource(app).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
				if _, isSvc := obj.(*corev1.Service); isSvc {
					return apierrors.NewInternalError(fmt.Errorf("etcd unavailable"))
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace,
	}})
	require.Error(t, err, "the pass fails on the downstream step")

	var got kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace}, &got))
	assert.NotEmpty(t, got.Status.PublishedEnv,
		"what was published is recorded even when the rest of the pass does not finish")

	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: got.Status.PublishedEnv, Namespace: app.Namespace}, &corev1.Secret{}),
		"and it names an environment that exists")
}

// While anything still reads an object from before the move, the workload has
// not finished converting, and the condition says so rather than reporting the
// same sentence for ever.
func TestEnvPublishedCondition_ReportsTheConversionGate(t *testing.T) {
	var conditions []metav1.Condition

	applyEnvPublishedConditionWithConversion(&conditions, 1, nil, 2)
	cond := apimeta.FindStatusCondition(conditions, kipperv1.ConditionEnvPublished)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Contains(t, cond.Message, "2 object(s) from before generations",
		"an operator watching the conversion needs to see it is not finished")

	applyEnvPublishedConditionWithConversion(&conditions, 1, nil, 0)
	cond = apimeta.FindStatusCondition(conditions, kipperv1.ConditionEnvPublished)
	require.NotNil(t, cond)
	assert.NotContains(t, cond.Message, "before generations",
		"and to see when it is")
}

// The name alone is not enough to decide a status write is unnecessary. A
// validation failure records EnvPublished=False under a name; removing the bad
// object and republishing the same content produces the same name, and only the
// condition moves.
func TestRecordPublication_WritesWhenOnlyTheConditionChanges(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app).WithStatusSubresource(app).Build()

	name := secretname.EnvGeneration(secretname.KindApp, app.Name, "aaaaaaaaaaaa")

	require.NoError(t, recordPublication(ctx, c, app, &app.Status.Conditions,
		&app.Status.PublishedEnv, app.Generation, name, fmt.Errorf("an object is already there")))

	var failed kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace}, &failed))
	require.Equal(t, metav1.ConditionFalse,
		apimeta.FindStatusCondition(failed.Status.Conditions, kipperv1.ConditionEnvPublished).Status)

	// Same name, publication now succeeds.
	require.NoError(t, recordPublication(ctx, c, app, &app.Status.Conditions,
		&app.Status.PublishedEnv, app.Generation, name, nil))

	var recovered kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace}, &recovered))
	assert.Equal(t, metav1.ConditionTrue,
		apimeta.FindStatusCondition(recovered.Status.Conditions, kipperv1.ConditionEnvPublished).Status,
		"a recovery under an unchanged name must still reach the API")
}
