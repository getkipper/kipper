package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
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

	reportStorageRestartCoverage(nodes)
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

	route, err := alerts.Lookup(ctx, client.Clientset())
	if err != nil {
		// Not every operator can read kipper-system. Saying alerts go nowhere
		// on the strength of a refused read would be a false alarm about the
		// one thing this section exists to report.
		fmt.Printf("    ⚠  not checked (%v)\n\n", err)
		return
	}

	switch route {
	case alerts.Slack:
		fmt.Printf("    ✔  delivered to Slack\n\n")
	case alerts.Email:
		fmt.Printf("    ✔  emailed to the cluster admins\n\n")
	default:
		fmt.Printf("    ⚠  not leaving this cluster\n")
		if alerts.NowhereReason(ctx, client.Clientset()) == alerts.NoRecipients {
			fmt.Printf("       An SMTP server is configured, but no cluster admin has an email\n")
			fmt.Printf("       address, so there is nobody to send to. Give an admin an address\n")
			fmt.Printf("       under Team access in the console, or add a Slack webhook.\n\n")
			return
		}
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

func checkHost(cluster *config.Cluster, nodes []k8s.NodeInfo) {
	if cluster.Host == "" {
		return
	}
	checkHostWith(os.Stdout, hostSSHConfig(cluster, ""), cluster, nodes)
}

// hostSSHConfig applies the shared SSH key precedence and disables interactive prompts.
func hostSSHConfig(cluster *config.Cluster, keyFlag string) ssh.Config {
	explicit, fallback := resolveSSHKey(keyFlag, cluster)
	return ssh.Config{
		Host:            cluster.Host,
		User:            "root",
		KeyPath:         explicit,
		FallbackKeyPath: fallback,
		Options:         []string{"BatchMode=yes"},
	}
}

func checkHostWith(out io.Writer, sshConfig ssh.Config, cluster *config.Cluster, nodes []k8s.NodeInfo) {
	client, err := ssh.Dial(sshConfig)
	if err != nil {
		// Show each skipped check so missing results cannot be mistaken for success.
		say(out, "  DNS resolvers:\n")
		say(out, "    ⚠  not checked (could not reach the host over SSH: %v)\n\n", err)
		say(out, "  Pending restarts:\n")
		say(out, "    ⚠  not checked (could not reach the host over SSH)\n\n")
		say(out, "  Volume mounts:\n")
		say(out, "    ⚠  not checked (could not reach the host over SSH)\n\n")
		return
	}
	defer func() { _ = client.Close() }()

	checkHostDNSResolvers(out, cluster, client)
	reportPendingRestarts(out, client)
	reportReadOnlyVolumes(out, cluster, client, nodes)
}

// reportStorageRestartCoverage checks node annotations for missing, outdated,
// or replaced-host needrestart configuration. Annotations record successful
// writes; later manual file changes are invisible to this check.
func reportStorageRestartCoverage(nodes []k8s.NodeInfo) {
	var uncovered []string
	withoutNeedrestart := 0
	for _, node := range nodes {
		if !installer.NeedrestartApplies(node.OSImage) {
			withoutNeedrestart++
			continue
		}
		covered, why := installer.StorageRestartCoverage(
			node.Annotations[installer.StorageRestartsVersionAnnotation],
			node.Annotations[installer.StorageRestartsMachineAnnotation],
			node.MachineID,
		)
		if !covered {
			uncovered = append(uncovered, fmt.Sprintf("%s (%s)", node.Name, why))
		}
	}

	fmt.Printf("  Host configuration:\n")
	if withoutNeedrestart > 0 {
		fmt.Printf("    ℹ  %d node(s) run a distribution without needrestart; package updates there\n", withoutNeedrestart)
		fmt.Printf("       may restart iscsid, so update storage packages during planned downtime\n")
	}
	if len(uncovered) == 0 {
		if withoutNeedrestart < len(nodes) {
			fmt.Printf("    ✔  needrestart leaves the storage path alone on every node that uses it\n")
		}
		fmt.Printf("\n")
		return
	}

	for _, node := range uncovered {
		fmt.Printf("    ⚠  %s\n", node)
	}
	fmt.Printf("       needrestart can restart iscsid on these after a library update, which\n")
	fmt.Printf("       drops every Longhorn volume on the node. Fix one with:\n")
	fmt.Printf("         kip node repair-host --host <address>\n\n")
}

// reportReadOnlyVolumes checks CSI mounts in the cluster host's mount namespace.
// Results are scoped to that host; other known nodes are listed as unchecked.
func reportReadOnlyVolumes(out io.Writer, cluster *config.Cluster, client *ssh.Client, nodes []k8s.NodeInfo) {
	say(out, "  Volume mounts:\n")

	readOnly, err := installer.ReadOnlyVolumeMounts(client)
	if err != nil {
		say(out, "    ⚠  not checked (could not read the mounts on %s: %v)\n\n", cluster.Host, err)
		return
	}

	if len(readOnly) == 0 {
		say(out, "    ✔  no read-only volumes on %s\n", cluster.Host)
	} else {
		for _, mount := range readOnly {
			say(out, "    ✗  mounted read-only: %s\n", mount)
		}
		say(out, "       Writes to these volumes fail. Recreate the affected pod\n")
		say(out, "       with 'kip service restart <name>'; a container restart alone\n")
		say(out, "       cannot restore a writable mount.\n")
	}

	var unchecked []string
	for _, node := range nodes {
		if !nodeIsHost(node, cluster.Host, nodes) {
			unchecked = append(unchecked, node.Name)
		}
	}
	if len(unchecked) > 0 {
		say(out, "    ⚠  not checked on %s (this command reads mounts on %s only)\n",
			strings.Join(unchecked, ", "), cluster.Host)
	}
	say(out, "\n")
}

// nodeIsHost reports whether a node is the one the status command connected to.
//
// The node name, the node address and the configured host are three things that
// usually agree and sometimes do not: a cluster configured by DNS name whose
// node registered under a bare hostname matches neither comparison, and the
// node whose mounts were just read then appeared under "not checked".
//
// A single-node cluster is the host by definition, which covers most of the
// fleet without guessing.
func nodeIsHost(node k8s.NodeInfo, host string, all []k8s.NodeInfo) bool {
	if len(all) == 1 {
		return true
	}
	if node.Name == host || node.IP == host {
		return true
	}
	// A host given as name.domain against a node registered as name.
	if base, _, found := strings.Cut(host, "."); found && node.Name == base {
		return true
	}
	return false
}

// reportPendingRestarts surfaces updates that still need a service restart or reboot.
// Storage service restarts can disrupt Longhorn volumes, so operators need
// to schedule downtime to load updated libraries.
func reportPendingRestarts(out io.Writer, client *ssh.Client) {
	say(out, "  Pending restarts:\n")

	pending, err := installer.PendingRestarts(client)
	if err != nil {
		say(out, "    ⚠  not checked (%v)\n\n", err)
		return
	}
	if !pending.Any() {
		say(out, "    ✔  nothing waiting on a restart\n\n")
		return
	}

	if len(pending.Deferred) > 0 {
		say(out, "    ⚠  storage services awaiting restart: %s\n", strings.Join(pending.Deferred, ", "))
		say(out, "       These services need a restart to load updated libraries.\n")
		say(out, "       Restarting them can disrupt Longhorn volumes. Schedule downtime\n")
		say(out, "       and reboot with 'kip node reboot' instead of restarting these\n")
		say(out, "       services directly.\n")
	}
	if len(pending.Other) > 0 {
		say(out, "    ⚠  waiting on a restart: %s\n", strings.Join(pending.Other, ", "))
	}
	if pending.KernelOutdated {
		say(out, "    ⚠  a kernel update is pending; reboot with 'kip node reboot'\n")
	}
	if pending.RebootRequired {
		say(out, "    ⚠  updates require a reboot; run 'kip node reboot'\n")
	}
	say(out, "\n")
}

// checkHostDNSResolvers checks resolver validity, configuration, and reachability.
// Read failures are reported as unchecked so status can continue with other checks.
func checkHostDNSResolvers(out io.Writer, cluster *config.Cluster, client *ssh.Client) {
	say(out, "  DNS resolvers:\n")

	body, err := client.Run("cat " + installer.ResolvConfPath())
	if err != nil {
		say(out, "    ⚠  not checked (could not read %s on the host: %v)\n\n", installer.ResolvConfPath(), err)
		return
	}

	resolvers, problem := installer.CheckResolvConf(body)
	if problem != nil {
		say(out, "    ⚠  %s\n", problem)
		say(out, "       run 'kip cluster dns repair' to restore the configured resolvers,\n")
		say(out, "       or fix the nameserver entries in %s by hand\n\n", installer.ResolvConfPath())
		return
	}

	// Report configuration drift and reachability independently.
	healthy := true
	configured := cluster.DNSResolvers
	if len(configured) == 0 {
		configured = installer.DefaultDNSResolvers()
	}
	if !installer.ResolversMatch(resolvers, configured) {
		healthy = false
		say(out, "    ⚠  live resolvers (%s) differ from the configured set (%s)\n",
			strings.Join(resolvers, ", "), strings.Join(configured, ", "))
		say(out, "       Run 'kip cluster dns repair' to restore %s\n", installer.ResolvConfPath())
		say(out, "       to the configured set, or update dns_resolvers in\n")
		say(out, "       ~/.kip/config.yaml if the new set is intended\n")
	}

	unreachable, probeErr := installer.ProbeResolvers(client, resolvers)
	switch {
	case probeErr != nil:
		healthy = false
		say(out, "    ⚠  reachability not checked (probing the resolvers from the host failed: %v)\n", probeErr)
	case len(unreachable) > 0:
		healthy = false
		for _, r := range unreachable {
			say(out, "    ⚠  resolver %s did not accept a TCP connection on port 53 from the host\n", r)
		}
	}

	if healthy {
		say(out, "    ✔  %s\n", strings.Join(resolvers, ", "))
	}
	say(out, "\n")
}
