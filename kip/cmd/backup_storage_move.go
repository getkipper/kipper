package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"

	"github.com/getkipper/kipper/kip/internal/installer"
)

var backupStorageSetCmd = &cobra.Command{
	Use:   "set --bucket <bucket> --region <region> --export-new-key-to <file> --export-previous-key-to <file>",
	Short: "Send future backups to another external bucket",
	Long: `Send the cluster's future backups to another external bucket under a new
repository key. The new bucket must hold no backups. Existing backups stay
intact in the old bucket.

Before Velero switches, kip saves the old bucket's repository key and the new
one to the files you name, so you can still read the old backups with a
read-only recovery install (kip install --backup-storage-read-only). Velero
stops for a few minutes during the switch.

If the command stops partway, run it again with the same flags to continue.
Until the switch begins, --abort cancels the move. After the move, wait for a
backup created in the new bucket to complete. Then run kip backup storage
retire-previous to make kip forget the old bucket.`,
	Example: `  kip backup storage set --bucket new-backups --region hel1 \
    --endpoint https://objects.example.com --sse-c \
    --export-new-key-to new-repository.key \
    --export-previous-key-to previous-repository.key \
    --export-sse-c-key-to new-sse-c.key
  kip backup storage set --abort`,
	Args: cobra.NoArgs,
	RunE: runBackupStorageSet,
}

var backupStorageRetireCmd = &cobra.Command{
	Use:   "retire-previous",
	Short: "Forget the previous backup location",
	Long: `Forget the previous backup location after kip backup storage set. kip
requires a completed backup created in the new bucket after the move, then
deletes the cluster's copy of the previous repository key. Keep the key file
saved during the move to read the old backups.

The old bucket and its contents stay intact. Delete the bucket and revoke its
credentials at the provider when you no longer need its backups.`,
	Example: "  kip backup storage retire-previous",
	Args:    cobra.NoArgs,
	RunE:    runBackupStorageRetire,
}

func init() {
	addMoveFlags(backupStorageSetCmd.Flags())
	backupStorageSetCmd.MarkFlagsMutuallyExclusive("abort", "bucket")
	backupStorageCmd.AddCommand(backupStorageSetCmd)

	backupStorageRetireCmd.Flags().String("ssh-key", "", "path to the SSH private key for the server")
	backupStorageRetireCmd.Flags().Bool("yes", false, "retire without asking for confirmation")
	backupStorageCmd.AddCommand(backupStorageRetireCmd)
}

func addMoveFlags(f *pflag.FlagSet) {
	f.String("bucket", "", "the new bucket. It must hold no backups")
	f.String("region", "", "the new bucket's region")
	f.String("endpoint", "", "S3-compatible endpoint URL of the new bucket. Leave empty for AWS S3")
	f.String("credentials", "", "AWS-style credentials file for the new bucket (default ~/.aws/credentials)")
	f.String("profile", "", "profile in the credentials file (default \"default\")")
	f.Bool("sse-c", false, "encrypt the new bucket's objects with SSE-C, keeping the cluster's SSE-C key or creating one. Required when the cluster already uses SSE-C")
	f.String("export-new-key-to", "", "file to save the new repository key to")
	f.String("export-previous-key-to", "", "file to save the old bucket's repository key to")
	f.String("export-sse-c-key-to", "", "file to save the SSE-C key to. Required with --sse-c")
	f.String("ssh-key", "", "path to the SSH private key for the server")
	f.Bool("yes", false, "move without asking for confirmation")
	f.Bool("abort", false, "cancel a move before the switch begins")
}

func runBackupStorageSet(cmd *cobra.Command, _ []string) error {
	abort, _ := cmd.Flags().GetBool("abort")
	var opts installer.MoveOptions
	if !abort {
		var err error
		if opts, err = moveOptionsFromFlags(cmd); err != nil {
			return err
		}
	}
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}
	sshKey, _ := cmd.Flags().GetString("ssh-key")
	lc, err := leaseCluster(cluster, sshKey, "kip backup storage set")
	if err != nil {
		return err
	}
	defer lc.close()
	if abort {
		return installer.AbortBackupStorageMove(lc.server, os.Stdout)
	}
	opts.ClusterName = cluster.Name
	return installer.MoveBackupStorage(lc.server, opts)
}

// moveOptionsFromFlags validates the set command's flags and loads the new
// bucket's credentials using only local files and terminal state.
func moveOptionsFromFlags(cmd *cobra.Command) (installer.MoveOptions, error) {
	str := func(name string) string { v, _ := cmd.Flags().GetString(name); return v }
	sseC, _ := cmd.Flags().GetBool("sse-c")
	yes, _ := cmd.Flags().GetBool("yes")
	for _, name := range []string{"bucket", "region", "export-new-key-to", "export-previous-key-to"} {
		if str(name) == "" {
			return installer.MoveOptions{}, fmt.Errorf("--%s is required", name)
		}
	}
	creds, err := loadBackupCredentials(str("credentials"), str("profile"))
	if err != nil {
		return installer.MoveOptions{}, err
	}
	interactive := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) //nolint:gosec // file descriptors fit an int
	return installer.MoveOptions{
		Target: &installer.BackupStorageConfig{
			Bucket: str("bucket"), Region: str("region"), Endpoint: str("endpoint"),
			AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey, SSEC: sseC,
		},
		NewKeyFile:      str("export-new-key-to"),
		PreviousKeyFile: str("export-previous-key-to"),
		SSECKeyFile:     str("export-sse-c-key-to"),
		Confirm: func(summary []string) error {
			return confirmSummary(os.Stdin, os.Stdout, interactive, yes, summary, "Change the backup location?", "changing the backup location")
		},
		Out: os.Stdout,
		Now: time.Now,
	}, nil
}

func runBackupStorageRetire(cmd *cobra.Command, _ []string) error {
	yes, _ := cmd.Flags().GetBool("yes")
	sshKey, _ := cmd.Flags().GetString("ssh-key")
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}
	lc, err := leaseCluster(cluster, sshKey, "kip backup storage retire-previous")
	if err != nil {
		return err
	}
	defer lc.close()
	interactive := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) //nolint:gosec // file descriptors fit an int
	return installer.RetirePreviousBackupStorage(lc.server, func(summary []string) error {
		return confirmSummary(os.Stdin, os.Stdout, interactive, yes, summary, "Forget the previous location?", "retiring the previous location")
	}, os.Stdout)
}

// confirmSummary always prints summary, then asks question unless yes is set.
// Non-interactive runs require yes. Errors use action to name the operation
// that needs confirmation.
func confirmSummary(in io.Reader, out io.Writer, interactive, yes bool, summary []string, question, action string) error {
	_, _ = fmt.Fprintln(out)
	for _, line := range summary {
		_, _ = fmt.Fprintf(out, "  %s\n", line)
	}
	if yes {
		return nil
	}
	if !interactive {
		return fmt.Errorf("%s requires confirmation. Pass --yes", action)
	}
	_, _ = fmt.Fprintf(out, "\n  %s [y/N]: ", question)
	answer, _ := bufio.NewReader(in).ReadString('\n')
	if isYes(answer) {
		return nil
	}
	return errors.New("canceled")
}
