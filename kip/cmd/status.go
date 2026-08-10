package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
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

	checkHostDNSResolvers(cluster)

	return nil
}

// checkHostDNSResolvers reads the curated resolv.conf from the host and
// audits it against the resolver set the cluster was configured with: an
// unsafe hand-edit, a divergence from the configured set, and an
// unreachable resolver each get their own warning. The file lives on the
// host, outside the K8s API, so this is the one place status reaches for
// SSH. Best-effort: no SSH key, an unreachable host, or a missing file
// must not fail status — but each prints a "not checked" line, because a
// silently absent section would read as "audited and fine". BatchMode
// keeps the dial from ever blocking on a passphrase prompt.
func checkHostDNSResolvers(cluster *config.Cluster) {
	if cluster.Host == "" {
		return
	}

	fmt.Printf("  DNS resolvers:\n")

	explicit, fallback := resolveSSHKey("", cluster)
	client, err := ssh.Dial(ssh.Config{
		Host:            cluster.Host,
		User:            "root",
		KeyPath:         explicit,
		FallbackKeyPath: fallback,
		Options:         []string{"BatchMode=yes"},
	})
	if err != nil {
		fmt.Printf("    ⚠  not checked (could not reach the host over SSH: %v)\n\n", err)
		return
	}
	defer func() { _ = client.Close() }()

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
