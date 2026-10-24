package longhorncap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const gib = int64(1) << 30

// node builds a Longhorn node with one filesystem disk of 100 GiB.
func node(name string, tags []interface{}, reserved, scheduled, available int64) unstructured.Unstructured {
	maximum := 100 * gib
	return unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": name},
		"spec": map[string]interface{}{
			"allowScheduling": true,
			"tags":            tags,
			"disks": map[string]interface{}{"disk-1": map[string]interface{}{
				"allowScheduling": true, "storageReserved": reserved, "diskType": "filesystem",
			}},
		},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": "True"},
				map[string]interface{}{"type": "Schedulable", "status": "True"},
			},
			"diskStatus": map[string]interface{}{"disk-1": map[string]interface{}{
				"storageMaximum": maximum, "storageScheduled": scheduled, "storageAvailable": available,
				"conditions": []interface{}{map[string]interface{}{"type": "Schedulable", "status": "True"}},
			}},
		},
	}}
}

var defaults = Settings{OverProvisioning: 100, MinimalAvailable: 25}

func TestRoomFollowsLonghornsTwoDiskRules(t *testing.T) {
	disks := SchedulingDisks([]unstructured.Unstructured{node("n", nil, 10*gib, 20*gib, 80*gib)}, defaults)
	if assert.Len(t, disks, 1) {
		assert.Equal(t, 70*gib, disks[0].Room, "(100 - 10) * 100% - 20 scheduled")
	}
	full := SchedulingDisks([]unstructured.Unstructured{node("n", nil, 0, 0, 25*gib)}, defaults)
	if assert.Len(t, full, 1) {
		assert.Zero(t, full[0].Room, "available must exceed the minimal share, strictly")
	}
}

// IsSchedulableToDisk in longhorn-manager v1.7.3 and v1.11.3: available minus
// the volume's actual size must exceed the minimal share of the maximum, and
// the nominal size plus scheduled space must fit the over-provisioned room.
func TestTakesChecksActualAndNominalSizeSeparately(t *testing.T) {
	disk := SchedulingDisks([]unstructured.Unstructured{node("n", nil, 0, 60*gib, 40*gib)}, defaults)[0]
	assert.True(t, disk.Takes(40*gib, 10*gib), "40 - 10 > 25 and 40 + 60 <= 100")
	assert.False(t, disk.Takes(40*gib, 15*gib), "40 - 15 is not more than 25")
	assert.False(t, disk.Takes(41*gib, 1*gib), "41 + 60 > 100")
	over := SchedulingDisks([]unstructured.Unstructured{node("n", nil, 0, 60*gib, 40*gib)}, Settings{OverProvisioning: 200, MinimalAvailable: 25})[0]
	assert.True(t, over.Takes(140*gib, 1*gib), "over-provisioning lets the nominal size exceed the disk")
}

func TestSchedulingDisksSkipNodesAndDisksLonghornWouldNot(t *testing.T) {
	unschedulable := node("off", nil, 0, 0, 100*gib)
	unschedulable.Object["spec"].(map[string]interface{})["allowScheduling"] = false
	evicting := node("evicting", nil, 0, 0, 100*gib)
	evicting.Object["spec"].(map[string]interface{})["disks"].(map[string]interface{})["disk-1"].(map[string]interface{})["evictionRequested"] = true
	disks := SchedulingDisks([]unstructured.Unstructured{unschedulable, evicting, node("ok", []interface{}{"kipper-replica"}, 0, 0, 100*gib)}, defaults)
	if assert.Len(t, disks, 1) {
		assert.Equal(t, "ok", disks[0].Node)
		assert.Equal(t, []string{"kipper-replica"}, disks[0].Tags)
	}
}

func TestNodesThatTakeAVolumeMatchTheSelector(t *testing.T) {
	disks := SchedulingDisks([]unstructured.Unstructured{
		node("server-1", []interface{}{"kipper-server", "kipper-replica"}, 0, 0, 100*gib),
		node("worker-1", []interface{}{"kipper-replica"}, 0, 95*gib, 100*gib),
		node("worker-2", []interface{}{"kipper-replica"}, 0, 0, 100*gib),
		node("worker-3", nil, 0, 0, 100*gib),
	}, defaults)
	assert.Equal(t, []string{"server-1", "worker-2"}, NodesThatTake(disks, []string{"kipper-replica"}, 10*gib, 1*gib),
		"worker-1 lacks scheduled room, worker-3 lacks the tag")
	assert.Equal(t, []string{"server-1"}, NodesThatTake(disks, []string{"kipper-server"}, 10*gib, 1*gib))
}

// A server with 6.8 GB left and a roomy worker: a 4 GiB volume with two copies
// puts one copy on each node, so its server copy and a 4 GiB single-copy dump
// pinned to the server need 8 GiB there.
func TestShortfallsCountACopyOnEachNode(t *testing.T) {
	disks := []Disk{{Node: "server-1", Room: 6_800_000_000}, {Node: "worker-1", Room: 500 * gib}}
	data := Demand{Bytes: 4 * gib, Copies: 2, Disks: []int{0, 1}}
	dump := Demand{Bytes: 4 * gib, Copies: 1, Disks: []int{0}}

	short := Shortfalls(disks, []Demand{data, dump})

	if assert.Len(t, short, 1) {
		assert.Equal(t, []string{"server-1"}, short[0].Nodes)
		assert.Equal(t, int64(6_800_000_000), short[0].Room)
		assert.Equal(t, 8*gib, short[0].Need)
		assert.Equal(t, []int{1, 1}, short[0].Copies)
	}

	data.Bytes, dump.Bytes = 3*gib, 3*gib
	assert.Empty(t, Shortfalls(disks, []Demand{data, dump}), "6 GiB fit in 6.8 GB")
}

func TestShortfallsCountBytesTimesCopies(t *testing.T) {
	disks := []Disk{{Node: "server-1", Room: 10 * gib}, {Node: "worker-1", Room: 10 * gib}, {Node: "worker-2", Room: 10 * gib}}
	all := []int{0, 1, 2}
	demands := []Demand{{Bytes: 6 * gib, Copies: 2, Disks: all}, {Bytes: 5 * gib, Copies: 2, Disks: all}, {Bytes: 10 * gib, Copies: 1, Disks: all}}

	short := Shortfalls(disks, demands)

	if assert.Len(t, short, 1, "no single node is forced, only the 32 GiB total is too much for 30") {
		assert.Equal(t, []string{"server-1", "worker-1", "worker-2"}, short[0].Nodes)
		assert.Equal(t, 32*gib, short[0].Need)
		assert.Equal(t, []int{2, 2, 1}, short[0].Copies)
	}
}

// Shortfalls counts at most one copy per node. The node has enough aggregate
// room for that copy even though neither disk can hold it.
func TestShortfallsPutOneCopyOnEachEligibleNodeAtMost(t *testing.T) {
	disks := []Disk{{Node: "server-1", Room: 5 * gib}, {Node: "server-1", Room: 5 * gib}}
	assert.Empty(t, Shortfalls(disks, []Demand{{Bytes: 6 * gib, Copies: 2, Disks: []int{0, 1}}}))
}
