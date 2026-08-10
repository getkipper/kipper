package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/migrate"
)

var userImportCmd = &cobra.Command{
	Use:   "import [snapshot.yaml]",
	Short: "Import Dex static users and connectors from a captured snapshot",
	Long: `Merges the staticPasswords and connectors blocks from a captured
dex-config snapshot into the live dex/dex-config ConfigMap. Used after a
post-rename install when production users live in a Velero snapshot but
the new install's dex-config has only the bootstrap admin.

The snapshot file can be either a full ConfigMap YAML manifest (the
output of "kubectl get cm dex-config -n dex -o yaml") or the raw Dex
config YAML body directly. Existing entries on the live side always win
on conflicts at the email/connector-id level, so the install admin can
never be overwritten with stale snapshot data.

Pass --restart-dex to roll the Dex Deployment afterwards so the new
config takes effect immediately.

Example:
  kip user import /tmp/dex-config-snapshot.yaml
  kip user import /tmp/dex-config-snapshot.yaml --restart-dex`,
	Args: cobra.ExactArgs(1),
	RunE: runUserImport,
}

func init() {
	userImportCmd.Flags().Bool("restart-dex", false, "roll the Dex Deployment after importing so the new config takes effect immediately")
	userCmd.AddCommand(userImportCmd)
}

func runUserImport(cmd *cobra.Command, args []string) error {
	snapshotPath := args[0]
	restart, _ := cmd.Flags().GetBool("restart-dex")

	raw, err := os.ReadFile(snapshotPath)
	if err != nil {
		return fmt.Errorf("reading snapshot %s: %w", snapshotPath, err)
	}
	configYAML, err := migrate.ExtractDexConfigYAML(raw)
	if err != nil {
		return fmt.Errorf("parsing snapshot: %w", err)
	}

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	report, err := migrate.ImportDexUsers(ctx, k8sClient.Clientset(), configYAML)
	if err != nil {
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
		fmt.Printf("\n  ✔  No new users or connectors in the snapshot. (%d entries were already present.)\n", report.AlreadyPresent)
	}

	// Always honor --restart-dex even when the import was a no-op.
	// Operators reach for this flag when a previous run wrote the
	// ConfigMap but failed before rolling Dex, or when they want to
	// force a rollout for any other reason. Bailing early on no-op
	// imports would silently swallow that intent.
	if restart {
		if err := rolloutRestartDex(ctx, k8sClient.Clientset()); err != nil {
			return fmt.Errorf("rolling Dex: %w", err)
		}
		fmt.Println("\n  ✔  Dex Deployment rolled. New users can log in once the new pod is Ready.")
		return nil
	}
	if report.HasChanges() {
		fmt.Println("\n  Run `kubectl -n dex rollout restart deploy/dex` (or pass --restart-dex) for the new config to take effect.")
	}
	return nil
}

// rolloutRestartDex annotates the Dex pod template to trigger a rolling
// restart, the same pattern `kubectl rollout restart` uses.
func rolloutRestartDex(ctx context.Context, clientset kubernetes.Interface) error {
	deploys := clientset.AppsV1().Deployments(migrate.DexNamespace)
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
}
