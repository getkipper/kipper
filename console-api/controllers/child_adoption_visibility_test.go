package controllers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// errRefusedByTheAPIServer stands in for any write the API server rejects, to
// drive the failure path without depending on a particular child.
var errRefusedByTheAPIServer = errors.New("the API server refused the write")

// refusedSecurityMiddleware is the shape that took eight apps off the air
// quietly: an object Kipper made, restored from a backup taken before the
// platform was renamed, so its managed-by label no longer says "kipper".
func refusedSecurityMiddleware() *unstructured.Unstructured {
	mw := &unstructured.Unstructured{}
	mw.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware",
	})
	mw.SetName("my-app-security")
	mw.SetNamespace("project-test")
	mw.SetLabels(map[string]string{"app": "my-app", kipperLabel: "skipper"})
	return mw
}

func routedApp() *kipperv1.App {
	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Generation = 4
	app.Spec.Route = &kipperv1.AppRoute{Host: "app.example.com", Path: "/"}
	return app
}

func appRequest() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"}}
}

// The refusal must still stop the pass. Everything else here makes that stop
// visible, and none of it is worth having if the object gets adopted anyway.
func TestReconcile_ARefusedChildStopsThePassAndLeavesLaterChildrenAlone(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := routedApp()
	app.Spec.Autoscale = &kipperv1.AppAutoscale{Enabled: true, MinReplicas: 2, MaxReplicas: 5, CPUTarget: 70}

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, refusedSecurityMiddleware()).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, appRequest())
	require.Error(t, err, "a child Kipper cannot establish as its own must stop the pass")
	assert.Contains(t, err.Error(), "my-app-security")

	err = c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"},
		&autoscalingv2.HorizontalPodAutoscaler{})
	assert.Error(t, err, "the autoscaler comes after the refusal and must not have been reconciled")
}

// The stop is correct; going quiet about it was not. The condition has to name
// the object, because that name is the whole remedy.
func TestReconcile_ARefusedChildIsNamedInTheStatus(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := routedApp()
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, refusedSecurityMiddleware()).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, appRequest())
	require.Error(t, err)

	var got kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &got))
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionChildrenAdopted)
	require.NotNil(t, cond, "a pass stopped by a refused child must say so on the workload")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Contains(t, cond.Message, "my-app-security",
		"the condition has to name the object that has to be renamed or removed")
}

// A workload held by a refused child kept reporting whatever the last complete
// pass found. The observation is independent of the children, so it survives.
func TestReconcile_ARefusedChildStillRecordsWhatTheDeploymentReports(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := routedApp()
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, refusedSecurityMiddleware()).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, appRequest())
	require.Error(t, err)

	var got kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &got))
	assert.NotEmpty(t, got.Status.Phase,
		"a pass that fails late still knows what the Deployment reports, and the console renders it")
}

// The condition is only discoverable if someone reads the CR. An event is what
// `kubectl describe` shows without being asked.
func TestReconcile_ARefusedChildRecordsAWarningEvent(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := routedApp()
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, refusedSecurityMiddleware()).WithStatusSubresource(app).Build()
	recorder := record.NewFakeRecorder(10)
	r := &AppReconciler{Client: c, Scheme: scheme, Recorder: recorder}

	_, err := r.Reconcile(ctx, appRequest())
	require.Error(t, err)

	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "Warning")
		assert.Contains(t, event, "ReconcileFailed")
		assert.Contains(t, event, "my-app-security")
	default:
		t.Fatal("a failed pass must leave an event on the workload; nothing was recorded")
	}
}

// The release blocker. updateStatus asserts the gate is engaged purely by having
// been reached, so a True written under an earlier generation must not outlive a
// pass that stopped before the gate was reconciled at all.
func TestReconcile_APassStoppedBeforeTheGateWithdrawsTheGateClaim(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := routedApp()
	app.Spec.Route.RequireAPIKey = true
	apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               kipperv1.ConditionAPIKeyGateReady,
		Status:             metav1.ConditionTrue,
		Reason:             "GateEngaged",
		Message:            "the API key gate is in place",
		ObservedGeneration: 3,
	})

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, refusedSecurityMiddleware()).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, appRequest())
	require.Error(t, err)

	var got kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &got))
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionAPIKeyGateReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"a gate this pass never reached must not still be reported as engaged")
}

// The failure repeats on every pass for as long as the object stays refused. An
// unconditional status write there would enqueue the next pass through the
// controller's own watch, and the failure would spin.
func TestReconcile_ARepeatedRefusalDoesNotRewriteAnUnchangedStatus(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := routedApp()
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, refusedSecurityMiddleware()).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, appRequest())
	require.Error(t, err)

	var afterFirst kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &afterFirst))

	_, err = r.Reconcile(ctx, appRequest())
	require.Error(t, err)

	var afterSecond kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &afterSecond))

	assert.Equal(t, afterFirst.ResourceVersion, afterSecond.ResourceVersion,
		"a second identical failure must not write the status again")
}

// A condition that never clears is a condition nobody trusts.
func TestReconcile_ChildrenAdoptedRecoversOnTheNextGoodPass(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := routedApp()
	refused := refusedSecurityMiddleware()
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, refused).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, appRequest())
	require.Error(t, err)

	// The remedy an operator applies: give the object back the label that says
	// whose it is.
	var live unstructured.Unstructured
	live.SetGroupVersionKind(schema.GroupVersionKind{Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware"})
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app-security", Namespace: "project-test"}, &live))
	live.SetLabels(map[string]string{"app": "my-app", kipperLabel: kipperValue})
	require.NoError(t, c.Update(ctx, &live))

	_, err = r.Reconcile(ctx, appRequest())
	require.NoError(t, err, "the pass must complete once the object is Kipper's again")

	var got kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &got))
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionChildrenAdopted)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "the condition must clear once the pass completes")
	assert.False(t, strings.Contains(cond.Message, "my-app-security"))
}

// Switching the gate off is also a claim that has to be withdrawn. Clearing the
// condition is updateStatus's job and updateStatus never runs on this path, so
// without the toggle-off branch a route that no longer asks to be gated goes on
// reporting that it is.
func TestReconcile_TurningTheGateOffWhileAChildIsRefusedClearsTheClaim(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := routedApp()
	app.Spec.Route.RequireAPIKey = false
	apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               kipperv1.ConditionAPIKeyGateReady,
		Status:             metav1.ConditionTrue,
		Reason:             "GateEngaged",
		Message:            "the API key gate is in place",
		ObservedGeneration: 3,
	})

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, refusedSecurityMiddleware()).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, appRequest())
	require.Error(t, err)

	var got kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &got))
	assert.Nil(t, apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionAPIKeyGateReady),
		"a gate that is switched off must not still be reported as engaged")
}

// A False left by the gate step itself is more specific and is kept — but only
// when this pass is the one that stopped there. Two passes in one generation can
// stop at different steps, so a pass held up earlier must not inherit the gate
// step's explanation of a gate it never reached.
func TestReconcile_AGateRefusalFromAnEarlierPassIsReplaced(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := routedApp()
	app.Spec.Route.RequireAPIKey = true
	apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               kipperv1.ConditionAPIKeyGateReady,
		Status:             metav1.ConditionFalse,
		Reason:             "MiddlewareReconcileFailed",
		Message:            "a failure recorded by an earlier pass at the gate step",
		ObservedGeneration: 4,
	})

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, refusedSecurityMiddleware()).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, appRequest())
	require.Error(t, err)

	var got kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &got))
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionAPIKeyGateReady)
	require.NotNil(t, cond)
	assert.Equal(t, "ReconcileIncomplete", cond.Reason,
		"a pass that never reached the gate must say so, not repeat what a previous pass found there")
}

// Function and Job get the event but not the condition: they have no gate
// invariant to protect and adding a condition would widen their status API for
// an incident that did not involve them. The event is what makes their failures
// discoverable, and it covers any reconcile error rather than adoption alone —
// which is why these drive a forced write failure rather than a refused child.
func TestFunctionReconcile_AFailedPassRecordsAWarningEvent(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "resize-images", Namespace: "project-test"},
		Spec:       kipperv1.FunctionSpec{Runtime: "python"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn).WithStatusSubresource(fn).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				return apierrors.NewInternalError(errRefusedByTheAPIServer)
			},
		}).Build()
	recorder := record.NewFakeRecorder(10)
	r := &FunctionReconciler{Client: c, Scheme: scheme, Recorder: recorder}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "resize-images", Namespace: "project-test"},
	})
	require.Error(t, err)

	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "Warning")
		assert.Contains(t, event, "ReconcileFailed")
	default:
		t.Fatal("a failed function pass must leave an event on the workload; nothing was recorded")
	}
}

func TestJobReconcile_AFailedPassRecordsAWarningEvent(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	job := newTestJob("0 2 * * *")
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(job).WithStatusSubresource(job).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				return apierrors.NewInternalError(errRefusedByTheAPIServer)
			},
		}).Build()
	recorder := record.NewFakeRecorder(10)
	r := &JobReconciler{Client: c, Scheme: scheme, Recorder: recorder}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: job.Name, Namespace: job.Namespace},
	})
	require.Error(t, err)

	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "Warning")
		assert.Contains(t, event, "ReconcileFailed")
	default:
		t.Fatal("a failed job pass must leave an event on the workload; nothing was recorded")
	}
}

// The other side of the deference: when the pass does stop at the gate, that
// step's own explanation is the better one and must survive the caller's
// blanket withdrawal.
func TestReconcile_AFailureAtTheGateKeepsTheGateStepsOwnExplanation(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := routedApp()
	app.Spec.Route.RequireAPIKey = true

	refusedGate := &unstructured.Unstructured{}
	refusedGate.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware",
	})
	refusedGate.SetName("my-app-apikey")
	refusedGate.SetNamespace("project-test")
	refusedGate.SetLabels(map[string]string{"app": "my-app", kipperLabel: "skipper"})

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, refusedGate).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, appRequest())
	require.Error(t, err)

	var got kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &got))
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionAPIKeyGateReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "MiddlewareReconcileFailed", cond.Reason,
		"the gate step said exactly what went wrong; the caller's general withdrawal must not overwrite it")
}
