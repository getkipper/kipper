package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func routedShop() *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Image: "registry.example.com/shop:1", Port: 8080, Route: &kipperv1.AppRoute{Host: "shop.example.com"}},
	}
}

func TestPortWatched(t *testing.T) {
	inferredNoCheck := kipperv1.AppHealthCheckStatus{Type: "none", Source: "inferred"}
	pending := kipperv1.AppHealthCheckStatus{Type: "none", Source: "pending"}
	inferredTCP := kipperv1.AppHealthCheckStatus{Type: "tcp", Port: 8080, Source: "inferred"}
	declaredNone := kipperv1.AppHealthCheckStatus{Type: "none", Source: "declared"}

	assert.True(t, portWatched(routedShop(), inferredNoCheck, false), "a routed app whose readiness tests nothing")
	assert.True(t, portWatched(routedShop(), pending, false))
	assert.False(t, portWatched(routedShop(), inferredTCP, false), "the readiness probe already tests the port")
	assert.False(t, portWatched(routedShop(), declaredNone, false), "the app said it serves nothing")

	worker := routedShop()
	worker.Spec.Route = nil
	assert.False(t, portWatched(worker, inferredNoCheck, false), "nothing reaches a worker without a route or a link")
	assert.True(t, portWatched(worker, inferredNoCheck, true), "another app links to it, so something reaches it")

	stopped := routedShop()
	stopped.Spec.Stopped = &kipperv1.AppStopped{}
	assert.False(t, portWatched(stopped, inferredNoCheck, false))
}

func observePortOf(t *testing.T, answers map[string]error, pods ...podState) (*kipperv1.App, time.Duration) {
	t.Helper()
	c, dep := inferenceWorld(t, pods...)
	r := &AppReconciler{Client: c, PortDial: (&fakeDialer{answers: answers}).dial}
	app := routedShop()
	next := r.observePort(context.Background(), app, dep, kipperv1.AppHealthCheckStatus{Type: "none", Source: "inferred"}, inferNow)
	return app, next
}

func TestObservePort_ReportsNothingListeningOnASettledPod(t *testing.T) {
	app, next := observePortOf(t, map[string]error{"10.0.0.1": refused}, podState{ip: "10.0.0.1", runningFor: 3 * time.Hour})

	cond := apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionPortListening)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "NothingListening", cond.Reason)
	assert.Contains(t, cond.Message, "8080")
	assert.Equal(t, portRecheck, next)
}

func TestObservePort_ReportsListeningWhenAPodAccepts(t *testing.T) {
	app, _ := observePortOf(t, map[string]error{"10.0.0.1": nil}, podState{ip: "10.0.0.1", runningFor: 3 * time.Hour})

	cond := apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionPortListening)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
}

func TestObservePort_StaysQuietWhenTheAnswerIsInconclusive(t *testing.T) {
	young, _ := observePortOf(t, map[string]error{"10.0.0.1": refused}, podState{ip: "10.0.0.1", runningFor: 2 * time.Minute})
	assert.Nil(t, apimeta.FindStatusCondition(young.Status.Conditions, kipperv1.ConditionPortListening))

	blocked, _ := observePortOf(t, map[string]error{"10.0.0.1": timedOut}, podState{ip: "10.0.0.1", runningFor: 3 * time.Hour})
	assert.Nil(t, apimeta.FindStatusCondition(blocked.Status.Conditions, kipperv1.ConditionPortListening))
}

func TestObservePort_OneAcceptingReplicaIsEnough(t *testing.T) {
	app, _ := observePortOf(t, map[string]error{"10.0.0.1": refused, "10.0.0.2": nil},
		podState{ip: "10.0.0.1", runningFor: 3 * time.Hour}, podState{ip: "10.0.0.2", runningFor: 3 * time.Hour})

	assert.Equal(t, metav1.ConditionTrue, apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionPortListening).Status)
}

func TestObservePort_DropsTheConditionWhenNoLongerWatched(t *testing.T) {
	c, dep := inferenceWorld(t, podState{ip: "10.0.0.1", runningFor: 3 * time.Hour})
	r := &AppReconciler{Client: c, PortDial: (&fakeDialer{answers: map[string]error{"10.0.0.1": refused}}).dial}
	app := routedShop()
	apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{Type: kipperv1.ConditionPortListening, Status: metav1.ConditionFalse, Reason: "NothingListening"})

	next := r.observePort(context.Background(), app, dep, kipperv1.AppHealthCheckStatus{Type: "tcp", Port: 8080, Source: "inferred"}, inferNow)

	assert.Nil(t, apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionPortListening))
	assert.Zero(t, next)
}

func TestObserveWorkload_SetsPortListening(t *testing.T) {
	c, _ := inferenceWorld(t, podState{ip: "10.0.0.1", runningFor: 3 * time.Hour})
	r := &AppReconciler{Client: c, PortDial: (&fakeDialer{answers: map[string]error{"10.0.0.1": refused}}).dial}
	app := routedShop()

	next, err := r.observeWorkload(context.Background(), app)

	require.NoError(t, err)
	cond := apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionPortListening)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.NotZero(t, next)
	assert.LessOrEqual(t, next, portRecheck)
}

func TestObservePort_DialsAtMostOncePerInterval(t *testing.T) {
	c, dep := inferenceWorld(t, podState{ip: "10.0.0.1", runningFor: 3 * time.Hour})
	dialer := &fakeDialer{answers: map[string]error{"10.0.0.1": refused}}
	r := &AppReconciler{Client: c, PortDial: dialer.dial}
	app := routedShop()
	app.UID = "shop-uid"
	check := kipperv1.AppHealthCheckStatus{Type: "none", Source: "inferred"}

	r.observePort(context.Background(), app, dep, check, inferNow)
	next := r.observePort(context.Background(), app, dep, check, inferNow.Add(time.Minute))

	assert.Len(t, dialer.dialled, 1, "a second reconcile within the interval must not dial again")
	assert.Equal(t, portRecheck-time.Minute, next)
	r.observePort(context.Background(), app, dep, check, inferNow.Add(portRecheck))
	assert.Len(t, dialer.dialled, 2)
}

func TestObservePort_ARefusalFromAProxiedPodDecidesNothing(t *testing.T) {
	c, dep := inferenceWorld(t, podState{ip: "10.0.0.1", runningFor: 3 * time.Hour, proxy: true})
	r := &AppReconciler{Client: c, PortDial: (&fakeDialer{answers: map[string]error{"10.0.0.1": refused}}).dial}
	app := routedShop()

	r.observePort(context.Background(), app, dep, kipperv1.AppHealthCheckStatus{Type: "none", Source: "inferred"}, inferNow)

	assert.Nil(t, apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionPortListening))
}

func TestObservePort_WatchesAnAppAnotherOneLinksTo(t *testing.T) {
	worker := routedShop()
	worker.Spec.Route = nil
	caller := &kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "shop-prod"},
		Spec: kipperv1.AppSpec{Image: "orders:1", Port: 8080, Links: []kipperv1.AppLink{{App: "shop", Namespace: "shop-prod"}}}}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(caller).
		WithIndex(&kipperv1.App{}, linkTargetIndex, LinkTargetKeys).Build()
	r := &AppReconciler{Client: c}

	assert.True(t, r.linkedTo(context.Background(), worker))
	other := worker.DeepCopy()
	other.Name = "unlinked"
	assert.False(t, r.linkedTo(context.Background(), other))
}

func TestEnqueueLinkTargets(t *testing.T) {
	caller := &kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "shop-prod"},
		Spec: kipperv1.AppSpec{Links: []kipperv1.AppLink{{App: "geo", Namespace: "shop-prod"}, {App: "ledger", Namespace: "finance-prod"}}}}

	got := (&AppReconciler{}).enqueueLinkTargets(context.Background(), caller)

	require.Len(t, got, 2)
	assert.Equal(t, "shop-prod/geo", got[0].String())
	assert.Equal(t, "finance-prod/ledger", got[1].String())
}

func TestSweepPortChecked(t *testing.T) {
	r := &AppReconciler{}
	r.portChecked.Store(types.UID("live"), inferNow.Add(-time.Minute))
	r.portChecked.Store(types.UID("gone"), inferNow.Add(-time.Hour))

	r.sweepPortChecked(inferNow)

	_, live := r.portChecked.Load(types.UID("live"))
	_, gone := r.portChecked.Load(types.UID("gone"))
	assert.True(t, live)
	assert.False(t, gone)
}
