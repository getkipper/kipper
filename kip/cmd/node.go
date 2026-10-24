package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/k8s"
	"github.com/getkipper/kipper/kip/internal/ssh"
	"github.com/getkipper/kipper/kip/internal/transport"
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
whose agent certificates are missing or expired.

For a worker fenced with 'kip node fence', kip rejoins it in a guarded order.
It makes sure k3s is stopped and cannot start, reboots the host unless it
has booted since the fence, then lifts the fence, runs the checks above and
starts the services the fence stopped. A service whose copies are not known
to be current starts only after you accept the older data, interactively
or with --accept-stale-data. In private mode, if a fenced worker responds at
an address different from the one its node publishes, kip records the new
address. After the rejoin, kip runs the add process again so the node publishes
the new address. In public mode, kip refuses to rejoin it at the new address.`,
	Args:         cobra.MaximumNArgs(1),
	SilenceUsage: true,
	RunE:         runNodeAdd,
}

var nodeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all nodes in the cluster",
	RunE:  runNodeList,
}

const nodeAddNotice = `
  ℹ  Workers add capacity. The server remains the sole control plane and ingress
     node, so the cluster still depends on it for availability.
`

func init() {
	nodeAddCmd.Flags().String("host", "", "IP address or hostname of the node to add (same as the argument)")
	nodeAddCmd.Flags().String("ssh-key", "", "path to SSH private key")
	nodeAddCmd.Flags().String("ssh-user", "", "SSH user for a node that needs a different login from the cluster's. kip uses passwordless sudo for users other than root. The default is the node's recorded user, falling back to the cluster's user.")
	nodeAddCmd.Flags().String("private-network", "", "private network the nodes share, in CIDR form; needed when it carries the default route, unless the worker is already recorded in a private-mode cluster")
	nodeAddCmd.Flags().Bool("public", false, "use the nodes' public addresses even when they share a private network")
	nodeAddCmd.Flags().Bool("yes", false, "apply or recover server network changes without prompting")
	nodeAddCmd.Flags().Bool("no-ssh-rate-limit", false, "Allow SSH without UFW rate limiting on the joining node, replacing any existing limit. By default, keep its managed rule or inherit the server's setting for a new firewall.")
	nodeAddCmd.Flags().Bool("accept-stale-data", false, "when rejoining a fenced worker, start its stopped services even when no copy of their data is known to be current")
	nodeAddCmd.Flags().Bool("no-copies", false, "skip the disk check that lets the node hold copies of volumes. Run 'kip node storage <node> --copies' later")
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

// shownAddress returns the address peers use, including the node's own address
// in public mode when NAT makes the two differ.
func shownAddress(mode string, n installer.NodeNetwork) string {
	peer := peerAddress(mode, n)
	if n.NodeIP != "" && n.NodeIP != peer {
		return peer + " (node address " + n.NodeIP + ")"
	}
	return peer
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

func applyPeerFirewall(out io.Writer, client installer.FirewallRunner, label string, mesh installer.FirewallMesh, mode nodeFirewall, freshLimit, noRateLimit bool) error {
	audit, err := installer.AuditFirewall(client)
	if err != nil {
		return fmt.Errorf("auditing the firewall on %s: %w", label, err)
	}
	switch peerFirewallActionFor(audit, mode) {
	case configureFirewall:
		limit, err := sshRateLimitFor(client, audit, freshLimit, noRateLimit)
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
				harden = ", or let kip manage it with 'kip cluster harden --adopt-firewall'"
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

// sshRateLimitFor selects whether to limit SSH in UFW. noRateLimit disables
// limiting. Otherwise, a managed firewall keeps its rule and a new one uses
// freshLimit. Reading an existing rule can return a command error.
func sshRateLimitFor(client installer.CommandRunner, audit installer.FirewallAudit, freshLimit, noRateLimit bool) (bool, error) {
	if noRateLimit {
		return false, nil
	}
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
	return explainSSHTimeout(addNode(cmd, args))
}

var sshTimeout = regexp.MustCompile(`ssh: connect to host (\S+) port (\d+): (Operation|Connection) timed out`)

// explainSSHTimeout wraps SSH connection timeouts with advice about UFW rate
// limiting, which can persist after an uninstall. It preserves nil and other errors.
func explainSSHTimeout(err error) error {
	if err == nil {
		return nil
	}
	m := sshTimeout.FindStringSubmatch(err.Error())
	if m == nil {
		return err
	}
	return fmt.Errorf("%w\n  A UFW %s/tcp LIMIT rule on %s may be the cause. It blocks connections from an address that attempts six or more SSH connections within 30 seconds. "+
		"The rule may remain from an earlier kip install, even after uninstalling. If the limit caused the timeout, wait a minute and run the command again, or replace the rule on that host with 'ufw allow %s/tcp'. "+
		"Pass --no-ssh-rate-limit to allow SSH without rate limiting when kip configures the joining node's UFW firewall", err, m[2], m[1], m[2])
}

func addNode(cmd *cobra.Command, args []string) error {
	fmt.Print(nodeAddNotice)

	workerHost, err := nodeAddHost(cmd, args)
	if err != nil {
		return err
	}
	sshKey, _ := cmd.Flags().GetString("ssh-key")
	userFlag, _ := cmd.Flags().GetString("ssh-user")
	privateNetwork, _ := cmd.Flags().GetString("private-network")
	forcePublic, _ := cmd.Flags().GetBool("public")
	autoYes, _ := cmd.Flags().GetBool("yes")
	noRateLimit, _ := cmd.Flags().GetBool("no-ssh-rate-limit")
	noCopies, _ := cmd.Flags().GetBool("no-copies")

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
	serverConn, err := dialNodeSSH(cluster.Host, sshKey, cluster)
	if err != nil {
		return fmt.Errorf("connecting to the server: %w", err)
	}
	defer func() { _ = serverConn.Close() }()
	// The lease comes before anything that changes a host, and every command
	// after this point goes through it.
	lease, err := installer.TakeNodeLease(serverConn, "kip node add "+workerHost)
	if err != nil {
		return err
	}
	defer lease.Release()
	server := lease.Server(serverConn)
	// The cluster's records include nodes added from other workstations.
	// An interrupted network change can leave the API down and records unreadable,
	// so the login is checked against them again after recovery.
	cluster, err = withRecordedNodes(server, cluster)
	if err != nil {
		fmt.Printf("  ⚠  Could not read the cluster's node records. kip selects the SSH user for %s from --ssh-user or locally saved settings: %v\n", workerHost, err)
	}
	workerLogin, err := nodeAddLogin(cluster, workerHost, userFlag)
	if err != nil {
		return err
	}
	fmt.Printf("  Connecting to %s...\n", workerHost)
	// A host missing from the device's node list needs a direct connection
	// to prepare its operator interface, even with --via tunnel.
	joinerMode := transport.Mode(cluster)
	if _, known := transport.NodeForHost(cluster, workerHost); !known {
		if joinerMode == transport.Tunnel {
			fmt.Printf("  ℹ  %s is not yet in this workstation's device node list, so kip connects to it directly to prepare its operator interface\n", workerHost)
		}
		joinerMode = transport.Direct
	}
	workerCfg, err := nodeSSHVia(workerHost, sshKey, cluster, joinerMode)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", workerHost, err)
	}
	workerCfg.User = workerLogin
	workerConn, err := ssh.Dial(workerCfg)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", workerHost, err)
	}
	defer func() { _ = workerConn.Close() }()
	worker := lease.Host(workerConn)
	var rejoinConns []*ssh.Client
	defer func() {
		for _, c := range rejoinConns {
			_ = c.Close()
		}
	}()

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
	mirrorNodes(cluster, records)
	if err := removalBlocksJoin(records, identity.MachineID); err != nil {
		return err
	}
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
	if err := confirmJoinLogin(cluster, existing, workerHost, userFlag, workerLogin); err != nil {
		return err
	}
	if err := replacedFencedHost(records, workerHost, identity.MachineID); err != nil {
		return err
	}
	if existing != nil {
		if err := fenceBlocksJoin(*existing); err != nil {
			return err
		}
		if existing.State == installer.NodeStateFenced {
			published, err := installer.NodeExternalIP(server, existing.Name)
			if err != nil {
				return err
			}
			moved, err := rejoinMoves(*existing, workerIP, published)
			if err != nil {
				return err
			}
			if moved {
				fmt.Printf("  ℹ  %s responds at %s. Its node currently publishes %s. kip records the new address and runs the add process again after the rejoin so the node publishes the new address\n", existing.Name, workerHost, published)
			}
			if moved && existing.Host != workerHost {
				// Save the new address so fence and rejoin retries can reach the host.
				existing.Host = workerHost
				if err := saveNodeRecord(server, cluster, *existing); err != nil {
					return err
				}
			}
			reconnect := func() (installer.CommandRunner, error) {
				cfg := workerCfg
				cfg.Options = append(append([]string(nil), cfg.Options...), "BatchMode=yes")
				c, err := ssh.Dial(cfg)
				if err != nil {
					return nil, err
				}
				rejoinConns = append(rejoinConns, c)
				return lease.Host(c), nil
			}
			acceptStale, _ := cmd.Flags().GetBool("accept-stale-data")
			if err := rejoinFenced(server, worker, reconnect, cluster, *existing, serverRec.Name, acceptStale); err != nil {
				return nodeAddRetry(err, workerHost)
			}
			if !moved {
				return nil
			}
			// The rejoin may have rebooted the host, which ends this connection.
			c, err := ssh.Dial(workerCfg)
			if err != nil {
				return nodeAddRetry(fmt.Errorf("connecting to %s after the rejoin: %w", workerHost, err), workerHost)
			}
			rejoinConns = append(rejoinConns, workerConn)
			workerConn, worker = c, lease.Host(c)
			// Refresh the boot ID so registration can verify the boot after the rejoin.
			current, err := installer.ReadHostIdentity(worker)
			if err != nil {
				return nodeAddRetry(fmt.Errorf("reading the host identity of %s after the rejoin: %w", workerHost, err), workerHost)
			}
			if current.MachineID != identity.MachineID {
				return fmt.Errorf("%s answered with machine ID %s after the rejoin, not %s. Check the address, then run 'kip node add %s' again", workerHost, current.MachineID, identity.MachineID, workerHost)
			}
			identity = current
			if records, err = installer.ReadNodeRecords(server); err != nil {
				return err
			}
			for i := range records {
				if records[i].MachineID == identity.MachineID {
					existing = &records[i]
				}
			}
		}
		if err := keepRemovingNode(worker, *existing); err != nil {
			return err
		}
	}

	// Connect to existing workers before updating their peer firewall rules.
	peers := map[string]installer.FirewallRunner{}
	accessNodes := map[string]accessNode{serverRec.MachineID: {rec: serverRec, node: server, base: serverConn}}
	touched := []accessNode{accessNodes[serverRec.MachineID]}
	var peerConns []*ssh.Client
	defer func() {
		for _, c := range peerConns {
			_ = c.Close()
		}
	}()
	for _, r := range records {
		if r.Role != installer.NodeRoleWorker || r.MachineID == identity.MachineID {
			continue
		}
		c, err := dialNodeSSH(r.Host, sshKey, cluster)
		if err != nil {
			return fmt.Errorf("connecting to node %s (%s), whose firewall must admit the new node: %w", r.Name, r.Host, err)
		}
		peerConns = append(peerConns, c)
		peer := lease.Host(c)
		peers[r.MachineID] = peer
		accessNodes[r.MachineID] = accessNode{rec: r, node: peer, base: c}
		touched = append(touched, accessNodes[r.MachineID])
		id, err := installer.ReadMachineID(peer)
		if err != nil || id != r.MachineID {
			return fmt.Errorf("%s no longer answers as node %s (machine id %q, recorded %s); check the address before adding nodes", r.Host, r.Name, id, r.MachineID)
		}
	}

	// A recorded retry or an unrecorded worker can already hold a list.
	joinerState, err := installer.ReadAccessGuardState(worker)
	if err != nil {
		return fmt.Errorf("%s: %w", workerHost, err)
	}
	joinerHeader := []installer.AccessNodeHeader{joinerState.Header(workerHost)}
	nc, err := newNodeAccessCluster(os.Stdout, server, cluster, sshKey)
	if err != nil {
		return err
	}
	access, _, err := prepareNodeAccess(nc, touched, joinerHeader, "node add "+workerHost, true)
	if err != nil {
		return err
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
	if access.on {
		// The joiner reaches the server's API port during planning and
		// joining, before the next load puts its peer address in cluster4.
		addrs, err := installer.JoinerAddresses(worker, workerIP)
		if err != nil {
			return err
		}
		if err := installer.AddJoiningAddresses(server, addrs); err != nil {
			return err
		}
	}
	recordedServer, recordedJoiner := recordedPeers(serverRec, existing)
	plan, serverDropIn, serverFlannel, err := prepareJoinHost(server, worker, identity.MachineID, workerHost, installer.NetworkModeOptions{
		PrivateNetwork: privateNetwork, Public: forcePublic, ClusterMode: serverRec.Mode,
		ServerExternalIP: serverIP, JoinerExternalIP: workerIP,
		RecordedServerIP: recordedServer, RecordedJoinerIP: recordedJoiner,
	}, autoYes)
	if err != nil {
		return err
	}
	// A matching drop-in is still checked against the address k3s runs with.
	if err := installer.ChangeServerNetwork(server, serverDropIn, serverFlannel); err != nil {
		return fmt.Errorf("moving the server's network: %w", err)
	}
	serverRec.Mode, serverRec.PeerAddress, serverRec.Interface = plan.Mode, peerAddress(plan.Mode, plan.Server), plan.Server.Interface
	if err := saveNodeRecord(server, cluster, serverRec); err != nil {
		return err
	}

	rec := installer.NodeRecord{Host: workerHost, MachineID: identity.MachineID, Role: installer.NodeRoleWorker, State: installer.NodeStateJoining,
		Mode: plan.Mode, PeerAddress: peerAddress(plan.Mode, plan.Joiner), Interface: plan.Joiner.Interface, SSHUser: recordedLogin(existing, userFlag),
		Storage: joinedNodeStorage(existing)}
	if existing != nil {
		rec.Name = existing.Name
	} else if rec.Name, err = installer.ProposedNodeName(worker); err != nil {
		return err
	}
	if err := saveNodeRecord(server, cluster, rec); err != nil {
		return err
	}
	if records, err = installer.ReadNodeRecords(server); err != nil {
		return err
	}
	if err := access.prepareJoiner(accessNode{rec: rec, node: worker, base: workerConn}, records); err != nil {
		return nodeAddRetry(err, workerHost)
	}
	if access.tunnelled(identity.MachineID) {
		// The joiner's access table can close the direct path. Use the tunnel
		// so commands still connect after SSH's idle multiplexing timeout.
		if workerConn, worker, err = rejoinThroughTunnel(workerConn, lease, workerHost, workerLogin, sshKey, cluster, identity.MachineID); err != nil {
			return nodeAddRetry(err, workerHost)
		}
	}

	fmt.Printf("\n  Opening the cluster ports between the nodes...\n")
	if err := access.load(accessNodes[serverRec.MachineID], records, ""); err != nil {
		return err
	}
	if err := applyPeerFirewall(os.Stdout, server, "the server", installer.FirewallMeshFor(records, serverRec.MachineID), existingFirewall, false, false); err != nil {
		return err
	}
	for _, r := range records {
		if c, ok := peers[r.MachineID]; ok {
			if err := access.load(accessNodes[r.MachineID], records, ""); err != nil {
				return err
			}
			if err := applyPeerFirewall(os.Stdout, c, r.Name, installer.FirewallMeshFor(records, r.MachineID), existingFirewall, false, false); err != nil {
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
	if clearsLeftoverInterlock(existing) {
		if cleared, err := installer.ClearLeftoverInterlock(worker); err != nil {
			return nodeAddRetry(err, workerHost)
		} else if cleared {
			fmt.Printf("  ℹ  Removed a fence interlock left on %s from an earlier cluster membership\n", workerHost)
		}
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
	if err := applyPeerFirewall(os.Stdout, worker, workerHost, installer.FirewallMeshFor(records, identity.MachineID), workerFirewall, workerLimit, noRateLimit); err != nil {
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
		if err := saveNodeRecord(server, cluster, rec); err != nil {
			return nodeAddRetry(err, workerHost)
		}
	}
	fmt.Printf("  ✔  %s registered as %s and is cordoned\n", workerHost, nodeName)
	// The guard needs the k3s binary that Register installs.
	if err := access.load(accessNode{rec: rec, node: worker, base: workerConn}, records, ""); err != nil {
		return nodeAddRetry(err, workerHost)
	}
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
	// The server-replica policy lists the server nodes, so it is rendered again
	// whenever the node set changes.
	if err := installer.InstallAdmissionPolicies(server); err != nil {
		return nodeAddRetry(fmt.Errorf("updating the admission policies: %w", err), workerHost)
	}

	rec.State = installer.NodeStateReady
	if err := saveNodeRecord(server, cluster, rec); err != nil {
		return nodeAddRetry(err, workerHost)
	}
	if err := installer.UncordonWorker(server, nodeName); err != nil {
		// Back to the state a retry expects: cordoned and joining.
		rec.State = installer.NodeStateJoining
		_, cordonErr := server.Run("kubectl cordon " + nodeName + " >/dev/null")
		saveErr := saveNodeRecord(server, cluster, rec)
		if cordonErr != nil || saveErr != nil {
			return fmt.Errorf("%w; putting %s back to cordoned and joining also failed (%v), so cordon it with 'kubectl cordon %s' and run 'kip node add %s' again", err, nodeName, errors.Join(cordonErr, saveErr), nodeName, workerHost)
		}
		return nodeAddRetry(err, workerHost)
	}

	fmt.Printf("\n  ✔  %s joined as worker %s (%s mode, %s)\n", workerHost, nodeName, plan.Mode, plan.Joiner.NodeIP)
	fmt.Printf("     Apps, functions, jobs and services can now run on it. The platform stays on the server.\n\n")
	if !noCopies {
		checkJoinedNodeForCopies(os.Stdout, server, worker, serverRec, rec, func(s config.NodeStorage) error {
			rec.Storage = &s
			return saveNodeRecord(server, cluster, rec)
		})
	}
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
	fmt.Printf("  ✔  Network: %s mode, server %s, %s at %s\n", plan.Mode, shownAddress(plan.Mode, plan.Server), workerHost, shownAddress(plan.Mode, plan.Joiner))

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
		fmt.Print(serverNetworkConsent(plan.Mode, shownAddress(plan.Mode, plan.Server)))
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

// rejoinThroughTunnel reconnects to host as login through this workstation's
// device tunnel and verifies its machine ID against id before closing old.
// The joiner must already be in the device's node list. On error, old stays
// open and is returned with its lease-bound runner.
func rejoinThroughTunnel(old *ssh.Client, lease *installer.NodeLease, host, login, sshKey string, cluster *config.Cluster, id string) (*ssh.Client, installer.FirewallRunner, error) {
	cfg, err := nodeSSHVia(host, sshKey, cluster, transport.Tunnel)
	if err != nil {
		return old, lease.Host(old), err
	}
	cfg.User = login
	conn, err := ssh.Dial(cfg)
	if err != nil {
		return old, lease.Host(old), fmt.Errorf("connecting to %s through the tunnel: %w", host, err)
	}
	runner := lease.Host(conn)
	got, err := installer.ReadMachineID(runner)
	if err == nil && got != id {
		err = fmt.Errorf("it answered as machine id %q, not %s", got, id)
	}
	if err != nil {
		_ = conn.Close()
		return old, lease.Host(old), fmt.Errorf("checking %s through the tunnel: %w", host, err)
	}
	_ = old.Close()
	return conn, runner, nil
}

// nodeAddLogin validates and returns an explicit user. If user is empty, it
// uses the host's recorded user, then the cluster's user, then root.
func nodeAddLogin(cluster *config.Cluster, host, user string) (string, error) {
	if user == "" {
		return nodeSSHUser(cluster, host), nil
	}
	if !installer.ValidSSHUser(user) {
		return "", fmt.Errorf("--ssh-user %q is not a valid login name", user)
	}
	return user, nil
}

// confirmJoinLogin checks login against the explicit user or the joining
// machine's recorded user, falling back to the cluster's user. This catches
// mismatches after connecting with unreadable records or a different host
// address. existing identifies a previously recorded machine.
func confirmJoinLogin(cluster *config.Cluster, existing *installer.NodeRecord, host, user, login string) error {
	want, err := nodeAddLogin(cluster, host, user)
	if err != nil {
		return err
	}
	if user == "" && existing != nil {
		want = existing.SSHUser
		if want == "" {
			want = sshUser(cluster)
		}
	}
	if want != login {
		return fmt.Errorf("the cluster records SSH user %q for %s, but kip connected as %q. Run 'kip node add %s --ssh-user %s' to join with the recorded user, or pass another --ssh-user to change it", want, host, login, host, want)
	}
	return nil
}

// recordedLogin returns the explicit user or the node's existing SSH user.
// For a new node, an empty result makes it follow the cluster's login.
func recordedLogin(existing *installer.NodeRecord, user string) string {
	if user != "" {
		return user
	}
	if existing != nil {
		return existing.SSHUser
	}
	return ""
}

// joinedNodeStorage returns the copies check recorded for a node that joins
// again, or nil for a new node.
func joinedNodeStorage(existing *installer.NodeRecord) *config.NodeStorage {
	if existing == nil {
		return nil
	}
	return existing.Storage
}

func nodeAddRetry(err error, host string) error {
	return fmt.Errorf("%w. The join is incomplete. Fix the cause, then run 'kip node add %s' again with the same options to resume", err, host)
}

// saveNodeRecord saves the cluster record and refreshes the local cache when possible.
func saveNodeRecord(server installer.CommandRunner, cluster *config.Cluster, rec installer.NodeRecord) error {
	if err := installer.SaveNodeRecord(server, rec); err != nil {
		return err
	}
	records, err := installer.ReadNodeRecords(server)
	if err != nil {
		fmt.Printf("  ⚠  could not update the local copy of the node records: %v\n", err)
		return nil
	}
	mirrorNodes(cluster, records)
	return nil
}

func runNodeList(cmd *cobra.Command, args []string) error {
	cluster, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	nodes, err := client.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}
	records, recordsErr := nodeRecordsFromAPI(ctx, client.Clientset(), cluster)
	writeNodeList(os.Stdout, nodes, records)
	if recordsErr != nil {
		say(os.Stdout, "  ⚠  could not read the node records (%v). Modes and addresses come from this machine's copy\n\n", recordsErr)
	}
	return nil
}

// writeNodeList prints the node table. Once a worker is recorded, it adds
// each node's network mode and SSH address. A missing record shows "-" in both
// columns, and a missing mode shows "-" in the mode column.
func writeNodeList(out io.Writer, nodes []k8s.NodeInfo, records []installer.NodeRecord) {
	byName := map[string]installer.NodeRecord{}
	workers := false
	for _, r := range records {
		byName[r.Name] = r
		workers = workers || r.Role == installer.NodeRoleWorker
	}

	if !workers {
		say(out, "\n  %-20s %-8s %-25s %-14s %s\n", "NAME", "ROLE", "STATUS", "VERSION", "IP")
		for _, node := range nodes {
			say(out, "  %-20s %-8s %-25s %-14s %s\n", node.Name, node.Role, node.Summary(), node.Version, node.IP)
		}
		say(out, "\n")
		return
	}

	say(out, "\n  %-20s %-8s %-25s %-14s %-16s %-9s %-9s %s\n", "NAME", "ROLE", "STATUS", "VERSION", "IP", "MODE", "COPIES", "ADDRESS")
	for _, node := range nodes {
		mode, copies, address := "-", "-", "-"
		if r, ok := byName[node.Name]; ok {
			address = r.Host
			if r.Mode != "" {
				mode = r.Mode
			}
			copies = nodeCopiesColumn(r)
		}
		say(out, "  %-20s %-8s %-25s %-14s %-16s %-9s %-9s %s\n", node.Name, node.Role, node.Summary(), node.Version, node.IP, mode, copies, address)
	}
	say(out, "\n")
}

// nodeRecordsFromAPI reads the node records through the Kubernetes API. A
// missing ConfigMap yields the local copy without an error.
// Unreadable or invalid records return the local copy together with the error.
func nodeRecordsFromAPI(ctx context.Context, client kubernetes.Interface, cluster *config.Cluster) ([]installer.NodeRecord, error) {
	cm, err := client.CoreV1().ConfigMaps(installer.NodeRecordsNamespace).Get(ctx, installer.NodeRecordsConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return cluster.Nodes, nil
	}
	if err != nil {
		return cluster.Nodes, fmt.Errorf("reading the node records: %w", err)
	}
	records, err := installer.ParseNodeRecordData(cm.Data)
	if err != nil {
		return cluster.Nodes, err
	}
	return records, nil
}

// reportRecordFallback warns that the worker list came from the local copy
// and prints nothing when err is nil.
func reportRecordFallback(out io.Writer, err error) {
	if err == nil {
		return
	}
	say(out, "  Node records:\n")
	say(out, "    ⚠  not read from the cluster (%v). Worker checks use this machine's copy, which can miss workers added from another machine\n\n", err)
}
