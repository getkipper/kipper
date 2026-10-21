package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/infra"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/ssh"
	"github.com/getkipper/kipper/kip/internal/transport"
)

var nodeRepairHostCmd = &cobra.Command{
	Use:          "repair-host",
	Short:        "Apply host configuration to nodes that predate it",
	SilenceUsage: true,
	Long: `Turns off NFS directory delegations to keep Longhorn's shared volumes
accessible on Linux 7.0 and later. Longhorn 1.7.3's shared volume server rejects
these requests. On Ubuntu or Debian, it then configures needrestart to defer
storage-service restarts and records that configuration on each Kubernetes node
object. Deferring restarts protects Longhorn volumes from disruptions after
library updates.

Use this for nodes added by older kip versions. It applies the same configuration
as kip install and kip node add, using the same connection details, and is safe
to repeat. Without --host it repairs every recorded node: the server first, then
each worker one at a time, stopping at the first node it cannot reach or repair.
Run it again to finish, starting from the server. With --host only that node
is updated.

On other OS families, the command prints storage package update guidance.
Local storage requires no storage-service restart configuration.

Examples:
  kip node repair-host
  kip node repair-host --host 203.0.113.11`,
	RunE: runNodeRepairHost,
}

func init() {
	nodeRepairHostCmd.Flags().String("host", "", "IP address or hostname of a single node to repair (default: every recorded node)")
	nodeRepairHostCmd.Flags().String("ssh-key", "", "path to SSH private key")

	nodeCmd.AddCommand(nodeRepairHostCmd)
}

func runNodeRepairHost(cmd *cobra.Command, _ []string) error {
	nodeHost, _ := cmd.Flags().GetString("host")
	sshKey, _ := cmd.Flags().GetString("ssh-key")

	cluster, _, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	// The control plane's kubectl reads cluster settings and records the repair.
	leased, err := leaseCluster(cluster, sshKey, "kip node repair-host")
	if err != nil {
		return err
	}
	defer leased.close()
	master := leased.server
	if done, err := repairHostNotNeeded(master); err != nil || done {
		return err
	}

	if nodeHost != "" {
		fmt.Printf("\n  Connecting to %s...\n", nodeHost)
		nodeConn, err := dialNodeSSH(nodeHost, sshKey, cluster)
		if err != nil {
			return fmt.Errorf("connecting to %s: %w", nodeHost, err)
		}
		defer func() { _ = nodeConn.Close() }()
		if err := repairNodeHost(master, leased.lease.Host(nodeConn), nodeHost, ""); err != nil {
			return err
		}
		fmt.Println()
		return nil
	}

	records, err := recordNodes(master, cluster.Name, cluster.Host)
	if err != nil {
		return err
	}
	if err := nodesSettled(records); err != nil {
		return err
	}
	if err := repairHostOnNodes(records, master, leased.dialer(cluster, sshKey)); err != nil {
		return err
	}
	fmt.Println()
	return nil
}

// repairHostOnNodes repairs the server, then each worker in turn, and
// records each one through the server using its recorded machine ID.
// Worker identities are checked before repair.
func repairHostOnNodes(records []installer.NodeRecord, server installer.FirewallRunner, dial nodeDialer) error {
	return forEachNode(records, server, dial, func(rec installer.NodeRecord, client installer.FirewallRunner) error {
		fmt.Printf("\n  Node %s (%s)\n", rec.Name, rec.Host)
		return repairNodeHost(server, client, rec.Host, rec.MachineID)
	})
}

// repairNodeHost turns off NFS directory delegations on one node, defers its
// storage-service restarts and records that configuration on its Kubernetes
// node through master. An empty machineID is read from the node. If that read
// fails, the configuration is left in place and a warning is printed instead of
// recording it. OS families that do not use needrestart receive package update
// guidance instead.
func repairNodeHost(master, node installer.CommandRunner, nodeHost, machineID string) error {
	if err := installer.DisableNFSDirectoryDelegations(node); err != nil {
		return fmt.Errorf("repairing %s: %w", nodeHost, err)
	}
	fmt.Printf("  ✔  NFS client configured for shared volumes\n")

	host, err := installer.DetectHostOS(node)
	if err != nil {
		return fmt.Errorf("reading the OS of %s: %w", nodeHost, err)
	}
	if !host.UsesNeedrestart() {
		fmt.Printf("  ℹ  Kipper skips needrestart configuration on %s. Update storage packages\n", nodeHost)
		fmt.Printf("     during planned downtime: their package scripts may restart iscsid.\n")
		return nil
	}
	if err := installer.ConfigureStorageRestarts(node); err != nil {
		return fmt.Errorf("configuring storage restarts on %s: %w", nodeHost, err)
	}
	fmt.Printf("  ✔  Storage-path restarts deferred\n")

	if machineID == "" {
		id, err := installer.ReadMachineID(node)
		if err != nil {
			fmt.Printf("  ⚠   %s has no machine id: %v\n      The host is configured. kip status cannot confirm it.\n", nodeHost, err)
			return nil
		}
		machineID = id
	}

	nodeName, err := installer.NodeNameForMachine(master, machineID)
	if err != nil {
		return fmt.Errorf("identifying %s: %w", nodeHost, err)
	}
	if err := installer.StampStorageRestarts(master, nodeName, machineID); err != nil {
		return fmt.Errorf("recording host configuration for %s: %w", nodeName, err)
	}

	fmt.Printf("  ✔  %s recorded as configured\n", nodeName)
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

// nodeSSH uses the shared SSH key precedence: flag, environment, then cluster
// config. It adds a ProxyCommand when the selected transport uses this
// workstation's device tunnel. Tunnel mode returns an error if host is absent
// from the device's node list or the ProxyCommand cannot be constructed.
func nodeSSH(host, flagValue string, cluster *config.Cluster) (ssh.Config, error) {
	return nodeSSHVia(host, flagValue, cluster, transport.Mode(cluster))
}

// nodeSSHVia is nodeSSH with the transport mode forced.
func nodeSSHVia(host, flagValue string, cluster *config.Cluster, mode string) (ssh.Config, error) {
	explicit, fallback := resolveSSHKey(flagValue, cluster)
	cfg := ssh.Config{
		Host:            host,
		User:            sshUser(cluster),
		KeyPath:         explicit,
		FallbackKeyPath: fallback,
	}
	opts, err := transport.SSHOptionsVia(cluster, host, "", mode)
	if err != nil {
		return ssh.Config{}, err
	}
	cfg.Options = append(cfg.Options, opts...)
	return cfg, nil
}

// retargetSSH returns cfg for the cluster node at host instead, with the
// transport options chosen for that node rather than the ones cfg carried.
func retargetSSH(cfg ssh.Config, cluster *config.Cluster, host string) (ssh.Config, error) {
	opts, err := transport.SSHOptions(cluster, host, "")
	if err != nil {
		return ssh.Config{}, err
	}
	cfg.Host = host
	kept := make([]string, 0, len(cfg.Options)+len(opts))
	for _, o := range cfg.Options {
		if !strings.HasPrefix(o, "ProxyCommand=") && !strings.HasPrefix(o, "Port=") {
			kept = append(kept, o)
		}
	}
	kept = append(kept, opts...)
	cfg.Options = kept
	return cfg, nil
}

// clusterProvider returns the provider for the cluster node at host, reached
// through this workstation's device tunnel when the command uses it.
func clusterProvider(cluster *config.Cluster, host, explicitKey, fallbackKey string) (*infra.BareMetalProvider, error) {
	opts, err := transport.SSHOptions(cluster, host, "")
	if err != nil {
		return nil, err
	}
	return &infra.BareMetalProvider{Host: host, User: sshUser(cluster), SSHKey: explicitKey, FallbackSSHKey: fallbackKey, Options: opts}, nil
}

// dialNodeSSH connects to the cluster node at host, through this
// workstation's device tunnel when the command uses it.
func dialNodeSSH(host, flagValue string, cluster *config.Cluster) (*ssh.Client, error) {
	conn, _, err := dialNode(host, flagValue, cluster)
	return conn, err
}

// dialNode connects to host using the selected transport and reports whether
// the SSH configuration uses the device tunnel, even if the dial fails.
func dialNode(host, flagValue string, cluster *config.Cluster) (*ssh.Client, bool, error) {
	cfg, err := nodeSSH(host, flagValue, cluster)
	if err != nil {
		return nil, false, err
	}
	tunnel := false
	for _, o := range cfg.Options {
		tunnel = tunnel || strings.HasPrefix(o, "ProxyCommand=")
	}
	conn, err := ssh.Dial(cfg)
	return conn, tunnel, err
}
