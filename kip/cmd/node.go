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

	fmt.Printf("  Joining worker node to cluster...\n")
	if err := installer.JoinWorkerNode(masterClient, workerClient, cluster.Host); err != nil {
		return err
	}
	fmt.Printf("  ✔  Worker node %s joined the cluster\n\n", workerHost)

	// Read the worker's own node IP so the registration wait can match this
	// specific node by address, which also works on an idempotent re-run against
	// an already-registered worker. The join has already succeeded, so a failure
	// here is a warning, not an abort.
	workerIP, err := installer.WorkerNodeIP(workerClient)
	if err != nil {
		fmt.Printf("  ⚠   could not determine %s's node IP: %v\n      Run 'kip upgrade' once it is Ready to add its IP to the build egress policy.\n\n", workerHost, err)
		return nil
	}

	// Re-apply build isolation so the new node's IP joins the egress
	// NetworkPolicy's deny list: a build pod must not reach the new node's
	// public IP on 80/443 (its ingress, host-port, or management services).
	// Wait for the node to publish an address first — join, registration, and
	// address publication are asynchronous, so refreshing immediately would race
	// and omit its IP.
	fmt.Printf("  Waiting for %s to register with the cluster...\n", workerHost)
	if err := installer.WaitForNodeAddress(masterClient, workerIP, 120*time.Second); err != nil {
		fmt.Printf("  ⚠   %s has not registered yet: %v\n      Run 'kip upgrade' once it is Ready to add its IP to the build egress policy.\n\n", workerHost, err)
		return nil
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
