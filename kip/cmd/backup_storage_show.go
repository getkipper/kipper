package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/kip/internal/installer"
)

var backupStorageShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show where the cluster's backups are stored",
	Long: `Shows whether Velero writes backups to in-cluster MinIO or an external bucket.
For external storage, it shows the bucket, region and endpoint. It also shows
how stored objects are encrypted and the result of Velero's last location check.

During or after kip backup storage set, it shows any unfinished move and
recorded previous bucket. It reports whether the credentials and encryption
key Secrets exist without printing their contents.`,
	Example: "  kip backup storage show",
	Args:    cobra.NoArgs,
	RunE:    runBackupStorageShow,
}

func init() {
	backupStorageCmd.AddCommand(backupStorageShowCmd)
}

func runBackupStorageShow(cmd *cobra.Command, _ []string) error {
	cmd.SilenceUsage = true
	_, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return showBackupStorage(ctx, client.Dynamic(), client.Clientset(), os.Stdout, time.Now())
}

// showBackupStorage prints Velero's default backup location and kip's storage
// move record. It reports Secret presence, strips URL user information, queries
// and fragments, and omits Velero's status message because it can quote provider
// errors. It returns errBackupsNotConfigured if the cluster records backups as
// disabled, or an error if the location is missing.
func showBackupStorage(ctx context.Context, dyn dynamic.Interface, cs kubernetes.Interface, out io.Writer, now time.Time) error {
	if !backupsConfiguredOn(ctx, dyn) {
		return errBackupsNotConfigured
	}
	record, err := readStorageRecordFrom(ctx, cs)
	if err != nil {
		return err
	}
	bsl, err := dyn.Resource(backupStorageLocationGVR).Namespace("velero").Get(ctx, "default", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if m := record.Move; m != nil {
			return fmt.Errorf("no Velero backup location velero/default exists while backups are moving to %s. Run the same kip backup storage set command again, with the same flags, to finish the move", m.Target.String())
		}
		return errors.New("no Velero backup location velero/default exists, so backups have nowhere to go")
	}
	if err != nil {
		return fmt.Errorf("reading Velero's backup location: %w", err)
	}

	str := func(fields ...string) string { v, _, _ := unstructured.NestedString(bsl.Object, fields...); return v }
	line := func(label, value string) { _, _ = fmt.Fprintf(out, "  %-13s %s\n", label, value) }
	endpoint := str("spec", "config", "s3Url")

	minio := installer.BackupsGoToMinIO(endpoint)
	_, _ = fmt.Fprintf(out, "\n  Backup storage\n\n")
	if minio {
		line("Location", "in-cluster MinIO, on this cluster's own disk")
	} else {
		line("Bucket", str("spec", "objectStorage", "bucket"))
		if prefix := str("spec", "objectStorage", "prefix"); prefix != "" {
			line("Prefix", prefix)
		}
		line("Region", str("spec", "config", "region"))
		line("Endpoint", installer.DisplayEndpoint(endpoint))
	}
	line("Objects", objectEncryption(ctx, cs, minio, str("spec", "config", "customerKeyEncryptionSecret"), str("spec", "config", "serverSideEncryption")))
	if str("spec", "accessMode") == "ReadOnly" {
		line("Access", "read-only, so Velero cannot create or delete backups here")
	}
	line("Credentials", "Secret velero/cloud-credentials "+secretPresence(ctx, cs, "cloud-credentials"))
	stopped := veleroStopped(ctx, cs)
	switch {
	case stopped && record.Move != nil:
		line("Velero", "stopped, so no backups run")
		restart := "when the move finishes"
		if record.Move.Cancelable() {
			restart = "when the move finishes or is canceled"
		}
		_, _ = fmt.Fprintf(out, "\n  kip stopped Velero for the unfinished move below. It starts again %s.\n", restart)
	case stopped:
		line("Velero", "stopped, so no backups run")
		_, _ = fmt.Fprintf(out, "\n  The Velero server is scaled to zero. kip backup repository-key reset stops Velero during the reset. If a reset was interrupted, run the command again to finish it.\n")
	default:
		line("Velero", locationPhase(str("status", "phase"), str("status", "lastValidationTime"), now))
		if str("status", "phase") == "Unavailable" {
			hint := "Velero cannot use this location, so new backups fail. Check that the bucket exists and that its credentials and endpoint are still valid."
			if minio {
				hint = "Velero cannot use the in-cluster MinIO, so new backups fail. MinIO may be down or its volume may be full. Use kip backup storage resize to grow the volume."
			}
			_, _ = fmt.Fprintf(out, "\n  %s\n", hint)
		}
	}

	if m := record.Move; m != nil {
		_, _ = fmt.Fprintf(out, "\n  A move to %s is unfinished (phase %s, started %s).\n", m.Target.String(), m.Phase, m.StartedAt)
		_, _ = fmt.Fprintf(out, "  Run the same kip backup storage set command again, with the same flags, to finish it.\n")
		if m.Cancelable() {
			_, _ = fmt.Fprintf(out, "  To cancel it instead, run kip backup storage set --abort.\n")
		}
	}
	// Defer retirement guidance until the move finishes because retire-previous
	// rejects unfinished moves. During the switched phase, the location above may
	// still name the previous bucket while the chart update takes effect.
	if p := record.Previous; p != nil && record.Move == nil {
		_, _ = fmt.Fprintf(out, "\n  The previous bucket was %s. The switch occurred at %s.\n", p.String(), p.SwitchedAt)
		_, _ = fmt.Fprintf(out, "  Wait for a backup that started in the current bucket after the switch to complete, then run kip backup storage retire-previous.\n")
	} else if p != nil && (p.Bucket != record.Move.Previous.Bucket || p.Endpoint != record.Move.Previous.Endpoint) {
		_, _ = fmt.Fprintf(out, "  The previous bucket %s is kept until the move finishes and kip backup storage retire-previous runs.\n", p.String())
	}
	_, _ = fmt.Fprintln(out)
	return nil
}

// readStorageRecordFrom reads kip's backup storage record through the API.
// A missing ConfigMap is an empty record.
func readStorageRecordFrom(ctx context.Context, cs kubernetes.Interface) (installer.StorageRecord, error) {
	cm, err := cs.CoreV1().ConfigMaps("velero").Get(ctx, installer.StorageRecordConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return installer.StorageRecord{}, nil
	}
	if err != nil {
		return installer.StorageRecord{}, fmt.Errorf("reading the backup storage record: %w", err)
	}
	return installer.ParseStorageRecord(cm.Data)
}

// objectEncryption describes the configured object encryption, preferring SSE-C
// when both settings are present. customerKeySecret uses <secret>/<key>, and the
// description includes the Secret's presence. With neither setting, in-cluster
// MinIO stores objects as written and external storage uses the provider's
// default. Volume data is encrypted with the backup repository key in every
// case. The MinIO description mentions this because MinIO adds no encryption.
func objectEncryption(ctx context.Context, cs kubernetes.Interface, minio bool, customerKeySecret, serverSide string) string {
	switch {
	case customerKeySecret != "":
		name, _, _ := strings.Cut(customerKeySecret, "/")
		return "SSE-C, key in Secret velero/" + name + " " + secretPresence(ctx, cs, name)
	case serverSide != "":
		return "SSE-S3 (" + serverSide + ")"
	case minio:
		return "MinIO stores objects as written and adds no encryption. Volume data in these objects is encrypted with the backup repository key"
	default:
		return "the provider's default"
	}
}

// secretPresence checks whether a Secret exists in the velero namespace.
// A missing Secret is reported as missing. Other read failures are reported as
// unreadable without exposing the API error.
func secretPresence(ctx context.Context, cs kubernetes.Interface, name string) string {
	_, err := cs.CoreV1().Secrets("velero").Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		return "present"
	case apierrors.IsNotFound(err):
		return "missing"
	case apierrors.IsForbidden(err):
		return "not readable with your access"
	default:
		return "not readable"
	}
}

// veleroStopped reports whether the velero/velero Deployment is scaled to zero.
// Read failures and an unset replica count return false so the caller falls
// back to the location's status.
func veleroStopped(ctx context.Context, cs kubernetes.Interface) bool {
	d, err := cs.AppsV1().Deployments("velero").Get(ctx, "velero", metav1.GetOptions{})
	return err == nil && d.Spec.Replicas != nil && *d.Spec.Replicas == 0
}

func locationPhase(phase, lastValidation string, now time.Time) string {
	if phase == "" {
		phase = "not validated yet"
	}
	validated, err := time.Parse(time.RFC3339, lastValidation)
	if err != nil {
		return phase
	}
	return fmt.Sprintf("%s, last checked %s ago", phase, humanAge(now, validated))
}
