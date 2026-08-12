package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// collidingFunction is a Function whose name an App in the same namespace
// already owns, which is what the console let an operator create.
func collidingFunction() *kipperv1.Function {
	return &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "hello", Namespace: "default"},
		Spec: kipperv1.FunctionSpec{
			Image:    "hello:v1",
			Port:     8080,
			Triggers: []kipperv1.FunctionTrigger{{Type: "http"}},
		},
	}
}

// deploymentOwnedByApp is the Deployment the App controller already controls,
// so the Function controller must refuse it rather than fight for it.
func deploymentOwnedByApp() *appsv1.Deployment {
	app := &kipperv1.App{ObjectMeta: metav1.ObjectMeta{
		Name: "hello", Namespace: "default", UID: "11111111-2222-3333-4444-555555555555",
	}}
	controller := true
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "hello",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(),
				Kind:       "App",
				Name:       app.Name,
				UID:        app.UID,
				Controller: &controller,
			}},
		},
	}
}

func functionRequest() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: "hello", Namespace: "default"}}
}

// The refusal is right: two controllers reconciling one Deployment would fight
// over replicas and image on every pass. Reaching only the console-api log was
// not. The Function showed an empty phase and an unrelated healthy-looking
// condition while its URL 404'd, so the only route to the cause was reading
// controller logs.
func TestFunctionReconcile_ABlockedFunctionSaysSoInItsStatus(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := collidingFunction()
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(fn, deploymentOwnedByApp()).WithStatusSubresource(fn).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, functionRequest())
	require.Error(t, err, "a child this function cannot establish as its own must stop the pass")

	var got kipperv1.Function
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "hello", Namespace: "default"}, &got))

	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionChildrenAdopted)
	require.NotNil(t, cond, "a pass stopped by a refused child must say so on the function")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Contains(t, cond.Message, "App",
		"the condition has to name the kind holding the object, because renaming is the remedy")

	assert.Equal(t, "Failed", got.Status.Phase,
		"an empty phase reads as idle in kip function list and in the console")
}

// The collision persists until somebody renames one of the two, so this path
// runs on every pass. The controller watches Functions with no status-change
// predicate, which makes each status write its own next trigger: writing
// unconditionally spins instead of backing off.
func TestFunctionReconcile_ABlockedFunctionStopsRewritingItsStatus(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := collidingFunction()
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(fn, deploymentOwnedByApp()).WithStatusSubresource(fn).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, functionRequest())
	require.Error(t, err)

	var afterFirst kipperv1.Function
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "hello", Namespace: "default"}, &afterFirst))

	_, err = r.Reconcile(ctx, functionRequest())
	require.Error(t, err, "the collision is unchanged, so the pass still fails")

	var afterSecond kipperv1.Function
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "hello", Namespace: "default"}, &afterSecond))

	assert.Equal(t, afterFirst.ResourceVersion, afterSecond.ResourceVersion,
		"a second pass that learned nothing new wrote status again, which re-enqueues itself")
}
