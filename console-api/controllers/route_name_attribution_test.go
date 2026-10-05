package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestRouteNameAttribution(t *testing.T) {
	now := planNow
	held := routeNameClaim{Key: "shop-web", Namespace: "shop", UID: "uid-shop", Intervals: []attributionInterval{
		{From: now.Add(-3 * time.Hour), To: ptrTime(now.Add(-2 * time.Hour)), Generation: "g1"},
		{From: now.Add(-time.Hour), Generation: "g2"},
	}}
	shared := routeNameClaim{Key: "team-prod-web", Namespace: "team", UID: "uid-team"}
	shared.setShared([]string{"team-prod"}, map[string]types.UID{"team-prod": "uid-team-prod"})
	objs := []crclient.Object{
		routeNameClaimConfigMap(held), routeNameClaimConfigMap(shared),
		namespaceWithUID("shop", "uid-shop"), namespaceWithUID("team", "uid-team"), namespaceWithUID("team-prod", "uid-team-prod"),
		markerConfigMap(routeNameMarker{Generation: "g2", ImageID: "img", Bootstrapped: true, Heartbeat: now.Add(-20 * time.Second)}),
	}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).Build()
	ctx := context.Background()

	got, err := RouteNameAttribution(ctx, c, "shop", "web")
	require.NoError(t, err)
	assert.Equal(t, AttributionHeld, got.State)
	require.Len(t, got.Spans, 2)
	assert.Equal(t, now.Add(-20*time.Second), got.Spans[1].To, "an open interval runs to the last verified scan, never past it")

	got, err = RouteNameAttribution(ctx, c, "team-prod", "web")
	require.NoError(t, err)
	assert.Equal(t, AttributionShared, got.State)
	assert.Empty(t, got.Spans)

	got, err = RouteNameAttribution(ctx, c, "shop", "other")
	require.NoError(t, err)
	assert.Equal(t, AttributionNone, got.State)
}

func TestRouteNameAttribution_AStaleHeartbeatEndsTheOpenInterval(t *testing.T) {
	now := planNow
	heartbeat := now.Add(-5 * time.Minute)
	held := routeNameClaim{Key: "shop-web", Namespace: "shop", UID: "uid-shop", Intervals: []attributionInterval{{From: now.Add(-time.Hour), Generation: "g2"}}}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(routeNameClaimConfigMap(held), namespaceWithUID("shop", "uid-shop"),
		markerConfigMap(routeNameMarker{Generation: "g2", ImageID: "img", Bootstrapped: true, Heartbeat: heartbeat})).Build()

	got, err := RouteNameAttribution(context.Background(), c, "shop", "web")
	require.NoError(t, err)
	require.Len(t, got.Spans, 1)
	assert.Equal(t, heartbeat, got.Spans[0].To, "a stuck sweeper cannot extend attribution")
}

func TestRouteNameAttribution_ARecreatedNamespaceHasNoHistory(t *testing.T) {
	now := planNow
	held := routeNameClaim{Key: "shop-web", Namespace: "shop", UID: "uid-old", Intervals: []attributionInterval{{From: now.Add(-time.Hour), Generation: "g2"}}}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(routeNameClaimConfigMap(held), namespaceWithUID("shop", "uid-new")).Build()

	got, err := RouteNameAttribution(context.Background(), c, "shop", "web")
	require.NoError(t, err)
	assert.Equal(t, AttributionOther, got.State)
	assert.Empty(t, got.Spans)
}

func TestRouteNameAttribution_AnOccupiedNameGivesNoTraffic(t *testing.T) {
	now := planNow
	occupied := routeNameClaim{Key: "shop-web", Namespace: "shop", UID: "uid-shop", Occupied: true, Intervals: []attributionInterval{{From: now.Add(-time.Hour), Generation: "g2"}}}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(routeNameClaimConfigMap(occupied), namespaceWithUID("shop", "uid-shop"),
		markerConfigMap(routeNameMarker{Generation: "g2", ImageID: "img", Bootstrapped: true, Heartbeat: now.Add(-10 * time.Second)})).Build()

	got, err := RouteNameAttribution(context.Background(), c, "shop", "web")
	require.NoError(t, err)
	assert.Equal(t, AttributionOther, got.State, "the platform route may produce the same name")
	assert.Empty(t, got.Spans)
}

func TestRouteNameAttribution_ReadsTheHeartbeatBeforeTheClaim(t *testing.T) {
	now := planNow
	held := routeNameClaim{Key: "shop-web", Namespace: "shop", UID: "uid-shop", Intervals: []attributionInterval{{From: now.Add(-time.Hour), Generation: "g1"}}}
	marker := markerConfigMap(routeNameMarker{Generation: "g1", ImageID: "img", Bootstrapped: true, Heartbeat: now})
	claim := routeNameClaimConfigMap(held)
	var order []string
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(claim, namespaceWithUID("shop", "uid-shop"), marker).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, cl crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if key.Name == marker.Name || key.Name == claim.Name {
				order = append(order, key.Name)
			}
			return cl.Get(ctx, key, obj, opts...)
		}}).Build()

	_, err := RouteNameAttribution(context.Background(), c, "shop", "web")
	require.NoError(t, err)
	assert.Equal(t, []string{marker.Name, claim.Name}, order)
}
