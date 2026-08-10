package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/ai"
)

var aiBackupRepairCmd = &cobra.Command{
	Use:   "repair",
	Short: "Reconcile orphan AI backup state between Velero CRs and MinIO",
	Long: `Detects and repairs three orphan states that can leave the
AI backup story in a broken half-state:

  - Backup CRs that point at Kopia repo data wiped manually from MinIO
  - MinIO directories left behind after 'kubectl delete backup'
    (which bypasses Velero's deletion pipeline so Kopia data is never
    freed)
  - BackupRepository CRs whose phase is not Ready (typically because
    MinIO was wiped from underneath Velero)

The command prints a plan, asks for explicit confirmation, then
executes cleanup. Safe to run repeatedly; a clean cluster reports
'nothing to do'.

Examples:
  kip ai backup repair
  kip ai backup repair --yes`,
	RunE: runAIBackupRepair,
}

func init() {
	aiBackupRepairCmd.Flags().BoolP("yes", "y", false, "skip the confirmation prompt (for non-interactive use)")
	aiBackupCmd.AddCommand(aiBackupRepairCmd)
}

func runAIBackupRepair(cmd *cobra.Command, _ []string) error {
	_, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	yes, _ := cmd.Flags().GetBool("yes")

	ctx := context.Background()
	installer := &ai.Installer{
		Clientset: client.Clientset(),
		Dynamic:   client.Dynamic(),
		Out:       os.Stdout,
	}

	fmt.Println()
	fmt.Println("  Inspecting Velero CRs and MinIO bucket...")
	plan, detectErr := installer.DetectOrphans(ctx)
	if detectErr != nil && plan == nil {
		return fmt.Errorf("detecting orphans: %w", detectErr)
	}
	if detectErr != nil {
		fmt.Printf("  ⚠   %s\n", detectErr.Error())
		fmt.Println("      Cluster-side findings are still actionable below; MinIO orphans cannot be detected without that listing.")
	}

	if plan.IsClean() {
		fmt.Println("  ✔   No orphan state detected; nothing to do.")
		return nil
	}

	printRepairPlan(plan)

	if !yes {
		if !confirmRepair(cmd.InOrStdin()) {
			fmt.Println("  Aborted; nothing changed.")
			return nil
		}
	}

	fmt.Println()
	fmt.Println("  Applying repair...")
	if err := installer.ApplyRepair(ctx, plan, os.Stdout); err != nil {
		return fmt.Errorf("applying repair: %w", err)
	}
	fmt.Println()
	fmt.Println("  ✔   Repair complete.")
	return nil
}

func printRepairPlan(plan *ai.RepairPlan) {
	fmt.Println()
	fmt.Println("  The following actions will be performed:")
	if len(plan.BrokenRepos) > 0 {
		fmt.Printf("  - Delete %d broken BackupRepository CR(s) so Velero re-initialises Kopia on the next backup:\n", len(plan.BrokenRepos))
		for _, name := range plan.BrokenRepos {
			fmt.Printf("      * %s\n", name)
		}
	}
	if len(plan.OrphanInCluster) > 0 {
		fmt.Printf("  - Delete %d Backup CR(s) whose Kopia data is gone from MinIO:\n", len(plan.OrphanInCluster))
		for _, name := range plan.OrphanInCluster {
			fmt.Printf("      * %s\n", name)
		}
	}
	if len(plan.OrphanInStorage) > 0 {
		fmt.Printf("  - Remove %d MinIO directory tree(s) under velero/backups/ that no Backup CR claims:\n", len(plan.OrphanInStorage))
		for _, name := range plan.OrphanInStorage {
			fmt.Printf("      * %s\n", name)
		}
	}
	if !plan.MinIOReachable {
		fmt.Println("  ⚠   MinIO was not reachable; orphan-in-storage detection skipped.")
	}
}

func confirmRepair(in interface{ Read([]byte) (int, error) }) bool {
	fmt.Print("\n  Proceed? [y/N]: ")
	reader := bufio.NewReader(in)
	ans, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	ans = strings.ToLower(strings.TrimSpace(ans))
	return ans == "y" || ans == "yes"
}
