package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/installer"
)

// dnsHost records the commands a host runs and can fail the resolver write.
type dnsHost struct {
	eachHost
	name      string
	log       *[]string
	failWrite bool
}

func (h *dnsHost) Run(command string) (string, error) {
	switch {
	case strings.Contains(command, "/etc/rancher/k3s/resolv.conf") && strings.HasPrefix(command, "cat >"):
		*h.log = append(*h.log, "write "+h.name)
		if h.failWrite {
			return "", errors.New("read-only file system")
		}
	case strings.Contains(command, "rollout restart deploy/coredns"):
		*h.log = append(*h.log, "restart coredns on "+h.name)
	}
	return h.eachHost.Run(command)
}

func dnsDialer(log *[]string, failing string) nodeDialer {
	ids := map[string]string{"203.0.113.11": eachWorkerA, "203.0.113.12": eachWorkerB}
	return func(rec installer.NodeRecord) (installer.FirewallRunner, func(), error) {
		return &dnsHost{eachHost: eachHost{id: ids[rec.Host]}, name: rec.Name, log: log, failWrite: rec.Name == failing}, func() {}, nil
	}
}

func TestRepairDNSWritesEveryNodeBeforeRestartingCoreDNS(t *testing.T) {
	var log []string
	server := &dnsHost{eachHost: eachHost{id: eachServerID}, name: "server", log: &log}

	err := repairDNSOnNodes(eachRecords(), server, dnsDialer(&log, ""), []string{"192.0.2.53"})

	require.NoError(t, err)
	assert.Equal(t, []string{"write server", "write worker-a", "write worker-b", "restart coredns on server"}, log,
		"CoreDNS restarts once, from the server, after every node has the file")
}

func TestRepairDNSStopsAtAFailingWorkerWithoutRestartingCoreDNS(t *testing.T) {
	var log []string
	server := &dnsHost{eachHost: eachHost{id: eachServerID}, name: "server", log: &log}

	err := repairDNSOnNodes(eachRecords(), server, dnsDialer(&log, "worker-a"), []string{"192.0.2.53"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-a (203.0.113.11)")
	assert.Equal(t, []string{"write server", "write worker-a"}, log, "worker-b and the restart wait for the rerun")
}
