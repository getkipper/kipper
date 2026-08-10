package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/ai"
)

var aiRestoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Restore the AI bundle from a Velero backup",
	Long: `Restore a previous 'kip ai backup' snapshot into the
kipper-ai namespace. Refuses to run if the namespace already exists,
since restoring on top of a live install corrupts state. Run
'kip ai uninstall' first to clear the slate.

Examples:
  kip ai restore --name pre-upgrade
  kip ai restore --name kipper-ai-20260509-143015`,
	RunE: runAIRestore,
}

func init() {
	aiRestoreCmd.Flags().String("name", "", "name of the backup to restore (required)")
	aiCmd.AddCommand(aiRestoreCmd)
}

func runAIRestore(cmd *cobra.Command, _ []string) error {
	name, _ := cmd.Flags().GetString("name")
	if name == "" {
		return fmt.Errorf("--name is required")
	}
	cluster, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	fmt.Printf("\n  Restoring AI bundle on %s from backup %q\n\n", cluster.Name, name)

	installer := &ai.Installer{
		Clientset: client.Clientset(),
		Dynamic:   client.Dynamic(),
		Out:       os.Stdout,
	}
	summary, err := installer.CreateRestore(context.Background(), name)
	if err != nil {
		return err
	}
	fmt.Printf("\n  ✔  Restore completed (%d items, %d errors, %d warnings)\n", summary.ItemsBackedUp, summary.Errors, summary.Warnings)
	fmt.Printf("  Run 'kip ai status' to verify the bundle is healthy.\n\n")
	return nil
}
