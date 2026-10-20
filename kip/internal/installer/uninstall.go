package installer

import (
	"fmt"
	"strings"
)

// firewallChangePendingMarker signals that cleanup preserved firewall state.
const firewallChangePendingMarker = "kipper-firewall-change-pending"

// clearKipperStateCommand builds a script to clear /etc/kipper beneath root
// while holding the firewall rollback locks. A reservation or an active or
// activating rollback unit preserves all state and prints firewallChangePendingMarker.
// Firewall ownership and fail-open records stay with the retained firewall rules, and
// lock files stay so all waiters keep using the same inodes. Lock, unit lookup
// and removal failures make the script exit non-zero.
func clearKipperStateCommand(root string) string {
	dir := root + firewalldStateDir
	keep := ""
	for _, name := range []string{"firewall-managed", "firewalld-managed", "ufw.lock", "firewalld.lock", "access.lock", ufwFailOpenFile} {
		keep += " ! -name " + name
	}
	return `if [ -d ` + dir + ` ]; then ` +
		`exec 9>>` + dir + `/ufw.lock 8>>` + root + firewalldLock + ` && flock -w 120 9 && flock -w 120 8 || exit 1; ` +
		`units=$(systemctl list-units --state=active,activating --plain --no-legend '` + ufwRollbackUnit + `*' '` + firewalldRollbackUnit + `*') || exit 1; ` +
		`if [ -e ` + dir + `/ufw-run ] || [ -e ` + dir + `/firewalld-run ] || [ -n "$units" ]; then ` +
		`echo ` + firewallChangePendingMarker + `; ` +
		`else find ` + dir + ` -mindepth 1 -maxdepth 1` + keep + ` -exec rm -rf {} + || exit 1; fi; fi`
}

// UninstallHost runs k3s's uninstall scripts, removes the host firewall rules
// kip added, Kipper's Longhorn integration and Dex hosts pin, and deletes
// cluster data, optional AI data, network recovery state and the API server
// configuration lock. Missing scripts and directories are tolerated. A firewall
// kip manages stays running or stopped with SSH allowed. Failures to remove
// firewall rules produce warnings and cleanup continues. The operator access
// table is removed after k3s is gone, because a reused host must not boot with
// the list of a cluster that no longer exists. OS hardening remains configured.
// State in /etc/kipper is cleared except for firewall ownership records,
// fail-open records and lock files. A firewall reservation or an active or
// activating rollback unit preserves all of that state and prints a notice.
// Failure to lock, inspect or clear that state returns an error.
func UninstallHost(client CommandRunner) error {
	// Stop k3s before removing its data. A failed shutdown aborts the uninstall
	// so cleanup can be retried after the failure is resolved.
	if _, err := client.Run("if [ -x /usr/local/bin/k3s-killall.sh ]; then /usr/local/bin/k3s-killall.sh; fi"); err != nil {
		return fmt.Errorf("running k3s-killall.sh: %w", err)
	}

	// Try both scripts to support server and worker hosts.
	for _, script := range []string{"k3s-uninstall.sh", "k3s-agent-uninstall.sh"} {
		if _, err := client.Run(fmt.Sprintf("if [ -x /usr/local/bin/%[1]s ]; then /usr/local/bin/%[1]s; fi", script)); err != nil {
			return fmt.Errorf("running %s: %w", script, err)
		}
	}

	if acted, err := removeKipperFirewall(client); err != nil {
		fmt.Printf("  ⚠  kip's firewall rules may still be on this host: %v\n", err)
	} else if acted {
		fmt.Printf("  ✔  kip's firewall rules removed. The firewall's running state is unchanged, with SSH allowed\n")
	}

	// Clear stale Longhorn sessions before a future install. Restrict logout
	// to Longhorn target IQNs and preserve multipath maps so unrelated host
	// storage stays connected.
	logoutLonghorn := `for iqn in $(iscsiadm -m session 2>/dev/null | awk '/iqn\.2019-10\.io\.longhorn:/{print $4}'); do
  iscsiadm -m node -T "$iqn" -u 2>/dev/null || true
done`
	if _, err := client.Run(logoutLonghorn); err != nil {
		return fmt.Errorf("logging out longhorn iscsi sessions: %w", err)
	}

	// Remove Kipper's Longhorn blacklist so it no longer affects multipath
	// device discovery on the host.
	stripMultipath := fmt.Sprintf("if [ -f /etc/multipath.conf ]; then %s; fi", stripKipperMultipathBlockAwk)
	if _, err := client.Run(stripMultipath); err != nil {
		return fmt.Errorf("stripping kipper block from /etc/multipath.conf: %w", err)
	}
	if _, err := client.Run("systemctl reload multipathd 2>/dev/null || true"); err != nil {
		return fmt.Errorf("reloading multipathd: %w", err)
	}

	// Clear residual data and device nodes for the next install, and remove
	// the Dex pin so the old hostname can resolve normally. Removing network
	// recovery state in /var/lib/kipper prevents recovery of the deleted cluster.
	// Removing the configuration lock lets the next install proceed immediately
	// after an interrupted run.
	sweep := fmt.Sprintf("sed -i '/# kipper-dex-pin/d' /etc/hosts; rm -rf /var/lib/longhorn /var/lib/rancher /etc/rancher /var/lib/zot /var/lib/kipper-ai /var/lib/kipper %s %s.guard /dev/longhorn",
		apiServerLockPath, apiServerLockPath)
	if _, err := client.Run(sweep); err != nil {
		return fmt.Errorf("sweeping kipper data directories: %w", err)
	}

	if err := RemoveAccessGuard(client); err != nil {
		return err
	}

	// Preserve state needed by pending firewall rollback jobs.
	out, err := client.Run(clearKipperStateCommand(""))
	if err != nil {
		return fmt.Errorf("clearing /etc/kipper: %w", err)
	}
	if strings.Contains(out, firewallChangePendingMarker) {
		fmt.Printf("  ℹ  A firewall change is pending, so state in /etc/kipper was preserved.\n")
	}

	return nil
}
