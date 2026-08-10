package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/ai"
)

var aiUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the AI bundle and all its data from the cluster",
	Long: `Uninstall the Kipper AI bundle (Ollama and LibreChat) and wipe
its data. The model cache, chat history, MongoDB content, and
LibreChat credentials are all destroyed. The kipper-ai namespace is
removed.

To preserve the data, snapshot it with 'kip ai backup --wait' before
you run uninstall. The --wait flag is important here: 'kip ai backup'
on its own returns after a 60-second warmup window, so the snapshot
is usually still uploading when the command exits. Uninstalling
during that upload window deletes the source PVCs before Velero is
done copying them, leaving you with an unrestorable snapshot. After
uninstall, 'kip ai restore --name <name>' recreates the bundle from
the snapshot. Do not run 'kip ai install' between uninstall and
restore — restore brings everything back on its own and refuses if
the namespace already exists.

Example:
  kip ai backup --name pre-uninstall --wait
  kip ai uninstall
  kip ai restore --name pre-uninstall`,
	RunE: runAIUninstall,
}

func init() {
	aiUninstallCmd.Flags().Bool("force", false, "delete the bundle even if a Kipper AI backup is still uploading")
	aiCmd.AddCommand(aiUninstallCmd)
}

func runAIUninstall(cmd *cobra.Command, _ []string) error {
	force, _ := cmd.Flags().GetBool("force")
	cluster, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx := context.Background()
	installer := &ai.Installer{
		Clientset: client.Clientset(),
		Dynamic:   client.Dynamic(),
		Out:       os.Stdout,
	}

	// Refuse to delete the source PVCs while Velero is still copying
	// them. `kip ai backup` on its own exits after a 60s warmup, so
	// without this guard the documented backup → uninstall sequence
	// races and leaves the user with an unrestorable snapshot.
	if !force {
		if names, err := installer.InFlightBackupNames(ctx); err != nil {
			return fmt.Errorf("checking in-flight backups: %w", err)
		} else if len(names) > 0 {
			return fmt.Errorf("AI backup still uploading: %v\n"+
				"  Run 'kip ai backup show --name %s' to check progress.\n"+
				"  Wait for it to reach Completed (or use 'kip ai backup --wait' next time),\n"+
				"  or pass --force to uninstall now (the snapshot will be incomplete and unrestorable)",
				names, names[0])
		}
	}

	fmt.Printf("\n  Removing AI bundle and data from %s\n\n", cluster.Name)
	if err := installer.Uninstall(ctx, ai.Options{}); err != nil {
		return err
	}
	fmt.Print("\n  ✔  AI bundle removed\n\n")
	return nil
}
