package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

var nodeRepairHostCmd = &cobra.Command{
	Use:   "repair-host",
	Short: "Apply host configuration to a node that predates it",
	Long: `Turns off NFS directory delegations to keep Longhorn's shared volumes
accessible on Linux 7.0 and later. Longhorn 1.7.3's shared volume server rejects
these requests. On Ubuntu or Debian, it then configures needrestart to defer
storage-service restarts and records that configuration on the Kubernetes node
object. Deferring restarts protects Longhorn volumes from disruptions after
library updates.

Use this for nodes added by older kip versions. It applies the same configuration
as kip install and kip node add, using the same connection details, and is safe
to repeat. Only the node specified by --host is updated.

On other OS families, the command prints storage package update guidance.
Local storage requires no storage-service restart configuration.`,
	RunE: runNodeRepairHost,
}

func init() {
	nodeRepairHostCmd.Flags().String("host", "", "IP address or hostname of the node to repair")
	nodeRepairHostCmd.Flags().String("ssh-key", "", "path to SSH private key")
	_ = nodeRepairHostCmd.MarkFlagRequired("host")

	nodeCmd.AddCommand(nodeRepairHostCmd)
}

func runNodeRepairHost(cmd *cobra.Command, args []string) error {
	nodeHost, _ := cmd.Flags().GetString("host")
	sshKey, _ := cmd.Flags().GetString("ssh-key")

	cluster, _, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	// Use the control plane's kubectl to read cluster settings and record the repair.
	masterClient, err := ssh.Dial(nodeSSH(cluster.Host, sshKey, cluster))
	if err != nil {
		return fmt.Errorf("connecting to the control plane: %w", err)
	}
	defer func() { _ = masterClient.Close() }()
	if done, err := repairHostNotNeeded(masterClient); err != nil || done {
		return err
	}

	fmt.Printf("\n  Connecting to %s...\n", nodeHost)
	nodeClient, err := ssh.Dial(nodeSSH(nodeHost, sshKey, cluster))
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", nodeHost, err)
	}
	defer func() { _ = nodeClient.Close() }()

	if err := installer.DisableNFSDirectoryDelegations(nodeClient); err != nil {
		return fmt.Errorf("repairing %s: %w", nodeHost, err)
	}
	fmt.Printf("  ✔  NFS client configured for shared volumes\n")

	host, err := installer.DetectHostOS(nodeClient)
	if err != nil {
		return fmt.Errorf("reading the OS of %s: %w", nodeHost, err)
	}
	if !host.UsesNeedrestart() {
		fmt.Printf("  ℹ  Kipper skips needrestart configuration on %s. Update storage packages\n", nodeHost)
		fmt.Printf("     during planned downtime: their package scripts may restart iscsid.\n\n")
		return nil
	}
	if err := installer.ConfigureStorageRestarts(nodeClient); err != nil {
		return fmt.Errorf("configuring storage restarts on %s: %w", nodeHost, err)
	}
	fmt.Printf("  ✔  Storage-path restarts deferred\n")

	machineID, err := installer.ReadMachineID(nodeClient)
	if err != nil {
		fmt.Printf("  ⚠   %s has no machine id: %v\n      The host is configured. kip status cannot confirm it.\n\n", nodeHost, err)
		return nil
	}

	nodeName, err := installer.NodeNameForMachine(masterClient, machineID)
	if err != nil {
		return fmt.Errorf("identifying %s: %w", nodeHost, err)
	}
	if err := installer.StampStorageRestarts(masterClient, nodeName, machineID); err != nil {
		return fmt.Errorf("recording host configuration for %s: %w", nodeName, err)
	}

	fmt.Printf("  ✔  %s recorded as configured\n\n", nodeName)
	return nil
}

// repairHostNotNeeded prints a notice and returns true for local storage.
// Storage-mode read errors stop the repair before any host changes.
func repairHostNotNeeded(master installer.CommandRunner) (bool, error) {
	mode, err := installer.ReadStorageMode(master)
	if err != nil {
		return false, err
	}
	if mode == installer.StorageLocal {
		fmt.Printf("\n  ℹ  This cluster uses local storage. Storage-service restart configuration is not needed.\n\n")
		return true, nil
	}
	return false, nil
}

// nodeSSH uses the shared SSH key precedence: flag, environment, then cluster config.
func nodeSSH(host, flagValue string, cluster *config.Cluster) ssh.Config {
	explicit, fallback := resolveSSHKey(flagValue, cluster)
	return ssh.Config{
		Host:            host,
		User:            sshUser(cluster),
		KeyPath:         explicit,
		FallbackKeyPath: fallback,
	}
}
