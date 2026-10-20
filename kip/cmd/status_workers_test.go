package cmd

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/k8s"
)

// statusHost answers the host check commands by what they read.
type statusHost struct {
	machineID string
	pending   string
	mounts    string
	failOpen  string
}

func (h statusHost) Run(cmd string) (string, error) {
	switch {
	case cmd == "cat /etc/machine-id":
		return h.machineID + "\n", nil
	case cmd == "cat /etc/os-release":
		return "ID=debian\n", nil
	case strings.Contains(cmd, "needrestart -b"):
		return h.pending, nil
	case cmd == "cat /proc/mounts":
		return h.mounts, nil
	case strings.Contains(cmd, "ufw-failopen"):
		return h.failOpen, nil
	case strings.HasPrefix(cmd, "cat "+installer.ResolvConfPath()):
		return "nameserver 1.1.1.1\nnameserver 8.8.8.8\nnameserver 9.9.9.9\n", nil
	case strings.Contains(cmd, "/dev/tcp/"):
		return "1.1.1.1 ok\n8.8.8.8 ok\n9.9.9.9 ok\n", nil
	case strings.Contains(cmd, "longhorn"), strings.Contains(cmd, "kubectl"):
		return "", nil
	}
	return "", errors.New("unexpected command " + cmd)
}

func (h statusHost) RunStdin(cmd string, _ io.Reader) (string, error) { return h.Run(cmd) }

const (
	statusServerID  = "00000000000000000000000000000001"
	statusWorker1ID = "00000000000000000000000000000002"
	statusWorker2ID = "00000000000000000000000000000003"
)

func statusRecords() []installer.NodeRecord {
	return []installer.NodeRecord{
		{Name: "server-1", Host: "203.0.113.10", MachineID: statusServerID, Role: installer.NodeRoleServer, State: installer.NodeStateReady},
		{Name: "worker-1", Host: "203.0.113.11", MachineID: statusWorker1ID, Role: installer.NodeRoleWorker, State: installer.NodeStateReady},
		{Name: "worker-2", Host: "203.0.113.12", MachineID: statusWorker2ID, Role: installer.NodeRoleWorker, State: installer.NodeStateReady},
	}
}

const csiMountRO = "/dev/longhorn/pvc-1 /var/lib/kubelet/pods/abc/volumes/kubernetes.io~csi/pvc-1/mount ext4 ro,relatime 0 0\n"

func TestHostChecksOnASingleNodeClusterAreUnlabelled(t *testing.T) {
	var out bytes.Buffer
	cluster := &config.Cluster{Host: "203.0.113.10"}
	server := serverTarget(cluster, statusHost{machineID: statusServerID, pending: "NEEDRESTART-KSTA: 3\n"}, nil, nil)

	reportHostChecks(&out, cluster, []k8s.NodeInfo{{Name: "server-1"}}, server, nil)

	got := out.String()
	assert.Contains(t, got, "  DNS resolvers:\n    ✔  1.1.1.1, 8.8.8.8, 9.9.9.9\n\n")
	assert.Contains(t, got, "  Pending restarts:\n    ⚠  a kernel update is pending. Reboot with 'kip node reboot'\n\n")
	assert.Contains(t, got, "  Volume mounts:\n    ✔  no read-only volumes on 203.0.113.10\n\n")
	assert.NotContains(t, got, "server-1:")
}

func TestHostChecksOnAnUnreachableSingleServer(t *testing.T) {
	var out bytes.Buffer
	cluster := &config.Cluster{Host: "203.0.113.10"}
	server := serverTarget(cluster, nil, errors.New("connection refused"), nil)

	reportHostChecks(&out, cluster, []k8s.NodeInfo{{Name: "server-1"}}, server, nil)

	assert.Equal(t, "  DNS resolvers:\n"+
		"    ⚠  not checked (could not reach the host over SSH: connection refused)\n\n"+
		"  Pending restarts:\n"+
		"    ⚠  not checked (could not reach the host over SSH)\n\n"+
		"  Volume mounts:\n"+
		"    ⚠  not checked (could not reach the host over SSH)\n\n", out.String())
}

func TestHostChecksCoverEveryWorker(t *testing.T) {
	var out bytes.Buffer
	cluster := &config.Cluster{Host: "203.0.113.10", BackupStorage: &config.BackupStorageRef{Mode: "external"}}
	records := statusRecords()
	server := serverTarget(cluster, statusHost{machineID: statusServerID}, nil, records)
	hosts := map[string]installer.CommandRunner{
		"203.0.113.11": statusHost{machineID: statusWorker1ID, pending: "NEEDRESTART-KSTA: 3\n", mounts: csiMountRO},
		"203.0.113.12": statusHost{machineID: statusWorker2ID, failOpen: "run42 2026-10-19T01:02:03Z\n"},
	}
	workers := inspectWorkers(records, func(rec installer.NodeRecord) (installer.CommandRunner, error) {
		return hosts[rec.Host], nil
	})
	nodes := []k8s.NodeInfo{{Name: "server-1", IP: "203.0.113.10"}, {Name: "worker-1"}, {Name: "worker-2"}}

	reportHostChecks(&out, cluster, nodes, server, workers)

	got := out.String()
	assert.Contains(t, got, "    ✔  server-1: 1.1.1.1, 8.8.8.8, 9.9.9.9\n")
	assert.Contains(t, got, "    ✔  worker-2: 1.1.1.1, 8.8.8.8, 9.9.9.9\n")
	assert.Contains(t, got, "    ✔  server-1: nothing waiting on a restart\n")
	assert.Contains(t, got, "    ⚠  worker-1: a kernel update is pending. Reboot with 'kip node reboot worker-1'\n")
	assert.Contains(t, got, "    ✔  no read-only volumes on server-1\n", "The server is displayed by node name, like the workers.")
	assert.Contains(t, got, "    ✗  worker-1: mounted read-only: /var/lib/kubelet/pods/abc/volumes/kubernetes.io~csi/pvc-1/mount\n")
	assert.Contains(t, got, "    ✔  no read-only volumes on worker-2\n")
	assert.NotContains(t, got, "not checked on", "every node was checked")
	assert.Contains(t, got, "  Host firewall:\n    ⚠  worker-2: a firewall rollback (run run42, 2026-10-19T01:02:03Z)")
	assert.Contains(t, got, "Run 'kip node add 203.0.113.12' again with the original options to turn the firewall back on")
	assert.NotContains(t, got, "kip cluster harden", "the server has no marker")
}

func TestHostChecksReportAnUnreachableWorker(t *testing.T) {
	var out bytes.Buffer
	cluster := &config.Cluster{Host: "203.0.113.10", BackupStorage: &config.BackupStorageRef{Mode: "external"}}
	records := statusRecords()
	server := serverTarget(cluster, statusHost{machineID: statusServerID}, nil, records)
	workers := inspectWorkers(records, func(rec installer.NodeRecord) (installer.CommandRunner, error) {
		if rec.Name == "worker-1" {
			return nil, errors.New("connection timed out")
		}
		return statusHost{machineID: statusWorker2ID}, nil
	})

	reportHostChecks(&out, cluster, []k8s.NodeInfo{{Name: "server-1"}, {Name: "worker-1"}, {Name: "worker-2"}}, server, workers)

	got := out.String()
	assert.Contains(t, got, "    ⚠  worker-1: not checked (could not reach 203.0.113.11 over SSH: connection timed out)\n")
	assert.Contains(t, got, "    ⚠  worker-1: not checked (could not reach 203.0.113.11 over SSH)\n")
	assert.Contains(t, got, "    ✔  worker-2: nothing waiting on a restart\n")
	assert.Contains(t, got, "    ✔  server-1: nothing waiting on a restart\n")
}

// An address that now answers as another machine is not the recorded worker,
// so its results would describe the wrong host.
func TestHostChecksRefuseAWorkerThatAnswersAsAnotherMachine(t *testing.T) {
	workers := inspectWorkers(statusRecords()[1:2], func(installer.NodeRecord) (installer.CommandRunner, error) {
		return statusHost{machineID: statusWorker2ID}, nil
	})
	assert.Nil(t, workers[0].runner)
	assert.Equal(t, "203.0.113.11 answers as machine "+statusWorker2ID+", not the recorded "+statusWorker1ID, workers[0].unreached)
}

// Capturing once per worker limits new SSH connections against UFW's rate limit.
func TestInspectWorkersReachesEachWorkerOnce(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	inspectWorkers(statusRecords(), func(rec installer.NodeRecord) (installer.CommandRunner, error) {
		mu.Lock()
		defer mu.Unlock()
		calls[rec.Name]++
		return statusHost{machineID: rec.MachineID}, nil
	})
	assert.Equal(t, map[string]int{"worker-1": 1, "worker-2": 1}, calls, "workers only, each once")
}

// 'kip cluster dns repair' rewrites the file on every node, so a worker's
// resolver warning offers it just as the server's does.
func TestWorkerDNSWarningsOfferTheRepairCommand(t *testing.T) {
	var out bytes.Buffer
	cluster := &config.Cluster{Host: "203.0.113.10", DNSResolvers: []string{"192.0.2.53"}, BackupStorage: &config.BackupStorageRef{Mode: "external"}}
	records := statusRecords()[:2]
	server := serverTarget(cluster, statusHost{machineID: statusServerID}, nil, records)
	workers := inspectWorkers(records, func(installer.NodeRecord) (installer.CommandRunner, error) {
		return statusHost{machineID: statusWorker1ID}, nil
	})

	reportHostChecks(&out, cluster, []k8s.NodeInfo{{Name: "server-1"}, {Name: "worker-1"}}, server, workers)

	got := out.String()
	assert.Contains(t, got, "    ⚠  worker-1: live resolvers (1.1.1.1, 8.8.8.8, 9.9.9.9) differ from the configured set (192.0.2.53)\n"+
		"       Run 'kip cluster dns repair' to restore "+installer.ResolvConfPath()+"\n")
	assert.Equal(t, 2, strings.Count(got, "Run 'kip cluster dns repair' to restore"), "the repair command restores the file on the server and every worker")
	assert.NotContains(t, got, "repairs the server only")
}

// The server being unreachable over SSH says nothing about the workers: they
// are still checked, from records read through the API.
func TestWorkersAreCheckedWhenTheServerIsUnreachable(t *testing.T) {
	var out bytes.Buffer
	cluster := &config.Cluster{Host: "203.0.113.10"}
	server := serverTarget(cluster, nil, errors.New("connection refused"), nil)
	workers := inspectWorkers(statusRecords(), func(rec installer.NodeRecord) (installer.CommandRunner, error) {
		return statusHost{machineID: rec.MachineID}, nil
	})

	reportHostChecks(&out, cluster, []k8s.NodeInfo{{Name: "server-1", IP: "203.0.113.10"}, {Name: "worker-1"}, {Name: "worker-2"}, {Name: "stray"}}, server, workers)

	got := out.String()
	assert.Contains(t, got, "    ⚠  203.0.113.10: not checked (could not reach the host over SSH: connection refused)\n")
	assert.Contains(t, got, "    ✔  worker-2: nothing waiting on a restart\n")
	assert.Contains(t, got, "    ⚠  not checked on stray")
}
