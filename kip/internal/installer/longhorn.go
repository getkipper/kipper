package installer

import (
	"fmt"
	"io"
	"os"
	"strings"
)

const longhornVersion = "v1.7.3"

const longhornSettingsFuncName = "add_kipper_longhorn_settings"

// longhornSettingsFunc copies the manifest from $1 to $2 with upgrade checks
// and usage metrics disabled. Drains wait for eviction of a volume's last
// healthy replica. Requiring exactly one settings header catches format changes
// before installation.
const longhornSettingsFunc = longhornSettingsFuncName + `() {
  header='  default-setting.yaml: |-'
  [ "$(grep -cxF "$header" "$1")" = 1 ] || { echo "expected exactly one default-setting block in the Longhorn manifest" >&2; return 1; }
  awk -v header="$header" '{ print } $0 == header { print "    upgrade-checker: false"; print "    allow-collecting-longhorn-usage-metrics: false"; print "    node-drain-policy: block-for-eviction-if-contains-last-replica" }' "$1" > "$2"
}`

// InstallLonghorn installs Longhorn for distributed persistent storage.
// It detects the OS so kip upgrade can run without gathering system info.
func InstallLonghorn(client CommandRunner) error {
	return InstallLonghornThen(client, nil)
}

// InstallLonghornThen installs Longhorn, calling afterPackages when non-nil
// after ensuring host packages are installed and starting required services, before
// configuring multipath or deploying Longhorn. A callback error stops installation.
func InstallLonghornThen(client CommandRunner, afterPackages func() error) error {
	host, err := DetectHostOS(client)
	if err != nil {
		return fmt.Errorf("installing longhorn dependencies: %w", err)
	}
	return installLonghorn(client, host, afterPackages)
}

func installStoragePackages(client CommandRunner, host HostOS, out io.Writer) error {
	pkgs := host.StoragePackages()
	install := host.InstallPackagesCommand(true, pkgs...)
	if install == "" {
		return fmt.Errorf("installing longhorn dependencies: kip cannot install packages on %s", host.ID)
	}
	present, err := client.Run(host.PackagesInstalledCommand(pkgs...))
	if err != nil {
		return fmt.Errorf("checking longhorn dependencies: %w", err)
	}
	if strings.TrimSpace(present) == "present" {
		return nil
	}
	_, _ = fmt.Fprint(out, packageInstallNotice(pkgs))
	if _, err := client.Run(install); err != nil {
		return fmt.Errorf("installing longhorn dependencies: %w", err)
	}
	return nil
}

func installLonghorn(client CommandRunner, host HostOS, afterPackages func() error) error {
	if err := installStoragePackages(client, host, os.Stdout); err != nil {
		return err
	}
	for _, svc := range host.StorageServices() {
		if _, err := client.Run("systemctl enable --now " + svc); err != nil {
			return fmt.Errorf("starting %s for longhorn: %w", svc, err)
		}
	}
	if afterPackages != nil {
		if err := afterPackages(); err != nil {
			return err
		}
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

	applyCmd := fmt.Sprintf(`set -e
%s
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
curl -fsSL %s -o "$dir/stock.yaml"
%s "$dir/stock.yaml" "$dir/longhorn.yaml"
kubectl apply -f "$dir/longhorn.yaml"`, longhornSettingsFunc, url, longhornSettingsFuncName)
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

// configureMultipathForLonghorn blacklists IET VIRTUAL-DISK devices so
// kubelet can format Longhorn volumes without multipathd claiming them.
// It replaces the previous Kipper block and reloads multipathd when installed.
func configureMultipathForLonghorn(client CommandRunner) error {
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
