package controllers

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func validPolicy() *kipperv1.AppAutoscale {
	return &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](2), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)}
}

func (h *stopHarness) app() *kipperv1.App {
	h.t.Helper()
	var app kipperv1.App
	require.NoError(h.t, h.c.Get(context.Background(), h.key, &app))
	return &app
}

func (h *stopHarness) hpa() (*autoscalingv2.HorizontalPodAutoscaler, error) {
	var hpa autoscalingv2.HorizontalPodAutoscaler
	err := h.c.Get(context.Background(), h.key, &hpa)
	return &hpa, err
}

func autoscalingCondition(app *kipperv1.App) *metav1.Condition {
	return apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionAutoscalingReady)
}

func TestAutoscaling_UsablePolicyReportsReadyAndDisablingRemovesTheCondition(t *testing.T) {
	app := newTestApp()
	app.Spec.Autoscale = validPolicy()
	app.Spec.Replicas = i32(2)
	h := newStopHarness(t, app)
	h.reconcile()
	cond := autoscalingCondition(h.app())
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)

	h.edit(func(a *kipperv1.App) { a.Spec.Autoscale.Enabled = false })
	h.reconcile()
	_, err := h.hpa()
	assert.Error(t, err, "switching the policy off deletes the autoscaler")
	assert.Nil(t, autoscalingCondition(h.app()), "with no policy and no bounds problem there is nothing to report")
}

func TestAutoscaling_UnusablePolicyLeavesTheExistingAutoscalerAndCountAlone(t *testing.T) {
	app := newTestApp()
	app.Spec.Autoscale = validPolicy()
	h := newStopHarness(t, app)
	h.reconcile()
	before, err := h.hpa()
	require.NoError(t, err)
	h.setLiveReplicas(4)

	h.edit(func(a *kipperv1.App) {
		a.Spec.Autoscale = &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](6), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)}
	})
	h.reconcile()

	after, err := h.hpa()
	require.NoError(t, err, "an unusable policy must not delete the autoscaler")
	assert.Equal(t, before.Spec, after.Spec, "an unusable policy must not rewrite the autoscaler")
	assert.Equal(t, int32(4), *h.deployment().Spec.Replicas)
	cond := autoscalingCondition(h.app())
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "InvalidPolicy", cond.Reason)
	assert.Contains(t, cond.Message, "minReplicas (6) must not exceed maxReplicas (5)")
}

func TestAutoscaling_UnusablePolicyWithoutAnAutoscalerKeepsTheCount(t *testing.T) {
	app := newTestApp()
	app.Spec.Replicas = i32(3)
	h := newStopHarness(t, app)
	h.reconcile()

	h.edit(func(a *kipperv1.App) {
		a.Spec.Autoscale = &kipperv1.AppAutoscale{Enabled: true, MaxReplicas: ptr.To[int32](5)}
	})
	h.setLiveReplicas(4)
	h.reconcile()

	_, err := h.hpa()
	assert.Error(t, err, "no autoscaler is built from a policy without a target")
	assert.Equal(t, int32(4), *h.deployment().Spec.Replicas)
	assert.Equal(t, "InvalidPolicy", autoscalingCondition(h.app()).Reason)
}

// Bootstrap a usable policy from zero, then return replica control to the HPA.
func TestAutoscaling_UsablePolicyStartsADeploymentAtZeroAtItsMinimum(t *testing.T) {
	app := newTestApp()
	app.Spec.Autoscale = validPolicy()
	h := newStopHarness(t, app)
	h.reconcile()
	h.setLiveReplicas(0)
	h.reconcile()
	assert.Equal(t, int32(2), *h.deployment().Spec.Replicas)

	// From here the autoscaler owns the count again.
	h.setLiveReplicas(4)
	h.reconcile()
	assert.Equal(t, int32(4), *h.deployment().Spec.Replicas)
}

func TestAutoscaling_UnusablePolicyAtZeroIsNotStarted(t *testing.T) {
	app := newTestApp()
	app.Spec.Autoscale = &kipperv1.AppAutoscale{Enabled: true, MaxReplicas: ptr.To[int32](5)}
	h := newStopHarness(t, app)
	h.reconcile()
	h.setLiveReplicas(0)
	h.reconcile()
	assert.Equal(t, int32(0), *h.deployment().Spec.Replicas)
}

// A zero minimum predates the CRD's minimum of 1, so only legacy objects hold it.
func legacyZeroMinimumPolicy() *kipperv1.AppAutoscale {
	return &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](0), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)}
}

func TestAutoscaling_ALegacyZeroMinimumAtZeroIsNotStarted(t *testing.T) {
	app := newTestApp()
	h := newStopHarness(t, app)
	h.reconcile()
	h.edit(func(a *kipperv1.App) { a.Spec.Autoscale = legacyZeroMinimumPolicy() })
	h.setLiveReplicas(0)
	h.reconcile()

	assert.Equal(t, int32(0), *h.deployment().Spec.Replicas)
	_, err := h.hpa()
	assert.Error(t, err, "no autoscaler is built from a zero minimum")
	cond := autoscalingCondition(h.app())
	require.NotNil(t, cond)
	assert.Equal(t, "InvalidPolicy", cond.Reason)
	assert.Contains(t, cond.Message, "minReplicas must be at least 1")
}

func TestAutoscaling_ALegacyZeroMinimumKeepsTheExistingAutoscaler(t *testing.T) {
	app := newTestApp()
	app.Spec.Autoscale = validPolicy()
	h := newStopHarness(t, app)
	h.reconcile()
	before, err := h.hpa()
	require.NoError(t, err)

	h.edit(func(a *kipperv1.App) { a.Spec.Autoscale = legacyZeroMinimumPolicy() })
	h.setLiveReplicas(0)
	h.reconcile()

	after, err := h.hpa()
	require.NoError(t, err)
	assert.Equal(t, before.Spec, after.Spec, "a zero minimum must not rewrite the autoscaler")
	assert.Equal(t, int32(0), *h.deployment().Spec.Replicas)
	assert.Equal(t, "InvalidPolicy", autoscalingCondition(h.app()).Reason)
}

func TestAutoscaling_AZeroTargetBesideAPositiveOneIsUsable(t *testing.T) {
	app := newTestApp()
	app.Spec.Autoscale = validPolicy()
	app.Spec.Autoscale.MemoryTarget = ptr.To[int32](0)
	app.Spec.Replicas = i32(2)
	h := newStopHarness(t, app)
	h.reconcile()

	hpa, err := h.hpa()
	require.NoError(t, err)
	require.Len(t, hpa.Spec.Metrics, 1, "a zero target adds no metric")
	assert.Equal(t, metav1.ConditionTrue, autoscalingCondition(h.app()).Status)
}

func TestAutoscaling_StartWithAnUnusablePolicyUsesTheStoredCount(t *testing.T) {
	app := newTestApp()
	app.Spec.Replicas = i32(3)
	app.Spec.Autoscale = &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](10), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)}
	stop(app)
	h := newStopHarness(t, app)
	h.reconcile()
	h.edit(start)
	h.reconcile()
	assert.Equal(t, int32(3), *h.deployment().Spec.Replicas, "an unusable minimum must not decide the start")
}

func TestAutoscaling_ReplicasOutsideBoundsKeepTheRunningCount(t *testing.T) {
	app := newTestApp()
	app.Spec.Replicas = i32(3)
	h := newStopHarness(t, app)
	h.reconcile()

	h.edit(func(a *kipperv1.App) {
		a.Spec.Replicas = i32(8)
		a.Spec.Autoscale = &kipperv1.AppAutoscale{MinReplicas: ptr.To[int32](1), MaxReplicas: ptr.To[int32](5)}
	})
	h.reconcile()

	assert.Equal(t, int32(3), *h.deployment().Spec.Replicas, "a stored count outside the bounds is not applied")
	cond := autoscalingCondition(h.app())
	require.NotNil(t, cond)
	assert.Equal(t, "ReplicasOutsideBounds", cond.Reason)
	assert.Contains(t, cond.Message, "8")
}

func TestAutoscaling_InvalidPolicyWinsOverReplicasOutsideBounds(t *testing.T) {
	app := newTestApp()
	app.Spec.Replicas = i32(8)
	app.Spec.Autoscale = &kipperv1.AppAutoscale{MinReplicas: ptr.To[int32](6), MaxReplicas: ptr.To[int32](5)}
	h := newStopHarness(t, app)
	h.reconcile()
	assert.Equal(t, "InvalidPolicy", autoscalingCondition(h.app()).Reason)
}

// Delete the disabled HPA before route reconciliation can fail, preserving
// the replica count set by the Deployment step.
func TestAutoscaling_DisableDeletesTheAutoscalerDespiteALaterRefusedChild(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := routedApp()
	app.Spec.Autoscale = &kipperv1.AppAutoscale{Enabled: false, MinReplicas: ptr.To[int32](2), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)}
	owned := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "project-test",
			Labels: map[string]string{"app": "my-app", kipperLabel: kipperValue}},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: 5,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "my-app"}},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, owned, refusedSecurityMiddleware()).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, appRequest())
	require.Error(t, err, "the refused middleware still stops the pass")
	err = c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &autoscalingv2.HorizontalPodAutoscaler{})
	assert.Error(t, err, "the autoscaler is deleted before the refused child")
}

func hpaDeleteFailures(n int) interceptor.Funcs {
	left := n
	return interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler); ok && left > 0 {
				left--
				return errors.New("delete refused for the test")
			}
			return c.Delete(ctx, obj, opts...)
		},
	}
}

func autoscaledHarness(t *testing.T, funcs interceptor.Funcs) *stopHarness {
	t.Helper()
	scheme := testScheme()
	app := newTestApp()
	app.Spec.Autoscale = validPolicy()
	app.Spec.Replicas = i32(2)
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).WithStatusSubresource(app).
		WithInterceptorFuncs(funcs).Build()
	h := &stopHarness{t: t, c: c, r: &AppReconciler{Client: c, Scheme: scheme},
		key: types.NamespacedName{Name: app.Name, Namespace: app.Namespace}}
	return h
}

func TestAutoscaling_AFailedEarlyDeleteIsRetriedByTheFinalStep(t *testing.T) {
	h := autoscaledHarness(t, hpaDeleteFailures(1))
	h.reconcile()
	h.edit(func(a *kipperv1.App) { a.Spec.Autoscale.Enabled = false })
	h.reconcile()
	_, err := h.hpa()
	assert.Error(t, err, "the final step deletes what the early step could not")
	assert.Nil(t, autoscalingCondition(h.app()), "a later success clears the early failure")
}

func TestAutoscaling_ADeleteThatKeepsFailingIsReported(t *testing.T) {
	h := autoscaledHarness(t, hpaDeleteFailures(2))
	h.reconcile()
	h.edit(func(a *kipperv1.App) { a.Spec.Autoscale.Enabled = false })
	_, err := h.r.Reconcile(context.Background(), appRequest())
	require.Error(t, err)
	_, getErr := h.hpa()
	assert.NoError(t, getErr)
	cond := autoscalingCondition(h.app())
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "AutoscalerDeleteFailed", cond.Reason)
}

// An HPA failure follows successful Deployment reconciliation, so rollout
// status should continue to describe that Deployment.
func TestAutoscaling_AnAutoscalerFailureLeavesRolloutAndGateAlone(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := routedApp()
	app.Spec.Autoscale = validPolicy()
	foreign := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "project-test",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "other", UID: "uid-other", Controller: ptrTrue()}}},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: 3,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "other"}},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, foreign).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, appRequest())
	require.Error(t, err)
	var got kipperv1.App
	require.NoError(t, c.Get(ctx, appRequest().NamespacedName, &got))
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionRolloutComplete); cond != nil {
		assert.NotEqual(t, "NotApplied", cond.Reason, "the Deployment did apply")
	}
	cond := autoscalingCondition(&got)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "AutoscalerReconcileFailed", cond.Reason)
}

func TestAutoscaling_AnAutoscalerFailureDoesNotWithdrawTheGate(t *testing.T) {
	app := routedApp()
	app.Spec.Route.RequireAPIKey = true
	apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type: kipperv1.ConditionAPIKeyGateReady, Status: metav1.ConditionTrue, Reason: "GateEngaged",
		Message: "the API key gate is in place", ObservedGeneration: 4,
	})
	r := &AppReconciler{}
	r.withdrawAPIKeyGate(app, autoscalerStepError{err: errors.New("reconciling hpa: refused")})
	cond := apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionAPIKeyGateReady)
	require.NotNil(t, cond)
	assert.Equal(t, "GateEngaged", cond.Reason, "the gate step ran before the autoscaler, so its claim stands")
}

func ptrTrue() *bool { b := true; return &b }

// Preserve the early deletion error when a later child prevents the HPA retry.
func TestAutoscaling_AFailedEarlyDeleteIsReportedWhenALaterChildStopsThePass(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := routedApp()
	app.Spec.Autoscale = &kipperv1.AppAutoscale{Enabled: false, MinReplicas: ptr.To[int32](1), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)}
	owned := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "project-test",
			Labels: map[string]string{"app": "my-app", kipperLabel: kipperValue}},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: 5,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "my-app"}},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, owned, refusedSecurityMiddleware()).WithStatusSubresource(app).
		WithInterceptorFuncs(hpaDeleteFailures(1)).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, appRequest())
	require.Error(t, err)
	var got kipperv1.App
	require.NoError(t, c.Get(ctx, appRequest().NamespacedName, &got))
	cond := autoscalingCondition(&got)
	require.NotNil(t, cond)
	assert.Equal(t, "AutoscalerDeleteFailed", cond.Reason)
}

func TestAutoscaling_StartWithACountOutsideTheBoundsStartsWithinThem(t *testing.T) {
	app := newTestApp()
	app.Spec.Replicas = i32(8)
	app.Spec.Autoscale = &kipperv1.AppAutoscale{MinReplicas: ptr.To[int32](1), MaxReplicas: ptr.To[int32](5)}
	stop(app)
	h := newStopHarness(t, app)
	h.reconcile()
	h.edit(start)
	h.reconcile()
	assert.Equal(t, int32(5), *h.deployment().Spec.Replicas, "a start never runs the app outside its bounds")
	cond := autoscalingCondition(h.app())
	require.NotNil(t, cond)
	assert.Equal(t, "ReplicasOutsideBounds", cond.Reason)
}

func TestAutoscaling_EnabledPolicyReportsAStoredCountOutsideTheBounds(t *testing.T) {
	app := newTestApp()
	app.Spec.Replicas = i32(1)
	app.Spec.Autoscale = validPolicy()
	h := newStopHarness(t, app)
	h.reconcile()
	_, err := h.hpa()
	require.NoError(t, err, "the autoscaler still runs")
	cond := autoscalingCondition(h.app())
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "ReplicasOutsideBounds", cond.Reason)
}

func TestAutoscaling_EarlyDeleteClearsAStaleConditionWhenALaterChildStopsThePass(t *testing.T) {
	for _, stale := range []struct {
		status metav1.ConditionStatus
		reason string
	}{
		{metav1.ConditionTrue, "PolicyApplied"},
		{metav1.ConditionFalse, "AutoscalerDeleteFailed"},
	} {
		t.Run(stale.reason, func(t *testing.T) {
			ctx := context.Background()
			scheme := testScheme()
			app := routedApp()
			app.Spec.Replicas = i32(2)
			app.Spec.Autoscale = &kipperv1.AppAutoscale{Enabled: false, MinReplicas: ptr.To[int32](2), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)}
			setAutoscalingCondition(app, stale.status, stale.reason, "left by an earlier pass")
			owned := &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "project-test",
					Labels: map[string]string{"app": "my-app", kipperLabel: kipperValue}},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: 5,
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "my-app"}},
			}
			c := crfake.NewClientBuilder().WithScheme(scheme).
				WithObjects(app, owned, refusedSecurityMiddleware()).WithStatusSubresource(app).Build()
			r := &AppReconciler{Client: c, Scheme: scheme}

			_, err := r.Reconcile(ctx, appRequest())
			require.Error(t, err)
			var got kipperv1.App
			require.NoError(t, c.Get(ctx, appRequest().NamespacedName, &got))
			assert.Nil(t, autoscalingCondition(&got), "the policy is off and the autoscaler is gone")
		})
	}
}

func TestAutoscaling_AnAutoscalerFailureRecordsTheGateItApplied(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := routedApp()
	app.Spec.Route.RequireAPIKey = true
	app.Spec.Autoscale = validPolicy()
	app.Spec.Replicas = i32(2)
	apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type: kipperv1.ConditionAPIKeyGateReady, Status: metav1.ConditionFalse, Reason: "MiddlewareReconcileFailed",
		Message: "an earlier pass could not apply the gate", ObservedGeneration: 4,
	})
	foreign := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "project-test",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "other", UID: "uid-other", Controller: ptrTrue()}}},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: 3,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "other"}},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, foreign).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, appRequest())
	require.Error(t, err)
	require.Contains(t, err.Error(), "reconciling hpa", "only the autoscaler may fail in this test")
	var got kipperv1.App
	require.NoError(t, c.Get(ctx, appRequest().NamespacedName, &got))
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionAPIKeyGateReady)
	require.NotNil(t, cond)
	assert.Equal(t, "GateEngaged", cond.Reason, "every route step applied before the autoscaler failed")
}

func TestAutoscaling_ARepeatedAutoscalerFailureWritesStatusOnce(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := routedApp()
	app.Spec.Autoscale = validPolicy()
	app.Spec.Replicas = i32(2)
	foreign := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "project-test",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "other", UID: "uid-other", Controller: ptrTrue()}}},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: 3,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "other"}},
	}
	writes := 0
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, foreign).WithStatusSubresource(app).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if _, ok := obj.(*kipperv1.App); ok {
					writes++
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, appRequest())
	require.Error(t, err)
	first := writes
	_, err = r.Reconcile(ctx, appRequest())
	require.Error(t, err)
	assert.Equal(t, first, writes, "a failure that has not changed must not write the status again")
}

func TestAutoscaling_ARecreatedDeploymentStartsWithinTheBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stored int32
		want   int32
	}{
		{"above the maximum", 8, 5},
		{"at zero", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestApp()
			app.Spec.Replicas = i32(tc.stored)
			app.Spec.Autoscale = &kipperv1.AppAutoscale{MinReplicas: ptr.To[int32](1), MaxReplicas: ptr.To[int32](5)}
			h := newStopHarness(t, app)
			h.reconcile()
			assert.Equal(t, tc.want, *h.deployment().Spec.Replicas, "a created Deployment never runs outside the bounds")
			h.reconcile()
			assert.Equal(t, tc.want, *h.deployment().Spec.Replicas)
			assert.Equal(t, "ReplicasOutsideBounds", autoscalingCondition(h.app()).Reason)
		})
	}
}

func TestAutoscaling_StartWithAMissingDeploymentStartsWithinTheBounds(t *testing.T) {
	app := newTestApp()
	app.Spec.Replicas = i32(8)
	app.Spec.Autoscale = &kipperv1.AppAutoscale{MinReplicas: ptr.To[int32](1), MaxReplicas: ptr.To[int32](5)}
	stop(app)
	h := newStopHarness(t, app)
	h.reconcile()
	require.NoError(t, h.c.Delete(context.Background(), h.deployment()))
	h.edit(start)
	h.reconcile()
	assert.Equal(t, int32(5), *h.deployment().Spec.Replicas)
}

// legacyCLIAutoscaler is the HPA `kip app autoscale` created before the CLI
// wrote the App spec: no owner reference, only Kipper's labels, stale bounds.
func legacyCLIAutoscaler() *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app",
			Namespace: "project-test",
			Labels:    map[string]string{"app": "my-app", kipperLabel: kipperValue},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "my-app"},
			MinReplicas:    ptr.To[int32](1),
			MaxReplicas:    3,
		},
	}
}

func TestAutoscaling_AnEnabledPolicyAdoptsALegacyCLIAutoscaler(t *testing.T) {
	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Replicas = i32(2)
	app.Spec.Autoscale = validPolicy()
	h := newStopHarness(t, app, legacyCLIAutoscaler())
	h.reconcile()

	hpa, err := h.hpa()
	require.NoError(t, err, "the legacy autoscaler is kept once the spec says enabled")
	owner := metav1.GetControllerOf(hpa)
	require.NotNil(t, owner, "the legacy autoscaler is adopted")
	assert.Equal(t, types.UID("uid-my-app"), owner.UID)
	assert.Equal(t, int32(2), *hpa.Spec.MinReplicas, "the spec's bounds replace the stale ones")
	assert.Equal(t, int32(5), hpa.Spec.MaxReplicas)
}

func TestAutoscaling_WithoutAnEnabledPolicyALegacyCLIAutoscalerIsDeleted(t *testing.T) {
	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Replicas = i32(2)
	h := newStopHarness(t, app, legacyCLIAutoscaler())
	h.reconcile()

	_, err := h.hpa()
	assert.Error(t, err, "a legacy autoscaler the spec does not ask for is deleted, as before")
	assert.Equal(t, int32(2), *h.deployment().Spec.Replicas)
}
