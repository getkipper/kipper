package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/getkipper/kipper/kip/internal/k8s"
	"github.com/getkipper/kipper/kip/internal/service"
)

var serviceRestoreDumpCmd = &cobra.Command{
	Use:   "restore-dump <service> --to <target>",
	Short: "Restore a Postgres service's consistent dump from a backup into another empty Postgres service",
	Long: `Restores a Postgres service's consistent pg_dump from a backup
into another Postgres service in the same namespace. The namespace's service
pods are first restored from the backup into the isolated namespace
kipper-restore-drill. The source service's dump manifest and checksums are
checked, then each database is streamed through the Kubernetes API into the
target in one transaction per database. Row counts in the target must match
the counts recorded with the dump.

The target must exist and have no user tables in any connectable, non-template
database. To protect existing table data from being overwritten, kip checks
this before the restore starts and again immediately before the first load.
Create it with kip service add postgres --name <target>, then bind apps to it
once the restore has finished. Roles and grants are not restored. Every object
belongs to the target's own user, which is the user its bindings connect as.`,
	Example: `  kip service restore-dump mydb --to mydb-restored
  kip service restore-dump mydb --to mydb-restored --backup velero-daily-apps-20261019030050`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runServiceRestoreDump,
}

func init() {
	serviceRestoreDumpCmd.Flags().String("to", "", "empty Postgres service to restore into (required)")
	serviceRestoreDumpCmd.Flags().String("backup", "", "backup to restore from (default: the latest completed backup that covers the namespace)")
	serviceRestoreDumpCmd.Flags().String("project", "default", "project namespace")
	serviceRestoreDumpCmd.Flags().String("environment", "", "target environment")
	_ = serviceRestoreDumpCmd.MarkFlagRequired("to")
	serviceCmd.AddCommand(serviceRestoreDumpCmd)
}

type dumpEntry struct {
	Database string
	File     string
	SHA256   string
}

var dumpFilePattern = regexp.MustCompile(`^[0-9]+\.dump$`)

// parseDumpManifest reads the backup hook's manifest. It returns an error
// unless the manifest is complete, names at least one database, and records
// a successful dump with a checksum for each.
func parseDumpManifest(manifest string) ([]dumpEntry, error) {
	lines := strings.Split(strings.TrimRight(manifest, "\n"), "\n")
	if len(lines) == 0 || lines[0] != "format\tkipper-dump/1" {
		return nil, errors.New("the dump manifest has an unknown format")
	}
	if lines[len(lines)-1] != "complete\ttrue" {
		return nil, errors.New("the consistent dump in this backup is incomplete")
	}
	var entries []dumpEntry
	for _, line := range lines {
		if !strings.HasPrefix(line, "database\t") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 6 || !dumpFilePattern.MatchString(f[2]) || f[3] != "0" || len(f[5]) != 64 {
			return nil, fmt.Errorf("the dump manifest records no usable dump in line %q", line)
		}
		entries = append(entries, dumpEntry{Database: f[1], File: f[2], SHA256: f[5]})
	}
	if len(entries) == 0 {
		return nil, errors.New("the dump manifest names no database")
	}
	return entries, nil
}

// postgresTablesScript lists connectable, non-template databases with user
// tables, one per line. On success, empty output means those databases have
// no user tables.
const postgresTablesScript = `U="${POSTGRES_USER:-postgres}"
dbs=$(psql -U "$U" -d postgres -XAtq -v ON_ERROR_STOP=1 -c "SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate") || exit 1
printf '%s\n' "$dbs" | while IFS= read -r db; do
  [ -n "$db" ] || continue
  n=$(PGDATABASE="$db" psql -U "$U" -XAtq -v ON_ERROR_STOP=1 -c "SELECT count(*) FROM pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema')") || exit 1
  [ "$n" = 0 ] || printf '%s\n' "$db"
done`

// createDatabaseScript creates the database named by $1 from template0 unless
// it already exists. psql quotes the name as an SQL identifier.
const createDatabaseScript = `psql -U "${POSTGRES_USER:-postgres}" -d postgres -XAtq -v ON_ERROR_STOP=1 -v db="$1" <<'SQL'
SELECT format('CREATE DATABASE %I TEMPLATE template0', :'db') WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = :'db') \gexec
SQL`

// loadDumpScript restores the custom-format dump on stdin into the database
// whose connection string is $1, in one transaction that a truncated or failing
// dump rolls back. Ownership and grants from the dump are skipped, so every
// restored object belongs to the connecting user.
const loadDumpScript = `pg_restore -U "${POSTGRES_USER:-postgres}" --no-owner --no-acl --single-transaction --exit-on-error -d "$1"`

// databaseConnString names a database in libpq's key-value form, which takes
// the name literally even if it contains "=" or resembles a connection URI.
func databaseConnString(name string) string {
	return "dbname='" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(name) + "'"
}

// countRowsScript runs dumpCountSQL from stdin in the database named by $1.
const countRowsScript = `PGDATABASE="$1" psql -U "${POSTGRES_USER:-postgres}" -XAtq -v ON_ERROR_STOP=1`

func runServiceRestoreDump(cmd *cobra.Command, args []string) error {
	source := args[0]
	target, _ := cmd.Flags().GetString("to")
	backupName, _ := cmd.Flags().GetString("backup")
	if target == source {
		return errors.New("--to must name another service because dumps must be restored into a separate service")
	}
	namespace := resolveServiceNamespace(cmd)

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	clientset := k8sClient.Clientset()
	dyn := k8sClient.Dynamic()
	if !backupsConfiguredOn(ctx, dyn) {
		return errBackupsNotConfigured
	}

	kubeconfig, err := clientcmd.BuildConfigFromFlags("", cluster.Kubeconfig)
	if err != nil {
		return fmt.Errorf("loading kubeconfig: %w", err)
	}
	execClient, err := kubernetes.NewForConfig(k8s.WithoutPodSecurityWarnings(kubeconfig))
	if err != nil {
		return fmt.Errorf("creating the Kubernetes client: %w", err)
	}
	targetPod, err := readyTargetPod(ctx, clientset, dyn, namespace, target)
	if err != nil {
		return err
	}
	dst := podExec(execClient, kubeconfig, namespace, targetPod.Name, workloadContainerName(targetPod, target))
	if err := requireEmptyPostgres(ctx, dst, target); err != nil {
		return err
	}

	if err := refuseLeftoverDrill(ctx, clientset, namespace); err != nil {
		return err
	}
	backup, err := chooseDrillBackup(ctx, dyn, namespace, backupName)
	if err != nil {
		return err
	}
	fmt.Printf("\n  Restoring the dump of %s from backup %s (completed %s) into %s\n", source, backup.GetName(), completionTime(backup), target)

	if err := createDrillNamespace(ctx, clientset); err != nil {
		return err
	}
	var restoreName string
	defer func() {
		if err := finishDrill(context.Background(), clientset, dyn, restoreName, namespace); err != nil {
			fmt.Printf("  ⚠  %v\n", err)
		}
	}()
	pods, err := restoreIntoDrill(ctx, clientset, dyn, backup, namespace, &restoreName)
	if err != nil {
		return err
	}
	sourcePod := restoredServicePod(pods, source)
	if sourcePod == nil {
		return fmt.Errorf("backup %s contains no pod for service %s in namespace %s", backup.GetName(), source, namespace)
	}
	if sourcePod.Labels["kipper.run/service-type"] != "postgres" {
		return fmt.Errorf("service %s is %s. Only Postgres services have consistent dumps", source, sourcePod.Labels["kipper.run/service-type"])
	}
	if err := waitForPodRunning(ctx, clientset, sourcePod.Name, 5*time.Minute); err != nil {
		return err
	}
	src := podExec(execClient, kubeconfig, drillNamespace, sourcePod.Name, workloadContainerName(sourcePod, source))

	entries, err := readDumpManifest(ctx, src, source)
	if err != nil {
		return err
	}
	if err := verifyDumpChecksums(ctx, src, entries); err != nil {
		return err
	}
	// A resource-tuning restart would interrupt the restore.
	resume, err := pauseTuning(ctx, execClient, namespace, target)
	if err != nil {
		return fmt.Errorf("pausing the resource tuner of %s: %w", target, err)
	}
	defer resume()
	if err := loadDumps(ctx, src, dst, target, entries); err != nil {
		return err
	}
	fmt.Printf("\n  Restored %d %s into %s. Bind apps to it with kip service bind %s <app>.\n\n", len(entries), plural(len(entries), "database", "databases"), target, target)
	return nil
}

// readyTargetPod returns the target Postgres service's ready pod. Otherwise,
// its error includes any TLSReady failure message from the controller, such as
// a pending certificate or a pod restart needed to load it.
func readyTargetPod(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface, namespace, target string) (*corev1.Pod, error) {
	snapshot, err := (&service.Manager{Dynamic: dyn}).Read(ctx, namespace, target)
	if err != nil {
		return nil, fmt.Errorf("reading target service %s in %s: %w. Create it first with kip service add postgres --name %s", target, namespace, err, target)
	}
	if snapshot.Type != "postgres" {
		return nil, fmt.Errorf("target service %s is %s. A Postgres dump restores only into a Postgres service", target, snapshot.Type)
	}
	pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, target+"-0", metav1.GetOptions{})
	if err == nil && podReady(pod) {
		return pod, nil
	}
	if snapshot.TLSWaiting != "" {
		return nil, fmt.Errorf("target service %s is not serving TLS yet: %s. Try again once kip service info %s shows it running", target, strings.TrimSuffix(snapshot.TLSWaiting, "."), target)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the pod of target service %s: %w", target, err)
	}
	return nil, fmt.Errorf("target service %s is not ready yet. Try again once kip service info %s shows it running", target, target)
}

// loadDumps rechecks that the target has no user tables before loading the dumps,
// because an app may have been bound and created tables during the backup restore.
func loadDumps(ctx context.Context, src, dst podExecFunc, target string, entries []dumpEntry) error {
	if err := requireEmptyPostgres(ctx, dst, target); err != nil {
		return err
	}
	for _, e := range entries {
		tables, err := restoreDumpEntry(ctx, src, dst, e)
		if err != nil {
			return fmt.Errorf("database %s: %w. Databases listed above were restored. Delete and recreate %s before trying again", e.Database, err, target)
		}
		fmt.Printf("  ✔  %s: restored. Row counts match the dump's snapshot (%d %s)\n", e.Database, tables, plural(tables, "table", "tables"))
	}
	return nil
}

// restoredServicePod returns the first pod labelled with the service name,
// or nil if none matches.
func restoredServicePod(pods []corev1.Pod, service string) *corev1.Pod {
	for i := range pods {
		if pods[i].Labels["app"] == service {
			return &pods[i]
		}
	}
	return nil
}

// requireEmptyPostgres returns an error if a connectable, non-template database
// in the target has a user table or the check fails.
func requireEmptyPostgres(ctx context.Context, dst podExecFunc, target string) error {
	var stdout, stderr bytes.Buffer
	if err := dst(ctx, []string{"sh", "-c", postgresTablesScript}, nil, &stdout, &stderr); err != nil {
		return fmt.Errorf("checking that %s is empty: %w: %s", target, err, strings.TrimSpace(stderr.String()))
	}
	if used := strings.Fields(stdout.String()); len(used) > 0 {
		return fmt.Errorf("target service %s already holds tables in %s. Restore into a new, empty service", target, strings.Join(used, ", "))
	}
	return nil
}

func readDumpManifest(ctx context.Context, src podExecFunc, source string) ([]dumpEntry, error) {
	var stdout, stderr bytes.Buffer
	if err := src(ctx, []string{"cat", path.Join(drillDumpDir, "manifest")}, nil, &stdout, &stderr); err != nil {
		if strings.Contains(stderr.String(), "No such file") {
			return nil, fmt.Errorf("this backup contains no consistent dump of %s. Enable dumps with kip service update %s --consistent-backups, then restore from a backup taken afterwards", source, source)
		}
		return nil, fmt.Errorf("reading the dump manifest: %w: %s", err, lastLine(stderr.String()))
	}
	return parseDumpManifest(stdout.String())
}

// verifyDumpChecksums checks every dump file before anything is written to
// the target.
func verifyDumpChecksums(ctx context.Context, src podExecFunc, entries []dumpEntry) error {
	for _, e := range entries {
		var stdout, stderr bytes.Buffer
		if err := src(ctx, []string{"sha256sum", path.Join(drillDumpDir, e.File)}, nil, &stdout, &stderr); err != nil {
			return fmt.Errorf("reading the dump of %s: %w: %s", e.Database, err, strings.TrimSpace(stderr.String()))
		}
		if sum, _, _ := strings.Cut(stdout.String(), " "); sum != e.SHA256 {
			return fmt.Errorf("the dump of %s does not match its recorded checksum", e.Database)
		}
	}
	return nil
}

// restoreDumpEntry loads one database's dump into the target and compares
// its row counts with the counts recorded with the dump. It returns the
// number of tables compared.
func restoreDumpEntry(ctx context.Context, src, dst podExecFunc, e dumpEntry) (int, error) {
	var stderr bytes.Buffer
	if err := dst(ctx, []string{"sh", "-c", createDatabaseScript, "sh", e.Database}, nil, io.Discard, &stderr); err != nil {
		return 0, fmt.Errorf("creating the database: %w: %s", err, lastLine(stderr.String()))
	}

	reader, writer := io.Pipe()
	srcCtx, cancelSource := context.WithCancel(ctx)
	defer cancelSource()
	var readErr bytes.Buffer
	type sourceResult struct {
		err     error
		stopped bool
	}
	copied := make(chan sourceResult, 1)
	go func() {
		err := src(srcCtx, []string{"cat", path.Join(drillDumpDir, e.File)}, nil, writer, &readErr)
		copied <- sourceResult{err: err, stopped: srcCtx.Err() != nil}
		_ = writer.CloseWithError(err)
	}()
	stderr.Reset()
	loadErr := dst(ctx, []string{"sh", "-c", loadDumpScript, "sh", databaseConnString(e.Database)}, reader, io.Discard, &stderr)
	// Stop the source stream when the load ends so a failed load can return
	// without waiting for the rest of a large dump.
	cancelSource()
	_ = reader.CloseWithError(errors.New("restore ended"))
	if read := <-copied; loadErr == nil && read.err != nil && !read.stopped {
		return 0, fmt.Errorf("reading the dump: %w: %s", read.err, lastLine(readErr.String()))
	}
	var exitErr utilexec.ExitError
	switch {
	case loadErr != nil && errors.As(loadErr, &exitErr):
		return 0, fmt.Errorf("pg_restore failed and its transaction was rolled back: %s", lastLine(stderr.String()))
	case loadErr != nil:
		return 0, fmt.Errorf("the connection to the target failed during the load. Whether the database was restored is unknown: %w", loadErr)
	}

	var want, got bytes.Buffer
	stderr.Reset()
	countsFile := strings.TrimSuffix(e.File, ".dump") + ".counts"
	if err := src(ctx, []string{"cat", path.Join(drillDumpDir, countsFile)}, nil, &want, &stderr); err != nil {
		return 0, fmt.Errorf("the row counts recorded with the dump are missing: %s", lastLine(stderr.String()))
	}
	stderr.Reset()
	if err := dst(ctx, []string{"sh", "-c", countRowsScript, "sh", e.Database}, strings.NewReader(dumpCountSQL+"\n"), &got, &stderr); err != nil {
		return 0, fmt.Errorf("counting the restored rows: %w: %s", err, lastLine(stderr.String()))
	}
	wantLines, gotLines := sortedLines(want.String()), sortedLines(got.String())
	if strings.Join(wantLines, "\n") != strings.Join(gotLines, "\n") {
		return 0, errors.New("the restored row counts differ from the counts taken with the dump")
	}
	return len(wantLines), nil
}

func sortedLines(s string) []string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimRight(l, "\r"); l != "" {
			lines = append(lines, l)
		}
	}
	sort.Strings(lines)
	return lines
}
