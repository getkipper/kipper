package cmd

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// workerCheckTimeout bounds the one batched command kip status runs on a
// worker, so a host that accepts SSH and then stalls cannot hold up the report.
const workerCheckTimeout = 2 * time.Minute

// hostTarget is one node whose host checks kip status reports.
type hostTarget struct {
	name           string
	where          string
	reboot         string
	firewallRepair string
	prefix         string
	runner         installer.CommandRunner
	unreached      string
	detail         error
}

// notChecked reports whether the target has no runner and prints its failure.
// withDetail includes the underlying error so callers can show it once across sections.
func (t hostTarget) notChecked(out io.Writer, withDetail bool) bool {
	if t.runner != nil {
		return false
	}
	if withDetail && t.detail != nil {
		say(out, "    ⚠  %snot checked (%s: %v)\n", t.prefix, t.unreached, t.detail)
	} else {
		say(out, "    ⚠  %snot checked (%s)\n", t.prefix, t.unreached)
	}
	return true
}

// serverTarget describes the configured host, using its recorded node name for
// display when available and cluster.Host otherwise. A nil runner marks the
// host as unreachable, with dialErr providing the failure details.
func serverTarget(cluster *config.Cluster, runner installer.CommandRunner, dialErr error, records []installer.NodeRecord) hostTarget {
	t := hostTarget{
		name:           cluster.Host,
		where:          cluster.Host,
		reboot:         "kip node reboot",
		firewallRepair: "Run 'kip cluster harden' to turn it back on",
		runner:         runner,
	}
	for _, r := range records {
		if r.Role == installer.NodeRoleServer {
			t.name, t.where = r.Name, r.Name
		}
	}
	if runner == nil {
		t.unreached = "could not reach the host over SSH"
		t.detail = dialErr
	}
	return t
}

// inspectWorkers checks every recorded worker concurrently and returns one
// target per worker in record order. capture is called once per worker and
// returns a runner that answers the host checks without reaching the host
// again. A worker whose address answers with another machine id is reported
// as not checked.
func inspectWorkers(records []installer.NodeRecord, capture func(installer.NodeRecord) (installer.CommandRunner, error)) []hostTarget {
	var workers []installer.NodeRecord
	for _, r := range records {
		if r.Role == installer.NodeRoleWorker {
			workers = append(workers, r)
		}
	}
	targets := make([]hostTarget, len(workers))
	var wg sync.WaitGroup
	for i, rec := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			targets[i] = inspectWorker(rec, capture)
		}()
	}
	wg.Wait()
	return targets
}

func inspectWorker(rec installer.NodeRecord, capture func(installer.NodeRecord) (installer.CommandRunner, error)) hostTarget {
	t := hostTarget{
		name:           rec.Name,
		where:          rec.Name,
		reboot:         "kip node reboot " + rec.Name,
		firewallRepair: fmt.Sprintf("Run 'kip node add %s' again with the original options to turn the firewall back on", rec.Host),
	}
	runner, err := capture(rec)
	if err != nil {
		t.unreached = fmt.Sprintf("could not reach %s over SSH", rec.Host)
		t.detail = err
		return t
	}
	id, err := installer.ReadMachineID(runner)
	switch {
	case err != nil:
		t.unreached = fmt.Sprintf("could not confirm that %s is still this node", rec.Host)
		t.detail = err
	case id != rec.MachineID:
		t.unreached = fmt.Sprintf("%s answers as machine %s, not the recorded %s", rec.Host, id, rec.MachineID)
	default:
		t.runner = runner
	}
	return t
}

// captureWorkerChecks batches host checks into one remote command to reduce
// SSH connections and avoid exhausting the worker's firewall rate limit.
func captureWorkerChecks(cfg ssh.Config, rec installer.NodeRecord) (installer.CommandRunner, error) {
	cfg.Host = rec.Host
	cfg.CommandTimeout = workerCheckTimeout
	client, err := ssh.Dial(cfg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()
	return installer.CaptureHostChecks(client)
}
