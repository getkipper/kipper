package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

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

Anyone who has the key and can read the bucket can read every backup.`,
	Args: cobra.NoArgs,
	RunE: runBackupRepositoryKeyExport,
}

func init() {
	backupRepositoryKeyExportCmd.Flags().String("to", "", "new file to save the key to (required)")
	backupRepositoryKeyExportCmd.Flags().Bool("yes", false, "export without asking for confirmation")
	_ = backupRepositoryKeyExportCmd.MarkFlagRequired("to")
	backupRepositoryKeyCmd.AddCommand(backupRepositoryKeyExportCmd)
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
	if err := confirmKeyExport(os.Stdin, term.IsTerminal(int(os.Stdin.Fd())), yes); err != nil {
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
	}
	fmt.Println()
	return nil
}

func confirmKeyExport(in io.Reader, interactive, yes bool) error {
	if yes {
		return nil
	}
	if !interactive {
		return errors.New("exporting the backup repository key requires confirmation. Pass --yes")
	}
	fmt.Print("\n  Anyone with this key and read access to the bucket can read every backup. Export it? [y/N]: ")
	answer, _ := bufio.NewReader(in).ReadString('\n')
	if isYes(answer) {
		return nil
	}
	return errors.New("export canceled")
}
