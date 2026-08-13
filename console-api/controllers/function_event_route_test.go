package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// The KEDA and Traefik kinds this controller writes are handled unstructured,
// so the fake client needs them registered to serve a Get or a Create.
func routingScheme() *runtime.Scheme {
	s := testScheme()
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "http.keda.sh", Version: "v1alpha1", Kind: "HTTPScaledObject"},
		{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"},
		{Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware"},
	} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

// A function triggered by a database event is invoked by its poll sidecar, not
// over HTTP. It still needs a long-lived Pod, which is why it shares the
// Deployment and Service path with an HTTP function.
func eventTriggeredFunction() *kipperv1.Function {
	return &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "shop-prod"},
		Spec: kipperv1.FunctionSpec{
			Image:    "orders:v1",
			Port:     8080,
			Triggers: []kipperv1.FunctionTrigger{{Type: "postgres"}},
		},
	}
}

// Two rules decided what "serves HTTP" meant and they disagreed. The serving
// path ran for any non-cron trigger, so an event-triggered function got an
// Ingress pointing at the KEDA interceptor, while the HTTPScaledObject that
// puts a host in the interceptor's routing table was created only for a
// literal http trigger. The interceptor answers 404 for a host it has no route
// for, so the function was published on a URL that could never reach it and no
// request could ever wake it.
func TestReconcileChildren_EventFunctionGetsNoDeadHTTPRoute(t *testing.T) {
	ctx := context.Background()
	scheme := routingScheme()

	fn := eventTriggeredFunction()
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(fn).WithStatusSubresource(fn).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme, Domain: "203-0-113-10.kipper.run"}

	require.NoError(t, r.reconcileChildren(ctx, fn, "", nil, "", renderedBindings{}))

	var ing networkingv1.Ingress
	err := c.Get(ctx, types.NamespacedName{Name: "fn-orders", Namespace: "keda"}, &ing)
	assert.True(t, apierrors.IsNotFound(err),
		"an event-triggered function has no HTTPScaledObject, so an Ingress to the interceptor is a route that can only 404")
}

// The counterpart: an HTTP function still gets both halves, so the fix cannot
// be "stop creating the Ingress".
func TestReconcileChildren_HTTPFunctionGetsRouteAndScaler(t *testing.T) {
	ctx := context.Background()
	scheme := routingScheme()

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "shop-prod"},
		Spec: kipperv1.FunctionSpec{
			Image:    "orders:v1",
			Port:     8080,
			Triggers: []kipperv1.FunctionTrigger{{Type: "http"}},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(fn).WithStatusSubresource(fn).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme, Domain: "203-0-113-10.kipper.run"}

	require.NoError(t, r.reconcileChildren(ctx, fn, "", nil, "", renderedBindings{}))

	var ing networkingv1.Ingress
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "fn-orders", Namespace: "keda"}, &ing),
		"an http function must keep its route")

	hso := &unstructured.Unstructured{}
	hso.SetGroupVersionKind(schema.GroupVersionKind{Group: "http.keda.sh", Version: "v1alpha1", Kind: "HTTPScaledObject"})
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "orders", Namespace: "shop-prod"}, hso),
		"the route only works because this puts the host in the interceptor's table")
}
