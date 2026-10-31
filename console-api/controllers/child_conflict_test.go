package controllers

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// TestFunctionReconcile_AConflictWithAScaleChangeIsNotAFailure checks that a
// Deployment write conflict, such as one caused by concurrent KEDA scaling,
// is returned for retry and preserves the function's phase and adoption condition.
func TestFunctionReconcile_AConflictWithAScaleChangeIsNotAFailure(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := collidingFunction()
	conflict := false
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn).WithStatusSubresource(fn).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok && conflict {
					return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, obj.GetName(), errors.New("the object has been modified"))
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}
	_, err := r.Reconcile(ctx, functionRequest())
	require.NoError(t, err)
	var before kipperv1.Function
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "hello", Namespace: "default"}, &before))
	// Simulate a defaulted field so the template differs and reconciliation
	// attempts the Deployment update that will conflict.
	var deploy appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "hello", Namespace: "default"}, &deploy))
	deploy.Spec.Template.Spec.Containers[0].TerminationMessagePath = "/dev/termination-log"
	require.NoError(t, c.Update(ctx, &deploy))

	conflict = true
	_, err = r.Reconcile(ctx, functionRequest())

	require.True(t, apierrors.IsConflict(err), "The conflict must be returned to the controller for a rate-limited retry. Got %v.", err)
	var got kipperv1.Function
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "hello", Namespace: "default"}, &got))
	assert.Equal(t, before.Status.Phase, got.Status.Phase)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionChildrenAdopted)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
}

// TestAppReconcile_AConflictWithAScaleChangeIsNotAFailure checks that a
// Deployment write conflict, such as one caused by concurrent HPA scaling,
// preserves the App's rollout and adoption conditions.
func TestAppReconcile_AConflictWithAScaleChangeIsNotAFailure(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	conflict := false
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).WithStatusSubresource(app).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok && conflict {
					return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, obj.GetName(), errors.New("the object has been modified"))
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}
	key := types.NamespacedName{Name: app.Name, Namespace: app.Namespace}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	var before kipperv1.App
	require.NoError(t, c.Get(ctx, key, &before))
	var deploy appsv1.Deployment
	require.NoError(t, c.Get(ctx, key, &deploy))
	deploy.Spec.Template.Spec.Containers[0].TerminationMessagePath = "/dev/termination-log"
	require.NoError(t, c.Update(ctx, &deploy))

	conflict = true
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})

	require.True(t, apierrors.IsConflict(err), "The conflict must be returned to the controller for a rate-limited retry. Got %v.", err)
	var got kipperv1.App
	require.NoError(t, c.Get(ctx, key, &got))
	assert.Equal(t, before.Status.Conditions, got.Status.Conditions)
}

// TestAppReconcile_AConflictWithdrawsAnUnverifiedGateClaim checks that a
// conflict before the API key gate step marks the gate as unverified because
// reconciliation stopped before checking it, and leaves adoption status unchanged.
func TestAppReconcile_AConflictWithdrawsAnUnverifiedGateClaim(t *testing.T) {
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
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).WithStatusSubresource(app).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, obj.GetName(), errors.New("the object has been modified"))
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, appRequest())

	require.True(t, apierrors.IsConflict(err), "The conflict must be returned to the controller for a rate-limited retry. Got %v.", err)
	var got kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &got))
	gate := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionAPIKeyGateReady)
	require.NotNil(t, gate)
	assert.Equal(t, metav1.ConditionFalse, gate.Status, "The gate must be reported as unverified because reconciliation stopped before checking it.")
	assert.Equal(t, "ReconcileIncomplete", gate.Reason)
	assert.Nil(t, apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionChildrenAdopted),
		"A conflict must leave adoption status unchanged.")
}

// TestFunctionReconcile_ASingleScaleConflictIsRetriedInPlace checks that one
// simulated Deployment write conflict is retried in the same reconciliation,
// as in the App controller. KEDA changing replicas between a read and write
// can cause such a conflict.
func TestFunctionReconcile_ASingleScaleConflictIsRetriedInPlace(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := collidingFunction()
	conflicts := 0
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn).WithStatusSubresource(fn).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok && conflicts > 0 {
					conflicts--
					return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, obj.GetName(), errors.New("the object has been modified"))
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}
	_, err := r.Reconcile(ctx, functionRequest())
	require.NoError(t, err)
	var deploy appsv1.Deployment
	key := types.NamespacedName{Name: "hello", Namespace: "default"}
	require.NoError(t, c.Get(ctx, key, &deploy))
	deploy.Spec.Template.Spec.Containers[0].TerminationMessagePath = "/dev/termination-log"
	require.NoError(t, c.Update(ctx, &deploy))

	conflicts = 1
	_, err = r.Reconcile(ctx, functionRequest())

	require.NoError(t, err)
	assert.Zero(t, conflicts, "The conflicting write was attempted.")
	require.NoError(t, c.Get(ctx, key, &deploy))
	assert.Empty(t, deploy.Spec.Template.Spec.Containers[0].TerminationMessagePath, "The retried write stored the generated template.")
}
