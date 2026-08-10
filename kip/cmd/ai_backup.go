package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/ai"
)

// backupWarmupWindow is how long `kip ai backup` waits for an
// immediate validation failure (bad name, BackupStorageLocation down,
// RBAC) before exiting and pointing the user at `kip ai backup show`.
// 60s comfortably covers Velero's controller pickup latency on a
// fresh cluster while keeping the operator's terminal free for
// long-running snapshots.
const backupWarmupWindow = 60 * time.Second

var aiBackupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Snapshot the AI bundle's data via Velero",
	Long: `Take a Velero-backed snapshot of the kipper-ai namespace
(model cache, MongoDB content, chat history, credentials). Restore
the snapshot later with 'kip ai restore --name <name>'.

Without --name a timestamped name is generated. The command starts
the backup, watches for up to 60 seconds to surface any immediate
validation failure, then exits. Use 'kip ai backup show --name <name>'
to check progress and 'kip ai backup list' to see all snapshots. Pass
--wait to block until the backup completes (useful from scripts).

Examples:
  kip ai backup
  kip ai backup --name pre-upgrade
  kip ai backup --name pre-upgrade --wait
  kip ai backup show --name pre-upgrade
  kip ai backup list
  kip ai backup delete --name pre-upgrade`,
	RunE: runAIBackupCreate,
}

var aiBackupListCmd = &cobra.Command{
	Use:   "list",
	Short: "List Kipper-managed AI backups",
	RunE:  runAIBackupList,
}

var aiBackupShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show detailed status of a Kipper-managed AI backup",
	RunE:  runAIBackupShow,
}

var aiBackupDeleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete a Kipper-managed AI backup",
	Long: `Free a snapshot's data in object storage and remove the
Velero Backup CRs. The command starts the deletion, watches for up
to 60 seconds for an immediate validation failure (broken object
storage, BackupStorageLocation issue), then exits. Velero finishes
freeing the Kopia repo data in the background — multi-gigabyte
snapshots can take several minutes after the command returns. Use
'kip ai backup list' to confirm the Backup CRs disappear, or pass
--wait to block until they do.`,
	RunE: runAIBackupDelete,
}

func init() {
	aiBackupCmd.Flags().String("name", "", "backup name (defaults to kipper-ai-<timestamp>)")
	aiBackupCmd.Flags().Bool("wait", false, "block until the backup finishes (default: exit after a 60s warmup)")
	aiBackupShowCmd.Flags().String("name", "", "name of the backup to show (required)")
	aiBackupDeleteCmd.Flags().String("name", "", "name of the backup to delete (required)")
	aiBackupDeleteCmd.Flags().Bool("wait", false, "block until both Backup CRs disappear (default: exit after a 60s warmup)")

	aiBackupCmd.AddCommand(aiBackupListCmd)
	aiBackupCmd.AddCommand(aiBackupShowCmd)
	aiBackupCmd.AddCommand(aiBackupDeleteCmd)
	aiCmd.AddCommand(aiBackupCmd)
}

func runAIBackupCreate(cmd *cobra.Command, _ []string) error {
	cluster, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	name, _ := cmd.Flags().GetString("name")
	wait, _ := cmd.Flags().GetBool("wait")

	fmt.Printf("\n  Backing up AI bundle on %s\n\n", cluster.Name)

	installer := &ai.Installer{
		Clientset: client.Clientset(),
		Dynamic:   client.Dynamic(),
		Out:       os.Stdout,
	}
	ctx := context.Background()

	started, err := installer.StartBackup(ctx, name)
	if err != nil {
		return err
	}

	if wait {
		summary, err := installer.WaitBackup(ctx, started)
		if err != nil {
			return err
		}
		fmt.Printf("\n  ✔  Backup %q completed (%d items, %d errors, %d warnings)\n\n",
			summary.Name, summary.ItemsBackedUp, summary.Errors, summary.Warnings)
		return nil
	}

	fmt.Printf("  ...  Snapshot started, watching for %s for immediate failures\n", backupWarmupWindow)
	state, summary, err := installer.WatchBackupForWindow(ctx, started, backupWarmupWindow)
	if err != nil {
		return err
	}
	switch state {
	case ai.BackupCompleted:
		fmt.Printf("\n  ✔  Backup %q completed (%d items, %d errors, %d warnings)\n\n",
			started.Name, summary.ItemsBackedUp, summary.Errors, summary.Warnings)
	case ai.BackupFailed:
		return fmt.Errorf("backup %q ended in phase %s with %d errors; run 'kip ai backup show --name %s' for details",
			started.Name, summary.Phase, summary.Errors, started.Name)
	default:
		fmt.Printf("\n  ✔  Backup %q running in the background\n", started.Name)
		fmt.Printf("      Check progress: kip ai backup show --name %s\n\n", started.Name)
	}
	return nil
}

func runAIBackupShow(cmd *cobra.Command, _ []string) error {
	name, _ := cmd.Flags().GetString("name")
	if name == "" {
		return fmt.Errorf("--name is required")
	}
	_, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	installer := &ai.Installer{
		Clientset: client.Clientset(),
		Dynamic:   client.Dynamic(),
	}
	status, err := installer.GetBackupStatus(context.Background(), name)
	if err != nil {
		return err
	}

	fmt.Println()
	fmt.Printf("  Backup: %s\n", name)
	fmt.Println()
	if status.Primary != nil {
		printBackupComponent(os.Stdout, "Primary (kipper-ai)", status.Primary)
	} else {
		fmt.Println("  Primary (kipper-ai): missing (the snapshot's namespace data was not preserved)")
	}
	switch {
	case status.Sibling != nil:
		printBackupComponent(os.Stdout, "Sibling (kipper-system + kube-system)", status.Sibling)
	case status.ExpectsSibling:
		// Primary recorded that a sibling was created; its absence
		// now means the sibling was lost (failed delete, manual
		// kubectl delete, etc.). The snapshot is no longer fully
		// restorable.
		fmt.Println("  Sibling: missing (expected — config Secret + HelmCharts will not be restored)")
	default:
		fmt.Println("  Sibling: none (no cross-namespace AI artefacts at backup time)")
	}
	fmt.Println()

	// Print a clear logical verdict so an operator does not misread
	// "Phase: Completed" on the primary as "safe to uninstall over".
	// Returning a non-zero error on unhealthy keeps `kip ai backup
	// show && kip ai uninstall` from chaining successfully against a
	// snapshot the restore path would later refuse.
	if reason := status.Restorable(); reason != "" {
		return fmt.Errorf("snapshot %q is not restorable: %s", name, reason)
	}
	fmt.Println("  Restorable: yes (Completed, no errors, all expected components present)")
	fmt.Println()
	return nil
}

func printBackupComponent(w *os.File, header string, s *ai.BackupSummary) {
	_, _ = fmt.Fprintf(w, "  %s\n", header)
	_, _ = fmt.Fprintf(w, "    Phase:    %s\n", s.Phase)
	if !s.StartedAt.IsZero() {
		_, _ = fmt.Fprintf(w, "    Started:  %s\n", s.StartedAt.Format(time.RFC3339))
	}
	if !s.FinishedAt.IsZero() {
		_, _ = fmt.Fprintf(w, "    Finished: %s\n", s.FinishedAt.Format(time.RFC3339))
	}
	_, _ = fmt.Fprintf(w, "    Items:    %d/%d backed up\n", s.ItemsBackedUp, s.TotalItems)
	_, _ = fmt.Fprintf(w, "    Errors:   %d\n", s.Errors)
	_, _ = fmt.Fprintf(w, "    Warnings: %d\n", s.Warnings)
}

func runAIBackupList(_ *cobra.Command, _ []string) error {
	_, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	installer := &ai.Installer{
		Clientset: client.Clientset(),
		Dynamic:   client.Dynamic(),
	}
	summaries, err := installer.ListBackups(context.Background())
	if err != nil {
		return err
	}
	if len(summaries) == 0 {
		fmt.Println("\n  No AI backups found. Create one with 'kip ai backup'.")
		fmt.Println()
		return nil
	}
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tPHASE\tAGE\tITEMS\tERRORS")
	for _, s := range summaries {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\n",
			s.Name, s.Phase, humanAge(s.CreatedAt), s.ItemsBackedUp, s.Errors)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("rendering table: %w", err)
	}
	fmt.Println()
	return nil
}

func runAIBackupDelete(cmd *cobra.Command, _ []string) error {
	name, _ := cmd.Flags().GetString("name")
	if name == "" {
		return fmt.Errorf("--name is required")
	}
	wait, _ := cmd.Flags().GetBool("wait")

	_, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	installer := &ai.Installer{
		Clientset: client.Clientset(),
		Dynamic:   client.Dynamic(),
	}
	ctx := context.Background()

	started, err := installer.StartDelete(ctx, name)
	if err != nil {
		return err
	}

	if wait {
		if err := installer.WaitDelete(ctx, started); err != nil {
			return err
		}
		fmt.Printf("\n  ✔  Backup %q removed (Backup CRs deleted)\n", name)
		fmt.Printf("      Note: Velero reclaims the Kopia repo data via scheduled maintenance jobs;\n")
		fmt.Printf("      run 'kubectl get pods -n %s -l job-name' to track those.\n\n", ai.VeleroNamespace)
		return nil
	}

	fmt.Printf("\n  ...  Deletion started, watching for %s for immediate failures\n", backupWarmupWindow)
	state, failedRequests, err := installer.WatchDeleteForWindow(ctx, started, backupWarmupWindow)
	if err != nil {
		return err
	}
	switch state {
	case ai.DeleteCompleted:
		fmt.Printf("\n  ✔  Backup %q removed (Backup CRs deleted)\n", name)
		fmt.Printf("      Note: Velero reclaims the Kopia repo data via scheduled maintenance jobs;\n")
		fmt.Printf("      run 'kubectl get pods -n %s -l job-name' to track those.\n\n", ai.VeleroNamespace)
	case ai.DeleteFailed:
		return fmt.Errorf("delete request(s) for %q failed or were not picked up by Velero. "+
			"Run 'kubectl describe deletebackuprequest -n %s %s' for details, "+
			"and 'kubectl -n %s get pods,deployment/velero' to confirm the controller is healthy",
			name, ai.VeleroNamespace, strings.Join(failedRequests, " "), ai.VeleroNamespace)
	default:
		fmt.Printf("\n  ✔  Backup %q deletion running in the background\n", name)
		fmt.Printf("      Confirm completion: kip ai backup list\n\n")
	}
	return nil
}

// humanAge renders a duration as e.g. "2d", "3h", "5m", "12s". Output
// is approximate, sized for the backup-list table.
func humanAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
}
