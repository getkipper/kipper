package installer

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// longhornState answers the reads LastCopiesWithoutRoom and
// CopiesWithoutRebuildRoom make.
type longhornState struct {
	nodes    string
	volumes  string
	settings string
	replicas string
}

func (s longhornState) Run(cmd string) (string, error) {
	switch {
	case strings.Contains(cmd, "nodes.longhorn.io"):
		return s.nodes, nil
	case strings.Contains(cmd, "volumes.longhorn.io"):
		return s.volumes, nil
	case strings.Contains(cmd, "settings.longhorn.io"):
		return s.settings, nil
	case strings.Contains(cmd, "replicas.longhorn.io"):
		return s.replicas, nil
	}
	return "", fmt.Errorf("unexpected command %q", cmd)
}

func (s longhornState) RunStdin(cmd string, _ io.Reader) (string, error) { return s.Run(cmd) }

const gi = int64(1) << 30

// longhornNodeJSON renders a Longhorn node with one disk of 40 GiB.
func longhornNodeJSON(name string, tags string, diskSchedulable string, available, scheduled int64) string {
	return longhornNodeWith(name, tags, `[{"type":"Ready","status":"True"},{"type":"Schedulable","status":"True"}]`, `[]`, diskSchedulable, available, scheduled)
}

func longhornNodeWith(name, tags, conditions, diskTags, diskSchedulable string, available, scheduled int64) string {
	return fmt.Sprintf(`{"metadata":{"name":%q},"spec":{"allowScheduling":true,"tags":[%s],"disks":{"default-disk":{"allowScheduling":true,"storageReserved":%d,"tags":%s}}},`+
		`"status":{"conditions":%s,`+
		`"diskStatus":{"default-disk":{"conditions":[{"type":"Ready","status":"True"},{"type":"Schedulable","status":%q,"message":"the disk is full"}],"storageAvailable":%d,"storageMaximum":%d,"storageScheduled":%d}}}}`,
		name, tags, 12*gi, diskTags, conditions, diskSchedulable, available, 40*gi, scheduled)
}

func longhornList(items ...string) string {
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

const pinnedVolume = `{"items":[{"metadata":{"name":"pvc-db"},"spec":{"size":"5368709120","nodeSelector":["kipper-server"],"diskSelector":[]},"status":{"actualSize":1073741824}}]}`

var dbLastCopy = []NodeVolume{{Name: "pvc-db", Claim: "shop/data-db-0", Here: 1}}

func TestLastCopiesWithoutRoomAcceptsAServerWithSpace(t *testing.T) {
	s := longhornState{
		nodes:   longhornList(longhornNodeJSON("server", `"kipper-server"`, "True", 30*gi, 10*gi), longhornNodeJSON("worker-1", "", "True", 30*gi, 0)),
		volumes: pinnedVolume,
	}

	blocked, err := LastCopiesWithoutRoom(s, "worker-1", dbLastCopy)

	require.NoError(t, err)
	assert.Empty(t, blocked)
}

// TestLastCopiesWithoutRoomNamesAnUnschedulableDisk covers a full server disk.
// The volume's node selector rules out the other worker, leaving no destination.
func TestLastCopiesWithoutRoomNamesAnUnschedulableDisk(t *testing.T) {
	s := longhornState{
		nodes: longhornList(
			longhornNodeJSON("server", `"kipper-server"`, "False", 2*gi, 10*gi),
			longhornNodeJSON("worker-1", "", "True", 30*gi, 0),
			longhornNodeJSON("worker-2", "", "True", 30*gi, 0)),
		volumes: pinnedVolume,
	}

	blocked, err := LastCopiesWithoutRoom(s, "worker-1", dbLastCopy)

	require.NoError(t, err)
	require.Len(t, blocked, 1)
	assert.Contains(t, blocked[0], "shop/data-db-0")
	assert.Contains(t, blocked[0], "server: disk default-disk is not schedulable: the disk is full")
	assert.Contains(t, blocked[0], "worker-2: lacks the node tag kipper-server")
	assert.NotContains(t, blocked[0], "worker-1:", "the node being drained is no destination")
}

func TestLastCopiesWithoutRoomChecksLonghornsSpaceRules(t *testing.T) {
	for name, tc := range map[string]struct {
		available, scheduled int64
		settings             string
		want                 string
	}{
		"minimal available": {available: 10 * gi, want: "Longhorn keeps 25% of its 40.0 GiB free"},
		"custom minimal":    {available: 20 * gi, settings: "storage-minimal-available-percentage\t50\n", want: "Longhorn keeps 50%"},
		"over-provisioning": {available: 30 * gi, scheduled: 25 * gi, want: "has 25.0 GiB scheduled of the 28.0 GiB it may hold, too little for 5.0 GiB"},
	} {
		s := longhornState{
			nodes:    longhornList(longhornNodeJSON("server", `"kipper-server"`, "True", tc.available, tc.scheduled)),
			volumes:  pinnedVolume,
			settings: tc.settings,
		}

		blocked, err := LastCopiesWithoutRoom(s, "worker-1", dbLastCopy)

		require.NoError(t, err, name)
		require.Len(t, blocked, 1, name)
		assert.Contains(t, blocked[0], tc.want, name)
	}
}

func TestLastCopiesWithoutRoomNeedsAnotherNode(t *testing.T) {
	s := longhornState{nodes: longhornList(longhornNodeJSON("worker-1", "", "True", 30*gi, 0)), volumes: pinnedVolume}

	blocked, err := LastCopiesWithoutRoom(s, "worker-1", dbLastCopy)

	require.NoError(t, err)
	require.Len(t, blocked, 1)
	assert.Contains(t, blocked[0], "there is no other Longhorn node")
}

// TestLastCopiesWithoutRoomRequiresAReadySchedulableNode covers missing, false
// and unknown conditions, since both Ready and Schedulable must be True.
func TestLastCopiesWithoutRoomRequiresAReadySchedulableNode(t *testing.T) {
	for want, conditions := range map[string]string{
		"not ready":                      `[{"type":"Schedulable","status":"True"}]`,
		"not schedulable: Node cordoned": `[{"type":"Ready","status":"True"},{"type":"Schedulable","status":"False","message":"Node cordoned"}]`,
		"not ready: kubelet down":        `[{"type":"Ready","status":"Unknown","message":"kubelet down"},{"type":"Schedulable","status":"True"}]`,
	} {
		s := longhornState{nodes: longhornList(longhornNodeWith("server", `"kipper-server"`, conditions, `[]`, "True", 30*gi, 0)), volumes: pinnedVolume}

		blocked, err := LastCopiesWithoutRoom(s, "worker-1", dbLastCopy)

		require.NoError(t, err, want)
		require.Len(t, blocked, 1, want)
		assert.Contains(t, blocked[0], "server: "+want)
	}
}

const unpinnedVolume = `{"items":[{"metadata":{"name":"pvc-db"},"spec":{"size":"5368709120"},"status":{"actualSize":1073741824}}]}`

func TestLastCopiesWithoutRoomHonorsTheEmptySelectorSettings(t *testing.T) {
	ready := `[{"type":"Ready","status":"True"},{"type":"Schedulable","status":"True"}]`
	tagged := longhornList(longhornNodeWith("server", `"kipper-server"`, ready, `["ssd"]`, "True", 30*gi, 0))
	for settings, want := range map[string]string{
		"": "",
		"allow-empty-node-selector-volume\tfalse\n": "is tagged, and Longhorn keeps volumes without a node selector off tagged nodes",
		"allow-empty-disk-selector-volume\tfalse\n": "disk default-disk is tagged, and Longhorn keeps volumes without a disk selector off tagged disks",
	} {
		s := longhornState{nodes: tagged, volumes: unpinnedVolume, settings: settings}

		blocked, err := LastCopiesWithoutRoom(s, "worker-1", dbLastCopy)

		require.NoError(t, err, settings)
		if want == "" {
			assert.Empty(t, blocked, "Longhorn's default lets volumes without selectors use tagged nodes and disks")
			continue
		}
		require.Len(t, blocked, 1, settings)
		assert.Contains(t, blocked[0], want)
	}
}

func TestLastCopiesWithoutRoomSkipsANodeBeingDeleted(t *testing.T) {
	deleting := strings.Replace(longhornNodeJSON("server", `"kipper-server"`, "True", 30*gi, 0), `"name":"server"`, `"name":"server","deletionTimestamp":"2026-10-20T01:00:00Z"`, 1)
	s := longhornState{nodes: longhornList(deleting), volumes: pinnedVolume}

	blocked, err := LastCopiesWithoutRoom(s, "worker-1", dbLastCopy)

	require.NoError(t, err)
	require.Len(t, blocked, 1)
	assert.Contains(t, blocked[0], "server: is being deleted")
}

const replicatedVolume = `{"items":[{"metadata":{"name":"pvc-db"},"spec":{"size":"5368709120","nodeSelector":["kipper-replica"],"diskSelector":[]},"status":{"actualSize":1073741824}}]}`

var dbSecondCopy = []NodeVolume{{Name: "pvc-db", Claim: "shop/data-db-0", Here: 1, Elsewhere: 1}}

func TestCopiesWithoutRebuildRoomNamesACopyNoOtherNodeCanTake(t *testing.T) {
	s := longhornState{
		nodes: longhornList(
			longhornNodeJSON("server", `"kipper-server","kipper-replica"`, "True", 30*gi, 0),
			longhornNodeJSON("worker-1", `"kipper-replica"`, "True", 30*gi, 0),
			longhornNodeJSON("worker-2", "", "True", 30*gi, 0)),
		volumes:  replicatedVolume,
		replicas: "pvc-db\tserver\t2026-10-24T01:00:00Z\t\t\npvc-db\tworker-1\t2026-10-24T01:00:00Z\t\t\n",
	}

	blocked, err := CopiesWithoutRebuildRoom(s, "worker-1", dbSecondCopy)

	require.NoError(t, err)
	require.Len(t, blocked, 1)
	assert.Contains(t, blocked[0], "shop/data-db-0")
	assert.Contains(t, blocked[0], "server: already holds a copy")
	assert.Contains(t, blocked[0], "worker-2: lacks the node tag kipper-replica")
}

func TestCopiesWithoutRebuildRoomAcceptsAThirdNodeThatCanHoldCopies(t *testing.T) {
	s := longhornState{
		nodes: longhornList(
			longhornNodeJSON("server", `"kipper-server","kipper-replica"`, "True", 30*gi, 0),
			longhornNodeJSON("worker-1", `"kipper-replica"`, "True", 30*gi, 0),
			longhornNodeJSON("worker-2", `"kipper-replica"`, "True", 30*gi, 0)),
		volumes:  replicatedVolume,
		replicas: "pvc-db\tserver\t2026-10-24T01:00:00Z\t\t\npvc-db\tworker-1\t2026-10-24T01:00:00Z\t\t\n",
	}

	blocked, err := CopiesWithoutRebuildRoom(s, "worker-1", dbSecondCopy)

	require.NoError(t, err)
	assert.Empty(t, blocked)
}

func TestCopiesWithoutRebuildRoomLeavesLastCopiesToTheDrainCheck(t *testing.T) {
	blocked, err := CopiesWithoutRebuildRoom(longhornState{}, "worker-1", dbLastCopy)

	require.NoError(t, err, "a last copy is LastCopiesWithoutRoom's, and no command runs")
	assert.Empty(t, blocked)
}
