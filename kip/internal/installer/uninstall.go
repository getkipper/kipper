package installer

import (
	"fmt"
)

// UninstallHost runs k3s's uninstall scripts, removes Kipper's Longhorn
// integration and Dex hosts pin, and deletes cluster data, optional AI data,
// network recovery state and the API server configuration lock.
// Missing scripts and directories are tolerated. Host firewall policy and
// OS hardening remain configured.
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
	// after an interrupted run. Preserve /etc/kipper because its firewall
	// ownership records still apply to the retained firewall rules.
	sweep := fmt.Sprintf("sed -i '/# kipper-dex-pin/d' /etc/hosts; rm -rf /var/lib/longhorn /var/lib/rancher /etc/rancher /var/lib/zot /var/lib/kipper-ai /var/lib/kipper %s %s.guard /dev/longhorn",
		apiServerLockPath, apiServerLockPath)
	if _, err := client.Run(sweep); err != nil {
		return fmt.Errorf("sweeping kipper data directories: %w", err)
	}

	return nil
}
