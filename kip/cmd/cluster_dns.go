package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/installer"
)

var clusterDNSCmd = &cobra.Command{
	Use:   "dns",
	Short: "Manage the cluster's upstream DNS resolvers",
}

var clusterDNSRepairCmd = &cobra.Command{
	Use:          "repair",
	Short:        "Restore the curated resolver file and restart CoreDNS",
	SilenceUsage: true,
	Long: `Rewrites /etc/rancher/k3s/resolv.conf on every recorded node from the
resolver set the cluster was configured with (dns_resolvers in
~/.kip/config.yaml, or the default public resolvers), then restarts CoreDNS
so it picks the file up. The server goes first, then each worker one at a
time. kip stops at the first node it cannot reach or repair, before the
restart. Run the command again to finish, starting from the server.

This repairs the resolver drift 'kip status' warns about, such as a
hand-edited or broken resolver file. It also synchronizes node records
with the local cluster configuration and leaves other installation
settings unchanged.

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
		return fmt.Errorf("no host recorded for cluster %s. DNS repair needs SSH access to the server", cluster.Name)
	}

	leased, err := leaseCluster(cluster, "", "kip cluster dns repair")
	if err != nil {
		return err
	}
	defer leased.close()
	records, err := recordNodes(leased.server, cluster)
	if err != nil {
		return err
	}
	if err := nodesSettled(records); err != nil {
		return err
	}

	fmt.Println()
	if err := repairDNSOnNodes(records, leased.server, leased.dialer(cluster, ""), cluster.DNSResolvers); err != nil {
		return err
	}
	fmt.Printf("  ✔  CoreDNS restarted to pick them up\n\n")
	return nil
}

// repairDNSOnNodes writes the resolver file on the server, then on each
// worker, and restarts CoreDNS once every node has it. It stops at the first
// node that fails, before the restart.
func repairDNSOnNodes(records []installer.NodeRecord, server installer.FirewallRunner, dial nodeDialer, resolvers []string) error {
	err := forEachNode(records, server, dial, func(rec installer.NodeRecord, client installer.FirewallRunner) error {
		written, err := installer.WriteHostDNS(client, resolvers)
		if err != nil {
			return err
		}
		fmt.Printf("  ✔  Restored resolvers on %s (%s): %s\n", rec.Name, rec.Host, strings.Join(written, ", "))
		return nil
	})
	if err != nil {
		return err
	}
	return installer.RestartCoreDNS(server)
}
