package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/kip/internal/installer"
)

var backupSSEKeyCmd = &cobra.Command{
	Use:   "sse-key",
	Short: "Manage the SSE-C key that encrypts this cluster's backup objects",
}

var backupSSEKeyExportCmd = &cobra.Command{
	Use:   "export --to <file>",
	Short: "Save the backup SSE-C key to a new local file",
	Long: `Saves the SSE-C key of a cluster whose backups use SSE-C to a new file
with mode 0600, or with its folder's permissions on Windows. The
file holds the key as base64 text. The key is required to read the backup
objects it protects, including when recovering into a new cluster with
--backup-storage-sse-c-key-file. Keep the file outside the cluster, for example
in a password manager, next to the backup repository key.

When kip replaces an unused SSE-C key, it saves the old key in a Secret named
kipper-backup-sse-key-previous-<UTC timestamp> and prints that name. Pass that
name with --previous to export the old key for backups written with it.`,
	Args: cobra.NoArgs,
	RunE: runBackupSSEKeyExport,
}

func init() {
	backupSSEKeyExportCmd.Flags().String("to", "", "new file to save the key to (required)")
	backupSSEKeyExportCmd.Flags().Bool("yes", false, "export without asking for confirmation")
	backupSSEKeyExportCmd.Flags().String("previous", "", "export the old key from the named kipper-backup-sse-key-previous-<timestamp> Secret")
	_ = backupSSEKeyExportCmd.MarkFlagRequired("to")
	backupSSEKeyCmd.AddCommand(backupSSEKeyExportCmd)
	backupCmd.AddCommand(backupSSEKeyCmd)
}

func runBackupSSEKeyExport(cmd *cobra.Command, _ []string) error {
	path, _ := cmd.Flags().GetString("to")
	yes, _ := cmd.Flags().GetBool("yes")
	previous, _ := cmd.Flags().GetString("previous")

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if !backupsConfiguredOn(ctx, k8sClient.Dynamic()) {
		return errBackupsNotConfigured
	}
	key, err := sseKeyToExport(ctx, k8sClient.Clientset(), previous)
	if err != nil {
		return err
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

const previousSSEKeyPrefix = "kipper-backup-sse-key-previous-"

// sseKeyToExport returns the cluster's current SSE-C key or, when previous is
// set, an old key saved when kip replaced an unused one. The previous name must
// start with previousSSEKeyPrefix and have a nonempty suffix. Keys must be 32
// bytes long.
func sseKeyToExport(ctx context.Context, cs kubernetes.Interface, previous string) ([]byte, error) {
	name := "kipper-backup-sse-key"
	if previous != "" {
		if !strings.HasPrefix(previous, previousSSEKeyPrefix) || len(previous) == len(previousSSEKeyPrefix) {
			return nil, fmt.Errorf("--previous takes the name of a Secret holding an old SSE-C key saved by kip. The name must start with %s and include a suffix", previousSSEKeyPrefix)
		}
		name = previous
	}
	secret, err := cs.CoreV1().Secrets("velero").Get(ctx, name, metav1.GetOptions{})
	switch {
	case kerrors.IsNotFound(err) && previous != "":
		return nil, fmt.Errorf("no Secret velero/%s was found. kip prints the Secret's name when it saves an old SSE-C key", previous)
	case kerrors.IsNotFound(err):
		return nil, errors.New("this cluster's backups do not use SSE-C, so there is no SSE-C key to export")
	case err != nil:
		return nil, fmt.Errorf("reading the SSE-C key in Secret velero/%s: %w", name, err)
	}
	key := secret.Data["customer-key"]
	if len(key) != 32 {
		return nil, fmt.Errorf("the SSE-C key in Secret velero/%s is not 32 bytes long, so it cannot be exported", name)
	}
	return key, nil
}
