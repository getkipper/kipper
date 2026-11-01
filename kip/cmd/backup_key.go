package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/kip/internal/installer"
)

var backupRepositoryKeyCmd = &cobra.Command{
	Use:   "repository-key",
	Short: "Manage the key that encrypts this cluster's volume backups",
}

var backupRepositoryKeyExportCmd = &cobra.Command{
	Use:   "export --to <file>",
	Short: "Save the backup repository key to a new local file",
	Long: `Saves the key that encrypts this cluster's volume backups to a new file
with mode 0600, or with its folder's permissions on Windows. Without the key,
nobody can read the backups, including a new cluster installed to recover
from them. Keep the file off the cluster, for example in a password manager.

Anyone who has this key and can read the bucket can decrypt its volume data.
Restoring SSE-C backups also requires the SSE-C key.`,
	Args: cobra.NoArgs,
	RunE: runBackupRepositoryKeyExport,
}

var backupRepositoryKeyResetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Delete every backup and start again with a new repository key",
	Long: `Replaces the key that encrypts this cluster's volume backups, for example
when it was exposed or is Velero's published default. The old backups cannot
be read with a new key, so the reset deletes them along with the old key.

kip stops Velero, waits for the node agents to finish their transfers and
deletes the backups, restores, Kopia and Restic repositories, and incomplete
uploads in the bucket, including older object versions where the store lists
them. Then it deletes Velero's records of them, creates a new key on the
server and starts Velero again. Finally, it takes a full backup from each
schedule and waits for all of them to complete. The command asks for the
bucket's name before deleting anything. It supports clusters with exactly
one backup location. It refuses a bucket whose default retention locks every
object, a versioned bucket whose versions kip cannot list, Velero chart
settings kip does not set, such as set overrides or a HelmChartConfig, and a
kip schedule that predates the exclusion of Dex's signing keys and sessions.
Run kip upgrade to update these schedules. If an earlier reset is unfinished,
kip adds the exclusion to its schedules before it starts Velero, finishes the
reset without taking backups, then asks you to run kip upgrade and kip backup
create. The command warns when the store prevents it from listing object
versions or incomplete uploads.

A backup mirror keeps its existing copies, each locked for at least the
mirror's retention period. Their volume data needs the old key, so export it
first if you may need them. After successfully stopping Velero, a failed run
restarts it with the old key only when kip confirms that nothing was
deleted. If Velero was already stopped, it stays stopped because an earlier
run may have deleted part of the bucket. A failure after deletion starts
leaves Velero stopped or running with the new key. Run the command again to
finish once the store allows deletion of any objects it previously refused
to delete. A run that reports the repository key as reset has finished, and
only its new backups may still be running. Follow them with kip backup list.
While a reset is unfinished, kip install refuses to set up backups on the
same bucket, kip upgrade skips Velero, and kip node add leaves the Velero
chart unchanged. Running kip install to move backups to another bucket
starts Velero again with the old key, so run the reset again afterward.

Examples:
  kip backup repository-key reset
  kip backup repository-key reset --yes --ssh-key ~/.ssh/kipper_ed25519`,
	Args: cobra.NoArgs,
	RunE: runBackupRepositoryKeyReset,
}

func init() {
	backupRepositoryKeyExportCmd.Flags().String("to", "", "new file to save the key to (required)")
	backupRepositoryKeyExportCmd.Flags().Bool("yes", false, "export without asking for confirmation")
	_ = backupRepositoryKeyExportCmd.MarkFlagRequired("to")
	backupRepositoryKeyResetCmd.Flags().Bool("yes", false, "reset without asking for the bucket name")
	backupRepositoryKeyResetCmd.Flags().String("ssh-key", "", "path to SSH private key (defaults to ~/.ssh/id_ed25519)")
	backupRepositoryKeyCmd.AddCommand(backupRepositoryKeyExportCmd, backupRepositoryKeyResetCmd)
	backupCmd.AddCommand(backupRepositoryKeyCmd)
}

func runBackupRepositoryKeyExport(cmd *cobra.Command, _ []string) error {
	path, _ := cmd.Flags().GetString("to")
	yes, _ := cmd.Flags().GetBool("yes")

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if !backupsConfiguredOn(ctx, k8sClient.Dynamic()) {
		return errBackupsNotConfigured
	}
	if err := confirmKeyExport(os.Stdin, os.Stdout, term.IsTerminal(int(os.Stdin.Fd())), yes, "backup repository key",
		"Anyone with this key and read access to the bucket can read the backed-up volume data."); err != nil {
		return err
	}
	secret, err := k8sClient.Clientset().CoreV1().Secrets("velero").Get(ctx, "velero-repo-credentials", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading the backup repository key: %w", err)
	}
	// Velero trims the key it reads, so the trimmed key is the one in use.
	key := bytes.TrimSpace(secret.Data["repository-password"])
	if err := installer.CheckRepositoryKey(key); err != nil {
		return fmt.Errorf("this cluster's backup repository key cannot be exported and restored unchanged: %w", err)
	}
	if err := installer.WriteRepositoryKeyFile(path, key); err != nil {
		return err
	}
	fmt.Printf("\n  ✔  Backup repository key saved to %s\n", path)
	if installer.IsDefaultRepositoryKey(key) {
		fmt.Printf("  ⚠  This cluster uses Velero's published default key, so its backups are protected only by access to the bucket.\n")
		fmt.Printf("     kip backup repository-key reset replaces it and deletes the existing backups.\n")
	}
	fmt.Println()
	return nil
}

// confirmKeyExport displays the risk before asking for consent to export a key.
// The yes flag bypasses confirmation and is required for non-interactive input.
// It returns an error if confirmation is required but consent is not given.
func confirmKeyExport(in io.Reader, out io.Writer, interactive, yes bool, what, risk string) error {
	if yes {
		return nil
	}
	if !interactive {
		return fmt.Errorf("exporting the %s requires confirmation. Pass --yes", what)
	}
	_, _ = fmt.Fprintf(out, "\n  %s Export it? [y/N]: ", risk)
	answer, _ := bufio.NewReader(in).ReadString('\n')
	if isYes(answer) {
		return nil
	}
	return errors.New("export canceled")
}

func runBackupRepositoryKeyReset(cmd *cobra.Command, _ []string) error {
	yes, _ := cmd.Flags().GetBool("yes")
	lc, err := leaseClusterServer(cmd, "kip backup repository-key reset")
	if err != nil {
		return err
	}
	defer lc.close()
	return resetRepositoryKeyOn(lc.server, os.Stdin, os.Stdout, term.IsTerminal(int(os.Stdin.Fd())), yes)
}

// resetRepositoryKeyOn describes what a reset deletes and keeps, then resets
// the key after the operator enters the bucket name or passes yes.
// A mismatched answer cancels the reset without an error or changes.
func resetRepositoryKeyOn(server installer.CommandRunner, in io.Reader, out io.Writer, interactive, yes bool) error {
	reset, err := installer.SurveyRepositoryKeyReset(server)
	if err != nil {
		return err
	}
	say(out, "\n  Resetting the backup repository key\n\n")
	say(out, "  This deletes every backup of this cluster in bucket %s at %s: %d backups and %d volume repositories.\n",
		reset.Bucket, reset.Endpoint, reset.Backups, reset.Repositories)
	if reset.OutdatedSchedule == "" {
		say(out, "  kip then creates a new key on the server and takes a full backup from each schedule.\n")
	} else {
		say(out, "  kip then creates a new key on the server. Schedule %s predates the exclusion of Dex's signing keys and sessions,\n", reset.OutdatedSchedule)
		say(out, "  so kip adds the exclusion to its schedules before it starts Velero. kip upgrade can update the rest of the\n")
		say(out, "  Velero chart only after the reset finishes, so this reset skips taking new backups.\n")
	}
	say(out, "  Velero is stopped during cleanup and key replacement, so backups and restores wait.\n")
	for _, m := range reset.Mirrors {
		say(out, "\n  ⚠  Mirror %s keeps the copies it made in %s, each locked for at least %s days.\n", m.Name, m.Target, m.RetentionDays)
		say(out, "     Their volume data needs the current key, so save it first if you may need them:\n")
		say(out, "       kip backup repository-key export --to <file>\n")
		if m.MaxDelete != "off" {
			say(out, "     Future mirror runs copy the deletions up to the --max-delete limit of %s per run. They fail until the remaining deletions fit within that limit.\n", m.MaxDelete)
		}
	}
	if !yes {
		if !interactive {
			return errors.New("resetting the repository key deletes every backup. Pass --yes to reset it without a prompt")
		}
		say(out, "\n  Type the bucket name to confirm (%s): ", reset.Bucket)
		answer, _ := bufio.NewReader(in).ReadString('\n')
		if strings.TrimSpace(answer) != reset.Bucket {
			say(out, "  Nothing was changed.\n\n")
			return nil
		}
	}
	say(out, "\n")
	results, err := reset.Run(func(step string) { say(out, "  ...  %s\n", step) })
	if reset.VersionsUnchecked {
		say(out, "  ⚠  The store prevented kip from listing object versions, so kip could not check for older versions of the deleted backups.\n")
		say(out, "     If bucket %s keeps versions, delete them with the provider's tools. Reading any volume data in them requires the old key.\n", reset.Bucket)
	}
	if reset.UploadsUnchecked {
		say(out, "  ⚠  The store prevented kip from listing incomplete uploads, so kip could not check for incomplete uploads of the deleted backups.\n")
		say(out, "     Abort any left in bucket %s with the provider's tools. Reading any volume data in them requires the old key.\n", reset.Bucket)
	}
	for _, b := range results {
		mark := "✔"
		if b.Phase != "Completed" {
			mark = "⚠"
		}
		say(out, "  %s  Backup %s: %s\n", mark, b.Name, b.Phase)
	}
	if err != nil {
		return err
	}
	say(out, "  ✔  Backups use the new repository key\n")
	say(out, "     Save it outside the cluster: kip backup repository-key export --to <file>\n")
	if reset.OutdatedSchedule != "" {
		say(out, "  ⚠  Run kip upgrade now to update the Velero chart, then run kip backup create.\n")
	}
	say(out, "\n")
	return nil
}
