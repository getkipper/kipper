package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/alerts"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/k8s"
	"github.com/getkipper/kipper/kip/internal/manifest"
	"github.com/getkipper/kipper/kip/internal/service"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

var statusCmd = &cobra.Command{
	Use:          "status",
	Short:        "Show cluster status",
	RunE:         runStatus,
	SilenceUsage: true,
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
		return nodesUnavailableError(err)
	}

	fmt.Printf("  Nodes:\n")
	for _, node := range nodes {
		fmt.Printf("    %s  %-20s %-8s %-8s %s\n",
			nodeMark(node), node.Name, node.Role, node.Summary(), node.Version)
	}
	fmt.Println()

	// Component health
	health, err := client.ClusterHealth(ctx)
	if err != nil {
		return fmt.Errorf("checking cluster health: %w", err)
	}

	fmt.Printf("  Components:\n")
	for _, comp := range health {
		msg := comp.Message
		if comp.Starting {
			msg += ", starting"
		}
		fmt.Printf("    %s  %-16s %s\n", componentMark(comp), comp.Name, msg)
	}
	fmt.Println()

	if client.StorageMode(ctx) == platform.StorageLocal {
		reportLocalStorage(ctx, os.Stdout, client.Clientset(), nodes, backupsConfiguredOn(ctx, client.Dynamic()), func() ([]volumeUsage, error) {
			if cluster.Host == "" {
				return nil, fmt.Errorf("no host recorded for this cluster")
			}
			cfg, err := hostSSHConfig(cluster, "")
			if err != nil {
				return nil, err
			}
			host, err := ssh.Dial(cfg)
			if err != nil {
				return nil, fmt.Errorf("could not reach the host over SSH: %w", err)
			}
			defer func() { _ = host.Close() }()
			return localVolumeUsage(host)
		})
	} else {
		reportStorageRestartCoverage(nodes)
		reportServerStorageRoom(ctx, os.Stdout, &service.Manager{Client: client.Clientset(), Dynamic: client.Dynamic()})
	}
	reportAlertDelivery(ctx, client)
	reportCrashLoopingServices(ctx, client)
	reportPlaintextPostgres(ctx, client)

	records, recordsErr := nodeRecordsFromAPI(ctx, client.Clientset(), cluster)
	reportRecordFallback(os.Stdout, recordsErr)
	checkHost(cluster, nodes, records)

	return nil
}

// nodeMark marks unready nodes with ✗ and cordoned Ready nodes with ⚠ so a
// cordon left by an interrupted command stands out. Other nodes get ✔.
func nodeMark(node k8s.NodeInfo) string {
	switch {
	case node.Status != "Ready":
		return "✗"
	case node.Cordoned:
		return "⚠"
	}
	return "✔"
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

// reportPlaintextPostgres names Postgres services that may still accept
// network connections without TLS. A failed listing prints nothing.
func reportPlaintextPostgres(ctx context.Context, client *k8s.Client) {
	list, err := client.Dynamic().Resource(manifest.ServiceGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	open := plaintextPostgres(list.Items)
	if len(open) == 0 {
		return
	}
	fmt.Printf("  Postgres services that may still accept connections without TLS:\n")
	for _, line := range open {
		fmt.Printf("    ⚠  %s\n", line)
	}
	fmt.Printf("       Require TLS with kip service update <name> --tls required once their apps connect with TLS.\n\n")
}

// plaintextPostgres describes each Postgres service that may still let a
// client connect without TLS: mode off or on, or mode required before the
// controller has confirmed the restart into that mode. Results are sorted by
// namespace and name.
func plaintextPostgres(services []unstructured.Unstructured) []string {
	var open []string
	for i := range services {
		svc := &services[i]
		if t, _, _ := unstructured.NestedString(svc.Object, "spec", "type"); t != "postgres" {
			continue
		}
		mode, _, _ := unstructured.NestedString(svc.Object, "spec", "tls")
		where := svc.GetNamespace() + "/" + svc.GetName()
		switch {
		case mode == "required" && service.ServesTLS(svc):
			continue
		case mode == "required":
			open = append(open, where+" (tls required, not confirmed yet)")
		case mode == "on":
			open = append(open, where+" (tls on)")
		default:
			open = append(open, where+" (tls off)")
		}
	}
	sort.Strings(open)
	return open
}

func checkHost(cluster *config.Cluster, nodes []k8s.NodeInfo, records []installer.NodeRecord) {
	if cluster.Host == "" {
		return
	}
	cfg, err := hostSSHConfig(cluster, "")
	if err != nil {
		fmt.Printf("\n  Host checks skipped: %v\n", err)
		return
	}
	checkHostWith(os.Stdout, cfg, cluster, nodes, records)
}

// sshUser returns the cluster's saved login user, defaulting to root when the
// cluster is nil or has no saved user.
func sshUser(cluster *config.Cluster) string {
	if cluster != nil && cluster.SSHUser != "" {
		return cluster.SSHUser
	}
	return "root"
}

// nodeSSHUser returns the first matching node's SSH user, falling back to the
// cluster's user and then root. Host matching ignores case and IPv6 brackets.
func nodeSSHUser(cluster *config.Cluster, host string) string {
	if cluster != nil {
		for _, n := range cluster.Nodes {
			if !strings.EqualFold(strings.Trim(n.Host, "[]"), strings.Trim(host, "[]")) {
				continue
			}
			if n.SSHUser != "" {
				return n.SSHUser
			}
			break
		}
	}
	return sshUser(cluster)
}

// withRecordedNodes returns a copy of cluster with records read through server
// so connections use the recorded logins. A read failure returns the original
// cluster and the error.
func withRecordedNodes(server installer.CommandRunner, cluster *config.Cluster) (*config.Cluster, error) {
	records, err := installer.ReadNodeRecords(server)
	if err != nil {
		return cluster, err
	}
	return withNodes(cluster, records), nil
}

// withNodes returns a copy of cluster with a copy of records so subsequent
// connections use their recorded logins.
func withNodes(cluster *config.Cluster, records []config.NodeRecord) *config.Cluster {
	c := *cluster
	c.Nodes = append([]config.NodeRecord(nil), records...)
	return &c
}

// hostSSHConfig is nodeSSH for the server, with interactive prompts disabled.
func hostSSHConfig(cluster *config.Cluster, keyFlag string) (ssh.Config, error) {
	cfg, err := nodeSSH(cluster.Host, keyFlag, cluster)
	if err != nil {
		return ssh.Config{}, err
	}
	cfg.Options = append([]string{"BatchMode=yes"}, cfg.Options...)
	return cfg, nil
}

// checkHostWith runs the host checks on the server over sshConfig and on every
// worker in records, then prints them.
func checkHostWith(out io.Writer, sshConfig ssh.Config, cluster *config.Cluster, nodes []k8s.NodeInfo, records []installer.NodeRecord) {
	var server installer.CommandRunner
	client, err := ssh.Dial(sshConfig)
	if err == nil {
		defer func() { _ = client.Close() }()
		server = client
		refreshDeviceNodes(out, cluster, client, records)
	}
	workers := inspectWorkers(records, func(rec installer.NodeRecord) (installer.CommandRunner, error) {
		return captureWorkerChecks(sshConfig, cluster, rec)
	})
	reportHostChecks(out, cluster, nodes, serverTarget(cluster, server, err, records), workers)
}

// reportHostChecks reports host checks for the server, then workers,
// marking unreachable hosts as unchecked. With workers, result prefixes identify
// nodes, while healthy mount results name the host within the message.
// Data placement uses the server, and firewall warnings require a readable marker.
// Operator access reporting requires a server runner to read the cluster record.
func reportHostChecks(out io.Writer, cluster *config.Cluster, nodes []k8s.NodeInfo, server hostTarget, workers []hostTarget) {
	targets := append([]hostTarget{server}, workers...)
	if len(workers) > 0 {
		for i := range targets {
			targets[i].prefix = targets[i].name + ": "
		}
	}

	say(out, "  DNS resolvers:\n")
	for _, t := range targets {
		if t.notChecked(out, true) {
			continue
		}
		checkHostDNSResolvers(out, cluster, t)
	}
	say(out, "\n")

	say(out, "  Pending restarts:\n")
	for _, t := range targets {
		if t.notChecked(out, false) {
			continue
		}
		reportPendingRestarts(out, t)
	}
	say(out, "\n")

	say(out, "  Volume mounts:\n")
	for _, t := range targets {
		if t.notChecked(out, false) {
			continue
		}
		reportReadOnlyVolumes(out, t)
	}
	reportUncheckedMounts(out, cluster.Host, nodes, workers)
	say(out, "\n")

	if server.runner != nil {
		reportDataPlacement(out, server.runner, cluster, nodes)
	}
	reportUFWFailOpen(out, targets)
	if server.runner != nil {
		reportAccessGuards(out, server.runner, targets)
	}
}

// reportUFWFailOpen reports failed firewall restoration attempts recorded on
// the nodes under one heading. It prints nothing when no marker can be read.
func reportUFWFailOpen(out io.Writer, targets []hostTarget) {
	heading := false
	for _, t := range targets {
		if t.runner == nil {
			continue
		}
		marker, err := installer.UFWFailOpen(t.runner)
		if err != nil || marker == "" {
			continue
		}
		if !heading {
			say(out, "  Host firewall:\n")
			heading = true
		}
		run, at, _ := strings.Cut(marker, " ")
		say(out, "    ⚠  %sa firewall rollback (run %s, %s) could not restore the previous firewall and attempted to switch ufw off. Check the host's firewall state. %s\n", t.prefix, run, at, t.firewallRepair)
	}
	if heading {
		say(out, "\n")
	}
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

// reportReadOnlyVolumes checks CSI mounts in one node's mount namespace.
func reportReadOnlyVolumes(out io.Writer, t hostTarget) {
	readOnly, err := installer.ReadOnlyVolumeMounts(t.runner)
	if err != nil {
		say(out, "    ⚠  %snot checked (could not read the mounts on %s: %v)\n", t.prefix, t.where, err)
		return
	}

	if len(readOnly) == 0 {
		say(out, "    ✔  no read-only volumes on %s\n", t.where)
		return
	}
	for _, mount := range readOnly {
		say(out, "    ✗  %smounted read-only: %s\n", t.prefix, mount)
	}
	say(out, "       Writes to these volumes fail. Recreate the affected pod\n")
	say(out, "       with 'kip service restart <name>'. A container restart alone\n")
	say(out, "       cannot restore a writable mount.\n")
}

// reportUncheckedMounts names the nodes that are neither the configured host
// nor a recorded worker, so no mount check reached them.
func reportUncheckedMounts(out io.Writer, host string, nodes []k8s.NodeInfo, workers []hostTarget) {
	recorded := map[string]bool{}
	for _, w := range workers {
		recorded[w.name] = true
	}
	var unchecked []string
	for _, node := range nodes {
		if !nodeIsHost(node, host, nodes) && !recorded[node.Name] {
			unchecked = append(unchecked, node.Name)
		}
	}
	if len(unchecked) > 0 {
		say(out, "    ⚠  not checked on %s (this command reads mounts on %s and recorded workers only)\n",
			strings.Join(unchecked, ", "), host)
	}
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
func reportPendingRestarts(out io.Writer, t hostTarget) {
	pending, err := installer.PendingRestarts(t.runner)
	if err != nil {
		say(out, "    ⚠  %snot checked (%v)\n", t.prefix, err)
		return
	}
	if !pending.Any() {
		say(out, "    ✔  %snothing waiting on a restart\n", t.prefix)
		return
	}

	if len(pending.Deferred) > 0 {
		say(out, "    ⚠  %sstorage services awaiting restart: %s\n", t.prefix, strings.Join(pending.Deferred, ", "))
		say(out, "       These services need a restart to load updated libraries.\n")
		say(out, "       Restarting them can disrupt Longhorn volumes. Schedule downtime\n")
		say(out, "       and reboot with '%s' instead of restarting these\n", t.reboot)
		say(out, "       services directly.\n")
	}
	if len(pending.Other) > 0 {
		say(out, "    ⚠  %swaiting on a restart: %s\n", t.prefix, strings.Join(pending.Other, ", "))
	}
	if pending.KernelOutdated {
		say(out, "    ⚠  %sa kernel update is pending. Reboot with '%s'\n", t.prefix, t.reboot)
	}
	if pending.RebootRequired {
		say(out, "    ⚠  %supdates require a reboot. Run '%s'\n", t.prefix, t.reboot)
	}
}

// checkHostDNSResolvers checks resolver validity, configuration, and reachability.
// Read failures are reported as unchecked so status can continue with other checks.
func checkHostDNSResolvers(out io.Writer, cluster *config.Cluster, t hostTarget) {
	body, err := installer.ReadResolvConf(t.runner)
	if err != nil {
		say(out, "    ⚠  %snot checked (could not read %s on the host: %v)\n", t.prefix, installer.ResolvConfPath(), err)
		return
	}

	resolvers, problem := installer.CheckResolvConf(body)
	if problem != nil {
		say(out, "    ⚠  %s%s\n", t.prefix, problem)
		say(out, "       run 'kip cluster dns repair' to restore the configured resolvers,\n")
		say(out, "       or fix the nameserver entries in %s by hand\n", installer.ResolvConfPath())
		return
	}

	// Report configuration drift and reachability independently.
	healthy := true
	configured := configuredResolvers(cluster)
	if !installer.ResolversMatch(resolvers, configured) {
		healthy = false
		say(out, "    ⚠  %slive resolvers (%s) differ from the configured set (%s)\n",
			t.prefix, strings.Join(resolvers, ", "), strings.Join(configured, ", "))
		say(out, "       Run 'kip cluster dns repair' to restore %s\n", installer.ResolvConfPath())
		say(out, "       to the configured set, or update dns_resolvers in\n")
		say(out, "       ~/.kip/config.yaml if the new set is intended\n")
	}

	unreachable, probeErr := installer.ProbeResolvers(t.runner, resolvers)
	switch {
	case probeErr != nil:
		healthy = false
		say(out, "    ⚠  %sreachability not checked (probing the resolvers from the host failed: %v)\n", t.prefix, probeErr)
	case len(unreachable) > 0:
		healthy = false
		for _, r := range unreachable {
			say(out, "    ⚠  %sresolver %s did not accept a TCP connection on port 53 from the host\n", t.prefix, r)
		}
	}

	if healthy {
		say(out, "    ✔  %s%s\n", t.prefix, strings.Join(resolvers, ", "))
	}
}

// configuredResolvers returns the cluster's resolver set, or the defaults
// when none is configured.
func configuredResolvers(cluster *config.Cluster) []string {
	if len(cluster.DNSResolvers) > 0 {
		return cluster.DNSResolvers
	}
	return installer.DefaultDNSResolvers()
}

func componentMark(c k8s.ComponentStatus) string {
	switch {
	case c.Healthy:
		return "✔"
	case c.Starting:
		return "…"
	default:
		return "✗"
	}
}
