package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
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
	Use:   "add <host>",
	Short: "Add a worker node to the cluster",
	Long: `Prepare a host and join it to the cluster as a worker.

kip uses a shared private network for cluster traffic when available, or public
addresses otherwise. The first join sets the cluster network mode. Applying
new network settings restarts k3s on the server. Pod traffic between nodes is
encrypted with WireGuard. By default, kip sets up a firewall on the new node
when it manages the server's firewall. Use --firewall or --firewall=false to
override this default. Existing kip-managed firewalls always get updated peer
rules. kip checks operator-managed firewalld rules on existing nodes and,
when firewall setup is enabled, on the joining node.

The worker accepts new workloads after checks pass for DNS, connectivity to
a pod on the server, Longhorn storage, host registry authentication,
containerd's registry mirror configuration and direct registry access, and
kubelet access. Run the command again with the same options to resume an
interrupted join. Running it again for a recorded worker also rejoins one
whose agent certificates are missing or expired.`,
	Args:         cobra.MaximumNArgs(1),
	Hidden:       true,
	SilenceUsage: true,
	RunE:         runNodeAdd,
}

var nodeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all nodes in the cluster",
	RunE:  runNodeList,
}

const nodeAddPreviewNotice = `
  ⚠   kip node add is a preview and not yet supported.
      Workers add capacity. The server remains the sole control plane and ingress
      node, so the cluster still depends on it for availability.
`

func init() {
	nodeAddCmd.Flags().String("host", "", "IP address or hostname of the node to add (same as the argument)")
	nodeAddCmd.Flags().String("ssh-key", "", "path to SSH private key")
	nodeAddCmd.Flags().String("private-network", "", "private network the nodes share, in CIDR form; needed when it carries the default route")
	nodeAddCmd.Flags().Bool("public", false, "use the nodes' public addresses even when they share a private network")
	nodeAddCmd.Flags().Bool("yes", false, "apply or recover server network changes without prompting")
	nodeAddCmd.Flags().Bool("firewall", true, "set up a host firewall on the new node (default: follow the server's kip-managed firewall setting); use --firewall=false to skip setup")

	nodeCmd.AddCommand(nodeAddCmd)
	nodeCmd.AddCommand(nodeListCmd)
	rootCmd.AddCommand(nodeCmd)
}

func nodeAddHost(cmd *cobra.Command, args []string) (string, error) {
	flag, _ := cmd.Flags().GetString("host")
	host := flag
	if len(args) == 1 {
		if flag != "" && flag != args[0] {
			return "", fmt.Errorf("the host is given twice (%s and --host %s); give it once", args[0], flag)
		}
		host = args[0]
	}
	if host == "" {
		return "", fmt.Errorf("name the host to add: kip node add <host>")
	}
	if !installer.ValidNodeHost(host) {
		return "", fmt.Errorf("invalid host %q: expected a hostname or IP address", host)
	}
	return host, nil
}

func resolveIPv4(host string) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() == nil {
			return "", fmt.Errorf("%s is not an IPv4 address; multi-node clusters require IPv4", host)
		}
		return ip.String(), nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", host, err)
	}
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("%s has no IPv4 address", host)
}

// peerAddress selects the source address to allow in peer firewall rules.
func peerAddress(mode string, n installer.NodeNetwork) string {
	if mode == installer.NetworkModePrivate {
		return n.NodeIP
	}
	return n.ExternalIP
}

// nodeFirewall represents the firewall policy requested for a node during a join.
type nodeFirewall int

const (
	existingFirewall nodeFirewall = iota
	managedFirewall
	followedServerFirewall
	declinedFirewall
)

type peerFirewallAction int

const (
	configureFirewall peerFirewallAction = iota
	checkOperatorFirewalld
	leaveFirewall
	refuseFirewall
)

// newNodeFirewall uses an explicit --firewall choice or inherits the server's
// kip-managed firewall setting.
func newNodeFirewall(cmd *cobra.Command, server installer.FirewallAudit) nodeFirewall {
	if cmd.Flags().Changed("firewall") {
		if on, _ := cmd.Flags().GetBool("firewall"); !on {
			return declinedFirewall
		}
		return managedFirewall
	}
	if server.KipperManaged {
		return managedFirewall
	}
	return followedServerFirewall
}

// peerFirewallActionFor keeps updates enabled for kip-managed firewalls, even
// with --firewall=false, so existing rules allow the joining node.
func peerFirewallActionFor(audit installer.FirewallAudit, mode nodeFirewall) peerFirewallAction {
	switch {
	case audit.KipperManaged:
		return configureFirewall
	case mode == followedServerFirewall || mode == declinedFirewall:
		return leaveFirewall
	case audit.Existing == "" && mode == managedFirewall:
		return configureFirewall
	case audit.Existing == "":
		return leaveFirewall
	case audit.Existing == "firewalld":
		return checkOperatorFirewalld
	}
	return refuseFirewall
}

func applyPeerFirewall(out io.Writer, client installer.FirewallRunner, label string, mesh installer.FirewallMesh, mode nodeFirewall, freshLimit bool) error {
	audit, err := installer.AuditFirewall(client)
	if err != nil {
		return fmt.Errorf("auditing the firewall on %s: %w", label, err)
	}
	switch peerFirewallActionFor(audit, mode) {
	case configureFirewall:
		limit, err := sshRateLimitFor(client, audit, freshLimit)
		if err != nil {
			return fmt.Errorf("reading the SSH rule on %s: %w", label, err)
		}
		plan := installer.PlanFirewall(audit, true, false)
		plan.Mesh = mesh
		if err := installer.ApplyFirewallPlan(client, plan, limit); err != nil {
			return fmt.Errorf("configuring the firewall on %s: %w", label, err)
		}
		_, _ = fmt.Fprintf(out, "%s\n", managedFirewallNotice(label, mode))
	case leaveFirewall:
		_, _ = fmt.Fprintf(out, "%s\n", unmanagedFirewallNotice(label, mesh, mode))
	case checkOperatorFirewalld:
		missing, err := installer.CheckOperatorFirewalld(client, mesh)
		if err != nil {
			return fmt.Errorf("checking the firewall on %s: %w", label, err)
		}
		if len(missing) > 0 {
			return fmt.Errorf("the operator-managed firewall on %s needs: %s", label, strings.Join(missing, "; "))
		}
	default:
		if mode == existingFirewall {
			harden := ""
			if label == "the server" {
				harden = " and run 'kip cluster harden' to let kip manage it"
			}
			return fmt.Errorf("kip needs to manage the %s firewall on %s to add peer rules, so the join stops even if the ports are open. Disable UFW with 'ufw disable'%s, then run 'kip node add' again", audit.Existing, label, harden)
		}
		return fmt.Errorf("the %s firewall on %s is managed outside kip. Pass --firewall=false to keep it and configure the required ports yourself, or disable it so kip can set up a firewall", audit.Existing, label)
	}
	return nil
}

func managedFirewallNotice(label string, mode nodeFirewall) string {
	if mode == followedServerFirewall || mode == declinedFirewall {
		return fmt.Sprintf("  ℹ  Updated peer rules on %s's existing kip-managed firewall. Existing kip-managed firewalls are updated even with --firewall=false.", label)
	}
	return fmt.Sprintf("  ✔  Firewall on %s opens the cluster ports to the other nodes", label)
}

func unmanagedFirewallNotice(label string, mesh installer.FirewallMesh, mode nodeFirewall) string {
	var peers []string
	for _, p := range mesh.Peers {
		peers = append(peers, p.Address)
	}
	ports := fmt.Sprintf("In your firewall rules, allow 51820/udp and 10250/tcp from the other nodes (%s), and 9100/tcp from the server. Restrict these ports to those sources, keep 8888/tcp closed to outside traffic, and limit access to any NodePort services you expose.", strings.Join(peers, ", "))
	switch mode {
	case followedServerFirewall:
		return fmt.Sprintf("  ℹ  Firewall setup skipped on %s: kip leaves firewall management to you, as on the server. %s Use --firewall to enable firewall setup on this node.", label, ports)
	case declinedFirewall:
		return fmt.Sprintf("  ⚠  Firewall setup skipped on %s (--firewall=false). %s", label, ports)
	}
	if mesh.Role == installer.RoleWorker {
		return fmt.Sprintf("  ℹ  Manage the firewall on %s yourself: allow 51820/udp and 10250/tcp from the other nodes and 9100/tcp from the server", label)
	}
	return fmt.Sprintf("  ℹ  Manage the firewall on %s yourself: allow 51820/udp and 10250/tcp from the other nodes", label)
}

func sshRateLimitFor(client installer.CommandRunner, audit installer.FirewallAudit, freshLimit bool) (bool, error) {
	if audit.KipperManaged {
		return installer.SSHRateLimited(client)
	}
	return freshLimit, nil
}

// refuseNodeAddOnLocalStorage enforces Kipper's single-node limit for local storage.
func refuseNodeAddOnLocalStorage(mode string) error {
	if mode != installer.StorageLocal {
		return nil
	}
	return fmt.Errorf("this cluster uses local storage and supports a single node. " +
		"To add nodes, install a Longhorn cluster with --storage longhorn and migrate your projects. See Cluster Migration in the Kipper docs")
}

func runNodeAdd(cmd *cobra.Command, args []string) error {
	fmt.Print(nodeAddPreviewNotice)

	workerHost, err := nodeAddHost(cmd, args)
	if err != nil {
		return err
	}
	sshKey, _ := cmd.Flags().GetString("ssh-key")
	privateNetwork, _ := cmd.Flags().GetString("private-network")
	forcePublic, _ := cmd.Flags().GetBool("public")
	autoYes, _ := cmd.Flags().GetBool("yes")

	cluster, _, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	serverIP, err := resolveIPv4(cluster.Host)
	if err != nil {
		return fmt.Errorf("finding the server's address: %w", err)
	}
	workerIP, err := resolveIPv4(workerHost)
	if err != nil {
		return err
	}

	fmt.Printf("\n  Connecting to the server (%s)...\n", cluster.Host)
	server, err := ssh.Dial(nodeSSH(cluster.Host, sshKey, cluster))
	if err != nil {
		return fmt.Errorf("connecting to the server: %w", err)
	}
	defer func() { _ = server.Close() }()
	fmt.Printf("  Connecting to %s...\n", workerHost)
	worker, err := ssh.Dial(nodeSSH(workerHost, sshKey, cluster))
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", workerHost, err)
	}
	defer func() { _ = worker.Close() }()

	// Capture the host identity before setup so registration can be verified.
	identity, err := installer.ReadHostIdentity(worker)
	if err != nil {
		return fmt.Errorf("reading what %s reports about itself: %w", workerHost, err)
	}

	// Inherit the server's firewall setting before the join changes either host.
	serverFirewall, err := installer.AuditFirewall(server)
	if err != nil {
		return fmt.Errorf("auditing the firewall on the server: %w", err)
	}
	workerFirewall := newNodeFirewall(cmd, serverFirewall)

	// Recover over SSH first: an interrupted network change can leave the API down.
	if pending, err := installer.NetworkChangePending(server); err != nil {
		return err
	} else if pending {
		fmt.Printf("%s\n", installer.PendingNetworkChangeDescription())
		if !autoYes {
			ok, err := confirmInteractive("  Recover the unfinished pod network change now? [y/N] ")
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("recover the unfinished pod network change before adding nodes")
			}
		}
		if err := installer.RecoverNetworkChange(server); err != nil {
			return fmt.Errorf("pod network: %w", err)
		}
	}

	mode, err := installer.ReadStorageMode(server)
	if err != nil {
		return err
	}
	if err := refuseNodeAddOnLocalStorage(mode); err != nil {
		return err
	}

	records, err := installer.EnsureServerRecord(server, cluster.Host)
	if err != nil {
		return err
	}
	mirrorNodes(cluster.Name, records)
	if err := installer.RequirePeersRecordedForJoin(server, records, identity.MachineID); err != nil {
		return err
	}
	serverRec, _ := installer.ServerRecord(records)
	if serverRec.MachineID == identity.MachineID {
		return fmt.Errorf("%s is the cluster's server", workerHost)
	}
	var existing *installer.NodeRecord
	firstWorker := true
	for i := range records {
		if records[i].MachineID == identity.MachineID {
			existing = &records[i]
		} else if records[i].Role == installer.NodeRoleWorker {
			firstWorker = false
		}
	}

	// Connect to existing workers before updating their peer firewall rules.
	peers := map[string]*ssh.Client{}
	defer func() {
		for _, c := range peers {
			_ = c.Close()
		}
	}()
	for _, r := range records {
		if r.Role != installer.NodeRoleWorker || r.MachineID == identity.MachineID {
			continue
		}
		c, err := ssh.Dial(nodeSSH(r.Host, sshKey, cluster))
		if err != nil {
			return fmt.Errorf("connecting to node %s (%s), whose firewall must admit the new node: %w", r.Name, r.Host, err)
		}
		peers[r.MachineID] = c
		id, err := installer.ReadMachineID(c)
		if err != nil || id != r.MachineID {
			return fmt.Errorf("%s no longer answers as node %s (machine id %q, recorded %s); check the address before adding nodes", r.Host, r.Name, id, r.MachineID)
		}
	}

	network, err := installer.PlanNetworkChange(server)
	if err != nil {
		return err
	}
	if network.Running != "wireguard" {
		return fmt.Errorf("the server's pod network is %q, not WireGuard; run 'kip upgrade' first, then add the node", network.Running)
	}

	fmt.Printf("\n  Running preflight checks on %s...\n", workerHost)
	sys, preflight, err := installer.PreflightWorker(worker)
	if err != nil {
		return fmt.Errorf("%s cannot join: %w", workerHost, err)
	}
	fmt.Printf("  ✔  %s %s, %d MB RAM, %d MB disk\n", sys.OS, sys.OSVersion, sys.RAMMB, sys.DiskMB)
	for _, w := range preflight.Warnings {
		fmt.Printf("  ⚠  %s\n", w)
	}
	plan, serverDropIn, serverFlannel, err := prepareJoinHost(server, worker, identity.MachineID, workerHost, installer.NetworkModeOptions{
		PrivateNetwork: privateNetwork, Public: forcePublic, ClusterMode: serverRec.Mode,
		ServerExternalIP: serverIP, JoinerExternalIP: workerIP,
	}, autoYes)
	if err != nil {
		return err
	}
	// A matching drop-in is still checked against the address k3s runs with.
	if err := installer.ChangeServerNetwork(server, serverDropIn, serverFlannel); err != nil {
		return fmt.Errorf("moving the server's network: %w", err)
	}
	serverRec.Mode, serverRec.PeerAddress, serverRec.Interface = plan.Mode, peerAddress(plan.Mode, plan.Server), plan.Server.Interface
	if err := saveNodeRecord(server, cluster.Name, serverRec); err != nil {
		return err
	}

	rec := installer.NodeRecord{Host: workerHost, MachineID: identity.MachineID, Role: installer.NodeRoleWorker, State: installer.NodeStateJoining,
		Mode: plan.Mode, PeerAddress: peerAddress(plan.Mode, plan.Joiner), Interface: plan.Joiner.Interface}
	if existing != nil {
		rec.Name = existing.Name
	} else if rec.Name, err = installer.ProposedNodeName(worker); err != nil {
		return err
	}
	if err := saveNodeRecord(server, cluster.Name, rec); err != nil {
		return err
	}
	if records, err = installer.ReadNodeRecords(server); err != nil {
		return err
	}

	fmt.Printf("\n  Opening the cluster ports between the nodes...\n")
	if err := applyPeerFirewall(os.Stdout, server, "the server", installer.FirewallMeshFor(records, serverRec.MachineID), existingFirewall, false); err != nil {
		return err
	}
	for _, r := range records {
		if c, ok := peers[r.MachineID]; ok {
			if err := applyPeerFirewall(os.Stdout, c, r.Name, installer.FirewallMeshFor(records, r.MachineID), existingFirewall, false); err != nil {
				return err
			}
		}
	}

	workerOS, err := installer.DetectHostOS(worker)
	if err != nil {
		return fmt.Errorf("reading the OS of %s: %w", workerHost, err)
	}
	// Defer storage-service restarts before joining so unattended upgrades
	// preserve active Longhorn sessions once this node starts hosting volumes.
	if workerOS.UsesNeedrestart() {
		if err := installer.ConfigureStorageRestarts(worker); err != nil {
			return fmt.Errorf("configuring storage restarts on %s: %w", workerHost, err)
		}
	}
	workerCurrent, err := installer.ReadNetworkDropIn(worker)
	if err != nil {
		return err
	}
	workerDropIn, err := installer.RenderNetworkDropIn(workerCurrent, installer.RoleWorker, plan.Mode, plan.Joiner)
	if err != nil {
		return err
	}
	// Pin existing and new single-copy volumes before the worker can host replicas.
	placement, err := installer.PinSingleCopiesForJoin(server)
	if err != nil {
		return fmt.Errorf("keeping single-copy volumes on the server: %w", err)
	}
	reportSingleCopyPlacement(os.Stdout, placement)
	fmt.Printf("  Preparing %s...\n", workerHost)
	workerFlannel, err := installer.RenderFlannelConf(plan.PodNetwork, plan.Joiner.TunnelMTU)
	if err != nil {
		return err
	}
	if err := installer.WriteFlannelConf(worker, workerFlannel); err != nil {
		return nodeAddRetry(fmt.Errorf("on %s: %w", workerHost, err), workerHost)
	}
	join, err := installer.PrepareWorker(server, worker, workerDropIn)
	if err != nil {
		return nodeAddRetry(err, workerHost)
	}
	// Inherit the server's kip-managed UFW rate limit so workers use the same
	// SSH policy. Otherwise, use the install default of rate limiting.
	workerLimit := true
	if audit, err := installer.AuditFirewall(server); err == nil && audit.KipperManaged && audit.Existing != "firewalld" {
		if workerLimit, err = installer.SSHRateLimited(server); err != nil {
			return nodeAddRetry(err, workerHost)
		}
	}
	if err := applyPeerFirewall(os.Stdout, worker, workerHost, installer.FirewallMeshFor(records, identity.MachineID), workerFirewall, workerLimit); err != nil {
		return nodeAddRetry(err, workerHost)
	}

	serverURLHost := serverIP
	if plan.Mode == installer.NetworkModePrivate {
		serverURLHost = plan.Server.NodeIP
	}
	fmt.Printf("  Joining %s to the cluster...\n", workerHost)
	nodeName, err := join.Register(server, worker, serverURLHost, identity)
	if err != nil {
		return nodeAddRetry(err, workerHost)
	}
	if nodeName != rec.Name {
		rec.Name = nodeName
		if err := saveNodeRecord(server, cluster.Name, rec); err != nil {
			return nodeAddRetry(err, workerHost)
		}
	}
	fmt.Printf("  ✔  %s registered as %s and is cordoned\n", workerHost, nodeName)
	if workerOS.UsesNeedrestart() {
		if err := installer.StampStorageRestarts(server, nodeName, identity.MachineID); err != nil {
			fmt.Printf("  ⚠  could not record host configuration for %s: %v\n      The host is configured; 'kip node repair-host' records it.\n", nodeName, err)
		}
	}

	fmt.Printf("  Waiting for %s and its storage services...\n", nodeName)
	if err := installer.WaitForWorkerStorage(server, nodeName); err != nil {
		return nodeAddRetry(err, workerHost)
	}
	// Finish the Velero rollout before the check taint blocks its node agent.
	if applied, err := installer.EnsureVeleroCoversNodes(server); err != nil {
		return nodeAddRetry(fmt.Errorf("updating backups for %s: %w", nodeName, err), workerHost)
	} else if applied {
		fmt.Printf("  ✔  Backups configured for SELinux hosts after joining %s\n", nodeName)
	}
	fmt.Printf("  Checking %s with a test pod...\n", nodeName)
	err = installer.OpenWorkerForChecks(server, nodeName)
	if err == nil {
		err = installer.ValidateWorker(server, worker, join, nodeName, serverRec.Name, rec.PeerAddress)
	}
	if err != nil {
		if _, cordonErr := server.Run("kubectl cordon " + nodeName + " >/dev/null"); cordonErr != nil {
			return fmt.Errorf("%w; failed to cordon %s after the checks (%v). Run 'kubectl cordon %s', then retry 'kip node add %s' with the same options", err, nodeName, cordonErr, nodeName, workerHost)
		}
		return nodeAddRetry(err, workerHost)
	}
	fmt.Printf("  ✔  DNS, server pod connectivity, Longhorn storage, registry and kubelet checks passed\n")

	if err := installer.WaitForNodeToPublishAddress(server, nodeName, 120*time.Second); err != nil {
		return nodeAddRetry(err, workerHost)
	}
	if err := installer.InstallBuildIsolation(server); err != nil {
		return nodeAddRetry(fmt.Errorf("updating build isolation for %s: %w", nodeName, err), workerHost)
	}

	rec.State = installer.NodeStateReady
	if err := saveNodeRecord(server, cluster.Name, rec); err != nil {
		return nodeAddRetry(err, workerHost)
	}
	if err := installer.UncordonWorker(server, nodeName); err != nil {
		// Back to the state a retry expects: cordoned and joining.
		rec.State = installer.NodeStateJoining
		_, cordonErr := server.Run("kubectl cordon " + nodeName + " >/dev/null")
		saveErr := saveNodeRecord(server, cluster.Name, rec)
		if cordonErr != nil || saveErr != nil {
			return fmt.Errorf("%w; putting %s back to cordoned and joining also failed (%v), so cordon it with 'kubectl cordon %s' and run 'kip node add %s' again", err, nodeName, errors.Join(cordonErr, saveErr), nodeName, workerHost)
		}
		return nodeAddRetry(err, workerHost)
	}

	fmt.Printf("\n  ✔  %s joined as worker %s (%s mode, %s)\n", workerHost, nodeName, plan.Mode, plan.Joiner.NodeIP)
	fmt.Printf("     Apps, functions, jobs and services can now run on it. The platform stays on the server.\n\n")
	fmt.Print(joinDataSummary)
	if len(placement.OffServer) > 0 {
		fmt.Printf("\n  ⚠  %s\n", offServerJoinWarning)
	}
	if firstWorker && backupsInCluster(cluster.BackupStorage) {
		fmt.Printf("\n  ⚠  %s\n", inClusterBackupWarning)
	}
	fmt.Println()
	return nil
}

func serverNetworkConsent(mode, address string) string {
	return fmt.Sprintf("\n  The server will use %s mode for cluster traffic (address %s). kip restarts\n"+
		"  k3s for this. Running containers keep running; the Kubernetes API, kip\n"+
		"  commands, console actions and any app that calls the API are unavailable\n"+
		"  meanwhile, usually for 10 to 30 seconds and longer on small or busy\n"+
		"  servers. If recovery checks fail, kip waits up to five minutes per check\n"+
		"  and then attempts to restore the previous configuration.\n\n", mode, address)
}

// prepareJoinHost validates network options and confirms server network changes
// before cordoning or hardening the worker. It returns the plan and the server's
// network drop-in and flannel configuration.
func prepareJoinHost(server, worker installer.CommandRunner, machineID, workerHost string, opts installer.NetworkModeOptions, autoYes bool) (installer.NetworkPlan, string, string, error) {
	sshPort, err := installer.SSHPort(worker)
	if err != nil {
		return installer.NetworkPlan{}, "", "", err
	}
	opts.SSHPort = sshPort
	plan, err := installer.PlanNodeNetwork(server, worker, opts)
	if err != nil {
		return installer.NetworkPlan{}, "", "", err
	}
	fmt.Printf("  ✔  Network: %s mode, server %s, %s %s\n", plan.Mode, plan.Server.NodeIP, workerHost, plan.Joiner.NodeIP)

	currentDropIn, err := installer.ReadNetworkDropIn(server)
	if err != nil {
		return installer.NetworkPlan{}, "", "", err
	}
	serverDropIn, err := installer.RenderNetworkDropIn(currentDropIn, installer.RoleServer, plan.Mode, plan.Server)
	if err != nil {
		return installer.NetworkPlan{}, "", "", err
	}
	serverFlannel, err := installer.RenderFlannelConf(plan.PodNetwork, plan.Server.TunnelMTU)
	if err != nil {
		return installer.NetworkPlan{}, "", "", err
	}
	changes, err := installer.ServerNetworkChanges(server, serverDropIn, serverFlannel)
	if err != nil {
		return installer.NetworkPlan{}, "", "", err
	}
	if changes {
		fmt.Print(serverNetworkConsent(plan.Mode, plan.Server.NodeIP))
		if !autoYes {
			ok, err := confirmInteractive("  Apply the server network changes? [y/N] ")
			if err != nil {
				return installer.NetworkPlan{}, "", "", err
			}
			if !ok {
				return installer.NetworkPlan{}, "", "", fmt.Errorf("node join canceled. Server network settings are unchanged")
			}
		}
	}
	// Prevent new workloads from being scheduled during host changes.
	existingNode, registered, err := installer.FindNodeForMachine(server, machineID)
	if err != nil {
		return installer.NetworkPlan{}, "", "", fmt.Errorf("looking for an existing node on %s: %w", workerHost, err)
	}
	if registered {
		if _, err := server.Run("kubectl cordon " + existingNode + " >/dev/null"); err != nil {
			return installer.NetworkPlan{}, "", "", fmt.Errorf("cordoning %s before changing it: %w", existingNode, err)
		}
		fmt.Printf("  ✔  %s is already node %s and is cordoned until the join is checked\n", workerHost, existingNode)
	}
	audit, err := installer.AuditHost(worker)
	if err != nil {
		return installer.NetworkPlan{}, "", "", fmt.Errorf("auditing %s: %w", workerHost, err)
	}
	if findings := audit.Findings(); len(findings) > 0 {
		fmt.Printf("  Hardening %s (%s)...\n", workerHost, strings.Join(findings, "; "))
		if err := installer.HardenHost(worker); err != nil {
			return installer.NetworkPlan{}, "", "", fmt.Errorf("hardening %s: %w", workerHost, err)
		}
	}
	return plan, serverDropIn, serverFlannel, nil
}

func nodeAddRetry(err error, host string) error {
	return fmt.Errorf("%w; join incomplete. Fix the cause, then run 'kip node add %s' again with the same options to resume", err, host)
}

// saveNodeRecord saves the cluster record and refreshes the local cache when possible.
func saveNodeRecord(server installer.CommandRunner, clusterName string, rec installer.NodeRecord) error {
	if err := installer.SaveNodeRecord(server, rec); err != nil {
		return err
	}
	records, err := installer.ReadNodeRecords(server)
	if err != nil {
		fmt.Printf("  ⚠  could not update the local copy of the node records: %v\n", err)
		return nil
	}
	mirrorNodes(clusterName, records)
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
