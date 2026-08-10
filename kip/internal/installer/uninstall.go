package installer

import (
	"fmt"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

// UninstallHost removes Kipper and its underlying k3s cluster from the
// remote server. It runs k3s's own uninstall script (which removes the
// k3s binary, systemd units, containers, and CNI state) and then sweeps
// the data directories Kipper writes outside k3s's purview: Longhorn
// volumes, Rancher state, k3s config, Zot blobs, and the optional AI
// bundle data.
//
// The function is best-effort about missing files: a host with no k3s
// installed yields no error from the killall/uninstall steps, and a
// missing data directory is also not an error. Real SSH transport
// failures bubble up.
//
// Host firewall rules and OS hardening (rpcbind disabling etc.) are
// intentionally not reverted. k3s-uninstall.sh already removes its own
// UFW entries; anything the user added by hand or for other workloads
// stays alone.
func UninstallHost(client *ssh.Client) error {
	// k3s-killall.sh stops k3s cleanly before uninstall. If the script
	// does not exist, k3s was not installed via the standard installer
	// and we silently skip. If it exists, we run it and propagate its
	// exit status — a half-failed killall means an unhealthy host and
	// we should not pretend to succeed.
	if _, err := client.Run("if [ -x /usr/local/bin/k3s-killall.sh ]; then /usr/local/bin/k3s-killall.sh; fi"); err != nil {
		return fmt.Errorf("running k3s-killall.sh: %w", err)
	}

	if _, err := client.Run("if [ -x /usr/local/bin/k3s-uninstall.sh ]; then /usr/local/bin/k3s-uninstall.sh; fi"); err != nil {
		return fmt.Errorf("running k3s-uninstall.sh: %w", err)
	}

	// Longhorn surfaces volumes as iSCSI targets via the in-cluster
	// iscsid + device-mapper stack. k3s-uninstall stops the cluster
	// but does not log out the iSCSI sessions or remove the
	// /dev/longhorn/* device nodes, so a subsequent install on the
	// same host inherits stale block devices that confuse the new
	// Longhorn instance.
	//
	// We log out only sessions whose target IQN matches Longhorn's
	// well-known prefix (iqn.2019-10.io.longhorn:) so unrelated iSCSI
	// storage the user may have attached to the host stays connected.
	// Similarly we leave multipath maps alone — flushing host-wide
	// would yank real SAN devices unrelated to Kipper.
	logoutLonghorn := `for iqn in $(iscsiadm -m session 2>/dev/null | awk '/iqn\.2019-10\.io\.longhorn:/{print $4}'); do
  iscsiadm -m node -T "$iqn" -u 2>/dev/null || true
done`
	if _, err := client.Run(logoutLonghorn); err != nil {
		return fmt.Errorf("logging out longhorn iscsi sessions: %w", err)
	}

	// The install added a kipper-managed blacklist block to
	// /etc/multipath.conf. Strip it on uninstall so the host returns
	// to its pre-kip multipath state.
	stripMultipath := fmt.Sprintf("if [ -f /etc/multipath.conf ]; then %s; fi", stripKipperMultipathBlockAwk)
	if _, err := client.Run(stripMultipath); err != nil {
		return fmt.Errorf("stripping kipper block from /etc/multipath.conf: %w", err)
	}
	if _, err := client.Run("systemctl reload multipathd 2>/dev/null || true"); err != nil {
		return fmt.Errorf("reloading multipathd: %w", err)
	}

	// Sweep the data Kipper writes outside k3s's purview. Each path
	// is independent; rm -rf is idempotent on missing paths so a
	// single command covers them all. /dev/longhorn holds the device
	// nodes Longhorn created; removing them ensures the next install
	// starts from a clean slate.
	// The dex loopback pin lives in /etc/hosts, outside every swept
	// directory; a surviving pin would trap future host processes resolving
	// the old cluster domain at 127.0.0.1 forever.
	sweep := "sed -i '/# kipper-dex-pin/d' /etc/hosts; rm -rf /var/lib/longhorn /var/lib/rancher /etc/rancher /var/lib/zot /var/lib/kipper-ai /dev/longhorn"
	if _, err := client.Run(sweep); err != nil {
		return fmt.Errorf("sweeping kipper data directories: %w", err)
	}

	return nil
}
