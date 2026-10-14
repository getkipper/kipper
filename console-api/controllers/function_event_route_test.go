package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// eventTriggeredFunction uses a poller that invokes the function over local HTTP,
// so it needs a Deployment but no public HTTP route.
func eventTriggeredFunction() *kipperv1.Function {
	return &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "shop-prod"},
		Spec: kipperv1.FunctionSpec{
			Image:    "orders:v1",
			Port:     8080,
			Triggers: []kipperv1.FunctionTrigger{{Type: "postgres", Config: map[string]string{"source": "db", "table": "orders"}}},
		},
	}
}

func eventSource() []client.Object {
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop-prod", UID: types.UID("db-uid")},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	yes := true
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-credentials", Namespace: "shop-prod",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: kipperv1.GroupVersion.String(), Kind: "Service", Name: "db", UID: svc.UID, Controller: &yes}},
		},
		Data: map[string][]byte{"HOST": []byte("db"), "PORT": []byte("5432"), "USERNAME": []byte("app"), "PASSWORD": []byte("pw")},
	}
	return []client.Object{svc, secret}
}

// Event functions have no HTTPScaledObject to register a route with the KEDA
// interceptor, so an Ingress pointing there would send requests to a missing route.
func TestReconcileChildren_EventFunctionGetsNoDeadHTTPRoute(t *testing.T) {
	ctx := context.Background()
	scheme := routingScheme()

	fn := eventTriggeredFunction()
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(append(eventSource(), fn)...).WithStatusSubresource(fn).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme, Domain: "203-0-113-10.kipper.run"}

	require.NoError(t, r.reconcileChildren(ctx, fn, "", nil, "", renderedBindings{}))

	var ing networkingv1.Ingress
	err := c.Get(ctx, types.NamespacedName{Name: "fn-orders", Namespace: "keda"}, &ing)
	assert.True(t, apierrors.IsNotFound(err),
		"an event-triggered function has no HTTPScaledObject, so an Ingress to the interceptor is a route that can only 404")
}

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
