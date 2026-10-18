package installer

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// drainHost matches command fragments, giving failures precedence over replies.
type drainHost struct {
	replies  map[string]string
	failures map[string]string
	commands []string
}

func (h *drainHost) Run(cmd string) (string, error) {
	h.commands = append(h.commands, cmd)
	for fragment, out := range h.failures {
		if strings.Contains(cmd, fragment) {
			return out, errors.New("exit status 1")
		}
	}
	for fragment, out := range h.replies {
		if strings.Contains(cmd, fragment) {
			return out, nil
		}
	}
	return "", nil
}

func (h *drainHost) RunStdin(cmd string, _ io.Reader) (string, error) { return h.Run(cmd) }

func TestVolumesOnNodeCountsHealthyReplicasHereAndElsewhere(t *testing.T) {
	h := &drainHost{replies: map[string]string{
		"replicas.longhorn.io": "pvc-db\tworker-1\t2026-10-16T01:00:00Z\t\n" +
			"pvc-loki\tworker-1\t2026-10-16T01:00:00Z\t\n" +
			"pvc-loki\tserver\t2026-10-16T01:00:00Z\t\n" +
			"pvc-loki\tworker-2\t2026-10-16T01:00:00Z\t2026-10-16T02:00:00Z\n" +
			"pvc-zot\tserver\t2026-10-16T01:00:00Z\t\n" +
			"pvc-new\tworker-1\t\t\n",
		"volumes.longhorn.io": "pvc-db\tshop\tdata-db-0\npvc-loki\tmonitoring\tstorage-loki-0\npvc-zot\tkipper-system\tzot-data\n",
	}}

	got, err := VolumesOnNode(h, "worker-1")
	require.NoError(t, err)
	assert.Equal(t, []NodeVolume{
		{Name: "pvc-loki", Claim: "monitoring/storage-loki-0", Here: 1, Elsewhere: 1},
		{Name: "pvc-db", Claim: "shop/data-db-0", Here: 1},
	}, got, "only volumes with healthy replicas on the requested node are included")
	assert.False(t, got[0].LastCopy())
	assert.True(t, got[1].LastCopy())
}

func TestVolumesOnNodeReportsUnreadableReplicas(t *testing.T) {
	h := &drainHost{failures: map[string]string{"replicas.longhorn.io": "forbidden"}}
	_, err := VolumesOnNode(h, "worker-1")
	require.Error(t, err)
}

func TestRequireEvictionDrainPolicy(t *testing.T) {
	ok := &drainHost{replies: map[string]string{"node-drain-policy": LonghornEvictionDrainPolicy}}
	require.NoError(t, RequireEvictionDrainPolicy(ok))

	blocking := &drainHost{replies: map[string]string{"node-drain-policy": "block-if-contains-last-replica"}}
	err := RequireEvictionDrainPolicy(blocking)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip upgrade")
}

func TestDrainNodeHonoursBudgetsAndBoundsTheWait(t *testing.T) {
	h := &drainHost{}
	require.NoError(t, DrainNode(h, "worker-1", 20*time.Minute))
	require.Len(t, h.commands, 1)
	cmd := h.commands[0]
	assert.Contains(t, cmd, "kubectl drain worker-1 --ignore-daemonsets --delete-emptydir-data --timeout=20m0s")
	assert.NotContains(t, cmd, "--force", "pods without a controller must stop the drain, not be deleted")
	assert.NotContains(t, cmd, "--disable-eviction", "disruption budgets protect replicas until eviction completes")
}

func TestDrainNodeNamesWhatCouldNotBeEvicted(t *testing.T) {
	h := &drainHost{failures: map[string]string{"kubectl drain": "node/worker-1 already cordoned\n" +
		"evicting pod longhorn-system/instance-manager-abc\n" +
		"error when evicting pods/\"instance-manager-abc\" -n \"longhorn-system\" (will retry after 5s): Cannot evict pod as it would violate the pod's disruption budget.\n" +
		"error: unable to drain node \"worker-1\" due to error: global timeout reached: 20m0s\n"}}

	err := DrainNode(h, "worker-1", 20*time.Minute)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `instance-manager-abc`)
	assert.Contains(t, err.Error(), "global timeout reached")
	assert.NotContains(t, err.Error(), "already cordoned")
}

func TestNodeCordoned(t *testing.T) {
	cordoned, err := NodeCordoned(&drainHost{replies: map[string]string{"spec.unschedulable": "true"}}, "worker-1")
	require.NoError(t, err)
	assert.True(t, cordoned)

	cordoned, err = NodeCordoned(&drainHost{}, "worker-1")
	require.NoError(t, err)
	assert.False(t, cordoned)
}

func TestDrainNodeKeepsFractionalTimeoutsAndRefusesUnboundedOnes(t *testing.T) {
	h := &drainHost{}
	require.NoError(t, DrainNode(h, "worker-1", 90500*time.Millisecond))
	assert.Contains(t, h.commands[0], "--timeout=1m30.5s")

	h = &drainHost{}
	require.Error(t, DrainNode(h, "worker-1", 500*time.Millisecond), "drain timeouts must be at least one second")
	assert.Empty(t, h.commands)
}

func TestLonghornVolumesReadsStateRobustnessAndReplicas(t *testing.T) {
	h := &drainHost{replies: map[string]string{
		"volumes.longhorn.io": "pvc-db\tshop\tdata-db-0\tattached\tdegraded\t2\n" +
			"pvc-old\t\t\tdetached\tunknown\t1\n",
	}}

	got, err := LonghornVolumes(h)
	require.NoError(t, err)
	assert.Equal(t, []VolumeHealth{
		{Name: "pvc-db", Claim: "shop/data-db-0", State: "attached", Robustness: "degraded", Replicas: 2},
		{Name: "pvc-old", State: "detached", Robustness: "unknown", Replicas: 1},
	}, got)
	assert.True(t, got[0].Unhealthy())
	assert.False(t, got[1].Unhealthy(), "a detached volume has no robustness to protect")
}

func TestVolumeHealthTreatsAnyAttachedRobustnessButHealthyAsUnhealthy(t *testing.T) {
	for _, robustness := range []string{"degraded", "faulted", "unknown", ""} {
		assert.True(t, VolumeHealth{State: "attached", Robustness: robustness}.Unhealthy(), robustness)
	}
	assert.False(t, VolumeHealth{State: "attached", Robustness: "healthy"}.Unhealthy())
}

func TestLonghornVolumesRefusesOutputItCannotParse(t *testing.T) {
	_, err := LonghornVolumes(&drainHost{replies: map[string]string{"volumes.longhorn.io": "pvc-db\tshop\tdata-db-0\tattached\tdegraded\tmany\n"}})
	require.Error(t, err)

	_, err = LonghornVolumes(&drainHost{replies: map[string]string{"volumes.longhorn.io": "pvc-db\tattached\n"}})
	require.Error(t, err)

	_, err = LonghornVolumes(&drainHost{failures: map[string]string{"volumes.longhorn.io": "forbidden"}})
	require.Error(t, err)
}

func TestNodeCount(t *testing.T) {
	n, err := NodeCount(&drainHost{replies: map[string]string{"kubectl get nodes": "node/server\nnode/worker-1\n"}})
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	n, err = NodeCount(&drainHost{replies: map[string]string{"kubectl get nodes": "Warning: v1 ComponentStatus is deprecated in v1.19+\nnode/server\n"}})
	require.NoError(t, err)
	assert.Equal(t, 1, n, "warnings on stderr reach the output and are not nodes")

	_, err = NodeCount(&drainHost{replies: map[string]string{"kubectl get nodes": "Warning: something\n"}})
	require.Error(t, err)

	_, err = NodeCount(&drainHost{replies: map[string]string{"kubectl get nodes": "\n"}})
	require.Error(t, err, "a cluster always has its server, so no nodes means the read went wrong")

	_, err = NodeCount(&drainHost{failures: map[string]string{"kubectl get nodes": "connection refused"}})
	require.Error(t, err)
}

func TestDrainNodeSummarisesRepeatedEvictionErrors(t *testing.T) {
	retry := "error when evicting pods/\"drainme-0\" -n \"gate\" (will retry after 5s): Cannot evict pod as it would violate the pod's disruption budget.\n"
	h := &drainHost{failures: map[string]string{"kubectl drain": strings.Repeat(retry, 24) +
		"error: unable to drain node \"worker-1\" due to error: global timeout reached: 2m0s\n"}}

	err := DrainNode(h, "worker-1", 2*time.Minute)
	require.Error(t, err)
	assert.Equal(t, 1, strings.Count(err.Error(), "drainme-0"), "one line per pod, however often kubectl retried: %s", err)
	assert.Contains(t, err.Error(), "24 attempts")
	assert.Contains(t, err.Error(), "disruption budget")
	assert.Less(t, len(err.Error()), 600)
}
