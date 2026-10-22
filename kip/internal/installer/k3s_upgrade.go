package installer

import (
	"fmt"
	"io"
	"time"
)

// PinnedK3sVersion returns the k3s release this kip installs and upgrades to.
func PinnedK3sVersion() string {
	return k3sVersion
}

// serverInstallCommand builds the pinned k3s install command in server mode.
// With forceRestart, the command restarts k3s even if its files are unchanged.
func serverInstallCommand(forceRestart bool) string {
	force := ""
	if forceRestart {
		force = "INSTALL_K3S_FORCE_RESTART=true "
	}
	return fmt.Sprintf("curl -sfL https://get.k3s.io | %sINSTALL_K3S_VERSION='%s' INSTALL_K3S_EXEC='server' sh -", force, k3sVersion)
}

// planServerK3sUpgrade reports whether a server running installed needs the
// install script to reach pin. It returns an error when k3s is absent, newer
// than pin, or more than one Kubernetes minor behind, because kip prevents
// downgrades and requires Kubernetes upgrades one minor at a time.
func planServerK3sUpgrade(installed, pin string) (bool, error) {
	if installed == "" {
		return false, fmt.Errorf("k3s is not installed on the server, so there is nothing to upgrade. Set up the cluster with kip install")
	}
	iv, err := parseK3sVersion(installed)
	if err != nil {
		return false, err
	}
	pv, err := parseK3sVersion(pin)
	if err != nil {
		return false, err
	}
	newer, err := k3sVersionNewer(installed, pin)
	if err != nil {
		return false, err
	}
	if newer {
		return false, fmt.Errorf("the server runs k3s %s, newer than the %s this kip installs. Nothing was changed. Upgrade kip to manage this cluster's k3s", installed, pin)
	}
	if iv[0] != pv[0] || pv[1]-iv[1] > 1 {
		return false, fmt.Errorf("the server runs k3s %s, more than one Kubernetes minor behind the %s this kip installs. Nothing was changed. Kubernetes upgrades one minor at a time, so run kip upgrade --k3s with a kip release that installs the next minor first", installed, pin)
	}
	return installed != pin, nil
}

// K3sUpgradePlan describes the changes needed to reach the pinned k3s release.
// Server is the running release. Workers lists names in input record order.
type K3sUpgradePlan struct {
	Server     string
	MoveServer bool
	Workers    []string
}

// PlanK3sUpgrade plans changes from node records and kubelet versions keyed by
// Node name. It uses the recorded server's Node version, or fallback when no
// server is recorded. Workers includes only workers recorded as ready whose
// versions differ from the pin, in record order. An empty list means no worker
// version changes are needed, regardless of Node readiness.
//
// It returns an error for invalid versions, an unsupported server upgrade,
// worker downgrades, a newly excessive version gap, joining workers or missing
// Nodes for the server or ready workers. Existing excessive gaps are left to
// the worker upgrade. The rerun command appears in worker recovery instructions.
func PlanK3sUpgrade(records []NodeRecord, kubelets map[string]string, fallback, rerun string) (K3sUpgradePlan, error) {
	running := fallback
	if server, ok := ServerRecord(records); ok {
		v, found := kubelets[server.Name]
		if !found {
			return K3sUpgradePlan{}, fmt.Errorf("the server %s has no Node in the cluster, so its k3s version is unknown", server.Name)
		}
		running = v
	}
	move, err := planServerK3sUpgrade(running, k3sVersion)
	if err != nil {
		return K3sUpgradePlan{}, err
	}
	versions := map[string]nodeVersion{}
	for name, v := range kubelets {
		versions[name] = nodeVersion{version: v}
	}
	if err := checkK3sSkew(running, records, versions, rerun); err != nil {
		return K3sUpgradePlan{}, err
	}
	plan := K3sUpgradePlan{Server: running, MoveServer: move}
	for _, r := range records {
		// A joining worker's Node name is provisional, so its version cannot be
		// read by name, and the join installs the server's version of the moment.
		if r.Role == NodeRoleWorker && r.State == NodeStateJoining {
			return K3sUpgradePlan{}, fmt.Errorf("worker %s is still joining. Nothing was changed. Finish the join with 'kip node add %s' or remove it, then run %s again", r.Name, r.Host, rerun)
		}
		if r.Role != NodeRoleWorker || r.State != NodeStateReady {
			continue
		}
		v, found := kubelets[r.Name]
		if !found {
			return K3sUpgradePlan{}, fmt.Errorf("worker %s is recorded as ready but has no Node in the cluster. Nothing was changed. Rejoin it with 'kip node add %s', then run %s again", r.Name, r.Host, rerun)
		}
		if v != k3sVersion {
			plan.Workers = append(plan.Workers, r.Name)
		}
	}
	return plan, nil
}

// CheckK3sUpgrade reads the server's installed k3s and every Node's version
// over SSH and plans the move onto the pinned release without changing
// anything. It uses the sole Node's version as the fallback when there is one
// Node, or the installed binary's version otherwise. Planning errors and recovery
// instructions follow PlanK3sUpgrade. A binary newer than the pin is refused
// even when the running server is older, to prevent downgrading the binary.
func CheckK3sUpgrade(client CommandRunner, records []NodeRecord, rerun string) (K3sUpgradePlan, error) {
	binary, err := installedK3sVersion(client)
	if err != nil {
		return K3sUpgradePlan{}, err
	}
	if binary == "" {
		return K3sUpgradePlan{}, fmt.Errorf("k3s is not installed on the server, so there is nothing to upgrade. Set up the cluster with kip install")
	}
	if newer, err := k3sVersionNewer(binary, k3sVersion); err != nil {
		return K3sUpgradePlan{}, err
	} else if newer {
		return K3sUpgradePlan{}, fmt.Errorf("the server has k3s %s installed, newer than the %s this kip installs. Nothing was changed. Upgrade kip to manage this cluster's k3s", binary, k3sVersion)
	}
	versions, err := readNodeVersions(client)
	if err != nil {
		return K3sUpgradePlan{}, err
	}
	kubelets := map[string]string{}
	fallback := binary
	for name, v := range versions {
		kubelets[name] = v.version
		// For a single-node cluster with no server record, use the running
		// version so an interrupted binary replacement cannot hide version skew.
		if len(versions) == 1 {
			fallback = v.version
		}
	}
	return PlanK3sUpgrade(records, kubelets, fallback, rerun)
}

// UpgradeServerK3s checks the cluster's upgrade plan before changing the server.
// It requires a server record. If the server's Node reports an older release,
// it runs the pinned install script with a forced restart so a binary replaced
// by an interrupted attempt takes effect. It preserves the k3s configuration.
// Restarting k3s pauses the Kubernetes API while running pods keep serving.
//
// Every call waits up to five minutes for the Node to report the pinned release
// and Ready, then restores addon placement that k3s resets on restart. Placement
// failures are warnings written to out. Other failures return an error, with
// rerun included in install and readiness recovery instructions.
func UpgradeServerK3s(out io.Writer, client CommandRunner, records []NodeRecord, rerun string) error {
	return upgradeServerK3s(out, client, records, rerun, defaultJoinTimeouts.ready, defaultJoinTimeouts.interval)
}

func upgradeServerK3s(out io.Writer, client CommandRunner, records []NodeRecord, rerun string, timeout, interval time.Duration) error {
	plan, err := CheckK3sUpgrade(client, records, rerun)
	if err != nil {
		return err
	}
	server, ok := ServerRecord(records)
	if !ok {
		return fmt.Errorf("the node records name no server, so kip cannot confirm the server's k3s version")
	}
	if plan.MoveServer {
		_, _ = fmt.Fprintf(out, "  Upgrading k3s on the server from %s to %s. The Kubernetes API pauses while k3s restarts, and running pods keep serving...\n", plan.Server, k3sVersion)
		if hostOut, err := client.Run(serverInstallCommand(true)); err != nil {
			return fmt.Errorf("running the k3s install script: %w. Run %s again to retry", hostFailure(hostOut, err), rerun)
		}
	}

	// The Node stays Ready on the old version until the restarted kubelet posts
	// its status, so only the new version counts as the server being back. A
	// rerun waits as well, so workers never follow an unready server.
	upgraded := func() (bool, error) {
		now, err := readNodeVersions(client)
		if err != nil {
			return false, err
		}
		node := now[server.Name]
		if node.version != k3sVersion || !node.ready {
			return false, fmt.Errorf("it reports k3s %q, ready %t", node.version, node.ready)
		}
		return true, nil
	}
	if err := pollUntil(timeout, interval, upgraded); err != nil {
		return fmt.Errorf("the server did not report Ready on k3s %s within %s (%v). Run %s again once it is back", k3sVersion, timeout, err, rerun)
	}

	if err := PinK3sAddonsToServer(client); err != nil {
		_, _ = fmt.Fprintf(out, "  ⚠  %v. Run %s again to retry\n", err, rerun)
	}
	if plan.MoveServer {
		_, _ = fmt.Fprintf(out, "  ✔  Server runs k3s %s\n", k3sVersion)
	} else {
		_, _ = fmt.Fprintf(out, "  ✔  Server already runs k3s %s\n", k3sVersion)
	}
	return nil
}
