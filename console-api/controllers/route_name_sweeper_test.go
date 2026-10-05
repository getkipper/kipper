package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func consoleAPIPod(name, imageID string, created time.Time, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: routeClaimNamespace, Labels: map[string]string{"app": "console-api"},
			CreationTimestamp: metav1.NewTime(created),
		},
		Status: corev1.PodStatus{
			Phase:             phase,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "console-api", ImageID: imageID}},
		},
	}
}

func routeIngress(namespace, service string, port int32, created time.Time) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: service, Namespace: namespace, CreationTimestamp: metav1.NewTime(created)},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
				Path: "/", Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
					Name: service, Port: networkingv1.ServiceBackendPort{Number: port},
				}},
			}}}},
		}}},
	}
}

func markerConfigMap(m routeNameMarker) *corev1.ConfigMap {
	return routeNameMarkerConfigMap(m, nil)
}

func newSweeper(c crclient.Client, now time.Time) *RouteNameSweeper {
	return &RouteNameSweeper{Client: c, Reader: c, PodName: "console-api-new", Gate: NewRouteNameGate(), Now: func() time.Time { return now }}
}

func TestRouteNameGate_AdmitsOnlyWhileOpenAndClosingWaitsForAdmittedWrites(t *testing.T) {
	g := NewRouteNameGate()
	_, ok := g.Admit()
	assert.False(t, ok, "a new gate admits nothing until a generation is bootstrapped")

	g.open()
	release, ok := g.Admit()
	require.True(t, ok)

	closed := make(chan struct{})
	go func() { g.close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("closing must wait for an admitted write to finish")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("closing did not finish after the admitted write released")
	}
	_, ok = g.Admit()
	assert.False(t, ok)
}

func TestRouteNameSweeper_WaitsOnlyForLiveEarlierPods(t *testing.T) {
	now := planNow
	cases := map[string]struct {
		other *corev1.Pod
		want  bool
	}{
		"an earlier pod still running": {consoleAPIPod("console-api-old", "img-old", now.Add(-time.Hour), corev1.PodRunning), true},
		"an earlier pod evicted":       {consoleAPIPod("console-api-old", "img-old", now.Add(-time.Hour), corev1.PodFailed), false},
		"an earlier pod that finished": {consoleAPIPod("console-api-old", "img-old", now.Add(-time.Hour), corev1.PodSucceeded), false},
		"a later pod":                  {consoleAPIPod("console-api-later", "img-new", now.Add(time.Minute), corev1.PodRunning), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := crfake.NewClientBuilder().WithScheme(testScheme()).
				WithObjects(consoleAPIPod("console-api-new", "img-new", now, corev1.PodRunning), tc.other).Build()

			got, err := newSweeper(c, now).earlierPodsLive(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestRouteNameSweeper_ForeignBinaryPod(t *testing.T) {
	now := planNow
	foreignCreated := now.Add(-30 * time.Second)
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		consoleAPIPod("console-api-new", "img-new", now.Add(-time.Hour), corev1.PodRunning),
		consoleAPIPod("console-api-same", "img-new", now.Add(-time.Minute), corev1.PodRunning),
		consoleAPIPod("console-api-evicted", "img-old", now.Add(-2*time.Hour), corev1.PodFailed),
		consoleAPIPod("console-api-rollback", "img-old", foreignCreated, corev1.PodPending),
	).Build()

	at, found, err := newSweeper(c, now).foreignBinarySince(context.Background(), "img-new")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, foreignCreated, at, "a pending pod of another binary counts, an evicted one does not")
}

func TestRouteNameSweeper_BootstrapWritesClaimsAndTheMarker(t *testing.T) {
	now := planNow
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		consoleAPIPod("console-api-new", "img-new", now.Add(-time.Minute), corev1.PodRunning),
		namespaceWithUID("shop", "uid-shop"),
		routeIngress("shop", "web", 8080, now.Add(-time.Hour)),
	).Build()
	ctx := context.Background()

	require.NoError(t, newSweeper(c, now).startGeneration(ctx, "img-new"))

	claim := readRouteNameClaim(t, c, "shop-web")
	assert.Equal(t, "shop", claim.Namespace)
	require.Len(t, claim.Intervals, 1)
	assert.Nil(t, claim.Intervals[0].To)

	m, found, err := readRouteNameMarker(ctx, c)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, m.Bootstrapped)
	assert.Equal(t, "img-new", m.ImageID)
	assert.Equal(t, claim.Intervals[0].Generation, m.Generation)
	assert.Equal(t, now, m.Heartbeat)
}

func TestRouteNameSweeper_AScanWithAForeignBinaryStopsTheHeartbeatAndCloses(t *testing.T) {
	now := planNow
	heartbeat := now.Add(-60 * time.Second)
	foreignCreated := now.Add(-40 * time.Second)
	open := routeNameClaimConfigMap(routeNameClaim{Key: "shop-web", Namespace: "shop", UID: "uid-shop",
		Intervals: []attributionInterval{{From: now.Add(-time.Hour), Generation: "g1"}}})
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		consoleAPIPod("console-api-new", "img-new", now.Add(-time.Hour), corev1.PodRunning),
		consoleAPIPod("console-api-rollback", "img-old", foreignCreated, corev1.PodRunning),
		markerConfigMap(routeNameMarker{Generation: "g1", ImageID: "img-new", Bootstrapped: true, Heartbeat: heartbeat}),
		namespaceWithUID("shop", "uid-shop"), routeIngress("shop", "web", 8080, now.Add(-2*time.Hour)), open,
	).Build()
	ctx := context.Background()

	require.NoError(t, newSweeper(c, now).scan(ctx, "img-new"))

	claim := readRouteNameClaim(t, c, "shop-web")
	require.NotNil(t, claim.Intervals[0].To)
	assert.Equal(t, heartbeat, *claim.Intervals[0].To, "the heartbeat came before the foreign pod, so the interval ends there")
	m, _, err := readRouteNameMarker(ctx, c)
	require.NoError(t, err)
	assert.Equal(t, heartbeat, m.Heartbeat, "no heartbeat while another binary's pod exists")
}

func TestRouteNameSweeper_DeletesWithPreconditions(t *testing.T) {
	now := planNow
	gone := routeNameClaimConfigMap(routeNameClaim{Key: "gone-web", Namespace: "gone", UID: "uid-gone"})
	var sawUID, sawVersion bool
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		consoleAPIPod("console-api-new", "img-new", now.Add(-time.Hour), corev1.PodRunning),
		markerConfigMap(routeNameMarker{Generation: "g1", ImageID: "img-new", Bootstrapped: true, Heartbeat: now.Add(-30 * time.Second)}),
		gone,
	).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.DeleteOption) error {
			do := &crclient.DeleteOptions{}
			do.ApplyOptions(opts)
			if do.Preconditions != nil {
				sawUID = do.Preconditions.UID != nil && *do.Preconditions.UID == obj.GetUID()
				sawVersion = do.Preconditions.ResourceVersion != nil && *do.Preconditions.ResourceVersion != ""
			}
			return cl.Delete(ctx, obj, opts...)
		},
	}).Build()

	require.NoError(t, newSweeper(c, now).scan(context.Background(), "img-new"))

	assert.True(t, sawUID, "the delete is conditioned on the claim's UID")
	assert.True(t, sawVersion, "the delete is conditioned on the claim's resourceVersion")
}

func TestRouteNameSweeper_AForeignPodOlderThanTheHeartbeatSetsTheClose(t *testing.T) {
	now := planNow
	foreignCreated := now.Add(-90 * time.Second)
	open := routeNameClaimConfigMap(routeNameClaim{Key: "shop-web", Namespace: "shop", UID: "uid-shop",
		Intervals: []attributionInterval{{From: now.Add(-time.Hour), Generation: "g1"}}})
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		consoleAPIPod("console-api-new", "img-new", now.Add(-time.Hour), corev1.PodRunning),
		consoleAPIPod("console-api-other", "", foreignCreated, corev1.PodPending),
		markerConfigMap(routeNameMarker{Generation: "g1", ImageID: "img-new", Bootstrapped: true, Heartbeat: now.Add(-30 * time.Second)}),
		namespaceWithUID("shop", "uid-shop"), routeIngress("shop", "web", 8080, now.Add(-2*time.Hour)), open,
	).Build()

	require.NoError(t, newSweeper(c, now).scan(context.Background(), "img-new"))

	claim := readRouteNameClaim(t, c, "shop-web")
	require.NotNil(t, claim.Intervals[0].To)
	assert.Equal(t, foreignCreated, *claim.Intervals[0].To, "a pod whose binary is not known yet ends attribution at its creation")
}

func TestRouteNameSweeper_DoesNotStartWhileAnotherBinaryIsLive(t *testing.T) {
	now := planNow
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		consoleAPIPod("console-api-new", "img-new", now.Add(-time.Minute), corev1.PodRunning),
		consoleAPIPod("console-api-rollback", "img-old", now, corev1.PodRunning),
	).Build()

	ready, err := newSweeper(c, now).readyToStart(context.Background(), "img-new")
	require.NoError(t, err)
	assert.False(t, ready, "a later pod of another binary could publish unchecked routes")
}

func TestRouteNameSweeper_RetriesAFailedBootstrapAndKeepsTheHeartbeat(t *testing.T) {
	now := planNow
	heartbeat := now.Add(-5 * time.Minute)
	failures := 1
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		consoleAPIPod("console-api-new", "img-new", now.Add(-time.Minute), corev1.PodRunning),
		markerConfigMap(routeNameMarker{Generation: "g1", ImageID: "img-old", Bootstrapped: true, Heartbeat: heartbeat}),
	).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl crclient.WithWatch, list crclient.ObjectList, opts ...crclient.ListOption) error {
			if _, ok := list.(*networkingv1.IngressList); ok && failures > 0 {
				failures--
				return errors.NewServiceUnavailable("apiserver unavailable")
			}
			return cl.List(ctx, list, opts...)
		},
	}).Build()
	s := newSweeper(c, now)
	s.retryDelay = time.Millisecond

	require.NoError(t, s.bootstrapWithRetry(context.Background(), "img-new"))

	m, _, err := readRouteNameMarker(context.Background(), c)
	require.NoError(t, err)
	assert.True(t, m.Bootstrapped)
	_, ok := s.Gate.Admit()
	assert.True(t, ok, "the gate opens once the generation is bootstrapped")
}

func TestRouteNameSweeper_AFailedAttemptKeepsThePreviousHeartbeat(t *testing.T) {
	now := planNow
	heartbeat := now.Add(-5 * time.Minute)
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		consoleAPIPod("console-api-new", "img-new", now.Add(-time.Minute), corev1.PodRunning),
		markerConfigMap(routeNameMarker{Generation: "g1", ImageID: "img-old", Bootstrapped: true, Heartbeat: heartbeat}),
	).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl crclient.WithWatch, list crclient.ObjectList, opts ...crclient.ListOption) error {
			if _, ok := list.(*networkingv1.IngressList); ok {
				return errors.NewServiceUnavailable("apiserver unavailable")
			}
			return cl.List(ctx, list, opts...)
		},
	}).Build()

	require.Error(t, newSweeper(c, now).startGeneration(context.Background(), "img-new"))

	m, _, err := readRouteNameMarker(context.Background(), c)
	require.NoError(t, err)
	assert.False(t, m.Bootstrapped)
	assert.Equal(t, heartbeat, m.Heartbeat, "the previous generation's last heartbeat survives a failed attempt")
}

func TestRouteNameSweeper_ACorruptClaimDoesNotStopTheScan(t *testing.T) {
	now := planNow
	corrupt := routeNameClaimConfigMap(routeNameClaim{Key: "bad-web", Namespace: "bad", UID: "uid-bad"})
	corrupt.Data["intervals"] = "{not json"
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		consoleAPIPod("console-api-new", "img-new", now.Add(-time.Hour), corev1.PodRunning),
		markerConfigMap(routeNameMarker{Generation: "g1", ImageID: "img-new", Bootstrapped: true, Heartbeat: now.Add(-30 * time.Second)}),
		namespaceWithUID("shop", "uid-shop"), routeIngress("shop", "web", 8080, now.Add(-time.Hour)), corrupt,
	).Build()

	require.NoError(t, newSweeper(c, now).scan(context.Background(), "img-new"))

	assert.Equal(t, "shop", readRouteNameClaim(t, c, "shop-web").Namespace, "other keys are still claimed")
	var cm corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Name: routeNameClaimName("bad-web"), Namespace: routeClaimNamespace}, &cm))
	assert.Equal(t, "{not json", cm.Data["intervals"], "a claim that cannot be read is left alone")
}

func TestRouteNameSweeper_BootstrapSharesACollisionFoundOnTheCluster(t *testing.T) {
	now := planNow
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		consoleAPIPod("console-api-new", "img-new", now.Add(-time.Minute), corev1.PodRunning),
		namespaceWithUID("team", "uid-team"), namespaceWithUID("team-prod", "uid-team-prod"),
		routeIngress("team", "prod-web", 8080, now.Add(-48*time.Hour)),
		routeIngress("team-prod", "web", 8080, now.Add(-time.Hour)),
		namespaceWithUID("kipper-ai-librechat", "uid-lc"),
		routeIngress("kipper-ai-librechat", "librechat", 3080, now.Add(-time.Hour)),
	).Build()

	require.NoError(t, newSweeper(c, now).startGeneration(context.Background(), "img-new"))

	assert.True(t, readRouteNameClaim(t, c, "kipper-ai-librechat-librechat").Occupied, "a tenant route on an optional platform name occupies it")
	claim := readRouteNameClaim(t, c, "team-prod-web")
	assert.Equal(t, "team", claim.Namespace, "the oldest producer holds the key")
	assert.Equal(t, []string{"team-prod"}, claim.Shared)
	assert.Equal(t, "uid-team-prod", claim.SharedUIDs["team-prod"])
	assert.Empty(t, claim.Intervals, "a shared key shows no traffic to anyone")
}

func TestRouteNameSweeper_ABootstrapRetryRechecksForAnotherBinary(t *testing.T) {
	now := planNow
	attempts := 0
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		consoleAPIPod("console-api-new", "img-new", now.Add(-time.Minute), corev1.PodRunning),
	).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl crclient.WithWatch, list crclient.ObjectList, opts ...crclient.ListOption) error {
			if _, ok := list.(*networkingv1.IngressList); ok {
				attempts++
				if attempts == 1 {
					rollback := consoleAPIPod("console-api-rollback", "img-old", now, corev1.PodRunning)
					if err := cl.Create(ctx, rollback); err != nil {
						return err
					}
					return errors.NewServiceUnavailable("apiserver unavailable")
				}
			}
			return cl.List(ctx, list, opts...)
		},
	}).Build()
	s := newSweeper(c, now)
	s.retryDelay = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := s.bootstrapWithRetry(ctx, "img-new")

	assert.Error(t, err, "no generation starts while the other binary is live")
	m, _, _ := readRouteNameMarker(context.Background(), c)
	assert.False(t, m.Bootstrapped)
	_, ok := s.Gate.Admit()
	assert.False(t, ok)
}

func TestRouteNameSweeper_AnIdleClaimIsKeptIfAnAppAppearedSinceTheSnapshot(t *testing.T) {
	now := planNow
	idle := routeNameClaimConfigMap(routeNameClaim{Key: "shop-web", Namespace: "shop", UID: "uid-shop"})
	app := namedRoutedApp("web", "shop", 8080)
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(idle, namespaceWithUID("shop", "uid-shop"), app).Build()
	s := newSweeper(c, now)
	snap := routeNameSnapshot{Claims: map[string]routeNameClaim{}}
	parsed, err := parseRouteNameClaim(idle)
	require.NoError(t, err)
	snap.Claims["shop-web"] = parsed

	require.NoError(t, s.apply(context.Background(), snap, routeNamePlan{Delete: []string{"shop-web"}}))

	assert.Equal(t, "shop", readRouteNameClaim(t, c, "shop-web").Namespace, "an app created after the snapshot keeps its claim")
}

func TestRouteNameSweeper_ADroppedClaimWriteKeepsTheHeartbeat(t *testing.T) {
	now := planNow
	heartbeat := now.Add(-30 * time.Second)
	claim := routeNameClaimConfigMap(routeNameClaim{Key: "shop-web", Namespace: "shop", UID: "uid-shop"})
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		consoleAPIPod("console-api-new", "img-new", now.Add(-time.Hour), corev1.PodRunning),
		markerConfigMap(routeNameMarker{Generation: "g1", ImageID: "img-new", Bootstrapped: true, Heartbeat: heartbeat}),
		namespaceWithUID("shop", "uid-shop"), routeIngress("shop", "web", 8080, now.Add(-2*time.Hour)), claim,
	).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
			if obj.GetName() == claim.Name {
				return errors.NewConflict(corev1.Resource("configmaps"), obj.GetName(), nil)
			}
			return cl.Update(ctx, obj, opts...)
		},
	}).Build()
	ctx := context.Background()

	require.NoError(t, newSweeper(c, now).scan(ctx, "img-new"))

	m, _, err := readRouteNameMarker(ctx, c)
	require.NoError(t, err)
	assert.Equal(t, heartbeat, m.Heartbeat, "a scan that could not write every claim vouches for none of them")
}

func TestRouteNameSweeper_AScanAfterAGapDoesNotVouchForIt(t *testing.T) {
	now := planNow
	heartbeat := now.Add(-5 * time.Minute)
	open := routeNameClaimConfigMap(routeNameClaim{Key: "shop-web", Namespace: "shop", UID: "uid-shop",
		Intervals: []attributionInterval{{From: now.Add(-time.Hour), Generation: "g1"}}})
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		consoleAPIPod("console-api-new", "img-new", now.Add(-2*time.Hour), corev1.PodRunning),
		markerConfigMap(routeNameMarker{Generation: "g1", ImageID: "img-new", Bootstrapped: true, Heartbeat: heartbeat}),
		namespaceWithUID("shop", "uid-shop"), routeIngress("shop", "web", 8080, now.Add(-2*time.Hour)), open,
	).Build()
	ctx := context.Background()

	require.NoError(t, newSweeper(c, now).scan(ctx, "img-new"))

	claim := readRouteNameClaim(t, c, "shop-web")
	require.Len(t, claim.Intervals, 1)
	require.NotNil(t, claim.Intervals[0].To, "nobody checked the five minutes since the last heartbeat")
	assert.Equal(t, heartbeat, *claim.Intervals[0].To)

	require.NoError(t, newSweeper(c, now.Add(30*time.Second)).scan(ctx, "img-new"))
	claim = readRouteNameClaim(t, c, "shop-web")
	require.Len(t, claim.Intervals, 2, "the next regular scan opens a new interval")
	assert.Equal(t, now.Add(30*time.Second), claim.Intervals[1].From)
}
