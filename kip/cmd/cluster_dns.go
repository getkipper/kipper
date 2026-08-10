package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

var clusterDNSCmd = &cobra.Command{
	Use:   "dns",
	Short: "Manage the cluster's upstream DNS resolvers",
}

var clusterDNSRepairCmd = &cobra.Command{
	Use:   "repair",
	Short: "Restore the curated resolver file and restart CoreDNS",
	Long: `Rewrites /etc/rancher/k3s/resolv.conf on the host from the resolver set the
cluster was configured with (dns_resolvers in ~/.kip/config.yaml, or the
default public resolvers), then restarts CoreDNS so it picks the file up.

This is the scoped fix for the resolver drift 'kip status' warns about: a
hand-edited or broken resolver file. Nothing else about the installation is
touched.

Examples:
  kip cluster dns repair`,
	Args: cobra.NoArgs,
	RunE: runClusterDNSRepair,
}

func init() {
	clusterDNSCmd.AddCommand(clusterDNSRepairCmd)
	clusterCmd.AddCommand(clusterDNSCmd)
}

func runClusterDNSRepair(_ *cobra.Command, _ []string) error {
	// Config-only load: DNS repair is a host-level recovery operation over
	// SSH and must work even when the local kubeconfig is missing or broken.
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}
	if cluster.Host == "" {
		return fmt.Errorf("no host recorded for cluster %s; DNS repair needs SSH access to the server", cluster.Name)
	}

	explicit, fallback := resolveSSHKey("", cluster)
	client, err := ssh.Dial(ssh.Config{
		Host:            cluster.Host,
		User:            "root",
		KeyPath:         explicit,
		FallbackKeyPath: fallback,
	})
	if err != nil {
		return fmt.Errorf("connecting to %s over SSH: %w", cluster.Host, err)
	}
	defer func() { _ = client.Close() }()

	applied, err := installer.RepairHostDNS(client, cluster.DNSResolvers)
	if err != nil {
		return err
	}
	fmt.Printf("\n  ✔  Restored resolvers on %s: %s\n", cluster.Host, strings.Join(applied, ", "))
	fmt.Printf("  ✔  CoreDNS restarted to pick them up\n\n")
	return nil
}
