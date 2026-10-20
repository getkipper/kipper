package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

const goodSum = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func manifestWith(lines ...string) string {
	return "format\tkipper-dump/1\nstarted\tx\n" + strings.Join(lines, "\n") + "\nfinished\tx\ncomplete\ttrue\n"
}

func TestADumpManifestNamesEachDatabase(t *testing.T) {
	entries, err := parseDumpManifest(manifestWith(
		"database\tapp\t1.dump\t0\t42\t"+goodSum,
		"database\tweird name\t2.dump\t0\t42\t"+goodSum,
	))
	require.NoError(t, err)
	assert.Equal(t, []dumpEntry{{"app", "1.dump", goodSum}, {"weird name", "2.dump", goodSum}}, entries)
}

func TestADumpManifestWithoutEvidenceIsRefused(t *testing.T) {
	for name, manifest := range map[string]string{
		"incomplete":   "format\tkipper-dump/1\ndatabase\tapp\t1.dump\t0\t42\t" + goodSum + "\ncomplete\tfalse\n",
		"failed dump":  manifestWith("database\tapp\t1.dump\t1\t42\t" + goodSum),
		"no checksum":  manifestWith("database\tapp\t1.dump\t0\t0\t"),
		"odd file":     manifestWith("database\tapp\t../x.dump\t0\t42\t" + goodSum),
		"no databases": manifestWith(),
		"other format": "format\tother/2\ncomplete\ttrue\n",
		"empty":        "",
	} {
		_, err := parseDumpManifest(manifest)
		assert.Error(t, err, name)
	}
}

func TestDatabaseNamesReachPgRestoreLiterally(t *testing.T) {
	assert.Equal(t, `dbname='app'`, databaseConnString("app"))
	assert.Equal(t, `dbname='host=evil dbname=x'`, databaseConnString("host=evil dbname=x"))
	assert.Equal(t, `dbname='it\'s \\ here'`, databaseConnString(`it's \ here`))
}

// fakePostgres simulates the drill pod (src) and the target pod (dst).
type fakePostgres struct {
	dump, counts, restoredCounts string
	loaded                       string
	loadErr, catErr              error
	created                      []string
}

func (f *fakePostgres) src(_ context.Context, command []string, _ io.Reader, stdout, stderr io.Writer) error {
	switch {
	case command[0] == "cat" && strings.HasSuffix(command[1], ".dump"):
		if f.catErr != nil {
			_, _ = io.WriteString(stderr, "read error")
			return f.catErr
		}
		_, _ = io.WriteString(stdout, f.dump)
	case command[0] == "cat" && strings.HasSuffix(command[1], ".counts"):
		_, _ = io.WriteString(stdout, f.counts)
	default:
		return fmt.Errorf("unexpected source command %v", command)
	}
	return nil
}

func (f *fakePostgres) dst(_ context.Context, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	switch command[2] {
	case createDatabaseScript:
		f.created = append(f.created, command[4])
	case loadDumpScript:
		b, _ := io.ReadAll(stdin)
		f.loaded = string(b)
		if f.loadErr != nil {
			_, _ = io.WriteString(stderr, "pg_restore: error: relation exists")
			return f.loadErr
		}
	case countRowsScript:
		_, _ = io.WriteString(stdout, f.restoredCounts)
	default:
		return fmt.Errorf("unexpected target command %v", command)
	}
	return nil
}

func TestADumpIsStreamedIntoTheTargetAndItsRowsCompared(t *testing.T) {
	f := &fakePostgres{dump: "PGDMP-bytes", counts: "public.b|2\npublic.a|1\n", restoredCounts: "public.a|1\npublic.b|2\n"}

	tables, err := restoreDumpEntry(context.Background(), f.src, f.dst, dumpEntry{"app", "1.dump", goodSum})

	require.NoError(t, err)
	assert.Equal(t, 2, tables)
	assert.Equal(t, "PGDMP-bytes", f.loaded)
	assert.Equal(t, []string{"app"}, f.created)
}

func TestDifferentRowCountsFailTheRestore(t *testing.T) {
	f := &fakePostgres{dump: "x", counts: "public.a|1\n", restoredCounts: "public.a|0\n"}
	_, err := restoreDumpEntry(context.Background(), f.src, f.dst, dumpEntry{"app", "1.dump", goodSum})
	assert.ErrorContains(t, err, "row counts differ")
}

func TestAFailedLoadIsReportedWithPgRestoresReason(t *testing.T) {
	f := &fakePostgres{dump: "x", loadErr: utilexec.CodeExitError{Err: errors.New("exit 1"), Code: 1}}
	_, err := restoreDumpEntry(context.Background(), f.src, f.dst, dumpEntry{"app", "1.dump", goodSum})
	assert.ErrorContains(t, err, "relation exists")
	assert.ErrorContains(t, err, "rolled back")
}

func TestALostConnectionNeverClaimsARollback(t *testing.T) {
	f := &fakePostgres{dump: "x", loadErr: errors.New("stream reset by peer")}
	_, err := restoreDumpEntry(context.Background(), f.src, f.dst, dumpEntry{"app", "1.dump", goodSum})
	assert.ErrorContains(t, err, "unknown")
	assert.NotContains(t, err.Error(), "rolled back")
}

func TestAFailedLoadStopsTheSourceStream(t *testing.T) {
	src := func(ctx context.Context, command []string, _ io.Reader, _, _ io.Writer) error {
		// Simulate a large dump that keeps streaming until cancelled.
		<-ctx.Done()
		return ctx.Err()
	}
	dst := func(_ context.Context, command []string, _ io.Reader, _, _ io.Writer) error {
		if command[2] == loadDumpScript {
			return utilexec.CodeExitError{Err: errors.New("exit 1"), Code: 1}
		}
		return nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := restoreDumpEntry(context.Background(), src, dst, dumpEntry{"app", "1.dump", goodSum})
		done <- err
	}()
	select {
	case err := <-done:
		assert.ErrorContains(t, err, "pg_restore failed")
	case <-time.After(5 * time.Second):
		t.Fatal("a failed load waited for the source stream to finish")
	}
}

func TestARestoreWhoseSubmissionFailedIsStillChecked(t *testing.T) {
	clientset := kubefake.NewClientset()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{restoreGVR: "RestoreList"})
	dyn.PrependReactor("create", "restores", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("timeout awaiting response")
	})
	backup := &unstructured.Unstructured{}
	backup.SetName("b1")
	var restoreName string

	_, err := restoreIntoDrill(context.Background(), clientset, dyn, backup, "blog-prod", "", &restoreName)

	require.Error(t, err)
	assert.NotEmpty(t, restoreName, "cleanup must ask Velero about a restore that may have been accepted")
}

func TestAFailedReadIsReportedEvenWhenTheLoadSucceeds(t *testing.T) {
	f := &fakePostgres{catErr: errors.New("stream reset"), counts: "", restoredCounts: ""}
	_, err := restoreDumpEntry(context.Background(), f.src, f.dst, dumpEntry{"app", "1.dump", goodSum})
	assert.ErrorContains(t, err, "reading the dump")
}

func TestAChecksumMismatchStopsBeforeTheTargetIsTouched(t *testing.T) {
	src := func(_ context.Context, command []string, _ io.Reader, stdout, _ io.Writer) error {
		_, _ = io.WriteString(stdout, strings.Repeat("b", 64)+"  "+command[1]+"\n")
		return nil
	}
	err := verifyDumpChecksums(context.Background(), src, []dumpEntry{{"app", "1.dump", goodSum}})
	assert.ErrorContains(t, err, "checksum")
}

func runTablesScript(t *testing.T, tables map[string]string) (string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the script runs in a Linux container")
	}
	bin := t.TempDir()
	var cases strings.Builder
	for db, n := range tables {
		fmt.Fprintf(&cases, "  '%s') echo %s;;\n", db, n)
	}
	psql := "#!/bin/sh\ncase \"$*\" in *pg_database*) printf '%s\\n' app 'my db'; exit 0;; esac\ncase \"$PGDATABASE\" in\n" + cases.String() + "  *) exit 2;;\nesac\n"
	//nolint:gosec // G306: the stub stands in for psql on PATH, so it has to be executable.
	require.NoError(t, os.WriteFile(filepath.Join(bin, "psql"), []byte(psql), 0o700))
	cmd := exec.Command("sh", "-c", postgresTablesScript) //nolint:gosec // This test executes a fixed script with a stubbed tool.
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.Output()
	return string(out), err
}

func TestTheEmptinessCheckNamesDatabasesWithTables(t *testing.T) {
	out, err := runTablesScript(t, map[string]string{"app": "0", "my db": "3"})
	require.NoError(t, err)
	assert.Equal(t, "my db\n", out)

	out, err = runTablesScript(t, map[string]string{"app": "0", "my db": "0"})
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestTheEmptinessCheckFailsWhenADatabaseCannotBeCounted(t *testing.T) {
	_, err := runTablesScript(t, map[string]string{"app": "0"})
	assert.Error(t, err)
}

func restoreDumpTarget(conditions ...interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": manifest.ServiceGVR.GroupVersion().String(),
		"kind":       "Service",
		"metadata":   map[string]interface{}{"name": "db2", "namespace": "shop-prod"},
		"spec":       map[string]interface{}{"type": "postgres", "tls": "required"},
		"status":     map[string]interface{}{"conditions": conditions},
	}}
}

func restoreDumpClients(cr *unstructured.Unstructured, pods ...k8sruntime.Object) (*kubefake.Clientset, *dynamicfake.FakeDynamicClient) {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(),
		map[schema.GroupVersionResource]string{manifest.ServiceGVR: "ServiceList"}, cr)
	return kubefake.NewClientset(pods...), dyn
}

func TestATargetWaitingForItsCertificateSaysSo(t *testing.T) {
	clientset, dyn := restoreDumpClients(restoreDumpTarget(map[string]interface{}{
		"type": "TLSReady", "status": "False", "reason": "Issuing",
		"message": "Postgres requires TLS, so startup and changes are paused until cert-manager issues its certificate",
	}))

	_, err := readyTargetPod(context.Background(), clientset, dyn, "shop-prod", "db2")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "db2 is not serving TLS yet")
	assert.Contains(t, err.Error(), "until cert-manager issues its certificate")
	assert.NotContains(t, err.Error(), "not found")
}

func TestATargetRestartingToLoadItsCertificateSaysSo(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "db2-0", Namespace: "shop-prod"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	clientset, dyn := restoreDumpClients(restoreDumpTarget(map[string]interface{}{
		"type": "TLSReady", "status": "False", "reason": "RollingOut",
		"message": "The certificate has been issued. Once its pod restarts, Postgres will serve it using the service's configured TLS mode",
	}), pod)

	_, err := readyTargetPod(context.Background(), clientset, dyn, "shop-prod", "db2")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "db2 is not serving TLS yet: The certificate has been issued")
}

func TestATargetWithoutAPodAndNoTLSWaitReportsTheMissingPod(t *testing.T) {
	clientset, dyn := restoreDumpClients(restoreDumpTarget())

	_, err := readyTargetPod(context.Background(), clientset, dyn, "shop-prod", "db2")

	assert.ErrorContains(t, err, "reading the pod of target service db2")
}

func TestAReadyTargetPodIsReturned(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "db2-0", Namespace: "shop-prod"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "db2", Ready: true}}},
	}
	clientset, dyn := restoreDumpClients(restoreDumpTarget(), pod)

	got, err := readyTargetPod(context.Background(), clientset, dyn, "shop-prod", "db2")

	require.NoError(t, err)
	assert.Equal(t, "db2-0", got.Name)
}

func TestATargetOfAnotherTypeIsRefused(t *testing.T) {
	cr := restoreDumpTarget()
	cr.Object["spec"] = map[string]interface{}{"type": "mysql"}
	clientset, dyn := restoreDumpClients(cr)

	_, err := readyTargetPod(context.Background(), clientset, dyn, "shop-prod", "db2")

	assert.ErrorContains(t, err, "target service db2 is mysql")
}

func TestATargetThatGainedTablesIsRefusedBeforeTheLoad(t *testing.T) {
	f := &fakePostgres{dump: "x", counts: "public.a|1\n", restoredCounts: "public.a|1\n"}
	dst := func(ctx context.Context, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
		if command[2] == postgresTablesScript {
			_, _ = io.WriteString(stdout, "app\n")
			return nil
		}
		return f.dst(ctx, command, stdin, stdout, stderr)
	}

	err := loadDumps(context.Background(), f.src, dst, "db2", []dumpEntry{{"app", "1.dump", goodSum}})

	assert.ErrorContains(t, err, "target service db2 already holds tables in app. Restore into a new, empty service")
	assert.Empty(t, f.created, "nothing may be written to a target that is no longer empty")
	assert.Empty(t, f.loaded)
}

func TestAnEmptyTargetIsLoaded(t *testing.T) {
	f := &fakePostgres{dump: "x", counts: "public.a|1\n", restoredCounts: "public.a|1\n"}
	dst := func(ctx context.Context, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
		if command[2] == postgresTablesScript {
			return nil
		}
		return f.dst(ctx, command, stdin, stdout, stderr)
	}

	err := loadDumps(context.Background(), f.src, dst, "db2", []dumpEntry{{"app", "1.dump", goodSum}})

	require.NoError(t, err)
	assert.Equal(t, "x", f.loaded)
}
