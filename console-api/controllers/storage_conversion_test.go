package controllers

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/longhorncap"
)

const gib = int64(1) << 30

var convNow = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// convDisks builds one roomy disk per node with the given Longhorn tags.
func convDisks(t *testing.T, nodes map[string][]string, available int64) []longhorncap.Disk {
	t.Helper()
	var objs []unstructured.Unstructured
	for name, tags := range nodes {
		tagList := make([]interface{}, 0, len(tags))
		for _, tag := range tags {
			tagList = append(tagList, tag)
		}
		objs = append(objs, unstructured.Unstructured{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": name},
			"spec": map[string]interface{}{
				"allowScheduling": true,
				"tags":            tagList,
				"disks": map[string]interface{}{"disk-1": map[string]interface{}{
					"allowScheduling": true, "storageReserved": int64(0), "diskType": "filesystem",
				}},
			},
			"status": map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{"type": "Ready", "status": "True"},
					map[string]interface{}{"type": "Schedulable", "status": "True"},
				},
				"diskStatus": map[string]interface{}{"disk-1": map[string]interface{}{
					"storageMaximum": 100 * gib, "storageScheduled": int64(0), "storageAvailable": available,
					"conditions": []interface{}{map[string]interface{}{"type": "Schedulable", "status": "True"}},
				}},
			},
		}})
	}
	return longhorncap.SchedulingDisks(objs, longhorncap.Settings{OverProvisioning: 100, MinimalAvailable: 25})
}

// convCluster is a server and three workers, all able to hold copies, with
// room everywhere, on a Longhorn release with offline rebuilding.
func convCluster(t *testing.T) *lhCluster {
	tags := map[string][]string{
		"server-1": {"kipper-server", "kipper-replica"},
		"worker-1": {"kipper-replica"},
		"worker-2": {"kipper-replica"},
		"worker-3": {"kipper-replica"},
	}
	c := &lhCluster{
		Servers:           map[string]bool{"server-1": true},
		Eligible:          map[string]bool{},
		Available:         map[string]bool{},
		Disks:             convDisks(t, tags, 90*gib),
		OfflineRebuilding: true,
		CapacityRead:      true,
	}
	for node := range tags {
		c.Eligible[node] = true
		c.Available[node] = true
	}
	return c
}

func healthyReplica(name, node string) lhReplica {
	return lhReplica{Name: name, UID: "uid-" + name, ResourceVersion: "7", Node: node, HealthyAt: "2026-10-24T10:00:00Z"}
}

// convVolume is an attached volume with locality and auto-balance off whose
// replicas are all RW in the engine.
func convVolume(count int64, selector []string, replicas ...lhReplica) *lhVolume {
	modes := map[string]string{}
	for _, r := range replicas {
		modes[r.Name] = "RW"
	}
	return &lhVolume{
		Name: "pvc-1", Count: count, Selector: selector, Locality: "disabled", AutoBalance: "disabled",
		Size: 10 * gib, Actual: 1 * gib, State: "attached", Image: "engine:v1", CurrentImage: "engine:v1",
		Replicas: replicas, Modes: modes,
	}
}

func patchJSON(t *testing.T, step conversionStep) string {
	t.Helper()
	require.Equal(t, stepPatch, step.Kind, "step: %+v", step)
	b, err := json.Marshal(step.Patch)
	require.NoError(t, err)
	return string(b)
}

var (
	serverOnly  = []string{"kipper-server"}
	replicaOnly = []string{"kipper-replica"}
)

func TestRaisingToTwoSendsOnePatchWithTheRebuildAnnotation(t *testing.T) {
	v := convVolume(1, serverOnly, healthyReplica("r-s", "server-1"))
	step := planConversion(2, v, convCluster(t), convNow)
	assert.Equal(t, leaseTake, step.Lease)
	assert.Equal(t, kipperv1.StorageCopiesConverting, step.Reason)
	assert.JSONEq(t, `{"metadata":{"annotations":{"kipper.run/copies-rebuild-started":"2026-10-24T12:00:00Z"}},
		"spec":{"nodeSelector":["kipper-replica"],"dataLocality":"disabled","replicaAutoBalance":"disabled","numberOfReplicas":2,"offlineRebuilding":"enabled"}}`,
		patchJSON(t, step))
}

func TestRaisingToTwoOnLonghornWithoutOfflineRebuildingLeavesTheFieldOut(t *testing.T) {
	c := convCluster(t)
	c.OfflineRebuilding = false
	step := planConversion(2, convVolume(1, serverOnly, healthyReplica("r-s", "server-1")), c, convNow)
	assert.JSONEq(t, `{"metadata":{"annotations":{"kipper.run/copies-rebuild-started":"2026-10-24T12:00:00Z"}},
		"spec":{"nodeSelector":["kipper-replica"],"dataLocality":"disabled","replicaAutoBalance":"disabled","numberOfReplicas":2}}`,
		patchJSON(t, step))
}

func TestADetachedVolumeGrowsOnlyWithOfflineRebuilding(t *testing.T) {
	v := convVolume(1, serverOnly, healthyReplica("r-s", "server-1"))
	v.State = "detached"
	v.Modes = nil
	c := convCluster(t)
	assert.Equal(t, stepPatch, planConversion(2, v, c, convNow).Kind, "Longhorn 1.9 and later rebuild a detached volume")
	c.OfflineRebuilding = false
	step := planConversion(2, v, c, convNow)
	assert.Equal(t, stepBlocked, step.Kind)
	assert.Equal(t, leaseRelease, step.Lease)
	assert.Contains(t, step.Message, "not attached")
}

// A rebuild Kipper asked for stays outstanding, and holds the
// lease, while Longhorn has not scheduled, has failed or has not started the
// new replica, within the budget.
func TestAnOutstandingRebuildWaitsAndHoldsTheLease(t *testing.T) {
	started := convNow.Add(-5 * time.Minute)
	cases := map[string][]lhReplica{
		"not scheduled yet": {healthyReplica("r-s", "server-1"), {Name: "r-new"}},
		"no replica yet":    {healthyReplica("r-s", "server-1")},
		"failed attempt":    {healthyReplica("r-s", "server-1"), {Name: "r-new", Node: "worker-1", FailedAt: "2026-10-24T11:58:00Z"}},
		"building":          {healthyReplica("r-s", "server-1"), {Name: "r-new", Node: "worker-1"}},
	}
	for name, replicas := range cases {
		t.Run(name, func(t *testing.T) {
			v := convVolume(2, replicaOnly, replicas...)
			v.RebuildStarted = started
			step := planConversion(2, v, convCluster(t), convNow)
			assert.Equal(t, stepWait, step.Kind)
			assert.Equal(t, leaseHold, step.Lease)
			assert.Equal(t, kipperv1.StorageCopiesConverting, step.Reason)
		})
	}
}

func TestAWriteOnlyReplicaIsStillBuilding(t *testing.T) {
	v := convVolume(2, replicaOnly, healthyReplica("r-s", "server-1"), healthyReplica("r-w", "worker-1"))
	v.Modes["r-w"] = "WO"
	v.RebuildStarted = convNow.Add(-time.Minute)
	assert.Equal(t, stepWait, planConversion(2, v, convCluster(t), convNow).Kind)
}

// Past the budget the rebuild is Blocked, the lease is released,
// and the same build is never asked for again with a new start time.
func TestARebuildPastItsBudgetIsBlockedWithoutAnotherPatch(t *testing.T) {
	v := convVolume(2, replicaOnly, healthyReplica("r-s", "server-1"))
	v.RebuildStarted = convNow.Add(-rebuildBudget(v.Size) - time.Second)
	step := planConversion(2, v, convCluster(t), convNow)
	assert.Equal(t, stepBlocked, step.Kind)
	assert.Equal(t, leaseRelease, step.Lease)
	assert.Equal(t, kipperv1.StorageCopiesBlocked, step.Reason)
	assert.Nil(t, step.Patch)
	assert.Equal(t, 40*time.Minute, rebuildBudget(v.Size), "30 minutes plus one per GiB")
}

func TestLonghornsOwnRebuildIsReportedAsItIs(t *testing.T) {
	v := convVolume(2, replicaOnly, healthyReplica("r-s", "server-1"), lhReplica{Name: "r-w", Node: "worker-1"})
	step := planConversion(2, v, convCluster(t), convNow)
	assert.Equal(t, conversionStep{Kind: stepWait}, step)
}

// Reducing two copies to one lowers the count first, then deletes the worker copy,
// then moves the selector to the server.
func TestRollbackToOneCopyKeepsTheServerCopy(t *testing.T) {
	c := convCluster(t)
	server, worker := healthyReplica("r-s", "server-1"), healthyReplica("r-w", "worker-1")
	v := convVolume(2, replicaOnly, server, worker)

	step := planConversion(1, v, c, convNow)
	assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-replica"],"dataLocality":"disabled","replicaAutoBalance":"disabled","numberOfReplicas":1}}`,
		patchJSON(t, step))
	assert.Equal(t, leaseRelease, step.Lease)

	v.Count = 1
	step = planConversion(1, v, c, convNow)
	require.Equal(t, stepDelete, step.Kind)
	assert.Equal(t, "r-w", step.Replica.Name)
	assert.Equal(t, []string{"r-s"}, step.Keep)

	v.Replicas = []lhReplica{server}
	assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-server"],"dataLocality":"disabled","replicaAutoBalance":"disabled"}}`,
		patchJSON(t, planConversion(1, v, c, convNow)))

	v.Selector = serverOnly
	assert.Equal(t, conversionStep{Kind: stepDone}, planConversion(1, v, c, convNow))
}

func TestNoCountIsLoweredWhileLonghornMayPickTheReplica(t *testing.T) {
	for _, settings := range [][2]string{{"best-effort", "disabled"}, {"disabled", "least-effort"}} {
		v := convVolume(2, replicaOnly, healthyReplica("r-s", "server-1"), healthyReplica("r-w", "worker-1"))
		v.Locality, v.AutoBalance = settings[0], settings[1]
		assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-replica"],"dataLocality":"disabled","replicaAutoBalance":"disabled"}}`,
			patchJSON(t, planConversion(1, v, convCluster(t), convNow)), "settings %v", settings)
	}
}

func TestANormalizingPatchNeverLowersTheCountWithLocalityOn(t *testing.T) {
	v := convVolume(2, replicaOnly, healthyReplica("r-s", "server-1"))
	v.Locality = "best-effort"
	assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-server"],"dataLocality":"disabled","replicaAutoBalance":"disabled"}}`,
		patchJSON(t, planConversion(1, v, convCluster(t), convNow)))
}

// The last healthy copy is never deleted. With the server's copy failed
// or unusable, a reduction builds or waits for a server copy and deletes
// nothing.
func TestAReductionNeverDeletesTheLastHealthyCopy(t *testing.T) {
	failed := lhReplica{Name: "r-s", Node: "server-1", FailedAt: "2026-10-24T11:00:00Z"}
	stale := healthyReplica("r-s", "server-1")
	stale.LastFailedAt = "2026-10-24T11:00:00Z"
	unreachable := healthyReplica("r-s", "server-1")
	evicting := healthyReplica("r-s", "server-1")
	evicting.EvictionRequested = true
	cases := map[string]struct {
		server lhReplica
		down   bool
		want   stepKind
	}{
		"server copy failed":    {server: failed, want: stepPatch},
		"server copy stale":     {server: stale, want: stepBlocked},
		"server node down":      {server: unreachable, down: true, want: stepBlocked},
		"server copy evicting":  {server: evicting, want: stepBlocked},
		"server copy not in RW": {server: healthyReplica("r-s", "server-1"), want: stepBlocked},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := convCluster(t)
			c.Available["server-1"] = !tc.down
			v := convVolume(1, replicaOnly, tc.server, healthyReplica("r-w", "worker-1"))
			if name == "server copy not in RW" {
				v.Modes["r-s"] = "ERR"
			}
			step := planConversion(1, v, c, convNow)
			assert.Equal(t, tc.want, step.Kind, "step: %+v", step)
			assert.Nil(t, step.Replica)
			if step.Kind == stepPatch {
				assert.Equal(t, serverOnly, step.Patch.Spec.NodeSelector, "only a server copy may be built")
			}
		})
	}
}

// Auto-salvage clears failedAt and keeps the old healthyAt, so Kipper cannot
// confirm that the copy is current. A requested change reports this cause,
// while a volume with nothing to change uses the live report.
func TestASalvagedCopyBlocksACopyChangeWithItsCause(t *testing.T) {
	salvaged := healthyReplica("r-s", "server-1")
	salvaged.LastFailedAt = "2026-10-24T11:00:00Z"
	v := convVolume(1, serverOnly, salvaged)

	step := planConversion(2, v, convCluster(t), convNow)

	assert.Equal(t, stepBlocked, step.Kind)
	assert.Contains(t, step.Message, "server-1")
	assert.Contains(t, step.Message, "came back after a failure")
	assert.Contains(t, step.Message, "Setting the copies back")

	idle := planConversion(1, v, convCluster(t), convNow)
	assert.Equal(t, stepWait, idle.Kind)
	assert.Empty(t, idle.Reason)
}

// Kipper's new copy is current, but the salvaged copy still lacks evidence
// of recovery, so the conversion stops waiting for it.
func TestASalvagedCopyEndsTheWaitForARebuild(t *testing.T) {
	salvaged := healthyReplica("r-s", "server-1")
	salvaged.LastFailedAt = "2026-10-24T11:00:00Z"
	v := convVolume(2, replicaOnly, salvaged, healthyReplica("r-w", "worker-1"))
	v.RebuildStarted = convNow.Add(-time.Minute)

	step := planConversion(2, v, convCluster(t), convNow)

	assert.Equal(t, stepBlocked, step.Kind)
	assert.Contains(t, step.Message, "came back after a failure")
}

// Longhorn deletes an eviction-requested replica once the healthy
// count exceeds the spec count, so nothing is lowered or deleted meanwhile.
func TestNoReductionWhileAReplicaIsEvicted(t *testing.T) {
	worker := healthyReplica("r-w", "worker-1")
	worker.EvictionRequested = true
	v := convVolume(2, replicaOnly, healthyReplica("r-s", "server-1"), worker)
	step := planConversion(1, v, convCluster(t), convNow)
	assert.Equal(t, stepBlocked, step.Kind)
	assert.Contains(t, step.Message, "evicted")

	v.Count = 1
	assert.Equal(t, stepBlocked, planConversion(1, v, convCluster(t), convNow).Kind)
}

func TestNoReductionWhileDetached(t *testing.T) {
	v := convVolume(2, replicaOnly, healthyReplica("r-s", "server-1"), healthyReplica("r-w", "worker-1"))
	v.State = "detached"
	v.Modes = nil
	step := planConversion(1, v, convCluster(t), convNow)
	assert.Equal(t, stepBlocked, step.Kind)
	assert.Contains(t, step.Message, "not attached")
}

// A single copy on a worker moves to the server when copies 1 is
// set: build the server copy, lower the count, delete the worker copy.
func TestCopiesOneMovesAWorkerOnlyCopyToTheServer(t *testing.T) {
	c := convCluster(t)
	worker := healthyReplica("r-w", "worker-1")
	v := convVolume(1, nil, worker)

	assert.JSONEq(t, `{"metadata":{"annotations":{"kipper.run/copies-rebuild-started":"2026-10-24T12:00:00Z"}},
		"spec":{"nodeSelector":["kipper-server"],"dataLocality":"disabled","replicaAutoBalance":"disabled","numberOfReplicas":2}}`,
		patchJSON(t, planConversion(1, v, c, convNow)))

	v.Count, v.Selector, v.RebuildStarted = 2, serverOnly, convNow
	v.Replicas = append(v.Replicas, healthyReplica("r-s", "server-1"))
	v.Modes["r-s"] = "RW"
	assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-server"],"dataLocality":"disabled","replicaAutoBalance":"disabled","numberOfReplicas":1}}`,
		patchJSON(t, planConversion(1, v, c, convNow)))

	v.Count = 1
	step := planConversion(1, v, c, convNow)
	require.Equal(t, stepDelete, step.Kind)
	assert.Equal(t, "r-w", step.Replica.Name)

	v.Replicas = v.Replicas[1:]
	assert.JSONEq(t, `{"metadata":{"annotations":{"kipper.run/copies-rebuild-started":null}}}`,
		patchJSON(t, planConversion(1, v, c, convNow)), "done clears the annotation")
}

// Reducing three worker copies to two builds a server copy under a
// temporary server-only selector, trims one worker copy at a time, then
// restores the copies-2 selector.
func TestThreeCopiesToTwoStagesThroughAServerCopy(t *testing.T) {
	c := convCluster(t)
	v := convVolume(3, replicaOnly, healthyReplica("r-1", "worker-1"), healthyReplica("r-2", "worker-2"), healthyReplica("r-3", "worker-3"))

	step := planConversion(2, v, c, convNow)
	assert.Equal(t, int64(4), *patchStepSpec(t, step).NumberOfReplicas)
	assert.Equal(t, serverOnly, step.Patch.Spec.NodeSelector)

	v.Count, v.Selector, v.RebuildStarted = 4, serverOnly, convNow
	v.Replicas = append(v.Replicas, healthyReplica("r-s", "server-1"))
	v.Modes["r-s"] = "RW"
	assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-server"],"dataLocality":"disabled","replicaAutoBalance":"disabled","numberOfReplicas":2}}`,
		patchJSON(t, planConversion(2, v, c, convNow)), "the server-only selector stays while trimming")

	v.Count = 2
	step = planConversion(2, v, c, convNow)
	require.Equal(t, stepDelete, step.Kind)
	assert.Equal(t, "r-2", step.Replica.Name)
	assert.Equal(t, []string{"r-s", "r-1"}, step.Keep)

	v.Replicas = []lhReplica{v.Replicas[0], v.Replicas[2], v.Replicas[3]}
	step = planConversion(2, v, c, convNow)
	require.Equal(t, stepDelete, step.Kind)
	assert.Equal(t, "r-3", step.Replica.Name)

	v.Replicas = []lhReplica{v.Replicas[0], v.Replicas[2]}
	assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-replica"],"dataLocality":"disabled","replicaAutoBalance":"disabled","offlineRebuilding":"enabled"}}`,
		patchJSON(t, planConversion(2, v, c, convNow)))
}

func patchStepSpec(t *testing.T, step conversionStep) *volumePatchSpec {
	t.Helper()
	require.Equal(t, stepPatch, step.Kind, "step: %+v", step)
	require.NotNil(t, step.Patch.Spec)
	require.NotNil(t, step.Patch.Spec.NumberOfReplicas)
	return step.Patch.Spec
}

func TestTheKeepSetPrefersACopyOnANodeTaggedForCopies(t *testing.T) {
	c := convCluster(t)
	c.Available["edge-1"] = true
	v := convVolume(2, replicaOnly, healthyReplica("r-a", "edge-1"), healthyReplica("r-s", "server-1"), healthyReplica("r-w", "worker-2"))
	step := planConversion(2, v, c, convNow)
	require.Equal(t, stepDelete, step.Kind)
	assert.Equal(t, "r-a", step.Replica.Name)
	assert.Equal(t, []string{"r-s", "r-w"}, step.Keep)
}

func TestAServerReplicaIsNeverPickedForDeletion(t *testing.T) {
	c := convCluster(t)
	reps := classify(convVolume(1, serverOnly, healthyReplica("r-s", "server-1"), healthyReplica("r-s2", "server-1")), c)
	assert.Nil(t, surplus(reps, reps[:1]))
}

func TestAReductionWithTooFewHealthyKeepersIsBlocked(t *testing.T) {
	c := convCluster(t)
	c.Available["worker-1"] = false
	c.Available["worker-2"] = false
	v := convVolume(2, replicaOnly, healthyReplica("r-s", "server-1"), healthyReplica("r-1", "worker-1"), healthyReplica("r-2", "worker-2"))
	step := planConversion(2, v, c, convNow)
	assert.Equal(t, stepBlocked, step.Kind, "a blocked state lets the copies be set back, a converting one would not")
	assert.Nil(t, step.Replica)
}

func TestTheServerMustHaveRoomBeforeAServerCopyIsBuilt(t *testing.T) {
	c := convCluster(t)
	c.Disks = convDisks(t, map[string][]string{
		"server-1": {"kipper-server", "kipper-replica"}, "worker-1": {"kipper-replica"},
	}, 30*gib)
	v := convVolume(2, replicaOnly, healthyReplica("r-1", "worker-1"), healthyReplica("r-2", "worker-2"))
	assert.Equal(t, stepPatch, planConversion(1, v, c, convNow).Kind, "30 - 1 is more than the 25 percent floor")

	c.ServerReserve = 5 * gib
	step := planConversion(1, v, c, convNow)
	assert.Equal(t, stepBlocked, step.Kind, "a dump's reserve takes the server below the floor")
	assert.Contains(t, step.Message, "The server has no room")
	assert.NotContains(t, step.Message, "worker")
}

func TestRaisingToTwoNeedsASecondNodeWithRoom(t *testing.T) {
	c := convCluster(t)
	c.Disks = convDisks(t, map[string][]string{
		"server-1": {"kipper-server", "kipper-replica"}, "worker-1": {"kipper-replica"}, "worker-2": nil,
	}, 20*gib)
	v := convVolume(1, serverOnly, healthyReplica("r-s", "server-1"))
	step := planConversion(2, v, c, convNow)
	assert.Equal(t, stepBlocked, step.Kind)
	assert.Contains(t, step.Message, "only one node has enough room")
}

// The worker's disk has plenty of physical space, but the replicas already
// scheduled on it leave 9 GiB of its 100 GiB, less than the 10 GiB volume.
func TestRaisingToTwoCountsWhatTheTargetNodeAlreadyHolds(t *testing.T) {
	c := convCluster(t)
	c.Disks = convDisks(t, map[string][]string{
		"server-1": {"kipper-server", "kipper-replica"}, "worker-1": {"kipper-replica"},
	}, 90*gib)
	v := convVolume(1, serverOnly, healthyReplica("r-s", "server-1"))
	assert.Equal(t, stepPatch, planConversion(2, v, c, convNow).Kind, "an empty worker takes the copy")

	for i := range c.Disks {
		if c.Disks[i].Node == "worker-1" {
			c.Disks[i].Scheduled = 91 * gib
		}
	}
	step := planConversion(2, v, c, convNow)
	assert.Equal(t, stepBlocked, step.Kind)
	assert.Contains(t, step.Message, "only one node has enough room")
}

func TestAMigrationOrEngineUpgradeBlocksAConversion(t *testing.T) {
	v := convVolume(1, serverOnly, healthyReplica("r-s", "server-1"))
	v.MigrationNodeID = "worker-1"
	assert.Equal(t, stepBlocked, planConversion(2, v, convCluster(t), convNow).Kind)

	v = convVolume(1, serverOnly, healthyReplica("r-s", "server-1"))
	v.Image = "engine:v2"
	assert.Equal(t, stepBlocked, planConversion(2, v, convCluster(t), convNow).Kind)
}

func TestAnAdoptedTwoCopyVolumeIsNormalized(t *testing.T) {
	v := convVolume(2, nil, healthyReplica("r-s", "server-1"), healthyReplica("r-w", "worker-1"))
	v.Locality = "best-effort"
	assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-replica"],"dataLocality":"disabled","replicaAutoBalance":"disabled","offlineRebuilding":"enabled"}}`,
		patchJSON(t, planConversion(2, v, convCluster(t), convNow)))
}

func TestAVolumeAtItsTargetIsDone(t *testing.T) {
	v := convVolume(2, replicaOnly, healthyReplica("r-s", "server-1"), healthyReplica("r-w", "worker-1"))
	v.OfflineRebuilding = "enabled"
	assert.Equal(t, conversionStep{Kind: stepDone}, planConversion(2, v, convCluster(t), convNow))
}

func TestATrimPatchCarriesNoCountWhileLonghornMayPickTheReplica(t *testing.T) {
	v := convVolume(2, serverOnly)
	v.AutoBalance = "best-effort"
	one := int64(1)
	assert.Nil(t, trimPatch(v, &one).Spec.NumberOfReplicas)
	v.AutoBalance = "disabled"
	assert.Equal(t, &one, trimPatch(v, &one).Spec.NumberOfReplicas)
}

// A rebuild patch never lowers the count. A spec count above
// the one asked for is lowered first, under the reduction guards.
func TestARebuildNeverLowersTheCountUnguarded(t *testing.T) {
	failed := lhReplica{Name: "r-f", Node: "worker-2", FailedAt: "2026-10-24T11:00:00Z"}
	growing := func() *lhVolume { return convVolume(3, replicaOnly, healthyReplica("r-s", "server-1"), failed) }
	serverBuild := func() *lhVolume {
		return convVolume(4, replicaOnly, healthyReplica("r-1", "worker-1"), healthyReplica("r-2", "worker-2"), failed)
	}
	for name, tc := range map[string]struct {
		volume func() *lhVolume
		target int64
		count  int64
	}{
		"towards two": {growing, 2, 2},
		"server copy": {serverBuild, 1, 3},
	} {
		t.Run(name, func(t *testing.T) {
			c := convCluster(t)
			v := tc.volume()
			v.Locality = "best-effort"
			step := planConversion(tc.target, v, c, convNow)
			assert.Nil(t, patchSpec(t, step).NumberOfReplicas, "locality first")
			assert.NotEqual(t, leaseTake, step.Lease)

			v.Locality = "disabled"
			step = planConversion(tc.target, v, c, convNow)
			assert.Equal(t, tc.count, *patchSpec(t, step).NumberOfReplicas)
			assert.Nil(t, step.Patch.Metadata, "a decrease starts no rebuild")
			assert.Equal(t, v.Selector, step.Patch.Spec.NodeSelector)

			evicted := healthyReplica("r-e", "worker-3")
			evicted.EvictionRequested = true
			v.Replicas = append(v.Replicas, evicted)
			v.Modes["r-e"] = "RW"
			if tc.target == 2 {
				v.Count = 4
			} else {
				v.Count = 5
			}
			assert.Equal(t, stepBlocked, planConversion(tc.target, v, c, convNow).Kind, "no decrease while a replica is evicted")

			v = tc.volume()
			v.State = "detached"
			v.Modes = nil
			assert.Equal(t, stepBlocked, planConversion(tc.target, v, c, convNow).Kind)
		})
	}
}

func patchSpec(t *testing.T, step conversionStep) *volumePatchSpec {
	t.Helper()
	require.Equal(t, stepPatch, step.Kind, "step: %+v", step)
	require.NotNil(t, step.Patch.Spec)
	return step.Patch.Spec
}

// A rebuild Kipper started keeps the lease until its copy exists, even beside
// a salvaged copy.
func TestASalvagedCopyWaitsForTheCopyKipperAskedFor(t *testing.T) {
	salvaged := healthyReplica("r-s", "server-1")
	salvaged.LastFailedAt = "2026-10-24T11:00:00Z"
	v := convVolume(2, replicaOnly, salvaged)
	v.RebuildStarted = convNow.Add(-time.Minute)

	step := planConversion(2, v, convCluster(t), convNow)

	assert.Equal(t, stepWait, step.Kind)
	assert.Equal(t, leaseHold, step.Lease)
}
