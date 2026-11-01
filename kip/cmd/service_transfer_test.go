package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/controller/pkg/labels"
)

// TestPauseTuningRoundTrip pins the bulk-operation shield handshake: pausing
// stamps the StatefulSet with a bounded RFC 3339 lease and resuming removes
// the stamp, so the resource tuner cannot roll the database mid-import yet
// always recovers even if the CLI dies.
func TestPauseTuningRoundTrip(t *testing.T) {
	clientset := fake.NewSimpleClientset(&appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "mongodb", Namespace: "blog-test"},
	})

	resume, err := pauseTuning(context.Background(), clientset, "blog-test", "mongodb")
	if err != nil {
		t.Fatalf("pauseTuning: %v", err)
	}

	sts, err := clientset.AppsV1().StatefulSets("blog-test").Get(context.Background(), "mongodb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting statefulset: %v", err)
	}
	raw, ok := sts.Annotations[labels.AnnoTuningPausedUntil]
	if !ok {
		t.Fatal("expected the tuning-paused-until annotation to be set")
	}
	until, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("annotation is not RFC 3339: %q", raw)
	}
	if remaining := time.Until(until); remaining <= 0 || remaining > tuningPauseLease {
		t.Errorf("deadline %s is outside the pause lease", raw)
	}

	resume()
	sts, err = clientset.AppsV1().StatefulSets("blog-test").Get(context.Background(), "mongodb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting statefulset after resume: %v", err)
	}
	if _, ok := sts.Annotations[labels.AnnoTuningPausedUntil]; ok {
		t.Error("expected resume to remove the annotation")
	}
}

// TestPauseTuningResumeLeavesForeignStamp: when another transfer has
// overwritten the lease, this transfer's resume must leave it in place —
// clearing it would expose the still-running transfer to the tuner.
func TestPauseTuningResumeLeavesForeignStamp(t *testing.T) {
	clientset := fake.NewSimpleClientset(&appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "mongodb", Namespace: "blog-test"},
	})

	resume, err := pauseTuning(context.Background(), clientset, "blog-test", "mongodb")
	if err != nil {
		t.Fatalf("pauseTuning: %v", err)
	}

	// A second, later transfer overwrites the stamp with its own lease.
	foreign := time.Now().Add(tuningPauseLease + time.Minute).UTC().Format(time.RFC3339)
	sts, _ := clientset.AppsV1().StatefulSets("blog-test").Get(context.Background(), "mongodb", metav1.GetOptions{})
	sts.Annotations[labels.AnnoTuningPausedUntil] = foreign
	if _, err := clientset.AppsV1().StatefulSets("blog-test").Update(context.Background(), sts, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("simulating the overlapping transfer: %v", err)
	}

	resume()
	sts, err = clientset.AppsV1().StatefulSets("blog-test").Get(context.Background(), "mongodb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting statefulset after resume: %v", err)
	}
	if got := sts.Annotations[labels.AnnoTuningPausedUntil]; got != foreign {
		t.Errorf("resume cleared a foreign stamp: annotation = %q, want %q", got, foreign)
	}
}

// TestPauseTuningMissingStatefulSet: no StatefulSet means nothing the tuner
// would restart, so the pause is a clean no-op rather than an error.
func TestPauseTuningMissingStatefulSet(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	resume, err := pauseTuning(context.Background(), clientset, "blog-test", "ghost")
	if err != nil {
		t.Fatalf("expected a no-op for a missing statefulset, got %v", err)
	}
	resume()
}

// A dump holds the whole database, so only its owner may read it, and an
// export never replaces a file that is already there.
func TestTheExportFileIsPrivateAndNew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shop.sql")

	f, err := createExportFile(path)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	f, err = createExportFile(filepath.Join(filepath.Dir(path), "failed.sql"))
	require.NoError(t, err)
	assert.Equal(t, os.Stdout, f.progress())
	f.discard()
	_, err = os.Stat(filepath.Join(filepath.Dir(path), "failed.sql"))
	assert.True(t, os.IsNotExist(err), "a failed export removes the file it created")

	require.NoError(t, os.WriteFile(path, []byte("earlier dump"), 0o600))
	_, err = createExportFile(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	kept, _ := os.ReadFile(path)
	assert.Equal(t, "earlier dump", string(kept))
}

// A dump can still be piped on, for example with --file /dev/stdout.
func TestTheExportCanGoToADevice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /dev/null on Windows")
	}
	f, err := createExportFile(os.DevNull)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// A regular file swapped in after the device check must not receive the dump.
func TestTheExportRefusesARegularFileSwappedInForADevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.sql")
	require.NoError(t, os.WriteFile(path, []byte("earlier contents"), 0o600))

	_, err := openExportStream(path)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	got, _ := os.ReadFile(path)
	assert.Equal(t, "earlier contents", string(got))
}

// A stream export can write the dump to stdout, so deferred resume warnings
// go to stderr to keep the dump usable.
func TestAFailedTunerResumeWarnsOutsideTheDump(t *testing.T) {
	clientset := fake.NewSimpleClientset(&appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "mongodb", Namespace: "blog-test"},
	})
	resume, err := pauseTuning(context.Background(), clientset, "blog-test", "mongodb")
	require.NoError(t, err)
	clientset.PrependReactor("patch", "statefulsets", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("connection refused")
	})

	var stdout string
	stderr := captureStderr(t, func() { stdout = captureStdout(t, resume) })

	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "could not resume the resource tuner")
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	saved := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = saved
	require.NoError(t, w.Close())
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(out)
}
