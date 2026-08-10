package installer

import (
	"fmt"
	"strings"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

const longhornVersion = "v1.7.3"

// InstallLonghorn installs Longhorn for distributed persistent storage.
func InstallLonghorn(client *ssh.Client) error {
	// Longhorn requires open-iscsi and nfs-common (for RWX shared volumes)
	if _, err := client.Run("apt-get update -qq && apt-get install -y -qq open-iscsi nfs-common"); err != nil {
		return fmt.Errorf("installing longhorn dependencies: %w", err)
	}

	// On hosts where multipath-tools is installed (Ubuntu cloud images
	// often include it), multipathd will grab every Longhorn iSCSI
	// device and hold it open as an mpath_member, so kubelet's mkfs.ext4
	// on freshly-provisioned PVCs fails with "apparently in use by the
	// system". The fix is the Longhorn-recommended blacklist of
	// SCSI devices in multipath.conf, plus a reload. No-op on hosts
	// without multipath-tools.
	if err := configureMultipathForLonghorn(client); err != nil {
		return fmt.Errorf("configuring multipathd for longhorn: %w", err)
	}

	url := fmt.Sprintf(
		"https://raw.githubusercontent.com/longhorn/longhorn/%s/deploy/longhorn.yaml",
		longhornVersion,
	)

	applyCmd := fmt.Sprintf("kubectl apply -f %s", url)
	if _, err := client.Run(applyCmd); err != nil {
		return fmt.Errorf("applying longhorn manifest: %w", err)
	}

	waitCmd := "kubectl -n longhorn-system rollout status deployment/longhorn-driver-deployer --timeout=180s"
	if _, err := client.Run(waitCmd); err != nil {
		return fmt.Errorf("waiting for longhorn: %w", err)
	}

	// Create a single-replica StorageClass for single-node clusters.
	// The default longhorn class uses 3 replicas which fails on 1 node.
	singleReplicaSC := `apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: longhorn-single
provisioner: driver.longhorn.io
allowVolumeExpansion: true
reclaimPolicy: Delete
volumeBindingMode: Immediate
parameters:
  numberOfReplicas: "1"
  staleReplicaTimeout: "30"
  fsType: ext4
  dataEngine: v1
`
	scCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", singleReplicaSC)
	if _, err := client.Run(scCmd); err != nil {
		return fmt.Errorf("creating longhorn-single StorageClass: %w", err)
	}

	return nil
}

// Fenced markers tag the block kip writes into multipath.conf so a
// future kip run can find and replace it cleanly without trampling
// rules the user added by hand. Both markers are also recognised by
// the strip awk used in the install and uninstall paths, which lets
// the install heal from a broken previous write that left a mangled
// single-line variant of the block in the file.
const (
	multipathConfStart = "# kipper-longhorn-blacklist:start"
	multipathConfEnd   = "# kipper-longhorn-blacklist:end"
)

// stripKipperMultipathBlockAwk removes any prior kipper-managed block
// from /etc/multipath.conf. Handles three cases: a well-formed
// :start/:end fence (range delete), a stray marker line from an old
// broken write (single-line literal-\n variant — delete the one
// matching line), and the absence of any prior block (no-op). Output
// is written to /tmp and atomically replaced.
const stripKipperMultipathBlockAwk = `awk '
  BEGIN { skip = 0 }
  /^# kipper-longhorn-blacklist:start/ { skip = 1; next }
  /^# kipper-longhorn-blacklist:end/   { skip = 0; next }
  /kipper-longhorn-blacklist/          { next }
  !skip                                { print }
' /etc/multipath.conf > /tmp/multipath.conf.kipper && mv /tmp/multipath.conf.kipper /etc/multipath.conf`

// configureMultipathForLonghorn installs a blacklist for Longhorn's
// iSCSI-backed block devices into /etc/multipath.conf and reloads
// multipathd. Without this, multipathd claims every Longhorn iSCSI LUN
// as an mpath member, kubelet cannot run mkfs on the underlying device,
// and freshly-provisioned PVCs hang in ContainerCreating with a
// "device apparently in use by the system" error.
//
// The blacklist targets the IET (iSCSI Enterprise Target) vendor that
// Longhorn uses, so it does not affect any real multipathed SAN devices
// the host may have. On hosts without multipath-tools installed the
// function is a no-op — multipathd absent means there is nothing to
// fight kubelet for the device.
//
// Safe to re-run: any prior kipper-managed block (including the
// mangled single-line variant from earlier versions) is stripped
// before the fresh block is appended.
func configureMultipathForLonghorn(client *ssh.Client) error {
	out, err := client.Run("command -v multipathd >/dev/null 2>&1 && echo present || echo absent")
	if err != nil {
		return fmt.Errorf("checking for multipathd: %w", err)
	}
	if strings.TrimSpace(out) != "present" {
		return nil
	}

	// Always strip any prior kip-managed block before writing a fresh
	// one. This heals from earlier kip versions that wrote a mangled
	// single-line variant (newlines escaped as literal \n by a bad
	// printf %q), and it keeps the operation idempotent — re-running
	// install never accumulates duplicate blocks.
	stripCmd := fmt.Sprintf("touch /etc/multipath.conf && %s", stripKipperMultipathBlockAwk)
	if _, err := client.Run(stripCmd); err != nil {
		return fmt.Errorf("stripping prior kipper block from /etc/multipath.conf: %w", err)
	}

	block := fmt.Sprintf(`
%s
blacklist {
    device {
        vendor "IET"
        product "VIRTUAL-DISK"
    }
}
%s
`, multipathConfStart, multipathConfEnd)
	// Heredoc, not printf %q: %q escapes newlines to literal \n which
	// the shell printf then writes as backslash-n into the file. The
	// 'KIPEOF' delimiter (quoted) prevents shell substitution inside
	// the block so the contents go through unchanged.
	appendCmd := fmt.Sprintf("cat >> /etc/multipath.conf <<'KIPEOF'\n%sKIPEOF", block)
	if _, err := client.Run(appendCmd); err != nil {
		return fmt.Errorf("appending blacklist to /etc/multipath.conf: %w", err)
	}

	// Reload multipathd so the blacklist takes effect. We do not flush
	// existing mpath maps host-wide because doing so would tear down
	// unrelated multipath devices (real SAN volumes, etc.) that have
	// nothing to do with Longhorn. The blacklist alone prevents new
	// IET-vendor (Longhorn) devices from being captured; any stale
	// pre-blacklist mappings can be flushed surgically by the caller
	// or wait for a node reboot.
	if _, err := client.Run("systemctl reload multipathd 2>/dev/null || systemctl restart multipathd"); err != nil {
		return fmt.Errorf("reloading multipathd: %w", err)
	}
	return nil
}
