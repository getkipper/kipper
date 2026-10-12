package handlers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
)

func TestAggregateRouteHealthSkipsAClosedRouteWithoutPublicPaths(t *testing.T) {
	live := routeEntry{Health: RouteHealth{IngressReady: true, TLSReady: true, Message: "Live."}}
	closed := routeEntry{Closed: true, Health: closedRouteHealth()}

	got := aggregateRouteHealth([]routeEntry{closed, live})

	assert.True(t, got.IngressReady)
	assert.True(t, got.TLSReady)
	assert.False(t, got.Unpublished)
	assert.Equal(t, "Live.", got.Message)
}

func TestAggregateRouteHealthOfOnlyClosedRoutes(t *testing.T) {
	got := aggregateRouteHealth([]routeEntry{{Closed: true, Health: closedRouteHealth()}})

	assert.False(t, got.IngressReady)
	assert.True(t, got.Unpublished)
	assert.Contains(t, got.Message, "nothing is published")
}

func closedApp() *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "pay", Namespace: "team-test"},
		Spec:       kipperv1.AppSpec{Image: "web:1", Port: 8080, Route: &kipperv1.AppRoute{Host: "shop.example.com", Path: "/", Closed: true}},
	}
}

func TestUnpublishedHealthWaitsForTheIngressesToGo(t *testing.T) {
	app := closedApp()
	for _, name := range []string{"pay", controllers.PublicPathsIngressName("pay")} {
		kube := fake.NewClientset(&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-test"}})
		_, ok := unpublishedHealth(context.Background(), kube, app)
		assert.False(t, ok, "Ingress %s still exists", name)
	}

	h, ok := unpublishedHealth(context.Background(), fake.NewClientset(), app)
	assert.True(t, ok)
	assert.True(t, h.Unpublished)
}

func TestUnpublishedHealthKeepsTheStopRecord(t *testing.T) {
	app := closedApp()
	app.Spec.Stopped = &kipperv1.AppStopped{}

	h, ok := unpublishedHealth(context.Background(), fake.NewClientset(), app)

	require.True(t, ok)
	assert.True(t, h.Unpublished)
	assert.True(t, h.Stopped)
}

// An invalid route base makes otherwise valid public paths unusable.
func TestUnpublishedHealthFollowsTheControllersPathRules(t *testing.T) {
	app := closedApp()
	app.Spec.Route.Path = "/pay//v1"
	app.Spec.Route.PublicPaths = []string{"/hook"}

	_, ok := unpublishedHealth(context.Background(), fake.NewClientset(), app)

	assert.True(t, ok)
}

func TestUnpublishedHealthTreatsAFailedReadAsUnconfirmed(t *testing.T) {
	kube := fake.NewClientset()
	kube.PrependReactor("get", "ingresses", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, kerrors.NewServiceUnavailable("the API server is unavailable")
	})

	_, ok := unpublishedHealth(context.Background(), kube, closedApp())

	assert.False(t, ok)
}
