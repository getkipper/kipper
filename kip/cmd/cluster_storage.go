package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/k8s"
)

var clusterStorageCmd = &cobra.Command{
	Use:   "storage",
	Short: "Show the cluster's storage and set how many copies it offers",
	Long: `Shows the storage mode, the most copies of a volume the cluster offers,
and how many nodes can hold copies. With --max-copies, sets the offer: with
2, deployers may create services and volumes with two copies. Existing
volumes keep their copies either way. Two copies need Longhorn storage.

Examples:
  kip cluster storage
  kip cluster storage --max-copies 2`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runClusterStorage,
}

func init() {
	clusterStorageCmd.Flags().Int32("max-copies", 0, "the most copies of a volume deployers may ask for: 1 or 2")
	clusterCmd.AddCommand(clusterStorageCmd)
}

func runClusterStorage(cmd *cobra.Command, _ []string) error {
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx := context.Background()
	dyn := k8sClient.Dynamic()
	if cmd.Flags().Changed("max-copies") {
		maxCopies, _ := cmd.Flags().GetInt32("max-copies")
		return setCopiesOffer(ctx, os.Stdout, dyn, maxCopies)
	}
	c := readCopiesCluster(ctx, dyn)
	say(os.Stdout, "\n  Storage:      %s\n", c.mode)
	if c.mode == platform.StorageLocal {
		say(os.Stdout, "  Copies:       1 (local storage keeps one copy on the node that runs the workload)\n\n")
		return nil
	}
	say(os.Stdout, "  Copies:       up to %d offered\n", recordedCopiesOffer(ctx, dyn))
	if c.replicaNodes == unknownReplicaNodes {
		say(os.Stdout, "  Copy nodes:   unknown (Longhorn's nodes could not be read)\n\n")
		return nil
	}
	say(os.Stdout, "  Copy nodes:   %d ready to hold copies\n\n", c.replicaNodes)
	return nil
}

// setCopiesOffer records the admin's copies offer in the PlatformConfig and
// renders it into the admission policy that enforces it. Running it again
// repairs a policy left behind by an interrupted run.
func setCopiesOffer(ctx context.Context, out io.Writer, dyn dynamic.Interface, maxCopies int32) error {
	if err := refuseCopiesOutOfRange(maxCopies); err != nil {
		return fmt.Errorf("--max-copies must be 1 or 2, got %d", maxCopies)
	}
	if maxCopies > 1 && k8s.ReadStorageMode(ctx, dyn) == platform.StorageLocal {
		return fmt.Errorf("two copies need Longhorn storage, and this cluster uses local storage")
	}
	patch := []byte(fmt.Sprintf(`{"spec":{"maxCopies":%d}}`, maxCopies))
	if _, err := dyn.Resource(platformConfigGVR).Patch(ctx, "platform", types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("recording the copies offer: %w", err)
	}
	if err := applyAdmissionPolicies(ctx, dyn); err != nil {
		return fmt.Errorf("applying the copies offer: %w. Run the command again to finish", err)
	}
	if maxCopies == 1 {
		say(out, "\n  ✔  The cluster offers one copy of each volume. Volumes that already have two keep them.\n\n")
		return nil
	}
	say(out, "\n  ✔  The cluster offers two copies. Deployers can choose them with 'kip service add --copies 2'.\n\n")
	return nil
}
