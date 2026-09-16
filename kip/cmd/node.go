package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

var nodeCmd = &cobra.Command{
	Use:   "node",
	Short: "Manage cluster nodes",
}

var nodeAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a worker node to the cluster",
	RunE:  runNodeAdd,
}

var nodeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all nodes in the cluster",
	RunE:  runNodeList,
}

func init() {
	nodeAddCmd.Flags().String("host", "", "IP address or hostname of the node to add")
	nodeAddCmd.Flags().String("ssh-key", "", "path to SSH private key")
	_ = nodeAddCmd.MarkFlagRequired("host")

	nodeCmd.AddCommand(nodeAddCmd)
	nodeCmd.AddCommand(nodeListCmd)
	rootCmd.AddCommand(nodeCmd)
}

func runNodeAdd(cmd *cobra.Command, args []string) error {
	workerHost, _ := cmd.Flags().GetString("host")
	sshKey, _ := cmd.Flags().GetString("ssh-key")

	if sshKey == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolving home directory: %w", err)
		}
		sshKey = filepath.Join(home, ".ssh", "id_ed25519")
	}

	cluster, _, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	// Connect to the master node
	fmt.Printf("\n  Connecting to master (%s)...\n", cluster.Host)
	masterClient, err := ssh.Dial(ssh.Config{
		Host:    cluster.Host,
		User:    "root",
		KeyPath: sshKey,
	})
	if err != nil {
		return fmt.Errorf("connecting to master: %w", err)
	}
	defer func() { _ = masterClient.Close() }()
	fmt.Printf("  ✔  Connected to master\n")

	// Connect to the worker node
	fmt.Printf("  Connecting to worker (%s)...\n", workerHost)
	workerClient, err := ssh.Dial(ssh.Config{
		Host:    workerHost,
		User:    "root",
		KeyPath: sshKey,
	})
	if err != nil {
		return fmt.Errorf("connecting to worker: %w", err)
	}
	defer func() { _ = workerClient.Close() }()
	fmt.Printf("  ✔  Connected to worker\n\n")

	// Read the identity used to confirm registration before changing the host.
	identity, err := installer.ReadHostIdentity(workerClient)
	if err != nil {
		return fmt.Errorf("reading what %s reports about itself: %w", workerHost, err)
	}

	// Before the join, so a failed write cannot leave a node in the cluster
	// running Longhorn volumes with nothing stopping an unattended upgrade from
	// restarting iscsid underneath them.
	fmt.Printf("  Configuring host restarts for storage...\n")
	if err := installer.ConfigureStorageRestarts(workerClient); err != nil {
		return fmt.Errorf("configuring storage restarts on %s: %w", workerHost, err)
	}
	fmt.Printf("  ✔  Storage-path restarts deferred on %s\n\n", workerHost)

	fmt.Printf("  Joining worker node to cluster...\n")
	nodeName, err := installer.JoinWorkerNode(masterClient, workerClient, cluster.Host, identity)
	if err != nil {
		return err
	}
	fmt.Printf("  ✔  Worker node %s joined the cluster as %s\n\n", workerHost, nodeName)

	// The stamp goes on after registration, because the node object it annotates
	// does not exist until then. A failure here leaves the host configured and
	// the record missing, which reads as uncovered: the safe direction.
	if err := installer.StampStorageRestarts(masterClient, nodeName, identity.MachineID); err != nil {
		fmt.Printf("  ⚠   could not record host configuration for %s: %v\n      The host is configured; 'kip node repair-host' records it.\n\n", nodeName, err)
	}

	// Wait for a published IPv4 address so the refreshed build egress policy
	// can block access to this node. Address publication can lag registration.
	// On timeout, refresh the available exclusions and return an error.
	addressMissing := false
	fmt.Printf("  Waiting for %s to publish its address...\n", nodeName)
	if err := installer.WaitForNodeToPublishAddress(masterClient, nodeName, 120*time.Second); err != nil {
		addressMissing = true
		fmt.Printf("  ⚠   %s has no address in the cluster yet: %v\n", nodeName, err)
	}

	fmt.Printf("  Updating build isolation for the new node...\n")
	if err := installer.InstallBuildIsolation(masterClient); err != nil {
		// The node has already joined, so failing the command now would misreport
		// what happened. The egress policy is closed rather than left permissive
		// when this fails (see installer.InstallBuildIsolation), so the message
		// has to say that builds stop until it is fixed.
		fmt.Printf("  ⚠   could not update build isolation: %v\n      Builds stay blocked until this is resolved. Fix the cause, then run 'kip upgrade'.\n\n", err)
	} else {
		fmt.Printf("  ✔  Build isolation updated\n\n")
	}

	if addressMissing {
		return fmt.Errorf("%s joined the cluster, but published no address in time, so the build egress policy does not yet block pods from reaching it; run 'kip upgrade' once 'kip node list' shows its address", nodeName)
	}

	return nil
}

func runNodeList(cmd *cobra.Command, args []string) error {
	_, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	nodes, err := client.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	fmt.Printf("\n  %-20s %-8s %-10s %-14s %s\n", "NAME", "ROLE", "STATUS", "VERSION", "IP")
	for _, node := range nodes {
		fmt.Printf("  %-20s %-8s %-10s %-14s %s\n",
			node.Name, node.Role, node.Status, node.Version, node.IP)
	}
	fmt.Println()

	return nil
}
