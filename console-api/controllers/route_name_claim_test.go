package controllers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func namespaceWithUID(name, uid string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)}}
}

func readRouteNameClaim(t *testing.T, c crclient.Client, key string) routeNameClaim {
	t.Helper()
	var cm corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: routeNameClaimName(key), Namespace: routeClaimNamespace}, &cm))
	claim, err := parseRouteNameClaim(&cm)
	require.NoError(t, err)
	return claim
}

func TestReserveRouteName_FirstNamespaceHoldsItAndOthersAreRefused(t *testing.T) {
	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(namespaceWithUID("team", "uid-team"), namespaceWithUID("team-prod", "uid-team-prod")).Build()
	ctx := context.Background()

	owned, err := reserveRouteName(ctx, c, c, "team", "team-prod-web")
	require.NoError(t, err)
	assert.True(t, owned)

	owned, err = reserveRouteName(ctx, c, c, "team-prod", "team-prod-web")
	require.NoError(t, err)
	assert.False(t, owned, "another live namespace must be refused")

	claim := readRouteNameClaim(t, c, "team-prod-web")
	assert.Equal(t, "team", claim.Namespace)
	assert.Equal(t, "uid-team", claim.UID)
	assert.Equal(t, "team-prod-web", claim.Key)
}

func TestReserveRouteName_StaysWithItsNamespaceWithoutARoute(t *testing.T) {
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(namespaceWithUID("shop", "uid-shop")).Build()
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		owned, err := reserveRouteName(ctx, c, c, "shop", "shop-web")
		require.NoError(t, err)
		assert.True(t, owned, "the owner reserves again after removing and restoring its route")
	}
}

func TestReserveRouteName_TakenOverOnceTheOwnerNamespaceIsGone(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	held := routeNameClaimConfigMap(routeNameClaim{
		Key: "team-prod-web", Namespace: "team", UID: "uid-team",
		Intervals: []attributionInterval{{From: now.Add(-time.Hour), To: ptrTime(now.Add(-time.Minute)), Generation: "g1"}},
	})
	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(held, namespaceWithUID("team-prod", "uid-team-prod")).Build()

	owned, err := reserveRouteName(context.Background(), c, c, "team-prod", "team-prod-web")
	require.NoError(t, err)
	assert.True(t, owned)

	claim := readRouteNameClaim(t, c, "team-prod-web")
	assert.Equal(t, "team-prod", claim.Namespace)
	assert.Equal(t, "uid-team-prod", claim.UID)
	assert.Empty(t, claim.Intervals, "a new owner must not inherit the previous owner's history")
}

func TestReserveRouteName_ARecreatedNamespaceIsANewOwner(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	held := routeNameClaimConfigMap(routeNameClaim{
		Key: "shop-web", Namespace: "shop", UID: "uid-old",
		Intervals: []attributionInterval{{From: now.Add(-time.Hour), To: ptrTime(now), Generation: "g1"}},
	})
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(held, namespaceWithUID("shop", "uid-new")).Build()

	owned, err := reserveRouteName(context.Background(), c, c, "shop", "shop-web")
	require.NoError(t, err)
	assert.True(t, owned, "a project restored under the same name is not locked out")

	claim := readRouteNameClaim(t, c, "shop-web")
	assert.Equal(t, "uid-new", claim.UID)
	assert.Empty(t, claim.Intervals)
}

func TestReserveRouteName_ConcurrentFirstReservationsHaveOneWinner(t *testing.T) {
	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(namespaceWithUID("team", "uid-team"), namespaceWithUID("team-prod", "uid-team-prod")).Build()
	ctx := context.Background()

	var wg sync.WaitGroup
	results := make([]bool, 2)
	for i, ns := range []string{"team", "team-prod"} {
		wg.Add(1)
		go func(i int, ns string) {
			defer wg.Done()
			owned, err := reserveRouteName(ctx, c, c, ns, "team-prod-web")
			assert.NoError(t, err)
			results[i] = owned
		}(i, ns)
	}
	wg.Wait()
	assert.NotEqual(t, results[0], results[1], "exactly one namespace may hold the key")
}

func TestReserveRouteName_RefusesWhenTheOwnerCannotBeRead(t *testing.T) {
	held := routeNameClaimConfigMap(routeNameClaim{Key: "shop-web", Namespace: "shop", UID: "uid-shop"})
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(held, namespaceWithUID("other", "uid-other")).
		WithInterceptorFuncs(failNamespaceGet("shop")).Build()

	owned, err := reserveRouteName(context.Background(), c, c, "other", "shop-web")
	assert.Error(t, err, "an unreadable owner is never taken over")
	assert.False(t, owned)
}

func TestPruneIntervals(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	in := []attributionInterval{
		{From: now.Add(-80 * time.Hour), To: ptrTime(now.Add(-74 * time.Hour)), Generation: "old"},
		{From: now.Add(-2 * time.Hour), To: ptrTime(now.Add(-2 * time.Hour)), Generation: "empty"},
		{From: now.Add(-time.Hour), To: ptrTime(now.Add(-30 * time.Minute)), Generation: "kept"},
		{From: now.Add(-10 * time.Minute), Generation: "open"},
	}
	got := pruneIntervals(in, now)
	require.Len(t, got, 2)
	assert.Equal(t, "kept", got[0].Generation)
	assert.Equal(t, "open", got[1].Generation)

	var many []attributionInterval
	for i := 0; i < 60; i++ {
		from := now.Add(time.Duration(i-60) * time.Minute)
		many = append(many, attributionInterval{From: from, To: ptrTime(from.Add(30 * time.Second)), Generation: "g"})
	}
	got = pruneIntervals(many, now)
	require.Len(t, got, maxAttributionIntervals)
	assert.Equal(t, many[len(many)-1], got[len(got)-1], "the cap drops the oldest entries")
}

func ptrTime(t time.Time) *time.Time { return &t }

func failNamespaceGet(name string) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, cl crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if _, ok := obj.(*corev1.Namespace); ok && key.Name == name {
				return errors.NewServiceUnavailable("apiserver unavailable")
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}
}

func TestReserveRouteName_ASharedKeyIsNeverTakenOver(t *testing.T) {
	held := routeNameClaimConfigMap(routeNameClaim{
		Key: "team-prod-web-api", Namespace: "team", UID: "uid-team", Shared: []string{"team-prod"}, SharedUIDs: map[string]string{"team-prod": "uid-team-prod"},
	})
	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(held, namespaceWithUID("team-prod", "uid-team-prod"), namespaceWithUID("team-prod-web", "uid-tpw")).Build()

	owned, err := reserveRouteName(context.Background(), c, c, "team-prod-web", "team-prod-web-api")
	require.NoError(t, err)
	assert.False(t, owned, "a shared key leaves the shared state only at a bootstrap, even with its holder gone")
	assert.Equal(t, []string{"team-prod"}, readRouteNameClaim(t, c, "team-prod-web-api").Shared)
}

func TestRouteNameRefusal(t *testing.T) {
	held := routeNameClaimConfigMap(routeNameClaim{Key: "team-prod-web", Namespace: "team", UID: "uid-team"})
	shared := routeNameClaimConfigMap(routeNameClaim{Key: "x-y-api", Namespace: "x", UID: "uid-x", Shared: []string{"x-y"}, SharedUIDs: map[string]string{"x-y": "uid-x-y"}})
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(held, shared,
		namespaceWithUID("team", "uid-team"), namespaceWithUID("team-prod", "uid-team-prod"),
		namespaceWithUID("x", "uid-x"), namespaceWithUID("x-y", "uid-x-y"), namespaceWithUID("kipper", "uid-k")).Build()
	ctx := context.Background()

	cases := []struct {
		name, namespace, app string
		refused              bool
	}{
		{"taken by a live namespace", "team-prod", "web", true},
		{"its own reservation", "team", "prod-web", false},
		{"a platform name", "kipper", "system-console-api", true},
		{"a grandfathered sharer", "x-y", "api", false},
		{"free", "team", "other", false},
	}
	for _, tc := range cases {
		refused, message, err := RouteNameRefusal(ctx, c, tc.namespace, tc.app)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.refused, refused, tc.name)
		if refused {
			assert.NotContains(t, message, "team", "%s: the refusal names no other project", tc.name)
		}
	}
}

func TestRouteNameRefusal_ARecreatedSharerNamespaceHasNoRight(t *testing.T) {
	c := routeNameClaim{Key: "x-y-api", Namespace: "x", UID: "uid-x"}
	c.setShared([]string{"x-y"}, map[string]types.UID{"x-y": "uid-old"})
	cl := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(routeNameClaimConfigMap(c),
		namespaceWithUID("x", "uid-x"), namespaceWithUID("x-y", "uid-new")).Build()

	refused, _, err := RouteNameRefusal(context.Background(), cl, "x-y", "api")
	require.NoError(t, err)
	assert.True(t, refused, "a namespace recreated under a sharer's name does not inherit its right")
}
