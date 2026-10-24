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
	"github.com/getkipper/kipper/controller/pkg/platform"
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
	return lhReplica{Name: name, UID: "uid-" + name, ResourceVersion: "7", Node: node, HealthyAt: "2026-10-24T10:00:00Z", CurrentState: "running"}
}

// convVolume is a volume held on server-1, with locality and auto-balance off,
// whose replicas are all RW in the engine there.
func convVolume(count int64, selector []string, replicas ...lhReplica) *lhVolume {
	modes := map[string]string{}
	for _, r := range replicas {
		modes[r.Name] = "RW"
	}
	return &lhVolume{
		Name: "pvc-1", Count: count, Selector: selector, Locality: "disabled", AutoBalance: "disabled",
		Size: 10 * gib, Actual: 1 * gib, State: "attached", Image: "engine:v1", CurrentImage: "engine:v1",
		Replicas: replicas, Modes: modes, EngineNode: "server-1", EngineOwner: "server-1", AttachedNode: "server-1",
		CounterOn: true, FrontendDisabled: true, Attachment: &lhAttachment{TicketNode: "server-1", TicketSatisfied: true}, Held: true,
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

// Once the requested single copy is on the server with the requested settings,
// its health belongs in the live copies status. A copy unable to serve, for
// example while the pod's node is down, blocks only a pending change.
func TestOnlyAPendingChangeWaitsForTheServerCopy(t *testing.T) {
	v := convVolume(1, serverOnly, healthyReplica("r-s", "server-1"))
	v.Modes = map[string]string{}

	step := planConversion(1, v, convCluster(t), convNow)
	assert.Equal(t, stepDone, step.Kind, "step: %+v", step)
	assert.Empty(t, step.Reason)

	v.Count = 2
	v.Replicas = append(v.Replicas, healthyReplica("r-w", "worker-1"))
	step = planConversion(1, v, convCluster(t), convNow)
	assert.Equal(t, stepWait, step.Kind)
	assert.Equal(t, kipperv1.StorageCopiesConversionPending, step.Reason)
	assert.True(t, step.Hold)
}

// A detached volume that was never attached holds no data, so its copy is
// complete and nothing waits for Longhorn to build it.
func TestAVolumeNeverAttachedIsNotWaitingForABuild(t *testing.T) {
	fresh := lhReplica{Name: "r-s", UID: "uid-r-s", ResourceVersion: "7", Node: "server-1", CurrentState: "stopped"}
	v := convVolume(1, serverOnly, fresh)
	v.State = "detached"
	v.Modes = nil

	assert.Equal(t, stepDone, planConversion(1, v, convCluster(t), convNow).Kind, "one copy on the server is what was asked for")
	step := planConversion(2, v, convCluster(t), convNow)
	assert.Equal(t, leaseTake, step.Lease, "a second copy is built: %+v", step)
}

// A volume waiting for its restore needs a copy with health markers before
// it can be reported as converted.
func TestAVolumeAwaitingItsRestoreIsNotDone(t *testing.T) {
	fresh := lhReplica{Name: "r-s", UID: "uid-r-s", ResourceVersion: "7", Node: "server-1", CurrentState: "stopped"}
	v := convVolume(1, serverOnly, fresh)
	v.State, v.Modes = "detached", nil
	v.Seeded = true

	assert.NotEqual(t, stepDone, planConversion(1, v, convCluster(t), convNow).Kind)

	obj := lhVolumeObj("pvc-1", 1, platform.ServerStorageTag)
	assert.False(t, volumeFrom(obj).Seeded)
	_ = unstructured.SetNestedField(obj.Object, "s3://backups@example/?backup=backup-1&volume=pvc-0", "spec", "fromBackup")
	assert.True(t, volumeFrom(obj).Seeded, "the converter reads the volume's restore source")
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
			v.Held = false
			v.RebuildStarted = started
			step := planConversion(2, v, convCluster(t), convNow)
			assert.Equal(t, stepWait, step.Kind)
			assert.Equal(t, leaseHold, step.Lease)
			assert.Equal(t, kipperv1.StorageCopiesConverting, step.Reason)

			v.Held = true
			assert.Equal(t, kipperv1.StorageCopiesConversionPending, planConversion(2, v, convCluster(t), convNow).Reason,
				"a build on a held volume may still be set back")
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

// Reducing two copies to one lowers the count with data locality on in one
// patch, waits while Longhorn removes the worker copy, turns data locality off
// again and then moves the selector to the server.
func TestRollbackToOneCopyKeepsTheServerCopy(t *testing.T) {
	c := convCluster(t)
	server, worker := healthyReplica("r-s", "server-1"), healthyReplica("r-w", "worker-1")
	v := convVolume(2, replicaOnly, server, worker)

	step := planConversion(1, v, c, convNow)
	assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-replica"],"dataLocality":"best-effort","replicaAutoBalance":"disabled","numberOfReplicas":1}}`,
		patchJSON(t, step))
	assert.Equal(t, kipperv1.StorageCopiesConversionPending, step.Reason, "the copies may still be set back")
	assert.Equal(t, "Lowering the copy count to 1. Longhorn removes the extra copies one at a time and keeps the copy on server-1.", step.Message)
	assert.True(t, step.Hold)
	assert.Equal(t, leaseRelease, step.Lease)

	v.Count, v.Locality = 1, "best-effort"
	step = planConversion(1, v, c, convNow)
	assert.Equal(t, stepWait, step.Kind)
	assert.Equal(t, kipperv1.StorageCopiesConversionPending, step.Reason)
	assert.Equal(t, "Longhorn is removing the extra copies: the volume has 2 and keeps 1.", step.Message)
	assert.True(t, step.Hold)

	v.Replicas = []lhReplica{server}
	step = planConversion(1, v, c, convNow)
	assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-replica"],"dataLocality":"disabled","replicaAutoBalance":"disabled"}}`, patchJSON(t, step))
	assert.True(t, step.Hold)

	v.Locality = "disabled"
	assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-server"],"dataLocality":"disabled","replicaAutoBalance":"disabled"}}`,
		patchJSON(t, planConversion(1, v, c, convNow)))

	v.Selector = serverOnly
	assert.Equal(t, conversionStep{Kind: stepDone}, planConversion(1, v, c, convNow))
}

// The one patch that lowers the count turns auto-balance off and data locality
// on, whatever they were, so only data locality chooses the copy to remove.
func TestTheLoweringPatchTurnsAutoBalanceOff(t *testing.T) {
	for _, settings := range [][2]string{{"best-effort", "disabled"}, {"disabled", "least-effort"}, {"strict-local", "best-effort"}} {
		v := convVolume(2, replicaOnly, healthyReplica("r-s", "server-1"), healthyReplica("r-w", "worker-1"))
		v.Locality, v.AutoBalance = settings[0], settings[1]
		assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-replica"],"dataLocality":"best-effort","replicaAutoBalance":"disabled","numberOfReplicas":1}}`,
			patchJSON(t, planConversion(1, v, convCluster(t), convNow)), "settings %v", settings)
	}
}

// A count above the copies present is lowered under the hold like any other.
// Longhorn then has no extra copy, so data locality goes off again at once.
func TestACountAboveTheCopiesIsLoweredWithoutAWaitForLonghorn(t *testing.T) {
	v := convVolume(2, replicaOnly, healthyReplica("r-s", "server-1"))
	assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-replica"],"dataLocality":"best-effort","replicaAutoBalance":"disabled","numberOfReplicas":1}}`,
		patchJSON(t, planConversion(1, v, convCluster(t), convNow)))

	v.Count, v.Locality = 1, "best-effort"
	assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-replica"],"dataLocality":"disabled","replicaAutoBalance":"disabled"}}`,
		patchJSON(t, planConversion(1, v, convCluster(t), convNow)))
}

// Once the count is lowered, the removal is Longhorn's: an eviction or a copy
// waiting for its node blocks nothing, and Kipper waits until the copies left
// are the target and every one is RW in the held engine.
func TestALoweredCountWaitsForLonghornAlone(t *testing.T) {
	evicting := healthyReplica("r-s", "server-1")
	evicting.EvictionRequested = true
	v := convVolume(1, replicaOnly, evicting, healthyReplica("r-w", "worker-1"))
	v.Locality = "best-effort"
	c := convCluster(t)
	c.Available["worker-1"] = false
	step := planConversion(1, v, c, convNow)
	assert.Equal(t, stepWait, step.Kind, "step: %+v", step)
	assert.Contains(t, step.Message, "Longhorn is removing the extra copies")

	v.Replicas = v.Replicas[1:]
	v.Modes["r-w"] = "ERR"
	step = planConversion(1, v, convCluster(t), convNow)
	assert.Equal(t, stepWait, step.Kind, "step: %+v", step)
	assert.Equal(t, "Longhorn removed the extra copies and is bringing the rest up to date (0 of 1 copies in sync).", step.Message)
}

// Longhorn's locality cleanup picks its candidates from the largest group of
// replicas on one disk, failed ones included, and removes only one away from
// the engine's node. A failed copy beside the server copy leaves it nothing to
// remove until the failed copy goes after the stale replica timeout, so the
// change waits and says so.
func TestALoweredCountNamesAFailedCopyBesideTheServerCopy(t *testing.T) {
	failed := healthyReplica("r-x", "server-1")
	failed.FailedAt = "2026-10-24T11:00:00Z"
	v := convVolume(1, replicaOnly, healthyReplica("r-s", "server-1"), failed, healthyReplica("r-w", "worker-1"))
	v.Locality = "best-effort"
	delete(v.Modes, "r-x")

	step := planConversion(1, v, convCluster(t), convNow)

	assert.Equal(t, stepWait, step.Kind, "step: %+v", step)
	assert.Equal(t, "Waiting for Longhorn to remove the extra copies: the volume has 2 and keeps 1. Longhorn picks the copy to remove among "+
		"r-s and r-x, which share a disk on server-1, and it keeps the copies there, so it removes the extra copy only after "+
		"the failed copy r-x is gone, once the volume's stale replica timeout has passed.", step.Message)
}

// Copies that share the server's disk leave Longhorn's locality cleanup
// nothing to remove until one of them is gone, whatever their state. The
// change waits with the copies named and changes nothing, and setting the
// copies back ends it.
func TestALoweredCountNamesCopiesThatShareTheServersDisk(t *testing.T) {
	deletingFailed := healthyReplica("r-s2", "server-1")
	deletingFailed.FailedAt, deletingFailed.Deleting = "2026-10-24T11:00:00Z", true
	deleting := healthyReplica("r-s2", "server-1")
	deleting.Deleting = true
	evicting := healthyReplica("r-s2", "server-1")
	evicting.EvictionRequested = true
	homeEvicted := healthyReplica("r-s2", "server-1")
	homeEvicted.HomeEvicted = true
	building := healthyReplica("r-s2", "server-1")
	building.HealthyAt = ""
	inactive := longhornReplica("r-s2", "pvc-1", "server-1", "2026-10-24T10:00:00Z", "")
	_ = unstructured.SetNestedField(inactive.Object, false, "spec", "active")
	for name, twin := range map[string]lhReplica{
		"healthy twin": healthyReplica("r-s2", "server-1"), "inactive twin left by an engine upgrade": replicaFrom(inactive),
		"deleting failed twin": deletingFailed, "deleting twin": deleting, "evicting twin": evicting,
		"twin on an evicted disk": homeEvicted, "twin still building": building,
	} {
		t.Run(name, func(t *testing.T) {
			v := convVolume(1, replicaOnly, healthyReplica("r-s", "server-1"), twin, healthyReplica("r-w", "worker-1"))
			v.Locality = "best-effort"

			step := planConversion(1, v, convCluster(t), convNow)

			assert.Equal(t, stepWait, step.Kind, "step: %+v", step)
			assert.True(t, step.Hold, "step: %+v", step)
			assert.Nil(t, step.Patch)
			assert.Contains(t, step.Message, "Waiting for Longhorn to remove the extra copies: ")
			assert.Contains(t, step.Message, "Longhorn picks the copy to remove among r-s and r-s2, which share a disk on server-1, "+
				"and it keeps the copies there until it can remove one of them.")
		})
	}

	v := convVolume(1, replicaOnly, healthyReplica("r-s", "server-1"), healthyReplica("r-w2", "worker-2"), healthyReplica("r-w", "worker-1"))
	v.Locality = "best-effort"
	step := planConversion(1, v, convCluster(t), convNow)
	assert.Equal(t, "Longhorn is removing the extra copies: the volume has 3 and keeps 1.", step.Message,
		"copies on different nodes leave Longhorn a copy to remove")
}

// An engine upgrade or a move blocks a reduction before the count is lowered.
// After it, the planner waits under the hold and writes nothing.
func TestAnUpgradeOrMoveBlocksOnlyBeforeTheLowering(t *testing.T) {
	for name, busy := range map[string]func(*lhVolume){
		"engine upgrade": func(v *lhVolume) { v.Image = "engine:v2" },
		"move":           func(v *lhVolume) { v.MigrationNodeID = "worker-1" },
	} {
		t.Run(name, func(t *testing.T) {
			v := convVolume(1, replicaOnly, healthyReplica("r-s", "server-1"), healthyReplica("r-w", "worker-1"))
			v.Locality = "best-effort"
			busy(v)

			step := planConversion(1, v, convCluster(t), convNow)

			assert.Equal(t, stepWait, step.Kind, "step: %+v", step)
			assert.True(t, step.Hold, "step: %+v", step)
			assert.Nil(t, step.Patch)

			v.Count, v.Locality = 2, "disabled"
			step = planConversion(1, v, convCluster(t), convNow)
			assert.Equal(t, stepBlocked, step.Kind, "before the lowering: %+v", step)
		})
	}
}

// Longhorn's locality cleanup waits for a copy on the engine's node, which an
// eviction of the server copy from three copies leaves without one. The change
// waits and says so, and setting the copies back ends it.
func TestALoweredCountWithoutAServerCopySaysLocalityCleanupWaits(t *testing.T) {
	v := convVolume(1, replicaOnly, healthyReplica("r-w", "worker-1"), healthyReplica("r-w2", "worker-2"))
	v.Locality = "best-effort"

	step := planConversion(1, v, convCluster(t), convNow)

	assert.Equal(t, stepWait, step.Kind, "step: %+v", step)
	assert.True(t, step.Hold, "step: %+v", step)
	assert.Nil(t, step.Patch)
	assert.Equal(t, "Waiting for Longhorn to remove the extra copies: the volume has 2 and keeps 1. Longhorn's data locality cleanup "+
		"waits for a copy on the server that holds the volume, and the volume has none.", step.Message)
}

// A server whose Longhorn node refuses copies is named as such, not as a
// server without room. The disks come from Longhorn's node objects the way
// Kipper reads them, and a node condition is named with Longhorn's reason.
func TestAServerThatRefusesCopiesIsNamed(t *testing.T) {
	condition := func(kind, reason string) func(*unstructured.Unstructured) {
		return func(n *unstructured.Unstructured) {
			var conditions []interface{}
			for _, k := range []string{"Ready", "Schedulable"} {
				c := map[string]interface{}{"type": k, "status": "True"}
				if k == kind {
					c["status"], c["reason"] = "False", reason
				}
				conditions = append(conditions, c)
			}
			_ = unstructured.SetNestedSlice(n.Object, conditions, "status", "conditions")
		}
	}
	spec := func(fields map[string]interface{}) func(*unstructured.Unstructured) {
		return func(n *unstructured.Unstructured) {
			for field, value := range fields {
				_ = unstructured.SetNestedField(n.Object, value, "spec", field)
			}
		}
	}
	for name, tc := range map[string]struct {
		change func(*unstructured.Unstructured)
		want   string
	}{
		"cordoned":           {condition("Schedulable", "KubernetesNodeCordoned"), "the server server-1 is cordoned"},
		"manager down":       {condition("Ready", "ManagerPodDown"), "Longhorn reports the server's node not Ready (ManagerPodDown)"},
		"scheduling off":     {spec(map[string]interface{}{"allowScheduling": false}), "the server's Longhorn node does not allow scheduling"},
		"eviction requested": {spec(map[string]interface{}{"allowScheduling": false, "evictionRequested": true}), "eviction is requested for the server's Longhorn node"},
		"disk evicted": {func(n *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(n.Object, true, "spec", "disks", "disk-1", "evictionRequested")
		}, "no disk of the server's Longhorn node is ready and allows scheduling without an eviction request"},
	} {
		t.Run(name, func(t *testing.T) {
			v := convVolume(2, replicaOnly, healthyReplica("r-w", "worker-1"), healthyReplica("r-w2", "worker-2"))
			server := lhDiskNode("server-1", 90*gib, platform.ServerStorageTag, platform.ReplicaStorageTag)
			tc.change(server)
			c := convCluster(t)
			c.nodes = []unstructured.Unstructured{*server}
			for _, worker := range []string{"worker-1", "worker-2", "worker-3"} {
				c.nodes = append(c.nodes, *lhDiskNode(worker, 90*gib, platform.ReplicaStorageTag))
			}
			c.Disks = longhorncap.SchedulingDisks(c.nodes, longhorncap.Settings{OverProvisioning: 100, MinimalAvailable: 25})
			require.NotEmpty(t, c.Disks)

			step := planConversion(1, v, c, convNow)

			assert.Equal(t, stepBlocked, step.Kind, "step: %+v", step)
			assert.Equal(t, "Longhorn takes no new copy on the server now, because "+tc.want+
				", so Kipper keeps every copy. The change continues once the server takes copies again.", step.Message)
		})
	}
}

// Canceling a reduction after the count was lowered restores the requested
// count and disables data locality in one patch, preserving the remaining copies.
func TestASetBackWhileTheCountIsLoweredRaisesItWithLocalityOff(t *testing.T) {
	v := convVolume(1, replicaOnly, healthyReplica("r-s", "server-1"), healthyReplica("r-w", "worker-1"))
	v.Locality = "best-effort"
	step := planConversion(2, v, convCluster(t), convNow)
	assert.False(t, step.Hold, "step: %+v", step)
	assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-replica"],"dataLocality":"disabled","replicaAutoBalance":"disabled","numberOfReplicas":2,"offlineRebuilding":"enabled"}}`,
		patchJSON(t, step))
}

// With the server's copy failed or unusable, a reduction builds or waits for a
// server copy and lowers no count.
func TestAReductionLowersNothingWithoutAServerCopyInSync(t *testing.T) {
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
		"server copy stale":     {server: stale, want: stepWait},
		"server node down":      {server: unreachable, down: true, want: stepBlocked},
		"server copy evicting":  {server: evicting, want: stepBlocked},
		"server copy not in RW": {server: healthyReplica("r-s", "server-1"), want: stepWait},
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
			if step.Kind == stepPatch {
				assert.Equal(t, serverOnly, step.Patch.Spec.NodeSelector, "only a server copy may be built")
				assert.Equal(t, "disabled", step.Patch.Spec.DataLocality)
			}
		})
	}
}

// Longhorn deletes an eviction-requested replica once the healthy
// count exceeds the spec count, so nothing is lowered meanwhile.
func TestNoReductionWhileAReplicaIsEvicted(t *testing.T) {
	worker := healthyReplica("r-w", "worker-1")
	worker.EvictionRequested = true
	v := convVolume(2, replicaOnly, healthyReplica("r-s", "server-1"), worker)
	step := planConversion(1, v, convCluster(t), convNow)
	assert.Equal(t, stepBlocked, step.Kind)
	assert.Equal(t, "Longhorn is evicting the copy on worker-1, because the node is cordoned or drained, or an eviction was requested for its Longhorn node or disk. "+
		"The change continues once the eviction ends: uncordon worker-1, or cancel the eviction request in Longhorn.", step.Message)

	v.Count = 1
	assert.Equal(t, stepBlocked, planConversion(1, v, convCluster(t), convNow).Kind)
}

// Removing a copy or lowering the count waits for the volume to be held,
// and changes nothing before.
func TestAReductionAsksForTheHoldFirst(t *testing.T) {
	both := func() *lhVolume {
		return convVolume(2, replicaOnly, healthyReplica("r-s", "server-1"), healthyReplica("r-w", "worker-1"))
	}
	for name, change := range map[string]func(v *lhVolume){
		"detached":               func(v *lhVolume) { v.State, v.Modes, v.Held = "detached", nil, false },
		"attached by a pod":      func(v *lhVolume) { v.FrontendDisabled, v.Attachment, v.Held = false, nil, false },
		"count above the copies": func(v *lhVolume) { v.Replicas, v.Held = v.Replicas[:1], false },
		"locality on":            func(v *lhVolume) { v.Locality, v.Held = "best-effort", false },
	} {
		t.Run(name, func(t *testing.T) {
			v := both()
			change(v)
			step := planConversion(1, v, convCluster(t), convNow)
			assert.Equal(t, stepHold, step.Kind, "step: %+v", step)
			assert.True(t, step.Hold)
			assert.Nil(t, step.Patch)
		})
	}
}

// The held engine compared the copies' revision counters when it started and
// marks a copy that lags behind ERR. Nothing is removed or lowered until
// Longhorn has rebuilt it and every copy is RW again.
func TestAHeldReductionWaitsUntilEveryCopyIsRW(t *testing.T) {
	for _, mode := range []string{"ERR", "WO", ""} {
		v := convVolume(1, replicaOnly, healthyReplica("r-s", "server-1"), healthyReplica("r-w", "worker-1"))
		v.Modes["r-w"] = mode
		step := planConversion(1, v, convCluster(t), convNow)
		assert.Equal(t, stepWait, step.Kind, "mode %q: %+v", mode, step)
		assert.Equal(t, kipperv1.StorageCopiesConversionPending, step.Reason, "the copies may still be set back")
		assert.Contains(t, step.Message, "(1 of 2 copies in sync)")
		assert.True(t, step.Hold)
	}
}

// A failed copy on a node that cannot report was not compared by the held
// engine, so it blocks the reduction like a present one.
func TestAFailedCopyOnAnUnavailableNodeBlocksAReduction(t *testing.T) {
	c := convCluster(t)
	c.Available["worker-1"] = false
	failed := lhReplica{Name: "r-w", Node: "worker-1", FailedAt: "2026-10-24T11:00:00Z"}
	v := convVolume(2, replicaOnly, healthyReplica("r-s", "server-1"), failed)
	step := planConversion(1, v, c, convNow)
	assert.Equal(t, stepBlocked, step.Kind, "step: %+v", step)
	assert.Contains(t, step.Message, "kip node fence worker-1")
	assert.Nil(t, step.Patch)
}

// Builds and placement changes run live, so they never ask for the hold.
func TestABuildIsNoReductionStep(t *testing.T) {
	v := convVolume(1, serverOnly, healthyReplica("r-s", "server-1"))
	v.Held = false
	step := planConversion(2, v, convCluster(t), convNow)
	assert.Equal(t, leaseTake, step.Lease)
	assert.False(t, step.Hold)
}

// A single copy on a worker moves to the server when copies 1 is set: build
// the server copy, lower the count, and Longhorn removes the worker copy.
func TestCopiesOneMovesAWorkerOnlyCopyToTheServer(t *testing.T) {
	c := convCluster(t)
	worker := healthyReplica("r-w", "worker-1")
	v := convVolume(1, nil, worker)

	step := planConversion(1, v, c, convNow)
	assert.JSONEq(t, `{"metadata":{"annotations":{"kipper.run/copies-rebuild-started":"2026-10-24T12:00:00Z"}},
		"spec":{"nodeSelector":["kipper-server"],"dataLocality":"disabled","replicaAutoBalance":"disabled","numberOfReplicas":2}}`,
		patchJSON(t, step))
	assert.Equal(t, kipperv1.StorageCopiesConversionPending, step.Reason, "the service is stopped, so the copies may still be set back")

	v.Count, v.Selector, v.RebuildStarted = 2, serverOnly, convNow
	v.Replicas = append(v.Replicas, healthyReplica("r-s", "server-1"))
	v.Modes["r-s"] = "RW"
	assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-server"],"dataLocality":"best-effort","replicaAutoBalance":"disabled","numberOfReplicas":1}}`,
		patchJSON(t, planConversion(1, v, c, convNow)))

	v.Count, v.Locality = 1, "best-effort"
	assert.Equal(t, stepWait, planConversion(1, v, c, convNow).Kind)

	v.Replicas = v.Replicas[1:]
	assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-server"],"dataLocality":"disabled","replicaAutoBalance":"disabled"}}`,
		patchJSON(t, planConversion(1, v, c, convNow)))

	v.Locality = "disabled"
	assert.JSONEq(t, `{"metadata":{"annotations":{"kipper.run/copies-rebuild-started":null}}}`,
		patchJSON(t, planConversion(1, v, c, convNow)), "done clears the annotation")
}

// Reducing three worker copies to two builds a server copy under a temporary
// server-only selector, lowers the count, waits while Longhorn removes worker
// copies and then restores the copies-2 selector.
func TestThreeCopiesToTwoStagesThroughAServerCopy(t *testing.T) {
	c := convCluster(t)
	v := convVolume(3, replicaOnly, healthyReplica("r-1", "worker-1"), healthyReplica("r-2", "worker-2"), healthyReplica("r-3", "worker-3"))

	step := planConversion(2, v, c, convNow)
	assert.Equal(t, int64(4), *patchStepSpec(t, step).NumberOfReplicas)
	assert.Equal(t, serverOnly, step.Patch.Spec.NodeSelector)

	v.Count, v.Selector, v.RebuildStarted = 4, serverOnly, convNow
	v.Replicas = append(v.Replicas, healthyReplica("r-s", "server-1"))
	v.Modes["r-s"] = "RW"
	assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-server"],"dataLocality":"best-effort","replicaAutoBalance":"disabled","numberOfReplicas":2}}`,
		patchJSON(t, planConversion(2, v, c, convNow)), "the server-only selector stays while Longhorn removes copies")

	v.Count, v.Locality = 2, "best-effort"
	assert.Contains(t, planConversion(2, v, c, convNow).Message, "the volume has 4 and keeps 2")

	v.Replicas = v.Replicas[2:]
	assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-server"],"dataLocality":"disabled","replicaAutoBalance":"disabled"}}`,
		patchJSON(t, planConversion(2, v, c, convNow)))

	v.Locality = "disabled"
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

func TestAReductionWithTooFewHealthyKeepersIsBlocked(t *testing.T) {
	c := convCluster(t)
	c.Available["worker-1"] = false
	c.Available["worker-2"] = false
	v := convVolume(2, replicaOnly, healthyReplica("r-s", "server-1"), healthyReplica("r-1", "worker-1"), healthyReplica("r-2", "worker-2"))
	step := planConversion(2, v, c, convNow)
	assert.Equal(t, stepBlocked, step.Kind, "a blocked state lets the copies be set back, a converting one would not")
	assert.Nil(t, step.Patch)
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
	v.Locality, v.Held = "best-effort", false
	assert.JSONEq(t, `{"spec":{"nodeSelector":["kipper-replica"],"dataLocality":"disabled","replicaAutoBalance":"disabled","offlineRebuilding":"enabled"}}`,
		patchJSON(t, planConversion(2, v, convCluster(t), convNow)))
}

func TestAVolumeAtItsTargetIsDone(t *testing.T) {
	v := convVolume(2, replicaOnly, healthyReplica("r-s", "server-1"), healthyReplica("r-w", "worker-1"))
	v.OfflineRebuilding = "enabled"
	assert.Equal(t, conversionStep{Kind: stepDone}, planConversion(2, v, convCluster(t), convNow))
}

// A rebuild patch never lowers the count. A spec count above the one asked
// for is lowered under the reduction's rules.
func TestARebuildNeverLowersTheCount(t *testing.T) {
	failed := lhReplica{Name: "r-f", Node: "worker-2", FailedAt: "2026-10-24T11:00:00Z"}
	c := convCluster(t)
	v := convVolume(3, replicaOnly, healthyReplica("r-s", "server-1"), failed)
	step := planConversion(2, v, c, convNow)
	assert.Equal(t, int64(2), *patchSpec(t, step).NumberOfReplicas)
	assert.NotEqual(t, leaseTake, step.Lease)
	assert.Nil(t, step.Patch.Metadata, "a decrease starts no rebuild")
	assert.Equal(t, v.Selector, step.Patch.Spec.NodeSelector)

	evicted := healthyReplica("r-e", "worker-3")
	evicted.EvictionRequested = true
	v.Replicas = append(v.Replicas, evicted)
	v.Modes["r-e"] = "RW"
	v.Count = 4
	assert.Equal(t, stepBlocked, planConversion(2, v, c, convNow).Kind, "no decrease while a replica is evicted")

	v = convVolume(3, replicaOnly, healthyReplica("r-s", "server-1"), failed)
	v.State, v.Modes, v.Held = "detached", nil, false
	assert.Equal(t, stepHold, planConversion(2, v, c, convNow).Kind)

	server := convVolume(4, replicaOnly, healthyReplica("r-1", "worker-1"), healthyReplica("r-2", "worker-2"), failed)
	server.Locality = "best-effort"
	step = planConversion(1, server, c, convNow)
	assert.Equal(t, leaseTake, step.Lease, "step: %+v", step)
	assert.Equal(t, int64(4), *patchSpec(t, step).NumberOfReplicas, "the server copy is built without lowering the count")
	assert.Equal(t, "disabled", step.Patch.Spec.DataLocality)
}

func patchSpec(t *testing.T, step conversionStep) *volumePatchSpec {
	t.Helper()
	require.Equal(t, stepPatch, step.Kind, "step: %+v", step)
	require.NotNil(t, step.Patch.Spec)
	return step.Patch.Spec
}

// The progress line counts healthy copies the way the StorageCopies status
// does, so a salvaged copy that serves writes again counts while Longhorn
// builds the new copy from it.
func TestTheBuildProgressCountsASalvagedCopyServingAgain(t *testing.T) {
	building := lhReplica{Name: "r-w", UID: "uid-r-w", ResourceVersion: "7", Node: "worker-1", CurrentState: "running"}
	v := convVolume(2, replicaOnly, salvagedRW("r-s", "server-1"), building)
	v.Modes["r-w"] = "WO"
	v.RebuildStarted = convNow.Add(-time.Minute)

	step := planConversion(2, v, convCluster(t), convNow)

	assert.Equal(t, stepWait, step.Kind, "step: %+v", step)
	assert.Equal(t, "Longhorn is building a copy of the volume (1 of 2 copies healthy).", step.Message)
}

// No count is lowered while a present copy is on a node that is not Ready or
// is fenced, including the decrease before a server copy is built and the
// final count adjustment. Placement alone still changes.
func TestNoCountIsLoweredWhileACopyIsOnAnUnavailableNode(t *testing.T) {
	c := convCluster(t)
	c.Available["worker-1"] = false

	noServer := convVolume(3, replicaOnly, healthyReplica("r-w", "worker-1"))
	noServer.Modes = nil
	step := planConversion(1, noServer, c, convNow)
	assert.Equal(t, stepBlocked, step.Kind, "the decrease before a server copy is built: %+v", step)
	assert.Contains(t, step.Message, "on worker-1, which is not Ready or is fenced")

	final := convVolume(3, replicaOnly, healthyReplica("r-s", "server-1"), healthyReplica("r-w", "worker-1"))
	step = planConversion(2, final, c, convNow)
	assert.Equal(t, stepBlocked, step.Kind, "the final count adjustment: %+v", step)
	assert.Contains(t, step.Message, "on worker-1, which is not Ready or is fenced")

	placement := convVolume(2, serverOnly, healthyReplica("r-s", "server-1"), healthyReplica("r-w", "worker-1"))
	step = planConversion(2, placement, c, convNow)
	require.Equal(t, stepPatch, step.Kind, "a placement change deletes nothing: %+v", step)
	assert.Nil(t, step.Patch.Spec.NumberOfReplicas)
}

// The count is lowered only while every copy is in sync: running, RW and on an
// available node. A server copy that is RW but not running keeps the count.
func TestTheCountIsLoweredOnlyWithEveryCopyInSync(t *testing.T) {
	stopped := healthyReplica("r-s", "server-1")
	stopped.CurrentState = "stopped"
	v := convVolume(2, replicaOnly, stopped, healthyReplica("r-w", "worker-1"))
	step := planConversion(1, v, convCluster(t), convNow)
	assert.Equal(t, stepWait, step.Kind, "step: %+v", step)

	v.Replicas[0].CurrentState = "running"
	assert.Equal(t, int64(1), *patchSpec(t, planConversion(1, v, convCluster(t), convNow)).NumberOfReplicas)
}

// salvagedRW is a replica Longhorn salvaged at 11:00 that has been RW since.
func salvagedRW(name, node string) lhReplica {
	r := healthyReplica(name, node)
	r.LastFailedAt = "2026-10-24T11:00:00Z"
	r.LastHealthyAt = "2026-10-24T11:00:05Z"
	return r
}

// With a copy on each of two servers, the count is lowered as on one server,
// and Longhorn removes the copy away from the server that holds the volume.
func TestOnTwoServersTheCopyAwayFromTheHoldGoes(t *testing.T) {
	c := convCluster(t)
	c.Servers["server-2"], c.Available["server-2"], c.Eligible["server-2"] = true, true, true
	v := convVolume(2, serverOnly, healthyReplica("r-s", "server-1"), healthyReplica("r-s2", "server-2"))
	step := planConversion(1, v, c, convNow)
	assert.Equal(t, "best-effort", patchSpec(t, step).DataLocality)
	assert.Contains(t, step.Message, "keeps the copy on server-1")
}
