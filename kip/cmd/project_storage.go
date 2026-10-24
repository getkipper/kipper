package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

var projectUpdateCmd = &cobra.Command{
	Use:   "update <project> --copies 1",
	Short: "Change the copies of a project's shared storage",
	Long: `Change the shared storage copy count in every environment of a project.
Kipper converts each environment's live volume one step at a time and keeps
the copy on the server. A reduction waits until no app mounts the volume. This
Kipper release keeps shared storage at one copy.
A request for two copies is accepted only to stop a blocked or pending reduction
to one, keeping each volume's existing copies. Changing a project's spec requires
cluster admin rights.`,
	Example:      `  kip project update shop --copies 1 --wait`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runProjectUpdate,
}

func init() {
	projectUpdateCmd.Flags().Int32("copies", 0, sharedCopiesHelp)
	projectUpdateCmd.Flags().Bool("wait", false, "wait until the conversion is done and every copy is healthy. The command exits non-zero when the change is blocked")
	_ = projectUpdateCmd.MarkFlagRequired("copies")
	projectCmd.AddCommand(projectUpdateCmd)
}

func runProjectUpdate(cmd *cobra.Command, args []string) error {
	copies, _ := cmd.Flags().GetInt32("copies")
	wait, _ := cmd.Flags().GetBool("wait")
	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	fmt.Println()
	if err := setProjectStorageCopies(context.Background(), os.Stdout, k8sClient.Dynamic(), cluster.ResolveNamespace(args[0], ""), copies, wait); err != nil {
		return err
	}
	fmt.Println()
	return nil
}

// setProjectStorageCopies sets the copies of the project's shared storage
// and follows the conversion like the other copies commands. It refuses a
// project without shared storage.
func setProjectStorageCopies(ctx context.Context, out io.Writer, dyn dynamic.Interface, project string, copies int32, wait bool) error {
	obj, err := dyn.Resource(manifest.ProjectGVR).Get(ctx, project, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading project %s: %w", project, err)
	}
	if enabled, _, _ := unstructured.NestedBool(obj.Object, "spec", "sharedStorage", "enabled"); !enabled {
		return fmt.Errorf("project %s has no shared storage", project)
	}
	return setCopies(ctx, out, dyn, copiesChange{
		gvr: manifest.ProjectGVR, name: project, field: []string{"spec", "sharedStorage", "copies"},
		want: copies, shared: true, wait: wait, infoCommand: "kip status",
	})
}
