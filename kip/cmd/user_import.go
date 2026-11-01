package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/getkipper/kipper/controller/pkg/migrate"
)

var userImportCmd = &cobra.Command{
	Use:   "import [snapshot.yaml | --from-backup <backup>]",
	Short: "Import console accounts and connectors from a backup or a captured snapshot",
	Long: `Brings console accounts back into this cluster.

With --from-backup, the accounts, sign-in connectors and roles come from a
backup of another cluster, for example after recovering onto a new one. Every
regular backup covers them. The backup is restored into a temporary namespace,
which is removed afterward. Connectors take their client secrets from the
backup too.

With a snapshot file, the staticPasswords and connectors blocks of a captured
dex-config are merged. The file can be either a full ConfigMap YAML manifest
or the raw Dex config YAML body directly. Connectors whose client secret is
stored outside the config are skipped.

Existing accounts, connectors and roles on this cluster always win, so the
install admin can never be overwritten with stale data.

Pass --restart-dex to restart the Dex Deployment afterward so the new
config takes effect immediately.

Example:
  kip user import --from-backup daily-20261101030000 --restart-dex
  kip user import /tmp/dex-config-snapshot.yaml
  kip user import /tmp/dex-config-snapshot.yaml --restart-dex`,
	Args: cobra.MaximumNArgs(1),
	RunE: runUserImport,
}

func init() {
	userImportCmd.Flags().String("from-backup", "", "import from this backup instead of a snapshot file")
	userImportCmd.Flags().Bool("restart-dex", false, "roll the Dex Deployment after importing so the new config takes effect immediately")
	userCmd.AddCommand(userImportCmd)
}

func runUserImport(cmd *cobra.Command, args []string) error {
	restart, _ := cmd.Flags().GetBool("restart-dex")
	fromBackup, _ := cmd.Flags().GetString("from-backup")
	if (fromBackup == "") == (len(args) == 0) {
		return errors.New("pass either a snapshot file or --from-backup <backup>")
	}

	var configYAML []byte
	if fromBackup == "" {
		raw, err := os.ReadFile(args[0])
		if err != nil {
			return fmt.Errorf("reading snapshot %s: %w", args[0], err)
		}
		if configYAML, err = migrate.ExtractDexConfigYAML(raw); err != nil {
			return fmt.Errorf("parsing snapshot: %w", err)
		}
	}

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	// Cancel on Ctrl-C or SIGTERM so the temporary namespace can still be removed.
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var report migrate.DexUserImportReport
	if fromBackup != "" {
		if !backupsConfiguredOn(ctx, k8sClient.Dynamic()) {
			return errBackupsNotConfigured
		}
		result, err := importAccountsFromBackup(ctx, k8sClient.Clientset(), k8sClient.Dynamic(), fromBackup)
		if len(result.roles) > 0 {
			fmt.Printf("\n  Roles added (%d):\n", len(result.roles))
			for _, email := range result.roles {
				fmt.Printf("    - %s\n", email)
			}
		}
		if err != nil {
			return reportInterrupt(fmt.Errorf("importing accounts from backup %s: %w", fromBackup, err))
		}
		report = result.dex
	} else if report, err = migrate.ImportDexUsers(ctx, k8sClient.Clientset(), configYAML); err != nil {
		return fmt.Errorf("merging snapshot into dex-config: %w", err)
	}

	if report.HasChanges() {
		fmt.Println()
		if len(report.StaticPasswordsAdded) > 0 {
			fmt.Printf("  Static passwords added (%d):\n", len(report.StaticPasswordsAdded))
			for _, email := range report.StaticPasswordsAdded {
				fmt.Printf("    - %s\n", email)
			}
		}
		if len(report.ConnectorsAdded) > 0 {
			fmt.Printf("  Connectors added (%d):\n", len(report.ConnectorsAdded))
			for _, id := range report.ConnectorsAdded {
				fmt.Printf("    - %s\n", id)
			}
		}
		if report.AlreadyPresent > 0 {
			fmt.Printf("  Entries already present (kept as-is): %d\n", report.AlreadyPresent)
		}
	} else {
		fmt.Printf("\n  ✔  No new users or connectors to import. (%d entries were already present.)\n", report.AlreadyPresent)
	}
	if report.ConnectorCallback != "" {
		fmt.Printf("  ⚠  Imported connectors that used the source Dex callback now use %s. Set this callback URL in those providers' app settings.\n", report.ConnectorCallback)
	}
	if len(report.ConnectorsSkipped) > 0 {
		fmt.Printf("  ⚠  Skipped %d connectors because the import is missing their client secrets. Enable them again in the console:\n", len(report.ConnectorsSkipped))
		for _, id := range report.ConnectorsSkipped {
			fmt.Printf("    - %s\n", id)
		}
	}

	// Honor --restart-dex even when no entries were added so operators can retry
	// a restart after a previous import saved the config, or force a rollout.
	if restart {
		if err := rolloutRestartDex(ctx, k8sClient.Clientset()); err != nil {
			return fmt.Errorf("rolling Dex: %w", err)
		}
		fmt.Println("\n  ✔  Dex Deployment rolled. New users can log in once the new pod is Ready.")
		return nil
	}
	if report.HasChanges() {
		fmt.Println("\n  Dex uses the new config once it restarts. Run the import again with --restart-dex to restart it now.")
	}
	return nil
}

// rolloutRestartDex annotates the Dex pod template to trigger a rolling
// restart, using the same pattern as kubectl rollout restart. Write conflicts,
// including concurrent restarts by console-api, trigger a fresh read and retry.
func rolloutRestartDex(ctx context.Context, clientset kubernetes.Interface) error {
	deploys := clientset.AppsV1().Deployments(migrate.DexNamespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deploy, err := deploys.Get(ctx, "dex", metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading dex Deployment: %w", err)
		}
		if deploy.Spec.Template.Annotations == nil {
			deploy.Spec.Template.Annotations = map[string]string{}
		}
		deploy.Spec.Template.Annotations["kipper.run/user-imported-at"] = time.Now().Format(time.RFC3339)
		if _, err := deploys.Update(ctx, deploy, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating dex Deployment: %w", err)
		}
		return nil
	})
}
