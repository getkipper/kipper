package controllers

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// The same promise as the existing no-restart test, but against a Deployment
// shaped the way an API server stores one.
//
// applyDeployment holds the running environment generation when nothing but the
// generation would change, which is what lets an env edit publish without
// rolling the app. It decides that by comparing the live pod template against a
// freshly built one. The live template has been through admission and the fresh
// one has not, so on a cluster they differ in fields neither the controller nor
// the operator ever set, the hold never engages, and every env edit restarts the
// app — against the banner that says it will not.
//
// Nothing afterwards shows it: envRestartPending compares Status.PublishedEnv
// with the template's generation, and once the roll has happened they agree. Admission fills in restartPolicy,
// dnsPolicy, terminationGracePeriodSeconds, schedulerName, the container's
// terminationMessagePath/Policy and imagePullPolicy, and a port's protocol.
// The fake client stores whatever it is handed, so every existing test compares
// an undefaulted live object against an undefaulted desired one.
// defaultPodTemplate fills in what admission fills in, so a fake client can
// stand in for an API server on the one question this file asks.
//
// The list is deliberately here rather than in the controller: the controller
// must not need to know it, which is the whole point of asking the server.
func defaultPodTemplate(t *corev1.PodTemplateSpec) {
	t.Spec.RestartPolicy = corev1.RestartPolicyAlways
	t.Spec.DNSPolicy = corev1.DNSClusterFirst
	t.Spec.SchedulerName = "default-scheduler"
	grace := int64(30)
	t.Spec.TerminationGracePeriodSeconds = &grace
	t.Spec.SecurityContext = &corev1.PodSecurityContext{}
	for i := range t.Spec.Containers {
		t.Spec.Containers[i].TerminationMessagePath = "/dev/termination-log"
		t.Spec.Containers[i].TerminationMessagePolicy = corev1.TerminationMessageReadFile
		t.Spec.Containers[i].ImagePullPolicy = corev1.PullIfNotPresent
		for p := range t.Spec.Containers[i].Ports {
			t.Spec.Containers[i].Ports[p].Protocol = corev1.ProtocolTCP
		}
	}
}

// defaultingClient is a fake that defaults a Deployment's pod template on write
// and on dry-run, the way an API server does. Without it every test in this
// package compares an undefaulted stored template against an undefaulted built
// one and agrees with itself.
func defaultingClient(scheme *runtime.Scheme, objects ...crclient.Object) crclient.WithWatch {
	return crfake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
				if d, ok := obj.(*appsv1.Deployment); ok {
					defaultPodTemplate(&d.Spec.Template)
				}
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				d, ok := obj.(*appsv1.Deployment)
				if !ok {
					return c.Update(ctx, obj, opts...)
				}
				defaultPodTemplate(&d.Spec.Template)
				for _, o := range opts {
					if o == crclient.DryRunAll {
						// Defaulted and handed back without being stored, which
						// is what a dry-run is.
						return nil
					}
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
}

func TestReconcileDeployment_HoldSurvivesApiServerDefaulting(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Env = map[string]string{"LOG_LEVEL": "info"}
	replicas := int32(1)
	app.Spec.Replicas = &replicas

	c := defaultingClient(scheme, app)
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
		"an env-only edit must leave a running app on the environment it started with, on a real cluster too")
}

// The hold must not become "always hold". A restart is what the banner asks for
// and it has to reach the pods, defaulting or not.
func TestReconcileDeployment_ARestartStillRollsUnderDefaulting(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Env = map[string]string{"LOG_LEVEL": "info"}
	replicas := int32(1)
	app.Spec.Replicas = &replicas

	c := defaultingClient(scheme, app)
	r := &AppReconciler{Client: c, Scheme: scheme}

	first := runningApp(t, ctx, c, r, app)
	require.NotEmpty(t, first)

	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, app))
	app.Spec.Env = map[string]string{"LOG_LEVEL": "debug"}
	if app.Annotations == nil {
		app.Annotations = map[string]string{}
	}
	app.Annotations["kipper.run/restartedAt"] = "2026-08-03T09:00:00Z"
	require.NoError(t, c.Update(ctx, app))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace,
	}})
	require.NoError(t, err)

	assert.NotEqual(t, first, currentGeneration(t, ctx, c, app),
		"a restart applies the pending environment; holding it here would be a banner that never clears")
}

// An image change rolls the app too, and takes the pending environment with it.
func TestReconcileDeployment_AnImageChangeStillRollsUnderDefaulting(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Env = map[string]string{"LOG_LEVEL": "info"}
	replicas := int32(1)
	app.Spec.Replicas = &replicas

	c := defaultingClient(scheme, app)
	r := &AppReconciler{Client: c, Scheme: scheme}

	first := runningApp(t, ctx, c, r, app)

	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, app))
	app.Spec.Env = map[string]string{"LOG_LEVEL": "debug"}
	app.Spec.Image = "nginx:1.27"
	require.NoError(t, c.Update(ctx, app))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace,
	}})
	require.NoError(t, err)

	assert.NotEqual(t, first, currentGeneration(t, ctx, c, app),
		"the pod is being replaced anyway, so it comes back on the newest environment")
}

// countingClient wraps the defaulting fake and counts the updates that are not
// dry-runs, which is the only kind that can roll a workload.
func countingClient(scheme *runtime.Scheme, writes *int, dryRunErr error, objects ...crclient.Object) crclient.WithWatch {
	return crfake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
				if d, ok := obj.(*appsv1.Deployment); ok {
					defaultPodTemplate(&d.Spec.Template)
				}
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				d, ok := obj.(*appsv1.Deployment)
				if !ok {
					return c.Update(ctx, obj, opts...)
				}
				dryRun := false
				for _, o := range opts {
					if o == crclient.DryRunAll {
						dryRun = true
					}
				}
				if dryRun && dryRunErr != nil {
					return dryRunErr
				}
				defaultPodTemplate(&d.Spec.Template)
				if dryRun {
					return nil
				}
				*writes++
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
}

// Holding means leaving the Deployment alone. Writing an object identical to the
// one already stored costs a webhook pass, an audit entry and a watch event on
// every reconcile for as long as the edit is pending.
func TestReconcileDeployment_AHeldGenerationWritesNothing(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Env = map[string]string{"LOG_LEVEL": "info"}
	replicas := int32(1)
	app.Spec.Replicas = &replicas

	writes := 0
	c := countingClient(scheme, &writes, nil, app)
	r := &AppReconciler{Client: c, Scheme: scheme}

	first := runningApp(t, ctx, c, r, app)
	require.NotEmpty(t, first)

	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, app))
	app.Spec.Env = map[string]string{"LOG_LEVEL": "debug"}
	require.NoError(t, c.Update(ctx, app))

	writes = 0
	for i := 0; i < 3; i++ {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
			Name: app.Name, Namespace: app.Namespace,
		}})
		require.NoError(t, err)
	}

	assert.Equal(t, first, currentGeneration(t, ctx, c, app))
	assert.Zero(t, writes, "a held generation is a Deployment nobody needs to write")
}

// A cluster whose admission declines dry-run requests must still be able to
// restart an app. The probe sits in front of every template change, so treating
// a refusal as fatal would stop the workload receiving any of them.
func TestReconcileDeployment_ARefusedProbeStillLetsARestartThrough(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Env = map[string]string{"LOG_LEVEL": "info"}
	replicas := int32(1)
	app.Spec.Replicas = &replicas

	writes := 0
	// What a cluster answers when a webhook in the path cannot promise its side
	// effects are safe to simulate.
	c := countingClient(scheme, &writes, errors.NewBadRequest(`admission webhook "policy.example.com" does not support dry run`), app)
	r := &AppReconciler{Client: c, Scheme: scheme}

	first := runningApp(t, ctx, c, r, app)
	require.NotEmpty(t, first)

	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, app))
	app.Spec.Env = map[string]string{"LOG_LEVEL": "debug"}
	if app.Annotations == nil {
		app.Annotations = map[string]string{}
	}
	app.Annotations["kipper.run/restartedAt"] = "2026-08-03T09:00:00Z"
	require.NoError(t, c.Update(ctx, app))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace,
	}})
	require.NoError(t, err, "a refused probe is not a reason to stop reconciling")

	assert.NotEqual(t, first, currentGeneration(t, ctx, c, app),
		"the restart the operator asked for still reaches the pods")
}

// A server that failed to answer is asked again: that is a moment rather than a
// property of the cluster, and guessing restarts an app nobody asked to restart.
func TestReconcileDeployment_ATimedOutProbeRetriesRatherThanGuessing(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Env = map[string]string{"LOG_LEVEL": "info"}
	replicas := int32(1)
	app.Spec.Replicas = &replicas

	writes := 0
	timeout := errors.NewServerTimeout(schema.GroupResource{Group: "apps", Resource: "deployments"}, "update", 1)
	c := countingClient(scheme, &writes, timeout, app)
	r := &AppReconciler{Client: c, Scheme: scheme}

	first := runningApp(t, ctx, c, r, app)
	require.NotEmpty(t, first)

	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, app))
	app.Spec.Env = map[string]string{"LOG_LEVEL": "debug"}
	require.NoError(t, c.Update(ctx, app))

	writes = 0
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace,
	}})
	require.Error(t, err, "an unanswered probe is retried, not guessed at")
	assert.Equal(t, first, currentGeneration(t, ctx, c, app))
	assert.Zero(t, writes, "and nothing rolled in the meantime")
}

// A cluster that cannot answer the probe cannot keep the no-restart promise, so
// the operator is told why their pods rolled rather than left to wonder.
func TestReconcileDeployment_ARefusedProbeIsSaidOutLoud(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Env = map[string]string{"LOG_LEVEL": "info"}
	replicas := int32(1)
	app.Spec.Replicas = &replicas

	writes := 0
	recorder := record.NewFakeRecorder(10)
	c := countingClient(scheme, &writes, errors.NewBadRequest(`admission webhook "policy.example.com" does not support dry run`), app)
	r := &AppReconciler{Client: c, Scheme: scheme, Recorder: recorder}

	require.NotEmpty(t, runningApp(t, ctx, c, r, app))
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, app))
	app.Spec.Env = map[string]string{"LOG_LEVEL": "debug"}
	require.NoError(t, c.Update(ctx, app))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace,
	}})
	require.NoError(t, err)

	var said string
	for len(recorder.Events) > 0 {
		e := <-recorder.Events
		if strings.Contains(e, "EnvHoldUnavailable") {
			said = e
		}
	}
	assert.NotEmpty(t, said, "a promise this cluster cannot keep is worth an event on the App")
	assert.Contains(t, said, "restarts")
}

// Permission and validity are answers about the write, not refusals to simulate
// it: the ordinary update would fail the same way, so carrying on would reach
// the same refusal with the reason thrown away.
func TestTemplateSettlesAs_ReturnsWhatIsAboutTheWriteRatherThanTheProbe(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	live := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "n"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"a": "b"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "i"}}},
			},
		},
	}

	for name, refusal := range map[string]error{
		"forbidden": errors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "d", context.DeadlineExceeded),
		"invalid":   errors.NewInvalid(schema.GroupKind{Group: "apps", Kind: "Deployment"}, "d", nil),
	} {
		t.Run(name, func(t *testing.T) {
			writes := 0
			c := countingClient(scheme, &writes, refusal, live.DeepCopy())
			_, _, err := templateSettlesAs(ctx, c, live, live.Spec.Template)
			require.Error(t, err, "this says the write is unacceptable, not that it cannot be simulated")
		})
	}

	// And the one that really is the cluster declining to simulate.
	writes := 0
	c := countingClient(scheme, &writes, errors.NewBadRequest(`admission webhook "policy.example.com" does not support dry run`), live.DeepCopy())
	_, answered, err := templateSettlesAs(ctx, c, live, live.Spec.Template)
	require.NoError(t, err)
	assert.False(t, answered, "the caller has to know it got no answer")
}

// The Function controller wrote its Deployment on every pass, held or not. An
// object identical to the stored one is not worth a webhook pass, an audit entry
// and a watch event.
//
// The fake here does not default, which is what makes the claim testable: the
// guard compares the built template against the stored one, and on a cluster
// that comparison is blind to defaulting the same way everything else in this
// file was. Where it bites regardless is the held case, because there desired
// carries the live template itself and the two are the same object. That is the
// case the review raised; this pins the guard that serves it.
func TestFunctionReconcileDeployment_WritesNothingWhenNothingChanged(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "shop-prod", UID: types.UID("uid-resize")},
		Spec:       kipperv1.FunctionSpec{Image: "resizer:1", Port: 8080},
	}

	writes := 0
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn).WithStatusSubresource(fn).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					writes++
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileDeployment(ctx, fn, "", nil, "gen-1", renderedBindings{}))

	writes = 0
	for i := 0; i < 3; i++ {
		require.NoError(t, r.reconcileDeployment(ctx, fn, "", nil, "gen-1", renderedBindings{}))
	}
	assert.Zero(t, writes, "a Deployment that already says this is not worth writing again")

	fn.Spec.Image = "resizer:2"
	require.NoError(t, r.reconcileDeployment(ctx, fn, "", nil, "gen-1", renderedBindings{}))
	assert.Equal(t, 1, writes, "a changed image is written")
}

// adoptChild repairs a lost controller reference in memory, and the repair is
// worth nothing unless something writes it. Returning early on an otherwise
// current Deployment threw it away every pass, and a Function deleted in that
// state would leave its Deployment behind.
func TestFunctionReconcileDeployment_WritesBackARepairedOwnerReference(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "shop-prod", UID: types.UID("uid-resize")},
		Spec:       kipperv1.FunctionSpec{Image: "resizer:1", Port: 8080},
	}

	writes := 0
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn).WithStatusSubresource(fn).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					writes++
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileDeployment(ctx, fn, "", nil, "gen-1", renderedBindings{}))

	// The reference is lost — a direct write, a restore — while everything else
	// stays exactly as this controller wants it.
	var live appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: fn.Name, Namespace: fn.Namespace}, &live))
	live.OwnerReferences = nil
	require.NoError(t, c.Update(ctx, &live))

	writes = 0
	require.NoError(t, r.reconcileDeployment(ctx, fn, "", nil, "gen-1", renderedBindings{}))
	assert.Equal(t, 1, writes, "the repaired reference has to be written or it is not a repair")

	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: fn.Name, Namespace: fn.Namespace}, &live))
	assert.True(t, metav1.IsControlledBy(&live, fn), "and the Function owns its Deployment again")

	// Once repaired there is nothing to say again.
	writes = 0
	require.NoError(t, r.reconcileDeployment(ctx, fn, "", nil, "gen-1", renderedBindings{}))
	assert.Zero(t, writes)
}

// A webhook can reject this candidate with a 400 for a policy reason that would
// reject the ordinary update too. Reading that as "this cluster cannot dry-run"
// tells the operator something untrue and then attempts a write already known
// to fail.
func TestDryRunRefused_IsTheApiServerDecliningRatherThanAWebhookRejecting(t *testing.T) {
	assert.True(t, dryRunRefused(errors.NewBadRequest(
		`admission webhook "policy.example.com" does not support dry run`)),
		"this is the API server declining to simulate")

	for name, err := range map[string]error{
		"a policy rejection carrying 400": errors.NewBadRequest(
			"admission webhook \"policy.example.com\" denied the request: images must be signed"),
		"a permission denial": errors.NewForbidden(
			schema.GroupResource{Group: "apps", Resource: "deployments"}, "d", context.DeadlineExceeded),
		"an invalid candidate": errors.NewInvalid(
			schema.GroupKind{Group: "apps", Kind: "Deployment"}, "d", nil),
		"a timeout": errors.NewServerTimeout(
			schema.GroupResource{Group: "apps", Resource: "deployments"}, "update", 1),
	} {
		assert.False(t, dryRunRefused(err), name)
	}
}

// The hold can return an error — a cluster that will not say whether the pod
// would roll. Repairing the owner reference before that point loses the repair
// on every such pass, since only a write makes it real.
func TestFunctionReconcileDeployment_DoesNotLoseAnAdoptionToAFailedProbe(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "shop-prod", UID: types.UID("uid-resize")},
		Spec:       kipperv1.FunctionSpec{Image: "resizer:1", Port: 8080},
	}

	writes := 0
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn).WithStatusSubresource(fn).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					writes++
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileDeployment(ctx, fn, "", nil, "gen-1", renderedBindings{}))

	var live appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: fn.Name, Namespace: fn.Namespace}, &live))
	live.OwnerReferences = nil
	require.NoError(t, c.Update(ctx, &live))

	// A pass that reaches the adoption writes it, whatever else it decides.
	writes = 0
	require.NoError(t, r.reconcileDeployment(ctx, fn, "", nil, "gen-1", renderedBindings{}))
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: fn.Name, Namespace: fn.Namespace}, &live))
	assert.True(t, metav1.IsControlledBy(&live, fn),
		"the repair is written on the pass that makes it, not left in memory")
}
