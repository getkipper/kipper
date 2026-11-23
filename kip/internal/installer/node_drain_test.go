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
		"replicas.longhorn.io": "pvc-db\tworker-1\t2026-10-16T01:00:00Z\t\t\n" +
			"pvc-loki\tworker-1\t2026-10-16T01:00:00Z\t\t\n" +
			"pvc-loki\tserver\t2026-10-16T01:00:00Z\t\t\n" +
			"pvc-loki\tworker-2\t2026-10-16T01:00:00Z\t2026-10-16T02:00:00Z\t2026-10-16T02:00:00Z\n" +
			"pvc-zot\tserver\t2026-10-16T01:00:00Z\t\t\n" +
			"pvc-new\tworker-1\t\t\t\n",
		"volumes.longhorn.io": "pvc-db\tshop\tdata-db-0\npvc-loki\tmonitoring\tstorage-loki-0\npvc-zot\tkipper-system\tzot-data\n",
		"get nodes --":        readyNodes,
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

func holderNodes(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

const readyNodes = "server\tTrue\t\nworker-1\tTrue\t\nworker-2\tTrue\t"

func TestVolumesOnNodeCountsOnlyCurrentCopiesOnAvailableNodesElsewhere(t *testing.T) {
	healthy := "2026-10-16T01:00:00Z"
	for name, tc := range map[string]struct {
		replicas, nodes string
		elsewhere       int
	}{
		"a ready holder": {"pvc-db\tserver\t" + healthy + "\t\t\n", holderNodes("server\tTrue\t", "worker-1\tTrue\t"), 1},
		"a fenced holder": {"pvc-db\tserver\t" + healthy + "\t\t\n",
			holderNodes("server\tTrue\tnode.kubernetes.io/out-of-service", "worker-1\tTrue\t"), 0},
		"a NotReady holder":  {"pvc-db\tserver\t" + healthy + "\t\t\n", holderNodes("server\tFalse\t", "worker-1\tTrue\t"), 0},
		"a missing holder":   {"pvc-db\tserver\t" + healthy + "\t\t\n", holderNodes("worker-1\tTrue\t"), 0},
		"a salvaged replica": {"pvc-db\tserver\t" + healthy + "\t\t2026-10-16T02:00:00Z\n", holderNodes("server\tTrue\t", "worker-1\tTrue\t"), 0},
		"two replicas on one holder": {"pvc-db\tserver\t" + healthy + "\t\t\npvc-db\tserver\t" + healthy + "\t\t\n",
			holderNodes("server\tTrue\t", "worker-1\tTrue\t"), 1},
	} {
		h := &drainHost{replies: map[string]string{
			"replicas.longhorn.io": "pvc-db\tworker-1\t" + healthy + "\t\t\n" + tc.replicas,
			"volumes.longhorn.io":  "pvc-db\tshop\tdata-db-0\n",
			"get nodes --":         tc.nodes,
		}}
		got, err := VolumesOnNode(h, "worker-1")
		require.NoError(t, err, name)
		require.Len(t, got, 1, name)
		assert.Equal(t, tc.elsewhere, got[0].Elsewhere, name)
		assert.Equal(t, tc.elsewhere == 0, got[0].LastCopy(), name)
	}
}

func TestVolumesOnNodeStopsWhenTheNodesCannotBeRead(t *testing.T) {
	h := &drainHost{
		replies:  map[string]string{"replicas.longhorn.io": "pvc-db\tworker-1\t2026-10-16T01:00:00Z\t\t\n"},
		failures: map[string]string{"get nodes --": "forbidden"},
	}
	_, err := VolumesOnNode(h, "worker-1")
	require.Error(t, err, "an unreadable holder is never counted as a copy elsewhere")
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
	assert.Contains(t, cmd, "kubectl drain worker-1 --ignore-daemonsets --delete-emptydir-data --timeout=2m0s")
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
	assert.Contains(t, err.Error(), "The drain did not finish within 20m0s")
	assert.NotContains(t, err.Error(), "global timeout reached")
	assert.NotContains(t, err.Error(), "already cordoned")
}

// roundsHost returns replies in order. An empty reply or exhausted replies
// means success, and a non-empty reply means failure.
type roundsHost struct {
	replies  []string
	commands []string
}

func (h *roundsHost) Run(cmd string) (string, error) {
	h.commands = append(h.commands, cmd)
	if len(h.replies) == 0 {
		return "", nil
	}
	out := h.replies[0]
	h.replies = h.replies[1:]
	if out == "" {
		return "", nil
	}
	return out, errors.New("exit status 1")
}

func (h *roundsHost) RunStdin(cmd string, _ io.Reader) (string, error) { return h.Run(cmd) }

const drainTimedOut = "error: unable to drain node \"worker-1\" due to error: global timeout reached: 2m0s\n"

func TestDrainNodeRunsInShortRoundsUpToTheTimeout(t *testing.T) {
	h := &roundsHost{replies: []string{drainTimedOut, drainTimedOut, drainTimedOut, drainTimedOut}}
	err := DrainNode(h, "worker-1", 5*time.Minute)
	require.Error(t, err)
	assert.Equal(t, "draining worker-1: the drain did not finish within 5m0s", err.Error())
	require.Len(t, h.commands, 3)
	assert.Contains(t, h.commands[0], "--timeout=2m0s")
	assert.Contains(t, h.commands[1], "--timeout=2m0s")
	assert.Contains(t, h.commands[2], "--timeout=1m0s")
}

func TestDrainNodeStopsAtTheFirstRoundThatFinishes(t *testing.T) {
	h := &roundsHost{replies: []string{drainTimedOut, ""}}
	require.NoError(t, DrainNode(h, "worker-1", 30*time.Minute))
	assert.Len(t, h.commands, 2)
}

func TestDrainNodeDoesNotRetryAnotherError(t *testing.T) {
	h := &roundsHost{replies: []string{"error: unable to drain node \"worker-1\" due to error: cannot delete Pods that declare no controller (use --force to override): default/bare\n"}}
	err := DrainNode(h, "worker-1", 30*time.Minute)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "default/bare")
	assert.Len(t, h.commands, 1)
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

// drainBlockedByBudget shows kubectl drain output when a pod disruption budget
// blocks eviction until the round times out.
const drainBlockedByBudget = "evicting pod gate/drainme-0\n" +
	"error when evicting pods/\"drainme-0\" -n \"gate\" (will retry after 5s): Cannot evict pod as it would violate the pod's disruption budget.\n" +
	"error when evicting pods/\"drainme-0\" -n \"gate\" (will retry after 5s): Cannot evict pod as it would violate the pod's disruption budget.\n" +
	"There are pending pods in node \"worker-1\" when an error occurred: error when evicting pods/\"drainme-0\" -n \"gate\": global timeout reached: 2m0s\n" +
	"error: unable to drain node \"worker-1\" due to error: [error when evicting pods/\"drainme-0\" -n \"gate\": global timeout reached: 2m0s], continuing command...\n" +
	"There are pending nodes to be drained:\n worker-1\n" +
	"error: error when evicting pods/\"drainme-0\" -n \"gate\": global timeout reached: 2m0s\n"

func TestDrainNodeStatesEachReasonOnceWithTheWholeTimeout(t *testing.T) {
	h := &roundsHost{replies: []string{drainBlockedByBudget, drainBlockedByBudget}}

	err := DrainNode(h, "worker-1", 4*time.Minute)

	require.Error(t, err)
	assert.Equal(t, `draining worker-1: error when evicting pods/"drainme-0" -n "gate": Cannot evict pod as it would violate the pod's disruption budget (2 attempts). The drain did not finish within 4m0s`, err.Error())
}

func TestDrainNodeKeepsAReasonOnlyKubectlsSummaryGives(t *testing.T) {
	h := &roundsHost{replies: []string{"error: unable to drain node \"worker-1\" due to error: cannot delete Pods that declare no controller (use --force to override): default/bare, continuing command...\n" +
		"There are pending nodes to be drained:\n worker-1\n" +
		"error: cannot delete Pods that declare no controller (use --force to override): default/bare\n"}}

	err := DrainNode(h, "worker-1", 30*time.Minute)

	require.Error(t, err)
	assert.Equal(t, "draining worker-1: cannot delete Pods that declare no controller (use --force to override): default/bare", err.Error())
}

func TestDrainNodeStatesEachFailureOfAnAggregateOnce(t *testing.T) {
	h := &roundsHost{replies: []string{"evicting pod shop/a\nevicting pod shop/b\n" +
		"There are pending pods in node \"worker-1\" when an error occurred: [error when evicting pods/\"a\" -n \"shop\": admission denied, error when evicting pods/\"b\" -n \"shop\": admission denied]\n" +
		"error: unable to drain node \"worker-1\" due to error: [error when evicting pods/\"a\" -n \"shop\": admission denied, error when evicting pods/\"b\" -n \"shop\": admission denied], continuing command...\n" +
		"There are pending nodes to be drained:\n worker-1\n" +
		"error: error when evicting pods/\"a\" -n \"shop\": admission denied\n" +
		"error: error when evicting pods/\"b\" -n \"shop\": admission denied\n"}}

	err := DrainNode(h, "worker-1", 30*time.Minute)

	require.Error(t, err)
	assert.Equal(t, `draining worker-1: error when evicting pods/"a" -n "shop": admission denied and error when evicting pods/"b" -n "shop": admission denied`, err.Error())
}

func TestDrainNodeKeepsAWaitFailureBesideATimeout(t *testing.T) {
	wait := `error when waiting for pod "a" in namespace "shop" to terminate: etcdserver: request timed out`
	h := &roundsHost{replies: []string{"evicting pod shop/a\nevicting pod gate/drainme-0\n" +
		"error when evicting pods/\"drainme-0\" -n \"gate\" (will retry after 5s): Cannot evict pod as it would violate the pod's disruption budget.\n" +
		"There are pending pods in node \"worker-1\" when an error occurred: [" + wait + ", error when evicting pods/\"drainme-0\" -n \"gate\": global timeout reached: 2m0s]\n" +
		"error: unable to drain node \"worker-1\" due to error: [" + wait + ", error when evicting pods/\"drainme-0\" -n \"gate\": global timeout reached: 2m0s], continuing command...\n" +
		"There are pending nodes to be drained:\n worker-1\n" +
		"error: " + wait + "\n" +
		"error: error when evicting pods/\"drainme-0\" -n \"gate\": global timeout reached: 2m0s\n"}}

	err := DrainNode(h, "worker-1", 2*time.Minute)

	require.Error(t, err)
	assert.Equal(t, `draining worker-1: error when evicting pods/"drainme-0" -n "gate": Cannot evict pod as it would violate the pod's disruption budget and `+wait+`. The drain did not finish within 2m0s`, err.Error())
}

func TestDrainNodeNamesAPodStillEvictingAtTheTimeout(t *testing.T) {
	h := &roundsHost{replies: []string{"evicting pod shop/slow\n" +
		"error: unable to drain node \"worker-1\" due to error: error when evicting pods/\"slow\" -n \"shop\": global timeout reached: 2m0s, continuing command...\n" +
		"There are pending nodes to be drained:\n worker-1\n" +
		"error: error when evicting pods/\"slow\" -n \"shop\": global timeout reached: 2m0s\n"}}

	err := DrainNode(h, "worker-1", 2*time.Minute)

	require.Error(t, err)
	assert.Equal(t, `draining worker-1: error when evicting pods/"slow" -n "shop". The drain did not finish within 2m0s`, err.Error())
}

func TestDrainNodeKeepsWaitingForAPodStillTerminating(t *testing.T) {
	terminating := "evicting pod shop/slow\npod/slow evicted\n" +
		"There are pending pods in node \"worker-1\" when an error occurred: error when waiting for pod \"slow\" in namespace \"shop\" to terminate: context deadline exceeded\n" +
		"error: unable to drain node \"worker-1\" due to error: error when waiting for pod \"slow\" in namespace \"shop\" to terminate: context deadline exceeded, continuing command...\n" +
		"There are pending nodes to be drained:\n worker-1\n" +
		"error: error when waiting for pod \"slow\" in namespace \"shop\" to terminate: context deadline exceeded\n"
	h := &roundsHost{replies: []string{terminating, terminating}}

	err := DrainNode(h, "worker-1", 4*time.Minute)

	require.Error(t, err)
	assert.Len(t, h.commands, 2, "a pod still terminating uses up the round, so the drain goes on")
	assert.Equal(t, `draining worker-1: error when waiting for pod "slow" in namespace "shop" to terminate. The drain did not finish within 4m0s`, err.Error())
}

func TestDrainNodeNamesAPodInATerminatingNamespaceOnce(t *testing.T) {
	h := &roundsHost{replies: []string{"evicting pod gone/web-0\n" +
		"error when evicting pod \"web-0\" from terminating namespace \"gone\" (will retry after 5s): pods \"web-0\" is forbidden: unable to create new content in namespace gone because it is being terminated\n" +
		"error when evicting pod \"web-0\" from terminating namespace \"gone\" (will retry after 5s): pods \"web-0\" is forbidden: unable to create new content in namespace gone because it is being terminated\n" +
		"error: unable to drain node \"worker-1\" due to error: error when evicting pods/\"web-0\" -n \"gone\": global timeout reached: 2m0s, continuing command...\n" +
		"There are pending nodes to be drained:\n worker-1\n" +
		"error: error when evicting pods/\"web-0\" -n \"gone\": global timeout reached: 2m0s\n"}}

	err := DrainNode(h, "worker-1", 2*time.Minute)

	require.Error(t, err)
	assert.Equal(t, `draining worker-1: error when evicting pod "web-0" from terminating namespace "gone": pods "web-0" is forbidden: unable to create new content in namespace gone because it is being terminated (2 attempts). The drain did not finish within 2m0s`, err.Error())
}

func TestDrainNodeKeepsAWebhookTimeoutAfterARetry(t *testing.T) {
	webhook := `error when evicting pods/"a" -n "shop": Internal error occurred: failed calling webhook "eviction.example": failed to call webhook: Post "https://webhook.example/evict": context deadline exceeded`
	h := &roundsHost{replies: []string{"evicting pod shop/a\n" +
		"error when evicting pods/\"a\" -n \"shop\" (will retry after 5s): Cannot evict pod as it would violate the pod's disruption budget.\n" +
		"error: unable to drain node \"worker-1\" due to error: " + webhook + ", continuing command...\n" +
		"There are pending nodes to be drained:\n worker-1\n" +
		"error: " + webhook + "\n"}}

	err := DrainNode(h, "worker-1", 4*time.Minute)

	require.Error(t, err)
	assert.Len(t, h.commands, 1, "a webhook timeout is not the round running out")
	assert.Equal(t, `draining worker-1: error when evicting pods/"a" -n "shop": Cannot evict pod as it would violate the pod's disruption budget and `+webhook, err.Error())
}
