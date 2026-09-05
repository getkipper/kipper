package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

var nodeRepairHostCmd = &cobra.Command{
	Use:   "repair-host",
	Short: "Apply host configuration to a node that predates it",
	Long: `Writes the host configuration a node should have and records what was applied.

A node added by an older kip has nothing stopping an unattended upgrade from
restarting iscsid, which fails every Longhorn volume on it: the block device
goes, the filesystem above it remounts read-only, and whatever was writing to it
crash-loops until its pod is recreated.

This applies the same configuration kip install and kip node add now apply,
using the same connection details. Running it against a node that already has
it changes nothing.

Nothing here converges the fleet on its own. It repairs the node you name.`,
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

	if sshKey == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolving home directory: %w", err)
		}
		sshKey = filepath.Join(home, ".ssh", "id_rsa")
	}

	cluster, _, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	fmt.Printf("\n  Connecting to %s...\n", nodeHost)
	nodeClient, err := ssh.Dial(ssh.Config{Host: nodeHost, User: "root", KeyPath: sshKey})
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", nodeHost, err)
	}
	defer func() { _ = nodeClient.Close() }()

	if err := installer.ConfigureStorageRestarts(nodeClient); err != nil {
		return fmt.Errorf("configuring storage restarts on %s: %w", nodeHost, err)
	}
	fmt.Printf("  ✔  Storage-path restarts deferred\n")

	machineID, err := installer.ReadMachineID(nodeClient)
	if err != nil {
		fmt.Printf("  ⚠   %s has no machine id: %v\n      The host is configured; kip status cannot confirm it.\n\n", nodeHost, err)
		return nil
	}

	// The record goes on the node object, which is reached through the control
	// plane rather than the node being repaired: a worker holds no kubectl.
	masterClient, err := ssh.Dial(ssh.Config{Host: cluster.Host, User: "root", KeyPath: sshKey})
	if err != nil {
		return fmt.Errorf("connecting to the control plane to record it: %w", err)
	}
	defer func() { _ = masterClient.Close() }()

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
