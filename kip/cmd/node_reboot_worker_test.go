package cmd

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/installer"
)

// drainingServer stands in for kubectl on the server during a worker reboot.
type drainingServer struct {
	policy     string
	cordoned   bool
	drainErr   string
	replicas   []string
	afterDrain []string
	commands   []string
	drained    bool
	health     string
	degradeOn  string
	healthErr  bool
	replicaErr bool
	nodes      string
	longhorn   string
}

func (s *drainingServer) Run(cmd string) (string, error) {
	s.commands = append(s.commands, cmd)
	if s.degradeOn != "" && strings.Contains(cmd, s.degradeOn) {
		s.health = dbDegraded
	}
	switch {
	case strings.Contains(cmd, "nodes.longhorn.io") && strings.HasSuffix(cmd, "-o json"):
		if s.longhorn == "" {
			return longhornNodes("True"), nil
		}
		return s.longhorn, nil
	case strings.Contains(cmd, "volumes.longhorn.io") && strings.HasSuffix(cmd, "-o json"):
		return `{"items":[{"metadata":{"name":"pvc-db"},"spec":{"size":"1073741824","nodeSelector":["kipper-server"]},"status":{"actualSize":1073741824}}]}`, nil
	case strings.Contains(cmd, "settings.longhorn.io storage-"):
		return "", nil
	case strings.Contains(cmd, "node-drain-policy"):
		return s.policy, nil
	case strings.Contains(cmd, "replicas.longhorn.io"):
		if s.replicaErr && !s.drained {
			return "connection refused", errors.New("exit status 1")
		}
		if s.drained && s.afterDrain != nil {
			return strings.Join(s.afterDrain, "\n"), nil
		}
		return strings.Join(s.replicas, "\n"), nil
	case strings.Contains(cmd, ".status.robustness"):
		if s.healthErr {
			return "connection refused", errors.New("exit status 1")
		}
		return s.health, nil
	case strings.HasPrefix(cmd, "kubectl get nodes"):
		if s.nodes == "" {
			return "node/server\nnode/worker-1\nnode/worker-2\n", nil
		}
		return s.nodes, nil
	case strings.Contains(cmd, "volumes.longhorn.io"):
		return "pvc-db\tshop\tdata-db-0\npvc-loki\tmonitoring\tstorage-loki-0\n", nil
	case strings.Contains(cmd, "deployments,statefulsets,daemonsets"):
		return "Deployment/shop/api 2 2\nStatefulSet/shop/db 1 1\n", nil
	case strings.Contains(cmd, "spec.unschedulable"):
		if s.cordoned {
			return "true", nil
		}
		return "", nil
	case strings.HasPrefix(cmd, "kubectl cordon"):
		s.cordoned = true
	case strings.HasPrefix(cmd, "kubectl uncordon"):
		s.cordoned = false
	case strings.HasPrefix(cmd, "kubectl drain"):
		s.drained = true
		if s.drainErr != "" {
			return s.drainErr, errors.New("exit status 1")
		}
	}
	return "", nil
}

func (s *drainingServer) RunStdin(cmd string, _ io.Reader) (string, error) { return s.Run(cmd) }

func (s *drainingServer) ran(prefix string) bool {
	for _, c := range s.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

const (
	dbOnlyOnWorker   = "pvc-db\tworker-1\t2026-10-16T01:00:00Z\t"
	dbMovedToServer  = "pvc-db\tserver\t2026-10-16T03:00:00Z\t"
	lokiOnWorker     = "pvc-loki\tworker-1\t2026-10-16T01:00:00Z\t"
	lokiOnServerToo  = "pvc-loki\tserver\t2026-10-16T01:00:00Z\t"
	evictionPolicy   = installer.LonghornEvictionDrainPolicy
	drainBudgetError = "error when evicting pods/\"instance-manager-abc\" -n \"longhorn-system\" (will retry after 5s): Cannot evict pod as it would violate the pod's disruption budget.\nerror: unable to drain node \"worker-1\" due to error: global timeout reached: 30m0s"
)

// longhornNodes lists the worker being rebooted and a server tagged for
// single-copy volumes. The server's single disk has the given Schedulable status.
func longhornNodes(serverDisk string) string {
	node := func(name, tags, schedulable string) string {
		return `{"metadata":{"name":"` + name + `"},"spec":{"allowScheduling":true,"tags":[` + tags + `],"disks":{"disk-1":{"allowScheduling":true}}},` +
			`"status":{"conditions":[{"type":"Ready","status":"True"},{"type":"Schedulable","status":"True"}],"diskStatus":{"disk-1":{"conditions":[{"type":"Schedulable","status":"` + schedulable +
			`","message":"Disk disk-1 on node ` + name + ` is not schedulable for more replica"}],"storageAvailable":32212254720,"storageMaximum":42949672960}}}}`
	}
	return `{"items":[` + node("server", `"kipper-server"`, serverDisk) + "," + node("worker-1", "", "True") + `]}`
}

func newWorkerReboot(s *drainingServer) (*workerReboot, *bytes.Buffer) {
	var out bytes.Buffer
	return &workerReboot{Out: &out, Master: s, Node: "worker-1", DrainTimeout: 30 * time.Minute}, &out
}

func TestWorkerRebootListsVolumesCordonsAndDrains(t *testing.T) {
	s := &drainingServer{policy: evictionPolicy,
		replicas:   []string{dbOnlyOnWorker, lokiOnWorker, lokiOnServerToo},
		afterDrain: []string{dbMovedToServer, lokiOnWorker, lokiOnServerToo}}
	w, out := newWorkerReboot(s)

	require.NoError(t, w.preview())
	assert.False(t, s.ran("kubectl cordon"), "the preview runs before the operator confirms and changes nothing")
	undo, err := w.prepare()
	require.NoError(t, err)
	assert.True(t, s.cordoned)
	assert.True(t, s.ran("kubectl drain worker-1 "))
	assert.Contains(t, out.String(), "shop/data-db-0: all healthy copies are here; Longhorn copies the volume to another node during the drain")
	assert.Contains(t, out.String(), "monitoring/storage-loki-0: 1 healthy copy on other nodes")
	assert.Equal(t, []string{"Deployment/shop/api", "StatefulSet/shop/db"}, w.ready, "the workloads to wait for are read before the drain moves them")

	require.NoError(t, undo())
	assert.False(t, s.cordoned)
}

func TestWorkerRebootKeepsAnExistingCordon(t *testing.T) {
	s := &drainingServer{policy: evictionPolicy, cordoned: true}
	w, out := newWorkerReboot(s)

	undo, err := w.prepare()
	require.NoError(t, err)
	require.NoError(t, undo())
	assert.True(t, s.cordoned, "a cordon the operator set must survive the reboot")
	assert.False(t, s.ran("kubectl uncordon"))
	assert.Contains(t, out.String(), "already cordoned")
}

func TestWorkerRebootStopsAndUncordonsWhenTheDrainFails(t *testing.T) {
	s := &drainingServer{policy: evictionPolicy, replicas: []string{dbOnlyOnWorker}, drainErr: drainBudgetError}
	w, _ := newWorkerReboot(s)

	_, err := w.prepare()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "instance-manager-abc")
	assert.Contains(t, err.Error(), "was not rebooted")
	assert.False(t, s.cordoned, "a failed drain must not leave a cordon that a later run would keep")
}

func TestWorkerRebootRefusesWhenALastCopyStayedBehind(t *testing.T) {
	s := &drainingServer{policy: evictionPolicy, replicas: []string{dbOnlyOnWorker}}
	w, _ := newWorkerReboot(s)

	_, err := w.prepare()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shop/data-db-0 still has its only healthy copy on worker-1")
	assert.False(t, s.cordoned)
}

func TestWorkerRebootRefusesABlockingDrainPolicyBeforeChangingAnything(t *testing.T) {
	s := &drainingServer{policy: "block-if-contains-last-replica"}
	w, _ := newWorkerReboot(s)

	err := w.preview()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip upgrade")
	assert.False(t, s.ran("kubectl cordon"))
	assert.False(t, s.ran("kubectl drain"))
}

func TestFindNodeRecordByNameOrAddress(t *testing.T) {
	records := []installer.NodeRecord{
		{Name: "server-1", Host: "192.0.2.10", Role: installer.NodeRoleServer},
		{Name: "worker-1", Host: "192.0.2.11", Role: installer.NodeRoleWorker},
	}
	byName, err := findNodeRecord(records, "worker-1")
	require.NoError(t, err)
	assert.Equal(t, "192.0.2.11", byName.Host)

	byHost, err := findNodeRecord(records, "192.0.2.11")
	require.NoError(t, err)
	assert.Equal(t, "worker-1", byHost.Name)

	_, err = findNodeRecord(records, "worker-9")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server-1, worker-1")
}

func TestNodeRebootDrainsBeforeAndUncordonsAfterTheReboot(t *testing.T) {
	h := &rebootingHost{bootID: "old-boot", downReads: 2}
	r, _ := h.reboot(t)
	r.ServingApps = nil
	r.Prepare = func() (func() error, error) {
		h.calls = append(h.calls, "drain")
		return func() error { h.calls = append(h.calls, "uncordon"); return nil }, nil
	}
	r.Settle = func(time.Duration) error { h.calls = append(h.calls, "workloads"); return nil }

	require.NoError(t, r.run("worker-1", true, true))
	assert.Equal(t, []string{"drain", "reboot", "uncordon", "workloads", "report"}, h.calls)
}

func TestNodeRebootDoesNotRebootWhenTheDrainFails(t *testing.T) {
	h := &rebootingHost{bootID: "old-boot"}
	r, _ := h.reboot(t)
	r.ServingApps = nil
	r.Prepare = func() (func() error, error) { return nil, errors.New("draining worker-1: global timeout reached") }
	r.Settle = func(time.Duration) error { return nil }

	require.Error(t, r.run("worker-1", true, true))
	assert.NotContains(t, h.calls, "reboot")
}

func TestNodeRebootUncordonsAWorkerThatDidNotRecover(t *testing.T) {
	h := &rebootingHost{bootID: "old-boot", unhealthyReads: 1000}
	r, _ := h.reboot(t)
	r.ServingApps = nil
	r.Prepare = func() (func() error, error) {
		return func() error { h.calls = append(h.calls, "uncordon"); return nil }, nil
	}
	r.Settle = func(time.Duration) error { h.calls = append(h.calls, "workloads"); return nil }

	require.Error(t, r.run("worker-1", true, true))
	assert.Equal(t, 1, strings.Count(strings.Join(h.calls, " "), "uncordon"), "the cordon is lifted once, even when the reboot fails")
	assert.NotContains(t, h.calls, "workloads")
}

func TestNodeRebootUncordonsOnceWhenWorkloadsDoNotRecover(t *testing.T) {
	h := &rebootingHost{bootID: "old-boot"}
	r, _ := h.reboot(t)
	r.ServingApps = nil
	r.Prepare = func() (func() error, error) {
		return func() error { h.calls = append(h.calls, "uncordon"); return nil }, nil
	}
	r.Settle = func(time.Duration) error { return errors.New("workloads that were ready before did not recover") }

	require.Error(t, r.run("worker-1", true, true))
	assert.Equal(t, 1, strings.Count(strings.Join(h.calls, " "), "uncordon"))
}

func TestNodeRebootUncordonsBeforeWaitingForComponents(t *testing.T) {
	h := &rebootingHost{bootID: "old-boot"}
	r, _ := h.reboot(t)
	r.ServingApps = nil
	uncordoned := false
	r.Prepare = func() (func() error, error) {
		return func() error { uncordoned = true; return nil }, nil
	}
	r.Settle = func(time.Duration) error { return nil }
	r.Healthy = func(time.Duration, string) (bool, string) {
		return uncordoned, "longhorn-driver-deployer 0/1 available"
	}

	require.NoError(t, r.run("worker-1", true, true), "a component that only fits on the rebooted worker needs it uncordoned first")
}

const (
	dbDegraded      = "pvc-db\tshop\tdata-db-0\tattached\tdegraded\t2\n"
	dbHealthy       = "pvc-db\tshop\tdata-db-0\tattached\thealthy\t2\n"
	lokiOnTwoNodes  = "pvc-loki\tmonitoring\tstorage-loki-0\tattached\tdegraded\t3\n"
	detachedUnknown = "pvc-old\t\t\tdetached\tunknown\t1\n"
)

func TestWorkerRebootRefusesADegradedVolumeBeforeConfirmation(t *testing.T) {
	s := &drainingServer{policy: evictionPolicy, health: dbDegraded + detachedUnknown}
	w, _ := newWorkerReboot(s)

	err := w.preview()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shop/data-db-0 (degraded, 2 replicas)")
	assert.Contains(t, err.Error(), "--ignore-degraded-volumes")
	assert.NotContains(t, err.Error(), "pvc-old", "detached volumes never block")
}

func TestWorkerRebootChecksVolumeHealthAgainRightBeforeTheCordon(t *testing.T) {
	s := &drainingServer{policy: evictionPolicy, health: dbHealthy}
	w, _ := newWorkerReboot(s)
	require.NoError(t, w.preview())

	s.health = dbDegraded
	_, err := w.prepare()
	require.Error(t, err, "a volume can degrade during the confirmation and backup")
	assert.Contains(t, err.Error(), "shop/data-db-0")
	assert.False(t, s.ran("kubectl cordon"))
	assert.False(t, s.ran("kubectl drain"))
}

func TestWorkerRebootRefusesWhenVolumeHealthCannotBeRead(t *testing.T) {
	s := &drainingServer{policy: evictionPolicy, healthErr: true}
	w, _ := newWorkerReboot(s)

	_, err := w.prepare()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--ignore-degraded-volumes")
	assert.False(t, s.ran("kubectl cordon"))
}

func TestWorkerRebootIgnoresVolumeHealthWhenAsked(t *testing.T) {
	s := &drainingServer{policy: evictionPolicy, health: dbDegraded}
	w, out := newWorkerReboot(s)
	w.IgnoreVolumes = true

	require.NoError(t, w.preview())
	undo, err := w.prepare()
	require.NoError(t, err)
	require.NoError(t, undo())
	assert.True(t, s.ran("kubectl drain worker-1 "))
	assert.Contains(t, out.String(), "shop/data-db-0 (degraded, 2 replicas)")

	s = &drainingServer{policy: evictionPolicy, healthErr: true}
	w, out = newWorkerReboot(s)
	w.IgnoreVolumes = true
	_, err = w.prepare()
	require.NoError(t, err)
	assert.Contains(t, out.String(), "volume health not checked")
}

func TestWorkerRebootOnlyWarnsAboutVolumesWithMoreReplicasThanNodes(t *testing.T) {
	s := &drainingServer{policy: evictionPolicy, health: lokiOnTwoNodes, nodes: "node/server\nnode/worker-1\n",
		replicas: []string{lokiOnWorker, lokiOnServerToo}}
	w, out := newWorkerReboot(s)

	require.NoError(t, w.preview(), "a volume that wants more replicas than there are nodes can never become healthy")
	assert.Contains(t, out.String(), "monitoring/storage-loki-0 wants 3 replicas but the cluster has 2 nodes")
}

func TestWorkerRebootRefusesAnOverReplicatedVolumeThatLostACopy(t *testing.T) {
	s := &drainingServer{policy: evictionPolicy, health: lokiOnTwoNodes, nodes: "node/server\nnode/worker-1\n",
		replicas: []string{lokiOnServerToo, "pvc-loki\tworker-1\t\t"}}
	w, _ := newWorkerReboot(s)

	err := w.preview()

	require.Error(t, err, "one usable copy on two nodes is genuine degradation, not the missing third replica")
	assert.Contains(t, err.Error(), "monitoring/storage-loki-0")
}

func TestWorkerRebootReportsVolumesStillRebuildingWithoutWaiting(t *testing.T) {
	s := &drainingServer{policy: evictionPolicy, health: dbDegraded}
	w, out := newWorkerReboot(s)

	require.NoError(t, w.settle(time.Minute))
	assert.Contains(t, out.String(), "shop/data-db-0 (degraded, 2 replicas)")

	s.healthErr = true
	out.Reset()
	require.NoError(t, w.settle(time.Minute), "the reboot itself succeeded; volume health is only reported")
	assert.Contains(t, out.String(), "could not check volume health")
}

func TestServerRebootChecksVolumesOnlyWhenTheClusterHasWorkers(t *testing.T) {
	var out bytes.Buffer
	r := &nodeReboot{}
	require.NoError(t, planServerReboot(&out, &drainingServer{nodes: "node/server\n", health: dbDegraded}, r, "192.0.2.10", false, false, false, 0))
	assert.Nil(t, r.Prepare, "a single-node cluster has no volume gate")

	r = &nodeReboot{}
	err := planServerReboot(&out, &drainingServer{health: dbDegraded}, r, "192.0.2.10", false, false, false, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shop/data-db-0")
}

func TestServerRebootWithAnUnreadableNodeListNeedsSkipBackup(t *testing.T) {
	var out bytes.Buffer
	err := planServerReboot(&out, failingNodes{}, &nodeReboot{}, "192.0.2.10", false, false, true, 0)
	require.Error(t, err, "--ignore-degraded-volumes is about known unhealthy volumes, not an unreadable node list")
	assert.Contains(t, err.Error(), "kip cannot read the cluster's node list from 192.0.2.10 (")
	assert.Contains(t, err.Error(), "run again with --skip-backup")
	assert.Contains(t, err.Error(), "The usual cause is that the Kubernetes API is not answering, and rebooting the server is the usual repair.")
	assert.NotContains(t, err.Error(), "--ignore-degraded-volumes")
}

func TestServerRebootWithAnUnreadableNodeListProceedsUnderSkipBackup(t *testing.T) {
	var out bytes.Buffer
	r := &nodeReboot{Impact: "Its apps will be unavailable during the reboot."}
	require.NoError(t, planServerReboot(&out, failingNodes{}, r, "192.0.2.10", true, true, false, 2))
	assert.Contains(t, out.String(), "kip's local node records list 2 worker(s)")
	assert.Contains(t, out.String(), "kip cannot read the cluster's node list from 192.0.2.10 (")
	assert.Contains(t, out.String(), "rebooting without a backup, node count or volume health check")
	assert.Contains(t, r.Impact, "kip cannot read the cluster's node list, so no backup is taken")

	require.NotNil(t, r.Prepare, "the late check stays armed")
	out.Reset()
	_, err := r.Prepare()
	require.NoError(t, err)
	assert.Contains(t, out.String(), "the node list still cannot be read")
}

func TestServerRebootOmitsTheLocalRecordsLineWithoutRecordedWorkers(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, planServerReboot(&out, failingNodes{}, &nodeReboot{}, "192.0.2.10", false, true, false, 0))
	assert.NotContains(t, out.String(), "local node records")
	assert.NotContains(t, out.String(), "rebooting without", "the attended run asks instead of warning")
}

func TestServerRebootLateCheckRecountsNodesWhenTheAPIComesBack(t *testing.T) {
	api := &recoveringNodes{}
	var out bytes.Buffer
	r := &nodeReboot{}
	require.NoError(t, planServerReboot(&out, api, r, "192.0.2.10", true, true, false, 0))

	api.answer = &drainingServer{nodes: "node/server\n", health: lokiOnTwoNodes}
	_, err := r.Prepare()
	require.NoError(t, err, "a single-node cluster whose API returned is not gated on its degraded Loki volume")

	api.answer = &drainingServer{health: dbDegraded}
	_, err = r.Prepare()
	require.Error(t, err, "a cluster with workers whose API returned still gets the volume gate")
	assert.Contains(t, err.Error(), "shop/data-db-0")
}

func TestNamedServerRebootMatchesTheServerHostWithoutTheRecords(t *testing.T) {
	assert.True(t, rebootsTheServer(nil, "192.0.2.10"))
	assert.True(t, rebootsTheServer([]string{"192.0.2.10"}, "192.0.2.10"))
	assert.False(t, rebootsTheServer([]string{"worker-1"}, "192.0.2.10"))
}

type recoveringNodes struct{ answer installer.CommandRunner }

func (r *recoveringNodes) Run(cmd string) (string, error) {
	if r.answer == nil {
		return failingNodes{}.Run(cmd)
	}
	return r.answer.Run(cmd)
}

func (r *recoveringNodes) RunStdin(cmd string, in io.Reader) (string, error) { return r.Run(cmd) }

func TestServerRebootGateRefusesADegradedVolumeOnAClusterWithWorkers(t *testing.T) {
	var out bytes.Buffer
	err := volumeGate(&out, &drainingServer{health: dbDegraded}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shop/data-db-0")
}

type failingNodes struct{}

func (failingNodes) Run(string) (string, error) {
	return "connection refused", errors.New("exit status 1")
}

func (f failingNodes) RunStdin(cmd string, _ io.Reader) (string, error) { return f.Run(cmd) }

func TestWorkerRebootChecksVolumeHealthAfterThePreCordonReads(t *testing.T) {
	for _, read := range []string{"deployments,statefulsets,daemonsets", "spec.unschedulable"} {
		s := &drainingServer{policy: evictionPolicy, health: dbHealthy, degradeOn: read}
		w, _ := newWorkerReboot(s)

		_, err := w.prepare()
		require.Error(t, err, read)
		assert.False(t, s.ran("kubectl cordon"), read)
		assert.False(t, s.ran("kubectl drain"), read)
	}
}

func TestWorkerRebootRefusesAnOverReplicatedVolumeWhoseCopiesSitOnAGoneNode(t *testing.T) {
	s := &drainingServer{policy: evictionPolicy, health: lokiOnTwoNodes, nodes: "node/server\nnode/worker-1\n",
		replicas: []string{lokiOnServerToo, "pvc-loki\tworker-old\t2026-10-16T01:00:00Z\t"}}
	w, _ := newWorkerReboot(s)

	require.Error(t, w.preview(), "a copy on a node that left the cluster does not cover worker-1")
}

func TestVolumeGateRefusalTellsTheOperatorToWaitForLonghorn(t *testing.T) {
	var out bytes.Buffer
	err := volumeGate(&out, &drainingServer{health: dbDegraded}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shop/data-db-0 (degraded, 2 replicas) is not healthy")
	assert.Contains(t, err.Error(), "Longhorn rebuilds degraded volumes on its own")
	assert.Contains(t, err.Error(), "Wait and run this command again")
	assert.NotContains(t, err.Error(), "Restore volume health", "there is no restore step for the operator to take")
}

func TestReplicaCountWarningSaysTheVolumeStaysDegraded(t *testing.T) {
	s := &drainingServer{policy: evictionPolicy, health: lokiOnTwoNodes, nodes: "node/server\nnode/worker-1\n",
		replicas: []string{lokiOnWorker, lokiOnServerToo}}
	w, out := newWorkerReboot(s)

	require.NoError(t, w.preview())
	assert.Contains(t, out.String(), "so it stays degraded until a node is added")
}

func TestPostRebootVolumeReportPointsAtTheRebuildAndTheException(t *testing.T) {
	s := &drainingServer{policy: evictionPolicy, health: dbDegraded}
	w, out := newWorkerReboot(s)

	require.NoError(t, w.settle(time.Minute))
	assert.Contains(t, out.String(), "Attached volumes not healthy: shop/data-db-0")
	assert.Contains(t, out.String(), "Longhorn may still be rebuilding")
	assert.Contains(t, out.String(), "does not block the next reboot while every node holds a healthy copy")
	assert.NotContains(t, out.String(), "Check their status in Longhorn", "kip does not expose the Longhorn UI")
}

func TestShownAddressNamesTheAddressPeersUse(t *testing.T) {
	natted := installer.NodeNetwork{NodeIP: "10.0.0.5", ExternalIP: "198.51.100.7"}
	assert.Equal(t, "198.51.100.7 (node address 10.0.0.5)", shownAddress(installer.NetworkModePublic, natted))
	assert.Equal(t, "198.51.100.8", shownAddress(installer.NetworkModePublic, installer.NodeNetwork{NodeIP: "198.51.100.8", ExternalIP: "198.51.100.8"}))
	assert.Equal(t, "10.0.0.3", shownAddress(installer.NetworkModePrivate, installer.NodeNetwork{NodeIP: "10.0.0.3", ExternalIP: "198.51.100.7"}))
}

func TestServerNetworkConsentStatesTheMeasuredImpact(t *testing.T) {
	text := strings.Join(strings.Fields(serverNetworkConsent("private", "192.0.2.10")), " ")
	assert.Contains(t, text, "The server will use private mode for cluster traffic (address 192.0.2.10)")
	assert.Contains(t, text, "Running containers keep running")
	assert.Contains(t, text, "usually for 10 to 30 seconds and longer on small or busy servers")
	assert.Contains(t, text, "attempts to restore the previous configuration")
}

// TestWorkerRebootRefusesALastCopyWithNowhereToGo verifies that a blocked copy
// leaves the node untouched, avoiding a drain timeout after other pods may
// have been evicted.
func TestWorkerRebootRefusesALastCopyWithNowhereToGo(t *testing.T) {
	s := &drainingServer{policy: evictionPolicy, replicas: []string{dbOnlyOnWorker}, longhorn: longhornNodes("False")}
	w, out := newWorkerReboot(s)

	err := w.preview()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shop/data-db-0 (server: disk disk-1 is not schedulable: Disk disk-1 on node server is not schedulable for more replica)")
	assert.NotContains(t, out.String(), "Longhorn copies the volume to another node during the drain")

	_, err = w.prepare()
	require.Error(t, err, "the disk can fill up between the preview and the cordon")
	assert.False(t, s.ran("kubectl cordon"))
	assert.False(t, s.ran("kubectl drain"))
}

func TestWorkerRebootOnlyWarnsWhenLonghornNodesCannotBeRead(t *testing.T) {
	s := &drainingServer{policy: evictionPolicy, replicas: []string{dbOnlyOnWorker}, longhorn: "error: the server is currently unable to handle the request"}
	w, out := newWorkerReboot(s)

	require.NoError(t, w.preview())
	assert.Contains(t, out.String(), "could not check where Longhorn can copy the volumes on worker-1")
}

func TestWorkerRebootDrainsWhenTheReplicasCannotBeReadBeforeTheCordon(t *testing.T) {
	s := &drainingServer{policy: evictionPolicy, replicaErr: true, afterDrain: []string{dbMovedToServer}}
	w, out := newWorkerReboot(s)

	_, err := w.prepare()
	require.NoError(t, err)
	assert.Contains(t, out.String(), "could not check where Longhorn can copy the volumes on worker-1")
	assert.True(t, s.ran("kubectl drain worker-1 "))
}
