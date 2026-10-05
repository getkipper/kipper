package controllers

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

var planNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func producerAt(namespace, service string, port int32, created time.Time) routeNameProducer {
	return routeNameProducer{Namespace: namespace, Service: service, Port: port, Created: created}
}

func baseSnapshot() routeNameSnapshot {
	return routeNameSnapshot{
		Now:        planNow,
		Generation: "g2",
		Namespaces: map[string]types.UID{"team": "uid-team", "team-prod": "uid-team-prod", "shop": "uid-shop"},
		Workloads:  map[string]map[string]bool{},
		Claims:     map[string]routeNameClaim{},
	}
}

func written(t *testing.T, p routeNamePlan, key string) routeNameClaim {
	t.Helper()
	for _, c := range p.Write {
		if c.Key == key {
			return c
		}
	}
	t.Fatalf("no claim written for %s; wrote %+v", key, p.Write)
	return routeNameClaim{}
}

func openInterval(c routeNameClaim) *attributionInterval {
	for i := range c.Intervals {
		if c.Intervals[i].To == nil {
			return &c.Intervals[i]
		}
	}
	return nil
}

func TestPlanRouteNames_BootstrapClaimsASingleProducerAndOpensAnInterval(t *testing.T) {
	s := baseSnapshot()
	s.Bootstrap = true
	s.Producers = []routeNameProducer{producerAt("shop", "web", 8080, planNow.Add(-time.Hour))}

	p := planRouteNames(s)

	c := written(t, p, "shop-web")
	assert.Equal(t, "shop", c.Namespace)
	assert.Equal(t, "uid-shop", c.UID)
	iv := openInterval(c)
	require.NotNil(t, iv)
	assert.Equal(t, planNow, iv.From)
	assert.Equal(t, "g2", iv.Generation)
}

func TestPlanRouteNames_BootstrapGivesACollisionToTheOldestProducerAndSharesIt(t *testing.T) {
	s := baseSnapshot()
	s.Bootstrap = true
	s.Producers = []routeNameProducer{
		producerAt("team-prod", "web", 8080, planNow.Add(-time.Hour)),
		producerAt("team", "prod-web", 8080, planNow.Add(-48*time.Hour)),
	}

	p := planRouteNames(s)

	c := written(t, p, "team-prod-web")
	assert.Equal(t, "team", c.Namespace, "the oldest producer holds the key")
	assert.Equal(t, []string{"team-prod"}, c.Shared)
	assert.Nil(t, openInterval(c), "a shared key shows no traffic to anyone")
}

func TestPlanRouteNames_BootstrapClosesTheOldGenerationAtItsLastHeartbeat(t *testing.T) {
	heartbeat := planNow.Add(-10 * time.Minute)
	s := baseSnapshot()
	s.Bootstrap = true
	s.Heartbeat = heartbeat
	s.Producers = []routeNameProducer{producerAt("shop", "web", 8080, planNow.Add(-time.Hour))}
	s.Claims["shop-web"] = routeNameClaim{Key: "shop-web", Namespace: "shop", UID: "uid-shop",
		Intervals: []attributionInterval{{From: planNow.Add(-time.Hour), Generation: "g1"}}}

	p := planRouteNames(s)

	c := written(t, p, "shop-web")
	require.Len(t, c.Intervals, 2)
	require.NotNil(t, c.Intervals[0].To)
	assert.Equal(t, heartbeat, *c.Intervals[0].To, "a rollback period after the last heartbeat is never attributed")
	assert.Equal(t, "g2", c.Intervals[1].Generation)
}

func TestPlanRouteNames_BootstrapClosesAtAForeignProducerThatAppearedBeforeTheHeartbeat(t *testing.T) {
	foreign := planNow.Add(-20 * time.Minute)
	s := baseSnapshot()
	s.Bootstrap = true
	s.Heartbeat = planNow.Add(-10 * time.Minute)
	s.Producers = []routeNameProducer{
		producerAt("team", "prod-web", 8080, planNow.Add(-48*time.Hour)),
		producerAt("team-prod", "web", 8080, foreign),
	}
	s.Claims["team-prod-web"] = routeNameClaim{Key: "team-prod-web", Namespace: "team", UID: "uid-team",
		Intervals: []attributionInterval{{From: planNow.Add(-time.Hour), Generation: "g1"}}}

	p := planRouteNames(s)

	c := written(t, p, "team-prod-web")
	require.NotNil(t, c.Intervals[0].To)
	assert.Equal(t, foreign, *c.Intervals[0].To)
	assert.Equal(t, []string{"team-prod"}, c.Shared)
}

func TestPlanRouteNames_AScanClosesAtAForeignProducersCreationTime(t *testing.T) {
	foreign := planNow.Add(-45 * time.Second)
	s := baseSnapshot()
	s.Producers = []routeNameProducer{
		producerAt("team", "prod-web", 8080, planNow.Add(-48*time.Hour)),
		producerAt("team-prod", "web", 8080, foreign),
	}
	s.Claims["team-prod-web"] = routeNameClaim{Key: "team-prod-web", Namespace: "team", UID: "uid-team",
		Intervals: []attributionInterval{{From: planNow.Add(-time.Hour), Generation: "g2"}}}

	p := planRouteNames(s)

	c := written(t, p, "team-prod-web")
	require.NotNil(t, c.Intervals[0].To)
	assert.Equal(t, foreign, *c.Intervals[0].To, "the close is exact even though the scan saw it later")
	assert.Equal(t, []string{"team-prod"}, c.Shared)
}

func TestPlanRouteNames_AScanClosesWhenTheOwnerStopsProducing(t *testing.T) {
	s := baseSnapshot()
	s.Claims["shop-web"] = routeNameClaim{Key: "shop-web", Namespace: "shop", UID: "uid-shop",
		Intervals: []attributionInterval{{From: planNow.Add(-time.Hour), Generation: "g2"}}}

	p := planRouteNames(s)

	c := written(t, p, "shop-web")
	require.NotNil(t, c.Intervals[0].To)
	assert.Equal(t, planNow, *c.Intervals[0].To)
	assert.Equal(t, "shop", c.Namespace, "the reservation stays with its namespace")
}

func TestPlanRouteNames_AScanClaimsANewSingleProducer(t *testing.T) {
	s := baseSnapshot()
	s.Producers = []routeNameProducer{producerAt("shop", "admin-made", 80, planNow.Add(-time.Minute))}

	p := planRouteNames(s)

	c := written(t, p, "shop-admin-made")
	assert.Equal(t, "shop", c.Namespace)
	assert.NotNil(t, openInterval(c))
}

func TestPlanRouteNames_NothingOpensWithoutAWorkingGeneration(t *testing.T) {
	s := baseSnapshot()
	s.Suspended = true
	s.Producers = []routeNameProducer{producerAt("shop", "web", 8080, planNow.Add(-time.Hour))}
	s.Claims["shop-web"] = routeNameClaim{Key: "shop-web", Namespace: "shop", UID: "uid-shop"}

	p := planRouteNames(s)

	for _, c := range p.Write {
		assert.Nil(t, openInterval(c), "no interval opens while another binary's pod exists")
	}
}

func TestPlanRouteNames_SharedExitOnlyAtBootstrapOnceTheOtherSideIsGone(t *testing.T) {
	shared := routeNameClaim{Key: "team-prod-web", Namespace: "team", UID: "uid-team", Shared: []string{"team-prod"}, SharedUIDs: map[string]string{"team-prod": "uid-team-prod"}}

	t.Run("a scan never leaves the shared state", func(t *testing.T) {
		s := baseSnapshot()
		s.Producers = []routeNameProducer{producerAt("team", "prod-web", 8080, planNow.Add(-48*time.Hour))}
		s.Claims["team-prod-web"] = shared
		assert.Empty(t, planRouteNames(s).Write, "the claim stays shared and unchanged")
	})

	t.Run("the other side still has an app with the name", func(t *testing.T) {
		s := baseSnapshot()
		s.Bootstrap = true
		s.Producers = []routeNameProducer{producerAt("team", "prod-web", 8080, planNow.Add(-48*time.Hour))}
		s.Workloads["team-prod"] = map[string]bool{"web": true}
		s.Claims["team-prod-web"] = shared
		assert.Empty(t, planRouteNames(s).Write, "a removed route can come back without a new check, so it stays shared")
	})

	t.Run("the other side renamed its app", func(t *testing.T) {
		s := baseSnapshot()
		s.Bootstrap = true
		s.Producers = []routeNameProducer{producerAt("team", "prod-web", 8080, planNow.Add(-48*time.Hour))}
		s.Workloads["team"] = map[string]bool{"prod-web": true}
		s.Claims["team-prod-web"] = shared
		c := written(t, planRouteNames(s), "team-prod-web")
		assert.Empty(t, c.Shared)
		assert.Equal(t, "team", c.Namespace)
		iv := openInterval(c)
		require.NotNil(t, iv)
		assert.Equal(t, planNow.Add(routeNameReloadLag), iv.From, "the interval opens after Traefik's reload lag")
	})

	t.Run("the holder renamed its app", func(t *testing.T) {
		s := baseSnapshot()
		s.Bootstrap = true
		s.Producers = []routeNameProducer{producerAt("team-prod", "web", 8080, planNow.Add(-time.Hour))}
		s.Workloads["team-prod"] = map[string]bool{"web": true}
		withHistory := shared
		withHistory.Intervals = []attributionInterval{{From: planNow.Add(-72 * time.Hour), To: ptrTime(planNow.Add(-71 * time.Hour)), Generation: "g0"}}
		s.Claims["team-prod-web"] = withHistory
		c := written(t, planRouteNames(s), "team-prod-web")
		assert.Equal(t, "team-prod", c.Namespace, "the reservation moves to the namespace still producing it")
		assert.Empty(t, c.Shared)
		require.Len(t, c.Intervals, 1, "a new owner never inherits another namespace's history")
		assert.Equal(t, "g2", c.Intervals[0].Generation)
	})
}

func TestPlanRouteNames_ATenantRouteOnAnOptionalPlatformNameIsOccupied(t *testing.T) {
	s := baseSnapshot()
	s.Namespaces["kipper-ai-librechat"] = "uid-x"
	s.Producers = []routeNameProducer{producerAt("kipper-ai-librechat", "librechat", 8080, planNow.Add(-time.Hour))}

	p := planRouteNames(s)

	c := written(t, p, "kipper-ai-librechat-librechat")
	assert.True(t, c.Occupied)
	assert.Equal(t, "kipper-ai-librechat", c.Namespace)
	assert.Nil(t, openInterval(c))
}

func TestPlanRouteNames_PlatformProducersClaimNothing(t *testing.T) {
	s := baseSnapshot()
	s.Namespaces["kipper-system"] = "uid-ks"
	s.Producers = []routeNameProducer{producerAt("kipper-system", "console-api", 8080, planNow.Add(-time.Hour))}

	p := planRouteNames(s)

	assert.Empty(t, p.Write)
}

func TestPlanRouteNames_DeletesAnAbandonedClaimAfterRetention(t *testing.T) {
	s := baseSnapshot()
	s.Claims["gone-web"] = routeNameClaim{Key: "gone-web", Namespace: "gone", UID: "uid-gone",
		Intervals: []attributionInterval{{From: planNow.Add(-80 * time.Hour), To: ptrTime(planNow.Add(-74 * time.Hour)), Generation: "g1"}}}
	s.Claims["recent-web"] = routeNameClaim{Key: "recent-web", Namespace: "recent", UID: "uid-recent",
		Intervals: []attributionInterval{{From: planNow.Add(-2 * time.Hour), To: ptrTime(planNow.Add(-time.Hour)), Generation: "g1"}}}

	p := planRouteNames(s)

	assert.Equal(t, []string{"gone-web"}, p.Delete)
	for _, c := range p.Write {
		assert.NotEqual(t, "gone-web", c.Key)
	}
}

func TestPlanRouteNames_WritesOnlyWhatChanged(t *testing.T) {
	s := baseSnapshot()
	s.Producers = []routeNameProducer{producerAt("shop", "web", 8080, planNow.Add(-time.Hour))}
	s.Claims["shop-web"] = routeNameClaim{Key: "shop-web", Namespace: "shop", UID: "uid-shop",
		Intervals: []attributionInterval{{From: planNow.Add(-time.Hour), Generation: "g2"}}}

	p := planRouteNames(s)

	assert.Empty(t, p.Write, "an unchanged claim is not rewritten every scan")
}

func TestPlanRouteNames_ASuspensionClosesAtTheForeignPodsCreation(t *testing.T) {
	foreignPod := planNow.Add(-40 * time.Second)
	s := baseSnapshot()
	s.Suspended = true
	s.SuspendedAt = foreignPod
	s.Producers = []routeNameProducer{producerAt("shop", "web", 8080, planNow.Add(-time.Hour))}
	s.Claims["shop-web"] = routeNameClaim{Key: "shop-web", Namespace: "shop", UID: "uid-shop",
		Intervals: []attributionInterval{{From: planNow.Add(-time.Hour), Generation: "g2"}}}

	c := written(t, planRouteNames(s), "shop-web")
	require.NotNil(t, c.Intervals[0].To)
	assert.Equal(t, foreignPod, *c.Intervals[0].To, "attribution ends before any other binary could have acted")
}

func TestPlanRouteNames_ASharedKeyKeepsItsSharersWhenTheHolderGoesOutsideABootstrap(t *testing.T) {
	s := baseSnapshot()
	s.Namespaces["team-prod-web"] = "uid-tpw"
	delete(s.Namespaces, "team")
	s.Producers = []routeNameProducer{producerAt("team-prod", "web-api", 8080, planNow.Add(-time.Hour))}
	s.Workloads["team-prod-web"] = map[string]bool{"api": true}
	s.Claims["team-prod-web-api"] = routeNameClaim{Key: "team-prod-web-api", Namespace: "team", UID: "uid-team", Shared: []string{"team-prod", "team-prod-web"}, SharedUIDs: map[string]string{"team-prod": "uid-team-prod", "team-prod-web": "uid-tpw"}}

	assert.Empty(t, planRouteNames(s).Write, "only a bootstrap resolves a shared key, even with its holder gone")
}

func TestPlanRouteNames_ABootstrapNeverLeavesASharedKeyWithoutAnOwner(t *testing.T) {
	s := baseSnapshot()
	s.Bootstrap = true
	s.Namespaces = map[string]types.UID{"team-a": "uid-a", "team-a-x": "uid-ax"}
	s.Workloads["team-a"] = map[string]bool{"x-app": true}
	s.Workloads["team-a-x"] = map[string]bool{"app": true}
	s.Claims["team-a-x-app"] = routeNameClaim{Key: "team-a-x-app", Namespace: "team", UID: "uid-old", Shared: []string{"team-a", "team-a-x"}, SharedUIDs: map[string]string{"team-a": "uid-a", "team-a-x": "uid-ax"}}

	c := written(t, planRouteNames(s), "team-a-x-app")
	assert.Equal(t, "team-a", c.Namespace, "an owner is chosen even when nobody produces the key right now")
	assert.Equal(t, "uid-a", c.UID)
	assert.Equal(t, []string{"team-a-x"}, c.Shared)
}

func TestPlanRouteNames_AWorkloadWhoseNameNormalizesToTheKeyKeepsItShared(t *testing.T) {
	s := baseSnapshot()
	s.Bootstrap = true
	s.Producers = []routeNameProducer{producerAt("team-prod", "web", 8080, planNow.Add(-time.Hour))}
	s.Workloads["team"] = map[string]bool{"prod--web": true}
	s.Workloads["team-prod"] = map[string]bool{"web": true}
	s.Claims["team-prod-web"] = routeNameClaim{Key: "team-prod-web", Namespace: "team-prod", UID: "uid-team-prod", Shared: []string{"team"}, SharedUIDs: map[string]string{"team": "uid-team"}}

	assert.Empty(t, planRouteNames(s).Write, "app prod--web in team can still publish the same Traefik name")
}

func TestPlanRouteNames_ARecreatedSharerDropsOutAtBootstrap(t *testing.T) {
	s := baseSnapshot()
	s.Bootstrap = true
	s.Producers = []routeNameProducer{producerAt("team", "prod-web", 8080, planNow.Add(-48*time.Hour))}
	s.Workloads["team-prod"] = map[string]bool{"web": true}
	c := routeNameClaim{Key: "team-prod-web", Namespace: "team", UID: "uid-team"}
	c.setShared([]string{"team-prod"}, map[string]types.UID{"team-prod": "uid-old"})
	s.Claims["team-prod-web"] = c

	got := written(t, planRouteNames(s), "team-prod-web")
	assert.Empty(t, got.Shared, "a recreated namespace's new app is not the sharer that was grandfathered")
	assert.NotNil(t, openInterval(got))
}

func TestPlanRouteNames_AForeignProducerDoesNotHideAnEarlierSuspension(t *testing.T) {
	suspended := planNow.Add(-60 * time.Second)
	s := baseSnapshot()
	s.Suspended = true
	s.SuspendedAt = suspended
	s.Producers = []routeNameProducer{
		producerAt("team", "prod-web", 8080, planNow.Add(-48*time.Hour)),
		producerAt("team-prod", "web", 8080, planNow.Add(-40*time.Second)),
	}
	s.Claims["team-prod-web"] = routeNameClaim{Key: "team-prod-web", Namespace: "team", UID: "uid-team",
		Intervals: []attributionInterval{{From: planNow.Add(-time.Hour), Generation: "g2"}}}

	c := written(t, planRouteNames(s), "team-prod-web")
	require.NotNil(t, c.Intervals[0].To)
	assert.Equal(t, suspended, *c.Intervals[0].To, "the earlier of suspension and producer creation ends the interval")
}

func TestPlanRouteNames_ARecreatedHolderIsANewOwnerAtTheSharedExit(t *testing.T) {
	s := baseSnapshot()
	s.Bootstrap = true
	s.Namespaces["team"] = "uid-new"
	s.Producers = []routeNameProducer{
		producerAt("team", "prod-web", 8080, planNow.Add(-time.Hour)),
		producerAt("team-prod", "web", 8080, planNow.Add(-48*time.Hour)),
	}
	c := routeNameClaim{Key: "team-prod-web", Namespace: "team", UID: "uid-old",
		Intervals: []attributionInterval{{From: planNow.Add(-72 * time.Hour), To: ptrTime(planNow.Add(-71 * time.Hour)), Generation: "g0"}}}
	c.setShared([]string{"team-prod"}, s.Namespaces)
	s.Claims["team-prod-web"] = c

	got := written(t, planRouteNames(s), "team-prod-web")
	assert.NotEqual(t, "uid-old", got.UID, "a dead UID is never kept as the holder")
	assert.Empty(t, got.Intervals, "a recreated namespace starts with no history")
}

func TestPlanRouteNames_ARecreatedOccupierIsANewOwner(t *testing.T) {
	s := baseSnapshot()
	s.Namespaces["kipper-ai-librechat"] = "uid-new"
	s.Producers = []routeNameProducer{producerAt("kipper-ai-librechat", "librechat", 8080, planNow.Add(-time.Hour))}
	s.Claims["kipper-ai-librechat-librechat"] = routeNameClaim{Key: "kipper-ai-librechat-librechat", Namespace: "kipper-ai-librechat", UID: "uid-old", Occupied: true}

	got := written(t, planRouteNames(s), "kipper-ai-librechat-librechat")
	assert.Equal(t, "uid-new", got.UID)
}

func TestPlanRouteNames_TwoBackendsInOneNamespaceWithOneKeyShowNoTraffic(t *testing.T) {
	s := baseSnapshot()
	s.Producers = []routeNameProducer{
		producerAt("team", "prod-web", 8080, planNow.Add(-time.Hour)),
		producerAt("team", "prod--web", 8080, planNow.Add(-time.Minute)),
	}
	s.Claims["team-prod-web"] = routeNameClaim{Key: "team-prod-web", Namespace: "team", UID: "uid-team",
		Intervals: []attributionInterval{{From: planNow.Add(-time.Hour), Generation: "g2"}}}

	c := written(t, planRouteNames(s), "team-prod-web")
	assert.Nil(t, openInterval(c), "two apps routed under one Traefik name cannot be told apart")
}

func TestPlanRouteNames_AnIdleClaimOfALiveNamespaceIsDeletedAfterRetention(t *testing.T) {
	s := baseSnapshot()
	s.Workloads["shop"] = map[string]bool{"web": true}
	s.Claims["shop-cm-acme-http-solver-x7k2"] = routeNameClaim{Key: "shop-cm-acme-http-solver-x7k2", Namespace: "shop", UID: "uid-shop",
		Intervals: []attributionInterval{{From: planNow.Add(-80 * time.Hour), To: ptrTime(planNow.Add(-79 * time.Hour)), Generation: "g1"}}}
	s.Claims["shop-web"] = routeNameClaim{Key: "shop-web", Namespace: "shop", UID: "uid-shop",
		Intervals: []attributionInterval{{From: planNow.Add(-80 * time.Hour), To: ptrTime(planNow.Add(-79 * time.Hour)), Generation: "g1"}}}
	s.Claims["shop-recent"] = routeNameClaim{Key: "shop-recent", Namespace: "shop", UID: "uid-shop",
		Intervals: []attributionInterval{{From: planNow.Add(-2 * time.Hour), To: ptrTime(planNow.Add(-time.Hour)), Generation: "g1"}}}

	p := planRouteNames(s)

	assert.Equal(t, []string{"shop-cm-acme-http-solver-x7k2"}, p.Delete,
		"a key nothing produces or could publish is let go; one an app could still publish, or with recent history, stays")
}
