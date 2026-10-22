package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestReconcile_AConflictLeavesNoWarningEvent checks that a conflict reaches
// the controller for retry without emitting a warning for concurrent updates.
func TestReconcile_AConflictLeavesNoWarningEvent(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := newTestApp()
	conflict := func(obj client.Object) error {
		if _, ok := obj.(*appsv1.Deployment); ok {
			return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, obj.GetName(), assert.AnError)
		}
		return nil
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).WithStatusSubresource(app).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if err := conflict(obj); err != nil {
					return err
				}
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if err := conflict(obj); err != nil {
					return err
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
	recorder := record.NewFakeRecorder(10)
	r := &AppReconciler{Client: c, Scheme: scheme, Recorder: recorder}

	_, err := r.Reconcile(ctx, appRequest())
	require.Error(t, err)
	require.True(t, apierrors.IsConflict(err), "the test needs the pass to fail on the conflict: %v", err)

	select {
	case event := <-recorder.Events:
		t.Fatalf("a conflict is retried and needs no event, got %q", event)
	default:
	}
}
