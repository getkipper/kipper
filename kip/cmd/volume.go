package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

var volumeCmd = &cobra.Command{
	Use:   "volume",
	Short: "Manage shared volumes",
}

var volumeCreateCmd = &cobra.Command{
	Use:   "create [name]",
	Short: "Create a shared volume that can be mounted by multiple apps",
	Long: `Create a shared persistent volume backed by Longhorn. The volume
supports ReadWriteMany access, multiple pods can read and write to it
simultaneously. Useful for shared file storage, uploads, and legacy
apps that need a shared filesystem.

For new projects, consider using MinIO (S3-compatible storage) instead.

Examples:
  kip volume create uploads --size 5Gi --project blog --environment test
  kip volume create media --size 20Gi`,
	Args: cobra.ExactArgs(1),
	RunE: runVolumeCreate,
}

var volumeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List shared volumes",
	RunE:  runVolumeList,
}

var volumeDeleteCmd = &cobra.Command{
	Use:   "delete [name]",
	Short: "Delete a shared volume and its data",
	Args:  cobra.ExactArgs(1),
	RunE:  runVolumeDelete,
}

var volumeMountCmd = &cobra.Command{
	Use:   "mount [volume-name] [app-name] --path /data",
	Short: "Mount a shared volume into an app",
	Args:  cobra.ExactArgs(2),
	RunE:  runVolumeMount,
}

var volumeUnmountCmd = &cobra.Command{
	Use:   "unmount [volume-name] [app-name]",
	Short: "Unmount a shared volume from an app",
	Args:  cobra.ExactArgs(2),
	RunE:  runVolumeUnmount,
}

func init() {
	volumeCreateCmd.Flags().String("size", "5Gi", "volume size")
	volumeCreateCmd.Flags().String("project", "", "project name")
	volumeCreateCmd.Flags().String("environment", "", "target environment")

	volumeListCmd.Flags().String("project", "", "project name")
	volumeListCmd.Flags().String("environment", "", "target environment")

	volumeDeleteCmd.Flags().String("project", "", "project name")
	volumeDeleteCmd.Flags().String("environment", "", "target environment")
	volumeDeleteCmd.Flags().Bool("delete-data", false, "confirm permanent data deletion")

	volumeMountCmd.Flags().String("path", "/data", "mount path inside the container")
	volumeMountCmd.Flags().String("project", "", "project name")
	volumeMountCmd.Flags().String("environment", "", "target environment")

	volumeUnmountCmd.Flags().String("project", "", "project name")
	volumeUnmountCmd.Flags().String("environment", "", "target environment")

	volumeCmd.AddCommand(volumeCreateCmd)
	volumeCmd.AddCommand(volumeListCmd)
	volumeCmd.AddCommand(volumeDeleteCmd)
	volumeCmd.AddCommand(volumeMountCmd)
	volumeCmd.AddCommand(volumeUnmountCmd)
	rootCmd.AddCommand(volumeCmd)
}

func runVolumeCreate(cmd *cobra.Command, args []string) error {
	name := args[0]
	size, _ := cmd.Flags().GetString("size")
	if _, err := resource.ParseQuantity(size); err != nil {
		return fmt.Errorf("invalid --size %q: %w", size, err)
	}

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	project, environment := resolveProjectAndEnvironment(cmd, cluster)
	ns := cluster.ResolveNamespace(project, environment)
	if ns == "" {
		return fmt.Errorf("no project context. Set one with `kip project use <name>` or pass --project")
	}

	// The Volume CR is the model: the volume reconciler provisions the
	// backing ReadWriteMany PVC and keeps mounted apps in sync.
	vol := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "Volume",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
			"labels": map[string]interface{}{
				"app.kubernetes.io/managed-by": "kipper",
				"kipper.run/resource-type":     "shared-volume",
				"kipper.run/volume-name":       name,
			},
		},
		"spec": map[string]interface{}{"size": size},
	}}

	_, err = k8sClient.Dynamic().Resource(manifest.VolumeGVR).Namespace(ns).Create(context.Background(), vol, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		fmt.Printf("  Volume %q already exists in %s\n", name, ns)
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating volume: %w", err)
	}

	fmt.Printf("\n  ✔  Shared volume %q created (%s)\n", name, size)
	fmt.Printf("  Access mode: ReadWriteMany (multiple pods can mount this)\n\n")
	fmt.Printf("  Mount into an app:\n")
	fmt.Printf("    kip volume mount %s <app-name> --path /data/uploads\n\n", name)

	return nil
}

func runVolumeList(cmd *cobra.Command, args []string) error {
	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	project, environment := resolveProjectAndEnvironment(cmd, cluster)
	ns := cluster.ResolveNamespace(project, environment)

	ctx := context.Background()
	crList, err := k8sClient.Dynamic().Resource(manifest.VolumeGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing volume CRs: %w", err)
	}

	if len(crList.Items) == 0 {
		fmt.Printf("\n  No shared volumes in %s\n\n", ns)
		return nil
	}

	fmt.Printf("\n  %-25s %-10s %-10s %s\n", "NAME", "SIZE", "STATUS", "ACCESS")
	for _, cr := range crList.Items {
		size, _, _ := unstructured.NestedString(cr.Object, "spec", "size")
		if actual, _, _ := unstructured.NestedString(cr.Object, "status", "actualSize"); actual != "" {
			size = actual
		}
		phase, _, _ := unstructured.NestedString(cr.Object, "status", "phase")
		if phase == "" {
			phase = "Pending"
		}
		fmt.Printf("  %-25s %-10s %-10s %s\n", cr.GetName(), size, strings.ToLower(phase), "ReadWriteMany")
	}
	fmt.Println()

	return nil
}

func runVolumeDelete(cmd *cobra.Command, args []string) error {
	name := args[0]
	deleteData, _ := cmd.Flags().GetBool("delete-data")

	if !deleteData {
		return fmt.Errorf("refusing to delete volume %q without --delete-data (this permanently destroys all data)", name)
	}

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	project, environment := resolveProjectAndEnvironment(cmd, cluster)
	ns := cluster.ResolveNamespace(project, environment)
	if ns == "" {
		return fmt.Errorf("no project context. Set one with `kip project use <name>` or pass --project")
	}

	// Deleting the Volume CR cascades to the owned PVC and its data.
	err = k8sClient.Dynamic().Resource(manifest.VolumeGVR).Namespace(ns).Delete(context.Background(), name, metav1.DeleteOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("volume %q not found", name)
		}
		return fmt.Errorf("deleting volume: %w", err)
	}

	fmt.Printf("  ✔  Volume %q deleted (data destroyed)\n", name)
	return nil
}

func runVolumeMount(cmd *cobra.Command, args []string) error {
	volumeName := args[0]
	appName := args[1]
	mountPath, _ := cmd.Flags().GetString("path")
	if !strings.HasPrefix(mountPath, "/") {
		return fmt.Errorf("--path must be an absolute path, got %q", mountPath)
	}

	ns, k8sClient, err := resolveAppNamespace(cmd, appName)
	if err != nil {
		return err
	}

	ctx := context.Background()

	// A mount entry for a nonexistent app would sit in the Volume CR doing
	// nothing — catch the typo here.
	_, err = k8sClient.Dynamic().Resource(deployer.AppGVR).Namespace(ns).Get(ctx, appName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return fmt.Errorf("app %q not found in %s", appName, ns)
	}
	if err != nil {
		return fmt.Errorf("getting app: %w", err)
	}

	if err := mountVolume(ctx, k8sClient.Dynamic(), ns, volumeName, appName, mountPath); err != nil {
		return err
	}

	fmt.Printf("\n  ✔  Volume %q mounted at %s in %s\n", volumeName, mountPath, appName)
	fmt.Printf("  The pod restarts to attach the volume.\n\n")

	return nil
}

func runVolumeUnmount(cmd *cobra.Command, args []string) error {
	volumeName := args[0]
	appName := args[1]

	ns, k8sClient, err := resolveAppNamespace(cmd, appName)
	if err != nil {
		return err
	}

	if err := unmountVolume(context.Background(), k8sClient.Dynamic(), ns, volumeName, appName); err != nil {
		return err
	}

	fmt.Printf("\n  ✔  Volume %q unmounted from %s\n", volumeName, appName)
	fmt.Printf("  The pod restarts to detach the volume. The volume and its data still exist.\n\n")

	return nil
}

// mountVolume records the mount on the Volume CR's spec.mounts — the
// authoritative mount list. The volume reconciler propagates it into the
// App CR's spec.volumes, and the app reconciler renders the Deployment from
// there, so the mount survives image updates and reconciles. Patching the
// Deployment (or even the App CR) directly would be undone by that chain.
// Mounting an already mounted volume updates its path.
func mountVolume(ctx context.Context, dyn dynamic.Interface, ns, volumeName, appName, mountPath string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vol, err := dyn.Resource(manifest.VolumeGVR).Namespace(ns).Get(ctx, volumeName, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("volume %q not found. Create it first with: kip volume create %s", volumeName, volumeName)
			}
			return fmt.Errorf("getting volume: %w", err)
		}

		existing, _, err := unstructured.NestedSlice(vol.Object, "spec", "mounts")
		if err != nil {
			return fmt.Errorf("reading spec.mounts: %w", err)
		}
		// Rebuild the list with exactly one entry for the app: an existing
		// entry moves to the new path and stray duplicates collapse, so the
		// reconciler (last entry wins there) can never apply a stale path.
		mounts := make([]interface{}, 0, len(existing)+1)
		mounted := false
		for _, m := range existing {
			mt, ok := m.(map[string]interface{})
			if !ok {
				return fmt.Errorf("volume %q has a malformed spec.mounts entry", volumeName)
			}
			if mt["app"] == appName {
				if mounted {
					continue
				}
				mounted = true
				mt["mountPath"] = mountPath
			}
			mounts = append(mounts, mt)
		}
		if !mounted {
			mounts = append(mounts, map[string]interface{}{
				"app":       appName,
				"mountPath": mountPath,
			})
		}

		if err := unstructured.SetNestedSlice(vol.Object, mounts, "spec", "mounts"); err != nil {
			return fmt.Errorf("setting spec.mounts: %w", err)
		}
		if _, err := dyn.Resource(manifest.VolumeGVR).Namespace(ns).Update(ctx, vol, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("mounting volume: %w", err)
		}
		return nil
	})
}

// unmountVolume removes the app from the Volume CR's spec.mounts; the volume
// reconciler then drops the entry from the App CR and the app reconciler
// renders the Deployment without the volume.
func unmountVolume(ctx context.Context, dyn dynamic.Interface, ns, volumeName, appName string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vol, err := dyn.Resource(manifest.VolumeGVR).Namespace(ns).Get(ctx, volumeName, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("volume %q not found", volumeName)
			}
			return fmt.Errorf("getting volume: %w", err)
		}

		mounts, _, err := unstructured.NestedSlice(vol.Object, "spec", "mounts")
		if err != nil {
			return fmt.Errorf("reading spec.mounts: %w", err)
		}
		kept := make([]interface{}, 0, len(mounts))
		for _, m := range mounts {
			mt, ok := m.(map[string]interface{})
			if !ok {
				return fmt.Errorf("volume %q has a malformed spec.mounts entry", volumeName)
			}
			if mt["app"] == appName {
				continue
			}
			kept = append(kept, m)
		}
		if len(kept) == len(mounts) {
			return fmt.Errorf("volume %q is not mounted in app %q", volumeName, appName)
		}

		if len(kept) == 0 {
			unstructured.RemoveNestedField(vol.Object, "spec", "mounts")
		} else if err := unstructured.SetNestedSlice(vol.Object, kept, "spec", "mounts"); err != nil {
			return fmt.Errorf("setting spec.mounts: %w", err)
		}
		if _, err := dyn.Resource(manifest.VolumeGVR).Namespace(ns).Update(ctx, vol, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("unmounting volume: %w", err)
		}
		return nil
	})
}
