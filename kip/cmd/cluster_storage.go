package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/installer"
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

var clusterStorageLonghornCmd = &cobra.Command{
	Use:   "longhorn",
	Short: "Switch a local-storage cluster to Longhorn in place",
	Long: `Installs Longhorn on a cluster that uses local storage and records Longhorn as
its storage mode. Nothing is reinstalled and no service stops.

New services and volumes then get one Longhorn copy. The data of existing
services stays where it is until you move each one with
'kip service storage longhorn <name>'. The platform's own volumes (registry,
backup store, logs) stay on local storage.

The server needs at least 6000 MiB of RAM. Run 'kip upgrade' first if the
cluster runs an older platform. Running the command again after an
interruption finishes the install.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runClusterStorageLonghorn,
}

var clusterStoragePruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Delete the local copies that services left behind when they moved to Longhorn",
	Long: `Lists the local directories 'kip service storage longhorn' kept after moving a
service, and deletes copies whose service still uses the verified Longhorn
volume or whose service and data claim are both gone. Copies from unfinished
moves are kept.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runClusterStoragePrune,
}

func init() {
	clusterStorageCmd.Flags().Int32("max-copies", 0, "the most copies of a volume deployers may ask for: 1 or 2")
	clusterStorageLonghornCmd.Flags().String("ssh-key", "", "path to SSH private key")
	addSkipMemoryFloorFlag(clusterStorageLonghornCmd)
	clusterStoragePruneCmd.Flags().BoolP("yes", "y", false, "skip the confirmation prompt")
	clusterStoragePruneCmd.Flags().String("ssh-key", "", "path to SSH private key")
	clusterStorageCmd.AddCommand(clusterStorageLonghornCmd, clusterStoragePruneCmd)
	clusterCmd.AddCommand(clusterStorageCmd)
}

// leaseClusterServer takes the node lifecycle lease on the current cluster's server
// and confirms the host is the cluster's recorded server.
func leaseClusterServer(cmd *cobra.Command, purpose string) (*leasedCluster, error) {
	cluster, _, err := loadCurrentCluster()
	if err != nil {
		return nil, err
	}
	if cluster.Host == "" {
		return nil, fmt.Errorf("no host recorded for cluster %s. This command needs SSH access to the server", cluster.Name)
	}
	sshKey, _ := cmd.Flags().GetString("ssh-key")
	lc, err := leaseCluster(cluster, sshKey, purpose)
	if err != nil {
		return nil, err
	}
	serverID, err := installer.ReadMachineID(lc.server)
	if err == nil {
		var records []installer.NodeRecord
		if records, err = installer.ReadNodeRecords(lc.server); err == nil {
			err = serverIsTheCluster(serverID, cluster.MachineID, records)
		}
	}
	if err != nil {
		lc.close()
		return nil, fmt.Errorf("%s: %w. Check the cluster's host. Nothing was changed", cluster.Host, err)
	}
	return lc, nil
}

func runClusterStorageLonghorn(cmd *cobra.Command, _ []string) error {
	lc, err := leaseClusterServer(cmd, "kip cluster storage longhorn")
	if err != nil {
		return err
	}
	defer lc.close()
	return installer.StorageGrowth{Server: lc.server, Out: os.Stdout, SkipMemoryFloor: skipMemoryFloor(cmd)}.Run()
}

func runClusterStoragePrune(cmd *cobra.Command, _ []string) error {
	lc, err := leaseClusterServer(cmd, "kip cluster storage prune")
	if err != nil {
		return err
	}
	defer lc.close()
	yes, _ := cmd.Flags().GetBool("yes")
	return pruneRetainedCopies(os.Stdout, lc.server, confirmInteractive, term.IsTerminal(int(os.Stdin.Fd())), yes, time.Now())
}

// pruneRetainedCopies lists the retained local copies and, after consent,
// deletes copies whose service still uses the verified Longhorn volume or
// whose service and data claim are both gone.
func pruneRetainedCopies(out io.Writer, server installer.CommandRunner, confirm func(string) (bool, error), interactive, yes bool, now time.Time) error {
	mode, err := installer.ReadStorageMode(server)
	if err != nil {
		return err
	}
	if platform.StorageMode(mode) == platform.StorageLocal {
		return fmt.Errorf("this cluster uses local storage, so no service has moved off it yet")
	}
	copies, err := installer.RetainedCopies(server)
	if err != nil {
		return err
	}
	var prunable []installer.RetainedCopy
	var bytes int64
	say(out, "\n  Local copies kept after a move to Longhorn:\n")
	for _, c := range copies {
		note := "moved, can be deleted"
		switch {
		case c.State != platform.RetainedLocalCopyMoved:
			note = fmt.Sprintf("kept: its move is unfinished. Finish it with 'kip service storage longhorn %s --project %s'", c.Service, c.Namespace)
		case !c.Prunable:
			note = c.Note
		default:
			if c.Note != "" {
				note = "moved, can be deleted: " + c.Note
			}
			prunable = append(prunable, c)
			bytes += c.UsedBytes
		}
		age := ""
		if !c.MovedAt.IsZero() {
			age = ", moved " + humanAge(now, c.MovedAt) + " ago"
		}
		say(out, "    %s/%s (%s%s): %s\n", c.Namespace, c.Service, humanVolumeSize(c.UsedBytes), age, note)
	}
	if len(copies) == 0 {
		say(out, "    none\n\n")
		return nil
	}
	if len(prunable) == 0 {
		say(out, "\n  Nothing to delete.\n\n")
		return nil
	}
	if !yes {
		if !interactive {
			return fmt.Errorf("deleting the copies cannot be undone. Pass --yes to delete them without a prompt")
		}
		ok, err := confirm(fmt.Sprintf("\n  Delete %d local copies (%s)? This cannot be undone. [y/N] ", len(prunable), humanVolumeSize(bytes)))
		if err != nil {
			return err
		}
		if !ok {
			say(out, "  Nothing was deleted.\n\n")
			return nil
		}
	}
	for _, c := range prunable {
		gone, err := installer.PruneRetainedCopy(server, c.PV, 60, 2*time.Second, time.Sleep)
		switch {
		case err != nil:
			return fmt.Errorf("deleting the local copy of %s/%s: %w", c.Namespace, c.Service, err)
		case gone:
			say(out, "  ✔  Deleted the local copy of %s/%s\n", c.Namespace, c.Service)
		default:
			say(out, "  ⚠  The local copy of %s/%s (volume %s) is still being deleted. Check it later with 'kip cluster storage prune'\n", c.Namespace, c.Service, c.PV)
		}
	}
	say(out, "\n")
	return nil
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
		say(os.Stdout, "  Copies:       1 (local storage keeps one copy on the node that runs the workload)\n")
		say(os.Stdout, "  Grow:         'kip cluster storage longhorn' switches this cluster to Longhorn in place\n\n")
		return nil
	}
	say(os.Stdout, "  Copies:       up to %d offered\n", recordedCopiesOffer(ctx, dyn))
	if c.replicaNodes == unknownReplicaNodes {
		say(os.Stdout, "  Copy nodes:   unknown (Longhorn's nodes could not be read)\n\n")
	} else {
		say(os.Stdout, "  Copy nodes:   %d ready to hold copies\n\n", c.replicaNodes)
	}
	reportLocalVolumes(ctx, os.Stdout, k8sClient.Clientset(), dyn)
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
