package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "kip",
	Short: "Kipper CLI: production Kubernetes for startups",
	Long: `Kipper gives small teams a production-ready Kubernetes cluster on any
Linux root server, with a web console, automatic SSL, and one-command
app deployments, without requiring Kubernetes expertise.`,
}

func init() {
	rootCmd.PersistentFlags().String("cluster", "", "cluster name (overrides current_cluster; also settable via KIP_CLUSTER env var)")
}

// SetVersion sets the CLI version string displayed by --version.
func SetVersion(v string) {
	rootCmd.Version = v
}

// Execute runs the root command and exits with its status.
func Execute() {
	os.Exit(execute(os.Stderr))
}

func execute(errOut io.Writer) int {
	// This function prints errors, so suppress Cobra's duplicate output.
	rootCmd.SilenceErrors = true
	if err := rootCmd.Execute(); err != nil {
		_, _ = fmt.Fprintf(errOut, "Error: %v\n", err)
		return 1
	}
	return 0
}
