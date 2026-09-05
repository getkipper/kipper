package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/alerts"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/k8s"
	"github.com/getkipper/kipper/kip/internal/service"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show cluster status",
	RunE:  runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, args []string) error {
	cluster, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()

	fmt.Printf("\n  Cluster: %s\n", cluster.Domain)
	fmt.Printf("  Host:    %s\n", cluster.Host)
	fmt.Printf("  Config:  %s\n\n", cluster.Kubeconfig)

	// Node summary
	nodes, err := client.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	fmt.Printf("  Nodes:\n")
	for _, node := range nodes {
		icon := "✔"
		if node.Status != "Ready" {
			icon = "✗"
		}
		fmt.Printf("    %s  %-20s %-8s %-8s %s\n",
			icon, node.Name, node.Role, node.Status, node.Version)
	}
	fmt.Println()

	// Component health
	health, err := client.ClusterHealth(ctx)
	if err != nil {
		return fmt.Errorf("checking cluster health: %w", err)
	}

	fmt.Printf("  Components:\n")
	for _, comp := range health {
		icon := "✔"
		if !comp.Healthy {
			icon = "✗"
		}
		fmt.Printf("    %s  %-16s %s\n", icon, comp.Name, comp.Message)
	}
	fmt.Println()

	reportAlertDelivery(ctx, client)
	reportCrashLoopingServices(ctx, client)

	checkHost(cluster, nodes)

	return nil
}

// reportAlertDelivery says how an alert leaves this cluster, and says it loudly
// when the answer is that it does not.
//
// An alert stored in the console bell and nowhere else is only seen by somebody
// who was already looking. The incident behind this fired hourly for three and a
// half days into a bell nobody had open.
func reportAlertDelivery(ctx context.Context, client *k8s.Client) {
	fmt.Printf("  Alerts:\n")

	switch alerts.RouteFor(ctx, client.Clientset()) {
	case alerts.Slack:
		fmt.Printf("    ✔  delivered to Slack\n\n")
	case alerts.Email:
		fmt.Printf("    ✔  emailed to the cluster admins\n\n")
	default:
		fmt.Printf("    ⚠  not leaving this cluster\n")
		fmt.Printf("       Alerts are stored in the console bell and delivered nowhere. Add a\n")
		fmt.Printf("       Slack webhook or SMTP server under Settings in the console.\n\n")
	}
}

// reportCrashLoopingServices names every service on the cluster whose container
// Kubernetes has given up restarting, and says nothing when there are none.
//
// Components above cover the platform's own pieces, so a tenant's database can
// be three days into a crash loop while every line on this screen reads ✔. The
// namespace is part of the line because two projects can each run a service
// called db.
func reportCrashLoopingServices(ctx context.Context, client *k8s.Client) {
	loops := service.CrashLoopsEverywhere(ctx, client.Clientset())
	if len(loops) == 0 {
		return
	}

	fmt.Printf("  Services needing attention:\n")
	for _, l := range loops {
		fmt.Printf("    ✗  %s/%s\n", l.Namespace, l.Service)
		fmt.Printf("       %s\n", l.Message)
	}
	fmt.Println()
}

// checkHost runs the checks that live on the host rather than in the API, over
// one SSH connection. Reaching for SSH at all is the exception here, so the two
// checks that need it share the dial.
func checkHost(cluster *config.Cluster, nodes []k8s.NodeInfo) {
	if cluster.Host == "" {
		return
	}

	explicit, fallback := resolveSSHKey("", cluster)
	client, err := ssh.Dial(ssh.Config{
		Host:            cluster.Host,
		User:            "root",
		KeyPath:         explicit,
		FallbackKeyPath: fallback,
		Options:         []string{"BatchMode=yes"},
	})
	if err != nil {
		fmt.Printf("  DNS resolvers:\n")
		fmt.Printf("    ⚠  not checked (could not reach the host over SSH: %v)\n\n", err)
		fmt.Printf("  Pending restarts:\n")
		fmt.Printf("    ⚠  not checked (could not reach the host over SSH)\n\n")
		return
	}
	defer func() { _ = client.Close() }()

	checkHostDNSResolvers(cluster, client)
	reportPendingRestarts(client)
	reportReadOnlyVolumes(cluster, client, nodes)
}

// reportReadOnlyVolumes checks the mounts on the one node this command can
// reach, and names every node it could not.
//
// The connection goes to the cluster host, so on a multi-node cluster this sees
// the control plane's mount namespace and nothing else. Reporting "no read-only
// volumes" from that would be a worse answer than reporting nothing: the
// controller's log-based detector is what covers the other nodes.
func reportReadOnlyVolumes(cluster *config.Cluster, client *ssh.Client, nodes []k8s.NodeInfo) {
	fmt.Printf("  Volume mounts:\n")

	readOnly, err := installer.ReadOnlyVolumeMounts(client)
	if err != nil {
		fmt.Printf("    ⚠  not checked (could not read the mounts on %s: %v)\n\n", cluster.Host, err)
		return
	}

	if len(readOnly) == 0 {
		fmt.Printf("    ✔  no read-only volumes on %s\n", cluster.Host)
	} else {
		for _, mount := range readOnly {
			fmt.Printf("    ✗  mounted read-only: %s\n", mount)
		}
		fmt.Printf("       Whatever writes to it has stopped being able to. Recreate the pod\n")
		fmt.Printf("       with 'kip service restart <name>'; restarting the container cannot\n")
		fmt.Printf("       clear a mount.\n")
	}

	// A node this command never looked at must not be covered by the line above.
	var unchecked []string
	for _, node := range nodes {
		if !nodeIsHost(node, cluster.Host) {
			unchecked = append(unchecked, node.Name)
		}
	}
	if len(unchecked) > 0 {
		fmt.Printf("    ⚠  not checked on %s (this command reads mounts on %s only)\n",
			strings.Join(unchecked, ", "), cluster.Host)
	}
	fmt.Println()
}

// nodeIsHost reports whether a node is the one the status command connected to.
// Its name and the configured host are often the same string and sometimes an
// address against a hostname, so both are compared.
func nodeIsHost(node k8s.NodeInfo, host string) bool {
	return node.Name == host || node.IP == host
}

// reportPendingRestarts says what the host still owes.
//
// Kipper tells needrestart to leave the storage path alone, because restarting
// iscsid fails every Longhorn volume on the node. That trades an availability
// risk for a security one: the patched library stays unloaded until something
// restarts the daemon. Reporting it here is what keeps the deferral from
// becoming drift nobody remembers agreeing to.
func reportPendingRestarts(client *ssh.Client) {
	fmt.Printf("  Pending restarts:\n")

	pending, err := installer.PendingRestarts(client)
	if err != nil {
		fmt.Printf("    ⚠  not checked (%v)\n\n", err)
		return
	}
	if !pending.Any() {
		fmt.Printf("    ✔  nothing waiting on a restart\n\n")
		return
	}

	if len(pending.Deferred) > 0 {
		fmt.Printf("    ⚠  deferred by Kipper: %s\n", strings.Join(pending.Deferred, ", "))
		fmt.Printf("       Patched libraries stay unloaded until these restart. Restarting them\n")
		fmt.Printf("       drops every Longhorn volume on this node, so reboot the node during a\n")
		fmt.Printf("       window you choose rather than restarting the units directly.\n")
	}
	if len(pending.Other) > 0 {
		fmt.Printf("    ⚠  waiting on a restart: %s\n", strings.Join(pending.Other, ", "))
	}
	if pending.KernelOutdated {
		fmt.Printf("    ⚠  the installed kernel is not the running one; the node needs a reboot\n")
	}
	fmt.Println()
}

// checkHostDNSResolvers audits the curated resolv.conf against the resolver set
// the cluster was configured with: an unsafe hand-edit, a divergence from the
// configured set, and an unreachable resolver each get their own warning.
//
// Best-effort: a missing file must not fail status, but it prints a "not
// checked" line, because a silently absent section would read as "audited and
// fine".
func checkHostDNSResolvers(cluster *config.Cluster, client *ssh.Client) {
	fmt.Printf("  DNS resolvers:\n")

	body, err := client.Run("cat " + installer.ResolvConfPath())
	if err != nil {
		fmt.Printf("    ⚠  not checked (could not read %s on the host: %v)\n\n", installer.ResolvConfPath(), err)
		return
	}

	resolvers, problem := installer.CheckResolvConf(body)
	if problem != nil {
		fmt.Printf("    ⚠  %s\n", problem)
		fmt.Printf("       run 'kip cluster dns repair' to restore the configured resolvers,\n")
		fmt.Printf("       or fix the nameserver entries in %s by hand\n\n", installer.ResolvConfPath())
		return
	}

	// Drift and reachability are independent failures, so both checks run
	// and both report. An empty dns_resolvers in the local config means the
	// install used the default set, so there is always an intent to compare
	// against.
	healthy := true
	configured := cluster.DNSResolvers
	if len(configured) == 0 {
		configured = installer.DefaultDNSResolvers()
	}
	if !installer.ResolversMatch(resolvers, configured) {
		healthy = false
		fmt.Printf("    ⚠  live resolvers (%s) differ from the configured set (%s)\n",
			strings.Join(resolvers, ", "), strings.Join(configured, ", "))
		fmt.Printf("       %s was changed outside kip: run 'kip cluster dns repair'\n", installer.ResolvConfPath())
		fmt.Printf("       to restore the configured set, or update dns_resolvers in\n")
		fmt.Printf("       ~/.kip/config.yaml if the new set is intended\n")
	}

	unreachable, probeErr := installer.ProbeResolvers(client, resolvers)
	switch {
	case probeErr != nil:
		healthy = false
		fmt.Printf("    ⚠  reachability not checked (probing the resolvers from the host failed: %v)\n", probeErr)
	case len(unreachable) > 0:
		healthy = false
		for _, r := range unreachable {
			fmt.Printf("    ⚠  resolver %s did not accept a TCP connection on port 53 from the host\n", r)
		}
	}

	if healthy {
		fmt.Printf("    ✔  %s\n", strings.Join(resolvers, ", "))
	}
	fmt.Println()
}
