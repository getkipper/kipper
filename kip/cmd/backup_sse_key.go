package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/kip/internal/installer"
)

var backupSSEKeyCmd = &cobra.Command{
	Use:   "sse-key",
	Short: "Manage the SSE-C key that encrypts this cluster's backup objects",
}

var backupSSEKeyExportCmd = &cobra.Command{
	Use:   "export --to <file>",
	Short: "Save the backup SSE-C key to a new local file",
	Long: `Saves the SSE-C key of a cluster installed with --backup-storage-sse-c to a
new file with mode 0600, or with its folder's permissions on Windows. The
file holds the key as base64 text. The key is required to read the backup
objects it protects, including when recovering into a new cluster with
--backup-storage-sse-c-key-file. Keep the file outside the cluster, for example
in a password manager, next to the backup repository key.`,
	Args: cobra.NoArgs,
	RunE: runBackupSSEKeyExport,
}

func init() {
	backupSSEKeyExportCmd.Flags().String("to", "", "new file to save the key to (required)")
	backupSSEKeyExportCmd.Flags().Bool("yes", false, "export without asking for confirmation")
	_ = backupSSEKeyExportCmd.MarkFlagRequired("to")
	backupSSEKeyCmd.AddCommand(backupSSEKeyExportCmd)
	backupCmd.AddCommand(backupSSEKeyCmd)
}

func runBackupSSEKeyExport(cmd *cobra.Command, _ []string) error {
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
	secret, err := k8sClient.Clientset().CoreV1().Secrets("velero").Get(ctx, "kipper-backup-sse-key", metav1.GetOptions{})
	if kerrors.IsNotFound(err) {
		return errors.New("this cluster's backups do not use SSE-C, so there is no SSE-C key to export")
	}
	if err != nil {
		return fmt.Errorf("reading the backup SSE-C key: %w", err)
	}
	key := secret.Data["customer-key"]
	if len(key) != 32 {
		return errors.New("this cluster's SSE-C key is not 32 bytes long, so it cannot be exported")
	}
	if err := confirmKeyExport(os.Stdin, os.Stdout, term.IsTerminal(int(os.Stdin.Fd())), yes, "backup SSE-C key",
		"Anyone with this key and read access to the bucket can read the backed-up resource objects. A full restore also needs the backup repository key."); err != nil {
		return err
	}
	if err := installer.WriteRepositoryKeyFile(path, []byte(installer.EncodeSSECKey(key)+"\n")); err != nil {
		return err
	}
	fmt.Printf("\n  ✔  Backup SSE-C key saved to %s\n\n", path)
	return nil
}
