package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/ai"
)

var aiRagUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the RAG bundle (Qdrant + AnythingLLM) and all its data",
	Long: `Uninstall the Phase 2 RAG bundle. This is destructive: Qdrant
collections, AnythingLLM workspaces, and uploaded documents are all
deleted. Phase 1 (Ollama + LibreChat) is left running.

To preserve the data, snapshot it with 'kip ai backup --wait' before
uninstall. The same backup also covers the Phase 1 chat data, so a
single snapshot is enough.

The embedding model in Ollama's PVC is intentionally left in place. A
later 'kip ai rag install' reuses it without re-downloading.

Example:
  kip ai backup --name pre-rag-uninstall --wait
  kip ai rag uninstall`,
	RunE: runAIRagUninstall,
}

func init() {
	aiRagUninstallCmd.Flags().Bool("force", false, "delete the bundle even if a Kipper AI backup is still uploading")
	aiRagCmd.AddCommand(aiRagUninstallCmd)
}

func runAIRagUninstall(cmd *cobra.Command, _ []string) error {
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

	// Refuse to delete chart-rendered PVCs while Velero is still
	// copying them. Same in-flight backup guard as 'kip ai uninstall',
	// since both bundles share the kipper-ai namespace and any active
	// backup snapshots data both bundles depend on.
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

	fmt.Printf("\n  Removing AI RAG bundle from %s\n\n", cluster.Name)
	if err := installer.UninstallRAG(ctx, ai.RAGOptions{}); err != nil {
		return err
	}
	fmt.Print("\n  ✔  AI RAG bundle removed\n\n")
	return nil
}
