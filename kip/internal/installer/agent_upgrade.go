package installer

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// kubeletMinorSkew is how many minor versions a kubelet may trail the API
// server. The Kubernetes version skew policy
// (https://kubernetes.io/releases/version-skew-policy/#kubelet) says that the
// kubelet must not be newer than kube-apiserver and may be up to three minor
// versions older (two for kubelets older than 1.25).
const kubeletMinorSkew = 3

// k3sAgentEnvFile holds the K3S_URL and K3S_TOKEN the agent joined with. The
// k3s install script rewrites it from its environment on every run.
var k3sAgentEnvFile = "/etc/systemd/system/k3s-agent.service.env"

// nodeVersionsQuery prints one line per node: name, kubelet version and the
// status of its Ready condition, separated by tabs.
const nodeVersionsQuery = `kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.nodeInfo.kubeletVersion}{"\t"}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}'`

type nodeVersion struct {
	version string
	ready   bool
}

// readNodeVersions returns each Node's kubelet version and readiness by name.
func readNodeVersions(client CommandRunner) (map[string]nodeVersion, error) {
	out, err := client.Run(nodeVersionsQuery)
	if err != nil {
		return nil, fmt.Errorf("reading the nodes' k3s versions: %w", err)
	}
	return parseNodeVersions(out)
}

func parseNodeVersions(out string) (map[string]nodeVersion, error) {
	versions := map[string]nodeVersion{}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) != 3 || fields[0] == "" {
			return nil, fmt.Errorf("reading the nodes' k3s versions: unexpected line %q", line)
		}
		versions[fields[0]] = nodeVersion{version: fields[1], ready: fields[2] == "True"}
	}
	return versions, nil
}

// HasWorkers reports whether any record is a worker, in any state.
func HasWorkers(records []NodeRecord) bool {
	for _, r := range records {
		if r.Role == NodeRoleWorker {
			return true
		}
	}
	return false
}

// checkClusterK3sSkew checks recorded workers against the planned server version.
// It reads versions only when the cluster has workers. rerun names the command
// to run again after fixing the worker.
func checkClusterK3sSkew(client CommandRunner, records []NodeRecord, rerun string) error {
	if !HasWorkers(records) {
		return nil
	}
	installed, err := installedK3sVersion(client)
	if err != nil {
		return err
	}
	versions, err := readNodeVersions(client)
	if err != nil {
		return err
	}
	return checkK3sSkew(installed, records, versions, rerun)
}

// checkK3sSkew compares recorded workers with k3sVersion if decideK3sInstall
// allows the server step, or installed otherwise. It rejects newer workers
// because kip never downgrades an agent. It also rejects a new gap of more
// than kubeletMinorSkew minor versions or a different major version. An existing
// gap is left to the subsequent agent upgrade. Records without a Node are skipped.
func checkK3sSkew(installed string, records []NodeRecord, versions map[string]nodeVersion, rerun string) error {
	run, _, err := decideK3sInstall(installed, k3sVersion)
	if err != nil {
		return err
	}
	planned := installed
	if run {
		planned = k3sVersion
	}
	pv, err := parseK3sVersion(planned)
	if err != nil {
		return err
	}
	var iv [4]int
	if installed != "" {
		if iv, err = parseK3sVersion(installed); err != nil {
			return err
		}
	}
	for _, r := range records {
		if r.Role != NodeRoleWorker {
			continue
		}
		node, ok := versions[r.Name]
		if !ok {
			continue
		}
		av, err := parseK3sVersion(node.version)
		if err != nil {
			return fmt.Errorf("reading the k3s version of worker %s: %w", r.Name, err)
		}
		newer, err := k3sVersionNewer(node.version, planned)
		if err != nil {
			return err
		}
		if newer {
			return fmt.Errorf("worker %s runs k3s %s, newer than the %s the server will run. kip never downgrades a worker, so nothing was changed. Use a kip release that installs %s or later", r.Name, node.version, planned, node.version)
		}
		tooFar := func(server [4]int) bool { return server[0] != av[0] || server[1]-av[1] > kubeletMinorSkew }
		if tooFar(pv) && (installed == "" || !tooFar(iv)) {
			return fmt.Errorf("worker %s runs k3s %s, and upgrading the server to %s would leave it more than three minor versions behind, which Kubernetes does not support. Nothing was changed. Bring the worker to the server's version first with 'kip node add %s', then run %s again", r.Name, node.version, planned, r.Host, rerun)
		}
	}
	return nil
}

// agentUpgradeCommand builds a command to install target in agent mode.
// The command checks the recorded machine ID and accepts an installed version
// of current or target, allowing retries after the binary has been replaced.
// Callers must validate the arguments and ensure target is at least current
// to prevent downgrades. The command loads the agent's environment file before
// the installer rewrites it, preserving the join URL and token. It forces a
// k3s-agent restart even if a previous attempt wrote the target files. Running
// pods are preserved.
func agentUpgradeCommand(machineID, current, target string) string {
	return fmt.Sprintf(`id=$(cat %[1]s 2>/dev/null)
[ "$id" = %[2]s ] || { echo "this host is machine ${id:-unknown}, but the node records name machine %[2]s at this address. kip left it unchanged"; exit 1; }
v=$(k3s --version 2>/dev/null | head -n 1 | awk '{print $3}')
[ "$v" = %[3]s ] || [ "$v" = %[4]s ] || { echo "this host has k3s ${v:-none} installed, but its node reports %[3]s. kip left it unchanged"; exit 1; }
f=%[5]s
[ -r "$f" ] || { echo "$f is missing, so this host's k3s agent settings are unknown"; exit 1; }
set -a && . "$f" && set +a || exit 1
[ -n "${K3S_URL:-}" ] || { echo "$f names no server (K3S_URL)"; exit 1; }
s=$(mktemp) || exit 1
trap 'rm -f "$s"' EXIT
curl -sfL https://get.k3s.io -o "$s" || { echo "downloading the k3s install script failed"; exit 1; }
INSTALL_K3S_VERSION=%[4]s INSTALL_K3S_EXEC=agent INSTALL_K3S_FORCE_RESTART=true sh "$s"`,
		leaseMachineIDFile, shellQuote(machineID), shellQuote(current), shellQuote(target), shellQuote(k3sAgentEnvFile))
}

// UpgradeAgents upgrades workers recorded as ready to the k3s version reported
// by the server's Node, in name order. It skips other lifecycle states and
// returns nil if no workers are recorded. Each upgrade runs one host command
// and restarts k3s-agent in place without draining pods. Newer workers are
// rejected to prevent downgrades.
//
// Workers already reporting the target version and Ready are skipped, so retries
// resume incomplete upgrades. Each remaining worker has five minutes to report
// Ready at the target version before the next worker is processed. The first
// error stops the upgrade, leaving completed upgrades in place.
//
// onWorker must connect to the host, call use synchronously and release the
// connection. The caller must hold the node lifecycle lease and pass runners
// that check lease ownership for both the server and worker commands.
// The rerun command appears in recovery instructions.
func UpgradeAgents(out io.Writer, master CommandRunner, onWorker func(host string, use func(CommandRunner) error) error, rerun string) error {
	return upgradeAgents(out, master, onWorker, rerun, defaultJoinTimeouts.ready, defaultJoinTimeouts.interval)
}

func upgradeAgents(out io.Writer, master CommandRunner, onWorker func(host string, use func(CommandRunner) error) error, rerun string, timeout, interval time.Duration) error {
	records, err := ReadNodeRecords(master)
	if err != nil {
		return err
	}
	if !HasWorkers(records) {
		return nil
	}
	server, ok := ServerRecord(records)
	if !ok {
		return fmt.Errorf("the node records name no server, so the workers' k3s version is unknown")
	}
	versions, err := readNodeVersions(master)
	if err != nil {
		return err
	}
	target := versions[server.Name].version
	if _, err := parseK3sVersion(target); err != nil {
		return fmt.Errorf("reading the k3s version of the server %s: %w", server.Name, err)
	}
	for _, w := range records {
		if w.Role != NodeRoleWorker {
			continue
		}
		if w.State != NodeStateReady {
			_, _ = fmt.Fprintf(out, "  ⚠  Worker %s is %s, so kip left its k3s unchanged\n", w.Name, w.State)
			continue
		}
		// Earlier workers can take minutes, so readiness is read again here.
		now, err := readNodeVersions(master)
		if err != nil {
			return err
		}
		current, ok := now[w.Name]
		if !ok {
			return fmt.Errorf("worker %s has no node in the cluster. Rejoin it with 'kip node add %s', then run %s again", w.Name, w.Host, rerun)
		}
		if current.version == target && current.ready {
			_, _ = fmt.Fprintf(out, "  ✔  Worker %s already runs k3s %s\n", w.Name, target)
			continue
		}
		if current.version != target {
			if err := decideK3sAgentJoin(current.version, target); err != nil {
				return fmt.Errorf("worker %s: %w", w.Name, err)
			}
			_, _ = fmt.Fprintf(out, "  Upgrading k3s on %s from %s to %s...\n", w.Name, current.version, target)
			var hostOut string
			err := onWorker(w.Host, func(c CommandRunner) error {
				var err error
				hostOut, err = c.Run(agentUpgradeCommand(w.MachineID, current.version, target))
				return err
			})
			if err != nil {
				return fmt.Errorf("upgrading k3s on worker %s: %w. Run %s again to resume", w.Name, hostFailure(hostOut, err), rerun)
			}
		}
		upgraded := func() (bool, error) {
			now, err := readNodeVersions(master)
			if err != nil {
				return false, err
			}
			node := now[w.Name]
			if node.version != target || !node.ready {
				return false, fmt.Errorf("it reports k3s %q, ready %t", node.version, node.ready)
			}
			return true, nil
		}
		if err := pollUntil(timeout, interval, upgraded); err != nil {
			return fmt.Errorf("worker %s did not report Ready on k3s %s within %s (%v). Run %s again to resume", w.Name, target, timeout, err, rerun)
		}
		_, _ = fmt.Fprintf(out, "  ✔  Worker %s runs k3s %s\n", w.Name, target)
	}
	return nil
}

// hostFailure names a failed host command by the last line it printed,
// keeping the script out of the message. Without output it returns the
// innermost cause, or err itself when it wraps nothing, such as
// ErrNodeLeaseLost.
func hostFailure(out string, err error) error {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return errors.New(last)
	}
	for cause := errors.Unwrap(err); cause != nil; cause = errors.Unwrap(err) {
		err = cause
	}
	return err
}
