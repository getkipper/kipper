package cmd

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	kernel    string
}

func (h statusHost) Run(cmd string) (string, error) {
	switch {
	case cmd == "cat /etc/machine-id":
		return h.machineID + "\n", nil
	case cmd == "cat /etc/os-release":
		return "ID=debian\n", nil
	case strings.Contains(cmd, "uname -r"):
		if h.kernel == "" {
			return "running=7.0.0-38-generic\ninstalled=7.0.0-38-generic\ndenials=0\n", nil
		}
		return h.kernel, nil
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
	case strings.Contains(cmd, "kipper-access.service"):
		return "----kip-access----\n----kip-access----\nenabled \nactive \nnftables  \n", nil
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
		"  Kernel:\n"+
		"    ⚠  not checked (could not reach the host over SSH)\n\n"+
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

func TestStatusChecksOperatorAccessOnEveryWorker(t *testing.T) {
	var out bytes.Buffer
	cluster := &config.Cluster{Host: "203.0.113.10", BackupStorage: &config.BackupStorageRef{Mode: "external"}}
	records := statusRecords()[:2]
	server := serverTarget(cluster, statusHost{machineID: statusServerID}, nil, records)
	workers := inspectWorkers(records, func(installer.NodeRecord) (installer.CommandRunner, error) {
		return statusHost{machineID: statusWorker1ID}, nil
	})

	reportHostChecks(&out, cluster, []k8s.NodeInfo{{Name: "server-1"}, {Name: "worker-1"}}, server, workers)

	_, access, found := strings.Cut(out.String(), "Operator access:")
	require.True(t, found)
	assert.Contains(t, access, "server-1: open to every address")
	assert.Contains(t, access, "worker-1: open to every address")
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

func TestHostChecksWarnWhenTheKernelBlocksContainerSignals(t *testing.T) {
	var out bytes.Buffer
	cluster := &config.Cluster{Host: "203.0.113.10"}
	server := serverTarget(cluster, statusHost{machineID: statusServerID,
		kernel: "running=7.0.0-42-generic\ninstalled=7.0.0-38-generic\ninstalled=7.0.0-42-generic\ndenials=460\n"}, nil, nil)

	reportHostChecks(&out, cluster, []k8s.NodeInfo{{Name: "server-1"}}, server, nil)

	assert.Contains(t, out.String(), "  Kernel:\n"+
		"    ⚠  kernel 7.0.0-42-generic blocks signals inside containers (460 AppArmor denials since boot)\n"+
		"       Container programs can hang, and Postgres services may never start.\n"+
		"       Pin a kernel that worked before with 'kip node kernel pin <version>', then run 'kip node reboot'. Installed: 7.0.0-38-generic, 7.0.0-42-generic\n\n")
}

// An installed pin that differs from the running kernel calls for a reboot.
func TestHostChecksAskForTheRebootAfterAPinOnABlockingKernel(t *testing.T) {
	var out bytes.Buffer
	cluster := &config.Cluster{Host: "203.0.113.10"}
	server := serverTarget(cluster, statusHost{machineID: statusServerID,
		kernel: "running=7.0.0-42-generic\npinned=7.0.0-38-generic\ninstalled=7.0.0-38-generic\ninstalled=7.0.0-42-generic\ndenials=460\n"}, nil, nil)

	reportHostChecks(&out, cluster, []k8s.NodeInfo{{Name: "server-1"}}, server, nil)

	assert.Contains(t, out.String(), "       Kernel 7.0.0-38-generic is pinned. Run 'kip node reboot' to boot it. Installed: 7.0.0-38-generic, 7.0.0-42-generic\n")
	assert.NotContains(t, out.String(), "Pin a kernel")
}

func TestHostChecksNameAPinnedKernelThatIsGone(t *testing.T) {
	var out bytes.Buffer
	cluster := &config.Cluster{Host: "203.0.113.10"}
	server := serverTarget(cluster, statusHost{machineID: statusServerID,
		kernel: "running=7.0.0-43-generic\npinned=7.0.0-38-generic\ninstalled=7.0.0-43-generic\ndenials=0\n"}, nil, nil)

	reportHostChecks(&out, cluster, []k8s.NodeInfo{{Name: "server-1"}}, server, nil)

	assert.Contains(t, out.String(), "    ⚠  running 7.0.0-43-generic. Pinned kernel 7.0.0-38-generic is no longer installed. Pin an installed kernel with 'kip node kernel pin <version>', or run 'kip node kernel unpin'\n")
}

func TestAPinnedKernelIsNotAPendingUpdate(t *testing.T) {
	var out bytes.Buffer
	cluster := &config.Cluster{Host: "203.0.113.10"}
	server := serverTarget(cluster, statusHost{machineID: statusServerID, pending: "NEEDRESTART-KSTA: 3\n",
		kernel: "running=7.0.0-38-generic\npinned=7.0.0-38-generic\ninstalled=7.0.0-38-generic\ninstalled=7.0.0-42-generic\ndenials=0\n"}, nil, nil)

	reportHostChecks(&out, cluster, []k8s.NodeInfo{{Name: "server-1"}}, server, nil)

	got := out.String()
	assert.Contains(t, got, "  Kernel:\n    ✔  7.0.0-38-generic, pinned with 'kip node kernel pin'\n\n")
	assert.Contains(t, got, "  Pending restarts:\n    ✔  nothing waiting on a restart\n\n",
		"the newer kernel stays installed while the pin holds")
}

func TestAPinThatHasNotBootedYetAsksForAReboot(t *testing.T) {
	var out bytes.Buffer
	cluster := &config.Cluster{Host: "203.0.113.10"}
	server := serverTarget(cluster, statusHost{machineID: statusServerID,
		kernel: "running=7.0.0-42-generic\npinned=7.0.0-38-generic\ninstalled=7.0.0-38-generic\ninstalled=7.0.0-42-generic\ndenials=0\n"}, nil, nil)

	reportHostChecks(&out, cluster, []k8s.NodeInfo{{Name: "server-1"}}, server, nil)

	assert.Contains(t, out.String(), "    ⚠  running 7.0.0-42-generic. Kernel 7.0.0-38-generic is pinned. Run 'kip node reboot' to boot it\n")
}

// kernelCountingHost counts how often a host is asked for its kernel state.
type kernelCountingHost struct {
	statusHost
	reads *int
}

func (h kernelCountingHost) Run(cmd string) (string, error) {
	if strings.Contains(cmd, "uname -r") {
		*h.reads++
	}
	return h.statusHost.Run(cmd)
}

func (h kernelCountingHost) RunStdin(cmd string, _ io.Reader) (string, error) { return h.Run(cmd) }

// Each host's kernel log is scanned once, though both the Kernel and the
// Pending restarts sections use the result.
func TestHostChecksReadEachKernelOnce(t *testing.T) {
	var out bytes.Buffer
	cluster := &config.Cluster{Host: "203.0.113.10"}
	records := statusRecords()[:2]
	pinned := "running=7.0.0-38-generic\npinned=7.0.0-38-generic\ninstalled=7.0.0-38-generic\ninstalled=7.0.0-42-generic\ndenials=0\n"
	var serverReads, workerReads int
	server := serverTarget(cluster, kernelCountingHost{statusHost{machineID: statusServerID, pending: "NEEDRESTART-KSTA: 3\n", kernel: pinned}, &serverReads}, nil, records)
	worker := kernelCountingHost{statusHost{machineID: statusWorker1ID, pending: "NEEDRESTART-KSTA: 3\n", kernel: pinned}, &workerReads}
	workers := inspectWorkers(records, func(installer.NodeRecord) (installer.CommandRunner, error) { return worker, nil })

	reportHostChecks(&out, cluster, []k8s.NodeInfo{{Name: "server-1", IP: "203.0.113.10"}, {Name: "worker-1"}}, server, workers)

	assert.Equal(t, 1, serverReads, "server kernel reads")
	assert.Equal(t, 1, workerReads, "worker kernel reads")
	assert.Contains(t, out.String(), "    ✔  worker-1: nothing waiting on a restart\n", "the pin still hides the newer kernel")
}
