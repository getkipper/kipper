package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/config"
)

var clusterHostsCmd = &cobra.Command{
	Use:   "hosts",
	Short: "Show or pin the hostnames kip uses to reach a cluster",
	Long: `Shows the console, API and login hostnames kip will use for a cluster,
and lets you correct them without contacting the cluster.

Everything else that fixes these reads them from the cluster itself, which is no
help when the stored login host is the reason you cannot sign in. This command
only edits ~/.kip/config.yaml, so it works when nothing else does.

By default each host follows the cluster's domain. A cluster serving on names
that do not follow that convention needs them pinned, and a fresh
'kip cluster export' from a working machine carries them for you.

Examples:
  kip cluster hosts
  kip cluster hosts --dex dex.example.com
  kip cluster hosts --console console.example.com --console-api api.example.com
  kip cluster hosts --reset`,
	RunE: runClusterHosts,
}

func init() {
	clusterHostsCmd.Flags().String("cluster", "", "cluster name (defaults to current)")
	clusterHostsCmd.Flags().String("console", "", "pin the console hostname")
	clusterHostsCmd.Flags().String("console-api", "", "pin the API hostname")
	clusterHostsCmd.Flags().String("dex", "", "pin the login hostname")
	clusterHostsCmd.Flags().Bool("reset", false, "clear every pinned hostname and follow the cluster domain again")
	clusterCmd.AddCommand(clusterHostsCmd)
}

func runClusterHosts(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	clusterName, _ := cmd.Flags().GetString("cluster")
	if clusterName == "" {
		clusterName = cfg.CurrentCluster
	}
	if cfg.GetCluster(clusterName) == nil {
		return fmt.Errorf("cluster %q not found", clusterName)
	}

	console, _ := cmd.Flags().GetString("console")
	consoleAPI, _ := cmd.Flags().GetString("console-api")
	dex, _ := cmd.Flags().GetString("dex")
	reset, _ := cmd.Flags().GetBool("reset")

	if reset && (console != "" || consoleAPI != "" || dex != "") {
		return fmt.Errorf("--reset clears every pinned hostname, so it cannot be combined with pinning one")
	}

	// This is the command an operator reaches for when a stored hostname has
	// already broken their login, so accepting another unusable one and
	// reporting success is the worst thing it could do.
	for flag, value := range map[string]string{"--console": console, "--console-api": consoleAPI, "--dex": dex} {
		if err := validateHostname(flag, value); err != nil {
			return err
		}
	}

	if !reset && console == "" && consoleAPI == "" && dex == "" {
		printClusterHosts(cfg.GetCluster(clusterName))
		return nil
	}

	if err := config.Update(func(live *config.Config) error {
		entry := live.GetCluster(clusterName)
		if entry == nil {
			return fmt.Errorf("cluster %q not found", clusterName)
		}
		if reset {
			entry.ConsoleDomain = ""
			entry.ConsoleAPIDomain = ""
			entry.DexDomain = ""
			return nil
		}
		// Each flag is independent, so pinning the login host does not silently
		// unpin the other two. An operator fixing one wrong host should not have
		// to restate the ones that already work.
		if console != "" {
			entry.ConsoleDomain = console
		}
		if consoleAPI != "" {
			entry.ConsoleAPIDomain = consoleAPI
		}
		if dex != "" {
			entry.DexDomain = dex
		}
		return nil
	}); err != nil {
		return err
	}

	updated, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	fmt.Fprintf(os.Stderr, "\n  ✔  Updated the hostnames kip uses for %s\n", clusterName)
	printClusterHosts(updated.GetCluster(clusterName))
	return nil
}

// printClusterHosts shows each host and whether it is pinned, because "why is
// kip using that name" is the question this command exists to answer.
func printClusterHosts(cluster *config.Cluster) {
	fmt.Printf("\n  Cluster: %s\n", cluster.Name)
	fmt.Printf("  Domain:  %s\n\n", cluster.Domain)
	for _, row := range []struct {
		label  string
		host   string
		pinned string
	}{
		{"Console", cluster.ConsoleHost(), cluster.ConsoleDomain},
		{"API", cluster.ConsoleAPIHost(), cluster.ConsoleAPIDomain},
		{"Login", cluster.DexHost(), cluster.DexDomain},
	} {
		origin := "follows the domain"
		if row.pinned != "" {
			origin = "pinned"
		}
		fmt.Printf("    %-8s %-40s %s\n", row.label, row.host, origin)
	}
	fmt.Println()
}
