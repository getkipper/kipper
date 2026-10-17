package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

const (
	minioNamespace     = "velero"
	minioClaimName     = "minio-storage"
	minioResizeTimeout = 5 * time.Minute
)

var backupStorageCmd = &cobra.Command{
	Use:   "storage",
	Short: "Manage the in-cluster backup storage",
}

var backupStorageResizeCmd = &cobra.Command{
	Use:   "resize --size <size>",
	Short: "Grow the in-cluster MinIO backup volume",
	Long: `Grow the in-cluster MinIO volume that holds Velero backups, for example
to 30Gi before installing the AI bundle with its default tier 1 model cache.
The command waits up to five minutes for the volume to report the requested
capacity. Run it again with the same size to resume waiting.

Volumes can only grow. Manage external backup storage through its provider.`,
	Example: "  kip backup storage resize --size 30Gi",
	Args:    cobra.NoArgs,
	RunE:    runBackupStorageResize,
}

func init() {
	backupStorageResizeCmd.Flags().String("size", "", "new size of the MinIO volume, for example 30Gi")
	_ = backupStorageResizeCmd.MarkFlagRequired("size")
	backupStorageCmd.AddCommand(backupStorageResizeCmd)
	backupCmd.AddCommand(backupStorageCmd)
}

func runBackupStorageResize(cmd *cobra.Command, _ []string) error {
	size, _ := cmd.Flags().GetString("size")
	_, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), minioResizeTimeout+30*time.Second)
	defer cancel()
	return resizeBackupStorage(ctx, client.Clientset(), size, 2*time.Second, minioResizeTimeout, os.Stdout)
}

// resizeBackupStorage raises the MinIO claim's storage request to size, a
// Kubernetes quantity, then polls for at least that capacity for up to timeout.
// An equal request resumes waiting. A smaller request returns an error.
// Lookup, update and wait failures also return errors.
func resizeBackupStorage(ctx context.Context, cs kubernetes.Interface, size string, interval, timeout time.Duration, out io.Writer) error {
	want, err := resource.ParseQuantity(size)
	if err != nil {
		return fmt.Errorf("invalid --size %q: %w", size, err)
	}
	claims := cs.CoreV1().PersistentVolumeClaims(minioNamespace)
	pvc, err := claims.Get(ctx, minioClaimName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return errors.New("this cluster has no in-cluster MinIO volume (velero/minio-storage). External backup storage is sized by its provider")
	}
	if err != nil {
		return fmt.Errorf("reading the MinIO volume: %w", err)
	}

	current := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	switch want.Cmp(current) {
	case -1:
		return fmt.Errorf("the MinIO volume already requests %s and volumes can only grow. Pass a size of at least %s", current.String(), current.String())
	case 0:
		if reached(pvc, want) {
			_, _ = fmt.Fprintf(out, "\n  ✔  The MinIO volume is already %s\n\n", want.String())
			return nil
		}
		_, _ = fmt.Fprintf(out, "\n  Waiting for the MinIO volume to reach %s...\n", want.String())
	default:
		if pvc.Spec.Resources.Requests == nil {
			pvc.Spec.Resources.Requests = corev1.ResourceList{}
		}
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = want
		if _, err := claims.Update(ctx, pvc, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("growing the MinIO volume: %w", err)
		}
		_, _ = fmt.Fprintf(out, "\n  Growing the MinIO volume from %s to %s...\n", current.String(), want.String())
	}

	err = wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
		latest, err := claims.Get(ctx, minioClaimName, metav1.GetOptions{})
		if err != nil {
			return false, nil //nolint:nilerr // Retry transient read failures until the timeout.
		}
		return reached(latest, want), nil
	})
	if err != nil {
		return fmt.Errorf("the MinIO volume has a requested capacity of %s and does not report that size yet. Check the volume status and available disk space. Run the same command again to resume waiting", want.String())
	}
	_, _ = fmt.Fprintf(out, "  ✔  The MinIO volume is now %s\n\n", want.String())
	return nil
}

func reached(pvc *corev1.PersistentVolumeClaim, want resource.Quantity) bool {
	capacity, ok := pvc.Status.Capacity[corev1.ResourceStorage]
	return ok && capacity.Cmp(want) >= 0
}
