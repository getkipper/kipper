package installer

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

const (
	// wireguardBackend is the k3s setting for native WireGuard networking.
	wireguardBackend = "wireguard-native"

	// networkChangeRecord marks a network change in progress on the server.
	// It outlives a lost connection, so the next run finishes or undoes it.
	networkChangeRecord = "/var/lib/kipper/network-change"

	networkChangeBackup = "/var/lib/kipper/network-change.d"

	networkUpMarker = "KIPPER_NETWORK_UP"
)

// flannelLinks maps the backend flannel reports in its node annotation to the
// link it creates on the host and the k3s setting that selects it.
var flannelLinks = map[string]struct{ link, setting string }{
	"vxlan":     {"flannel.1", "vxlan"},
	"wireguard": {"flannel-wg", wireguardBackend},
}

// ErrNetworkChangeNotConfirmed indicates missing consent for migration or recovery.
var ErrNetworkChangeNotConfirmed = errors.New("pod network changes require approval and may restart k3s; re-run with --yes to approve")

// NetworkChange describes the server's current pod network and pending recovery.
type NetworkChange struct {
	Running     string
	Interrupted bool
}

// Needed reports whether ChangeNetwork has anything to do.
func (c NetworkChange) Needed() bool {
	return c.Interrupted || c.Running != "wireguard"
}

// Describe summarizes the network change for the consent prompt.
func (c NetworkChange) Describe() string {
	var b strings.Builder
	if c.Interrupted {
		b.WriteString(interruptedDescription)
	}
	if c.Running != "wireguard" {
		from := ""
		if c.Running != "" {
			from = " from " + c.Running
		}
		fmt.Fprintf(&b, "  The pod network moves%s to WireGuard, which encrypts pod traffic\n"+
			"  between nodes. The change restarts k3s and temporarily interrupts API access\n"+
			"  and pod networking. If the network or previously ready workloads fail to\n"+
			"  recover, kip attempts to restore the previous configuration and restart k3s.\n", from)
	}
	return b.String()
}

const interruptedDescription = "  An earlier change to this server's pod network did not finish. If the server\n" +
	"  runs WireGuard and previously ready workloads recover, kip marks the change\n" +
	"  complete. Otherwise, it attempts to restore the previous configuration\n  and restart k3s.\n"

// NetworkChangePending reports whether an earlier network change left its
// record on the server. It needs no Kubernetes API, so it works while k3s is
// stopped.
func NetworkChangePending(client CommandRunner) (bool, error) {
	record, err := client.Run(fmt.Sprintf("[ -f %[1]s ] && cat %[1]s || true", networkChangeRecord))
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", networkChangeRecord, err)
	}
	return strings.TrimSpace(record) != "", nil
}

// PendingNetworkChangeDescription tells the operator what
// RecoverNetworkChange will do, for the consent prompt.
func PendingNetworkChangeDescription() string { return interruptedDescription }

// RecoverNetworkChange completes an interrupted change if WireGuard and
// previously ready workloads are healthy; otherwise, it attempts a rollback.
// It can start recovery while the Kubernetes API is unavailable.
func RecoverNetworkChange(client CommandRunner) error {
	locked, release, err := lockAPIServerConfig(client)
	if err != nil {
		return err
	}
	defer release()
	return finishInterrupted(locked, defaultNetworkChangeTimeouts)
}

// PlanNetworkChange reads the server's flannel backend and pending change record.
func PlanNetworkChange(client CommandRunner) (NetworkChange, error) {
	record, err := client.Run(fmt.Sprintf("[ -f %[1]s ] && cat %[1]s || true", networkChangeRecord))
	if err != nil {
		return NetworkChange{}, fmt.Errorf("reading %s: %w", networkChangeRecord, err)
	}
	change := NetworkChange{Interrupted: strings.TrimSpace(record) != ""}
	running, err := runningFlannelBackend(client)
	if err != nil {
		if change.Interrupted {
			return change, nil
		}
		return NetworkChange{}, err
	}
	change.Running = running
	return change, nil
}

// runningFlannelBackend requires exactly one server node reporting a backend.
func runningFlannelBackend(client CommandRunner) (string, error) {
	out, err := client.Run(fmt.Sprintf(`kubectl --request-timeout=20s get nodes -l %s=true -o jsonpath='{range .items[*]}{.metadata.annotations.flannel\.alpha\.coreos\.com/backend-type}{"\n"}{end}'`, serverNodeLabel))
	if err != nil {
		return "", fmt.Errorf("reading the server's pod network backend: %w", err)
	}
	backends := strings.Fields(out)
	if len(backends) != 1 {
		return "", fmt.Errorf("expected one server node reporting a pod network backend, found %d", len(backends))
	}
	return backends[0], nil
}

// networkChangeTimeouts bounds each wait of the change. Tests shorten them.
type networkChangeTimeouts struct {
	start     time.Duration
	workloads time.Duration
	interval  time.Duration
}

var defaultNetworkChangeTimeouts = networkChangeTimeouts{start: 5 * time.Minute, workloads: 5 * time.Minute, interval: 5 * time.Second}

// ChangeNetwork moves a single-node cluster's pod network to WireGuard.
// It requires a loadable WireGuard kernel module and no flannel-backend drop-in.
// It recovers interrupted changes first, then backs up the configuration and
// restarts k3s with fresh flannel links. Network or workload recovery failures
// trigger a rollback attempt; the backup remains until recovery succeeds.
func ChangeNetwork(client CommandRunner) error {
	return changeNetwork(client, defaultNetworkChangeTimeouts)
}

func changeNetwork(client CommandRunner, t networkChangeTimeouts) error {
	locked, release, err := lockAPIServerConfig(client)
	if err != nil {
		return err
	}
	defer release()
	client = locked

	if err := finishInterrupted(client, t); err != nil {
		return err
	}
	change, err := PlanNetworkChange(client)
	if err != nil {
		return err
	}
	if !change.Needed() {
		return nil
	}
	from, ok := flannelLinks[change.Running]
	if !ok || change.Running == "wireguard" {
		return fmt.Errorf("the server reports pod network backend %q, which kip cannot move to WireGuard", change.Running)
	}
	if err := checkNetworkChangeEligible(client); err != nil {
		return err
	}
	ready, err := readyWorkloads(client)
	if err != nil {
		return err
	}
	config, err := readK3sConfig(client)
	if err != nil {
		return err
	}
	desired, err := withFlannelBackend(config, wireguardBackend)
	if err != nil {
		return err
	}

	if _, err := client.RunStdin(backupNetworkScript(from.setting), strings.NewReader(strings.Join(ready, "\n")+"\n")); err != nil {
		return fmt.Errorf("saving the k3s configuration before the network change: %w", err)
	}
	if _, err := client.Run("systemctl stop k3s"); err != nil {
		return restoreAfter(client, t, ready, fmt.Errorf("stopping k3s: %w", err))
	}
	if _, err := client.Run(resetFlannelCommand); err != nil {
		return restoreAfter(client, t, ready, fmt.Errorf("removing the old flannel link: %w", err))
	}
	if _, err := client.RunStdin(replaceFileScript(k3sConfigPath), strings.NewReader(desired)); err != nil {
		return restoreAfter(client, t, ready, fmt.Errorf("writing %s: %v", k3sConfigPath, withoutOutput(err)))
	}
	if _, err := client.Run("systemctl start --no-block k3s"); err != nil {
		return restoreAfter(client, t, ready, fmt.Errorf("starting k3s: %w", err))
	}
	if err := waitForNetwork(client, "flannel-wg", ready, t); err != nil {
		return restoreAfter(client, t, ready, err)
	}
	return clearNetworkChange(client)
}

// finishInterrupted uses the saved workload readiness to decide whether to
// complete a pending change or restore the previous network.
func finishInterrupted(client CommandRunner, t networkChangeTimeouts) error {
	pending, err := NetworkChangePending(client)
	if err != nil || !pending {
		return err
	}
	ready, err := savedReadyWorkloads(client)
	if err != nil {
		return err
	}
	if running, err := runningFlannelBackend(client); err == nil && running == "wireguard" && networkIsUp(client, "flannel-wg") {
		if err := waitForWorkloads(client, ready, t); err == nil {
			return clearNetworkChange(client)
		}
	}
	if err := restoreNetwork(client, t, ready); err != nil {
		return fmt.Errorf("undoing the unfinished network change: %w", err)
	}
	return nil
}

// savedReadyWorkloads reads the workloads the change found ready before it
// stopped k3s. The backup writes the list before the record, so a record
// without one is refused rather than read as "nothing was ready".
func savedReadyWorkloads(client CommandRunner) ([]string, error) {
	path := networkChangeBackup + "/ready-workloads"
	out, err := client.Run(fmt.Sprintf("if [ -f %[1]s ]; then cat %[1]s; else echo %[2]s; fi", path, noSnapshotMarker))
	if err != nil {
		return nil, fmt.Errorf("reading the saved workload list: %w", err)
	}
	if strings.Contains(out, noSnapshotMarker) {
		return nil, fmt.Errorf("%s exists but %s does not, so kip cannot tell which workloads must recover. Inspect the server; the configuration backup is in %s", networkChangeRecord, path, networkChangeBackup)
	}
	return strings.Fields(out), nil
}

const noSnapshotMarker = "KIPPER_NO_WORKLOAD_SNAPSHOT"

func checkNetworkChangeEligible(client CommandRunner) error {
	out, err := client.Run("kubectl get nodes -o name")
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}
	if nodes := strings.Fields(out); len(nodes) != 1 {
		return fmt.Errorf("the cluster has %d nodes; kip moves the pod network to WireGuard only while the server is the only node. Remove the other nodes with 'kip node remove' (or 'kubectl delete node' for a node kip did not add), then re-run", len(nodes))
	}
	if _, err := client.Run("modprobe wireguard"); err != nil {
		return fmt.Errorf("the server cannot load the wireguard kernel module, so the pod network stays as it is: %w", err)
	}
	out, err = client.Run("grep -l '^flannel-backend:' /etc/rancher/k3s/config.yaml.d/*.yaml 2>/dev/null || true")
	if err != nil {
		return fmt.Errorf("checking the k3s drop-ins: %w", err)
	}
	if found := strings.TrimSpace(out); found != "" {
		return fmt.Errorf("%s sets flannel-backend, which would override the change; remove that line and re-run", found)
	}
	return nil
}

// resetFlannelCommand clears stale links and subnet state before either
// backend starts. Call it with k3s stopped so flannel cannot recreate them.
const resetFlannelCommand = `set -e
for link in flannel.1 flannel-wg; do
  if ip link show "$link" >/dev/null 2>&1; then ip link delete "$link"; fi
  if ip link show "$link" >/dev/null 2>&1; then echo "$link is still present"; exit 1; fi
done
rm -f /run/flannel/subnet.env
[ ! -e /run/flannel/subnet.env ]`

// backupNetworkScript copies the files the change replaces, saves the ready
// workloads it reads on stdin, and writes the record naming the backend to
// return to.
func backupNetworkScript(fromSetting string) string {
	return strings.NewReplacer(
		"RECORD", networkChangeRecord,
		"BACKUP", networkChangeBackup,
		"CONFIG", k3sConfigPath,
		"DROPIN", k3sNetworkConfigPath,
		"FROM", fromSetting,
	).Replace(`set -e
umask 077
rm -rf BACKUP
mkdir -p BACKUP
cp -p "$(readlink -f CONFIG)" BACKUP/config.yaml
if [ -e DROPIN ]; then cp -p DROPIN BACKUP/kipper-network.yaml; else touch BACKUP/kipper-network.absent; fi
cat > BACKUP/ready-workloads
printf 'from=FROM\n' > RECORD.tmp
mv RECORD.tmp RECORD`)
}

var restoreScript = strings.NewReplacer(
	"BACKUP", networkChangeBackup,
	"CONFIG", k3sConfigPath,
	"DROPIN", k3sNetworkConfigPath,
).Replace(`set -e
[ -f BACKUP/config.yaml ] || { echo "no backup of the k3s configuration in BACKUP"; exit 1; }
cp -p BACKUP/config.yaml "$(readlink -f CONFIG)"
if [ -f BACKUP/kipper-network.absent ]; then rm -f DROPIN; else cp -p BACKUP/kipper-network.yaml DROPIN; fi`)

// restoreAfter undoes a failed change and reports cause together with how the
// restore went.
func restoreAfter(client CommandRunner, t networkChangeTimeouts, ready []string, cause error) error {
	if err := restoreNetwork(client, t, ready); err != nil {
		return fmt.Errorf("the network change failed: %w; restoring the previous configuration also failed: %v. The backup is in %s on the server", cause, err, networkChangeBackup)
	}
	return fmt.Errorf("the network change failed and the previous configuration is back in place: %w", cause)
}

// restoreNetwork stops k3s, puts the backed-up configuration back, resets
// flannel and starts k3s on the recorded backend, then clears the record.
func restoreNetwork(client CommandRunner, t networkChangeTimeouts, ready []string) error {
	record, err := client.Run("cat " + networkChangeRecord)
	if err != nil {
		return fmt.Errorf("reading %s: %w", networkChangeRecord, err)
	}
	from := ""
	for _, line := range strings.Split(record, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "from="); ok {
			from = v
		}
	}
	link := ""
	for _, l := range flannelLinks {
		if l.setting == from {
			link = l.link
		}
	}
	if link == "" {
		return fmt.Errorf("%s names no backend kip can restore (%q)", networkChangeRecord, from)
	}
	if _, err := client.Run("systemctl stop k3s"); err != nil {
		return fmt.Errorf("stopping k3s: %w", err)
	}
	if _, err := client.Run(resetFlannelCommand); err != nil {
		return fmt.Errorf("removing the flannel links: %w", err)
	}
	if _, err := client.Run(restoreScript); err != nil {
		return fmt.Errorf("restoring the configuration: %w", err)
	}
	if _, err := client.Run("systemctl start --no-block k3s"); err != nil {
		return fmt.Errorf("starting k3s: %w", err)
	}
	if err := waitForNetwork(client, link, ready, t); err != nil {
		return err
	}
	return clearNetworkChange(client)
}

func clearNetworkChange(client CommandRunner) error {
	if _, err := client.Run(fmt.Sprintf("rm -rf %s %s", networkChangeRecord, networkChangeBackup)); err != nil {
		return fmt.Errorf("clearing the network change record: %w", err)
	}
	return nil
}

// networkUpCheck prints networkUpMarker once k3s is active, the API answers,
// the flannel link exists and the server node is Ready.
func networkUpCheck(link string) string {
	return boundedCommand(fmt.Sprintf(`systemctl is-active --quiet k3s || exit 0
kubectl --request-timeout=10s get --raw /readyz >/dev/null 2>&1 || exit 0
ip link show %s >/dev/null 2>&1 || exit 0
[ "$(kubectl --request-timeout=10s get nodes -l %s=true -o jsonpath='{.items[*].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" = True ] || exit 0
echo %s`, link, serverNodeLabel, networkUpMarker))
}

// boundedCommand limits each probe to 30 seconds. A polling deadline can
// still be exceeded by the final probe and sleep interval.
func boundedCommand(script string) string {
	return "timeout 30 bash -c " + shellQuote(script)
}

func networkIsUp(client CommandRunner, link string) bool {
	out, err := client.Run(networkUpCheck(link))
	return err == nil && strings.Contains(out, networkUpMarker)
}

// waitForNetwork waits for k3s to serve on the given flannel link, then for
// every workload in ready to be ready again.
func waitForNetwork(client CommandRunner, link string, ready []string, t networkChangeTimeouts) error {
	deadline := time.Now().Add(t.start)
	for !networkIsUp(client, link) {
		if time.Now().After(deadline) {
			return fmt.Errorf("k3s did not come back with flannel link %s within %s", link, t.start)
		}
		time.Sleep(t.interval)
	}
	return waitForWorkloads(client, ready, t)
}

func waitForWorkloads(client CommandRunner, ready []string, t networkChangeTimeouts) error {
	deadline := time.Now().Add(t.workloads)
	for {
		now, err := readyWorkloads(client)
		var missing []string
		if err == nil {
			missing = notReadyAgain(ready, now)
		}
		if err == nil && len(missing) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("reading workloads after the restart: %w", err)
			}
			return fmt.Errorf("workloads that were ready before did not recover within %s: %s", t.workloads, strings.Join(missing, ", "))
		}
		time.Sleep(t.interval)
	}
}

// readyWorkloads lists, as "Kind/namespace/name", every Deployment,
// StatefulSet and DaemonSet that wants at least one pod and has all of them
// ready.
func readyWorkloads(client CommandRunner) ([]string, error) {
	out, err := client.Run(boundedCommand(`kubectl --request-timeout=20s get deployments,statefulsets,daemonsets -A -o jsonpath='{range .items[*]}{.kind}/{.metadata.namespace}/{.metadata.name} {.spec.replicas}{.status.desiredNumberScheduled} {.status.readyReplicas}{.status.numberReady}{"\n"}{end}'`))
	if err != nil {
		return nil, fmt.Errorf("listing workloads: %w", err)
	}
	var ready []string
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(strings.TrimSpace(line), " ")
		if len(parts) != 3 {
			continue
		}
		want, err := strconv.Atoi(parts[1])
		if err != nil || want == 0 {
			continue
		}
		have, _ := strconv.Atoi(parts[2])
		if have >= want {
			ready = append(ready, parts[0])
		}
	}
	return ready, nil
}

func notReadyAgain(before, now []string) []string {
	present := make(map[string]bool, len(now))
	for _, w := range now {
		present[w] = true
	}
	var missing []string
	for _, w := range before {
		if !present[w] {
			missing = append(missing, w)
		}
	}
	return missing
}

// withFlannelBackend replaces top-level backend lines, preserving other YAML
// lines and normalizing trailing newlines.
func withFlannelBackend(config, backend string) (string, error) {
	line := "flannel-backend: " + backend
	lines := strings.Split(strings.TrimRight(config, "\n"), "\n")
	replaced := false
	for i, l := range lines {
		if strings.HasPrefix(l, "flannel-backend:") {
			lines[i] = line
			replaced = true
		}
	}
	if !replaced {
		lines = append(lines, line)
	}
	out := strings.Join(lines, "\n") + "\n"
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil || doc["flannel-backend"] != backend {
		return "", fmt.Errorf("setting flannel-backend in %s would not leave a valid config, so it was not changed", k3sConfigPath)
	}
	return out, nil
}

// replaceFileScript writes stdin to path through a staged file in the same
// directory, keeping a symlinked path's target and its mode and owner. The
// content travels on stdin because a k3s config can hold credentials.
func replaceFileScript(path string) string {
	return strings.ReplaceAll(`set -e
target=$(readlink -f TARGET)
mode=$(stat -c %a "$target")
owner=$(stat -c %u:%g "$target")
staged=$(mktemp "$(dirname "$target")"/.kipper-XXXXXX)
trap 'rm -f "$staged"' EXIT
cat > "$staged"
chmod "$mode" "$staged"
chown "$owner" "$staged"
mv "$staged" "$target"
trap - EXIT`, "TARGET", path)
}
