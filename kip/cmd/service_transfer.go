package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/kip/internal/service"
)

// tuningPauseLease is the deadline each tuner-pause stamp carries, and
// tuningPauseRefresh is how often a running transfer renews it. Renewal
// keeps a transfer of any length shielded, while a crashed CLI leaves at
// most one lease of untuned time behind.
const (
	tuningPauseLease   = 15 * time.Minute
	tuningPauseRefresh = 5 * time.Minute
)

var serviceImportCmd = &cobra.Command{
	Use:   "import [service] --file dump",
	Short: "Load a database dump into a service",
	Long: `Streams a dump file into a database service and runs the engine's own
restore tool inside the service pod. Supported engines: mongodb (mongodump
archives), postgres (pg_dump custom format or plain SQL, optionally
gzipped), mysql (SQL scripts, optionally gzipped).

The resource tuner is paused for the service while the import runs, so a
resource adjustment cannot restart the database mid-restore.

Examples:
  kip service import mongodb --file backup.archive.gz
  kip service import mongodb --file backup.archive.gz --database supplemento --source-database prod
  kip service import mydb --file dump.sql --database app
  kip service import mydb --file nightly.dump --database app --drop`,
	Args: cobra.ExactArgs(1),
	RunE: runServiceImport,
}

var serviceExportCmd = &cobra.Command{
	Use:   "export [service] --file out",
	Short: "Dump a database service to a local file",
	Long: `Runs the engine's own dump tool inside the service pod and streams the
result to a local file. mongodb produces a gzipped archive, postgres a
custom-format dump (pg_restore input), mysql a gzipped SQL script.

Examples:
  kip service export mongodb --file backup.archive.gz
  kip service export mydb --file nightly.dump --database app`,
	Args: cobra.ExactArgs(1),
	RunE: runServiceExport,
}

func init() {
	serviceImportCmd.Flags().String("file", "", "dump file to load (required)")
	serviceImportCmd.Flags().String("database", "", "target database (defaults to the service's own database for postgres/mysql)")
	serviceImportCmd.Flags().String("source-database", "", "database name inside a mongodb archive, for renaming on restore")
	serviceImportCmd.Flags().Bool("drop", false, "replace existing data instead of failing on it")
	serviceImportCmd.Flags().String("project", "default", "project namespace")
	serviceImportCmd.Flags().String("environment", "", "target environment")
	_ = serviceImportCmd.MarkFlagRequired("file")

	serviceExportCmd.Flags().String("file", "", "output file (required)")
	serviceExportCmd.Flags().String("database", "", "database to dump (defaults to the service's own database; mongodb dumps all databases when omitted)")
	serviceExportCmd.Flags().String("project", "default", "project namespace")
	serviceExportCmd.Flags().String("environment", "", "target environment")
	_ = serviceExportCmd.MarkFlagRequired("file")

	serviceCmd.AddCommand(serviceImportCmd)
	serviceCmd.AddCommand(serviceExportCmd)
}

func runServiceImport(cmd *cobra.Command, args []string) error {
	name := args[0]
	filePath, _ := cmd.Flags().GetString("file")
	database, _ := cmd.Flags().GetString("database")
	sourceDB, _ := cmd.Flags().GetString("source-database")
	drop, _ := cmd.Flags().GetBool("drop")
	namespace := resolveServiceNamespace(cmd)

	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("opening dump file: %w", err)
	}
	defer func() { _ = file.Close() }()

	head := make([]byte, 5)
	n, err := io.ReadFull(file, head)
	if err != nil && n == 0 {
		return fmt.Errorf("dump file %s is empty", filePath)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewinding dump file: %w", err)
	}
	format := service.SniffDumpFormat(head[:n])

	target, err := resolveTransferTarget(cmd.Context(), name, namespace, &database)
	if err != nil {
		return err
	}
	// mongodb restores the archive's own databases unless a rename pair is
	// given, so the default database must not leak into the options there.
	if target.svcType == "mongodb" && sourceDB == "" && !cmd.Flags().Changed("database") {
		database = ""
	}

	command, err := service.BuildImportCommand(target.svcType, service.ImportOptions{
		Database:       database,
		SourceDatabase: sourceDB,
		Drop:           drop,
		Format:         format,
	})
	if err != nil {
		return err
	}

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("reading dump file size: %w", err)
	}
	fmt.Printf("\n  Importing %s (%s) into %s/%s...\n\n", filePath, humanBytes(info.Size()), namespace, name)

	// Fail closed: without the shield, a tuning restart could kill the
	// restore halfway through and leave a partially imported database.
	resume, err := pauseTuning(cmd.Context(), target.clientset, namespace, name)
	if err != nil {
		return fmt.Errorf("pausing the resource tuner: %w", err)
	}
	defer resume()

	// Engine progress goes to the user's terminal via stderr while the dump
	// streams up through stdin.
	if err := target.exec(cmd.Context(), command, file, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("import failed: %w", err)
	}

	fmt.Printf("\n  ✔  Import complete\n\n")
	return nil
}

func runServiceExport(cmd *cobra.Command, args []string) error {
	name := args[0]
	filePath, _ := cmd.Flags().GetString("file")
	database, _ := cmd.Flags().GetString("database")
	namespace := resolveServiceNamespace(cmd)

	target, err := resolveTransferTarget(cmd.Context(), name, namespace, &database)
	if err != nil {
		return err
	}
	if target.svcType == "mongodb" && !cmd.Flags().Changed("database") {
		database = ""
	}

	command, err := service.BuildExportCommand(target.svcType, database)
	if err != nil {
		return err
	}

	out, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("creating output file: %w", err)
	}

	fmt.Printf("\n  Exporting %s/%s to %s...\n\n", namespace, name, filePath)

	resume, err := pauseTuning(cmd.Context(), target.clientset, namespace, name)
	if err != nil {
		_ = out.Close()
		_ = os.Remove(filePath)
		return fmt.Errorf("pausing the resource tuner: %w", err)
	}
	defer resume()

	if err := target.exec(cmd.Context(), command, nil, out, os.Stderr); err != nil {
		_ = out.Close()
		_ = os.Remove(filePath)
		return fmt.Errorf("export failed: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("writing output file: %w", err)
	}

	info, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("reading output file size: %w", err)
	}
	fmt.Printf("\n  ✔  Exported %s (%s)\n", filePath, humanBytes(info.Size()))
	if ext := service.ExportExtension(target.svcType); ext != "" {
		fmt.Printf("  Restore with: kip service import %s --file %s\n\n", name, filePath)
	}
	return nil
}

// transferTarget bundles what an import/export needs to talk to the service
// pod: the engine type, the running pod, and an exec function bound to it.
type transferTarget struct {
	svcType   string
	clientset kubernetes.Interface
	exec      func(ctx context.Context, command []string, stdin io.Reader, stdout, stderr io.Writer) error
}

// resolveTransferTarget looks up the service, verifies the engine supports
// transfer, finds its running pod, and defaults the database to the
// service's own database when the flag was left empty.
func resolveTransferTarget(ctx context.Context, name, namespace string, database *string) (*transferTarget, error) {
	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return nil, err
	}
	clientset := k8sClient.Clientset()

	mgr := &service.Manager{Client: clientset, Dynamic: k8sClient.Dynamic()}
	conn, err := mgr.Info(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	if !service.TransferSupported(conn.Type) {
		return nil, fmt.Errorf("service %q is type %q — import/export supports mongodb, postgres, and mysql", name, conn.Type)
	}
	if *database == "" {
		*database = conn.Database
	}

	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", name),
	})
	if err != nil {
		return nil, fmt.Errorf("listing service pods: %w", err)
	}
	var pod *corev1.Pod
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			pod = &pods.Items[i]
			break
		}
	}
	if pod == nil {
		return nil, fmt.Errorf("no running pod for service %q", name)
	}
	container := workloadContainerName(pod, name)

	restConfig, err := clientcmd.BuildConfigFromFlags("", cluster.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	return &transferTarget{
		svcType:   conn.Type,
		clientset: clientset,
		exec: func(ctx context.Context, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
			req := clientset.CoreV1().RESTClient().Post().
				Resource("pods").
				Name(pod.Name).
				Namespace(namespace).
				SubResource("exec").
				VersionedParams(&corev1.PodExecOptions{
					Container: container,
					Command:   command,
					Stdin:     stdin != nil,
					Stdout:    true,
					Stderr:    true,
				}, scheme.ParameterCodec)

			executor, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
			if err != nil {
				return fmt.Errorf("creating executor: %w", err)
			}
			return executor.StreamWithContext(ctx, remotecommand.StreamOptions{
				Stdin:  stdin,
				Stdout: stdout,
				Stderr: stderr,
			})
		},
	}, nil
}

// pauseTuning stamps the service's StatefulSet so the resource controller
// leaves it alone while the transfer runs, and keeps renewing the lease
// until resume is called. Acquisition failure is an error — the shield is
// the whole point, a transfer must not run exposed to a tuning restart.
// Resume clears the stamp only when it still holds this transfer's own
// value, so overlapping transfers on one service cannot strip each other's
// protection; a lease nobody renews simply expires.
func pauseTuning(ctx context.Context, clientset kubernetes.Interface, namespace, name string) (resume func(), err error) {
	stamp := func(ctx context.Context) (string, error) {
		until := time.Now().Add(tuningPauseLease).UTC().Format(time.RFC3339)
		patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, labels.AnnoTuningPausedUntil, until)
		_, err := clientset.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
		return until, err
	}

	mine, err := stamp(ctx)
	if errors.IsNotFound(err) {
		// No StatefulSet means nothing the tuner would restart.
		return func() {}, nil
	}
	if err != nil {
		return nil, err
	}

	var mu sync.Mutex
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(tuningPauseRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if renewed, err := stamp(ctx); err == nil {
					mu.Lock()
					mine = renewed
					mu.Unlock()
				}
			}
		}
	}()

	return func() {
		close(stop)
		<-done
		// The transfer's own context may already be cancelled (Ctrl-C);
		// cleanup gets a fresh bounded one.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		mu.Lock()
		own := mine
		mu.Unlock()
		sts, err := clientset.AppsV1().StatefulSets(namespace).Get(cleanupCtx, name, metav1.GetOptions{})
		if err != nil || sts.Annotations[labels.AnnoTuningPausedUntil] != own {
			// Gone, unreadable, or overwritten by a newer transfer —
			// either way this stamp is not ours to clear any more.
			return
		}
		clear := fmt.Sprintf(`{"metadata":{"annotations":{%q:null}}}`, labels.AnnoTuningPausedUntil)
		if _, err := clientset.AppsV1().StatefulSets(namespace).Patch(cleanupCtx, name, types.MergePatchType, []byte(clear), metav1.PatchOptions{}); err != nil {
			fmt.Printf("  ⚠  could not resume the resource tuner (it resumes by itself at %s): %v\n", own, err)
		}
	}, nil
}
