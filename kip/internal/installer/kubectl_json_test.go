package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// exitCodeError stands in for *exec.ExitError, which the SSH client wraps.
type exitCodeError struct{ code int }

func (e exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e exitCodeError) ExitCode() int { return e.code }

// outputInErrorRunner fails every command the way the SSH client does: the error
// wraps the process's exit error and its text carries the command's combined output.
// A zero code stands for kubectl's exit status 1.
type outputInErrorRunner struct {
	out  string
	code int
}

func (r outputInErrorRunner) Run(string) (string, error) {
	code := r.code
	if code == 0 {
		code = 1
	}
	return r.out, fmt.Errorf("running \"kubectl get pods\" on server: %w: %s", exitCodeError{code: code}, r.out)
}

func (r outputInErrorRunner) RunStdin(command string, _ io.Reader) (string, error) {
	return r.Run(command)
}

func TestAFailedJSONListKeepsItsOutputOutOfTheError(t *testing.T) {
	partial := `{"items":[{"spec":{"containers":[{"env":[{"name":"TOKEN","value":"marker-7f3a"}]}]}}`
	var pods struct{}
	err := kubectlJSON(outputInErrorRunner{out: partial}, "the pods", "kubectl get pods -A -o json", &pods)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "marker-7f3a")
	assert.Contains(t, err.Error(), "listing the pods failed")
	assert.Contains(t, err.Error(), "may hold secrets")
}

// TestAFailedJSONListSaysTheSSHConnectionFailed covers exit status 255, which
// ssh uses for its own failures, and reports it without the output.
func TestAFailedJSONListSaysTheSSHConnectionFailed(t *testing.T) {
	var pods struct{}
	err := kubectlJSON(outputInErrorRunner{out: "ssh: connect to host 203.0.113.10 port 22: Connection refused marker-5e2b\n", code: 255}, "the pods", "kubectl get pods -A -o json", &pods)
	require.Error(t, err)
	assert.Equal(t, "listing the pods failed: the ssh connection failed", err.Error())
}

func TestAFailedJSONListNamesAnotherExitStatus(t *testing.T) {
	err := failedRead("listing the pods", "partial marker-5e2b", fmt.Errorf("running: %w", exitCodeError{code: 2}))
	assert.Equal(t, "listing the pods failed with exit status 2: the command wrote 19 bytes, which are not shown because they may hold secrets", err.Error())
}

// TestAFailedReadSaysItTimedOut covers a command the SSH client stopped at its
// CommandTimeout, whose error wraps context.DeadlineExceeded.
func TestAFailedReadSaysItTimedOut(t *testing.T) {
	cause := fmt.Errorf("ssh command did not finish within 1s: %w", errors.Join(exitCodeError{code: -1}, context.DeadlineExceeded))
	err := failedRead("listing the pods", `{"items":[{"metadata":{"name":"marker-5e2b"`, cause)
	assert.Equal(t, "listing the pods timed out", err.Error())
}

func TestAFailedJSONListNamesWhatKubectlReported(t *testing.T) {
	for out, want := range map[string]string{
		"Warning: something\nThe connection to the server 127.0.0.1:6443 was refused - did you specify the right host or port?\n": "kubectl could not reach the API server",
		`Error from server (Forbidden): pods is forbidden: User "example" cannot list resource "pods"`:                            "the API server answered Forbidden",
		`error: the server doesn't have a resource type "backupstoragelocations"`:                                                 "the API server does not serve this resource type",
		"error: You must be logged in to the server (Unauthorized)\n":                                                             "kubectl is not logged in to the API server",
	} {
		var pods struct{}
		err := kubectlJSON(outputInErrorRunner{out: out}, "the pods", "kubectl get pods -A -o json", &pods)
		require.Error(t, err)
		assert.Contains(t, err.Error(), want)
	}
}

// TestAFailedReadNeverRepeatsALineWithAKnownPrefix covers object data that looks
// like a kubectl error line, such as a multiline value printed by jsonpath.
func TestAFailedReadNeverRepeatsALineWithAKnownPrefix(t *testing.T) {
	for _, out := range []string{
		"bsl[https://s3.example.com\nerror: https://key:markerSecret@s3.example.com]",
		"Error from server (NotFound) markerSecret",
		"The connection to the server markerSecret",
		"Error from server (markerSecret): denied",
		`error: the server doesn't have a resource type "markerSecret"`,
		"error: You must be logged in to the server (markerSecret)",
	} {
		err := failedRead("reading Velero's backup location", out, exitCodeError{code: 1})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "markerSecret", out)
	}
}

func TestAFailedPodReadKeepsItsOutputOutOfTheError(t *testing.T) {
	partial := `{"spec":{"containers":[{"env":[{"name":"TOKEN","value":"marker-7f3a"}]}]`
	_, _, err := PodPlacement(outputInErrorRunner{out: partial}, "app", "web-0")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "marker-7f3a")
	assert.Contains(t, err.Error(), "reading pod app/web-0 failed")
}

func TestAFailedJSONListWithoutOutputKeepsItsCause(t *testing.T) {
	var pods struct{}
	err := kubectlJSON(&drainHost{failures: map[string]string{"get pods": ""}}, "the pods", "kubectl get pods -A -o json", &pods)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit status 1")
}

// TestAFailedBackupLocationReadKeepsItsOutputOutOfTheError covers credentials
// embedded in s3Url when a backup location is configured by hand.
func TestAFailedBackupLocationReadKeepsItsOutputOutOfTheError(t *testing.T) {
	const location = "bsl[https://key:marker-9c1d@s3.example.com" //nolint:gosec // a test value, not a credential
	for name, runner := range map[string]CommandRunner{
		"a failed read":     outputInErrorRunner{out: location},
		"unexpected output": &scriptedRunner{out: "Warning: marker-9c1d"},
		"an unclosed value": &scriptedRunner{out: location},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := veleroWritesExternally(runner)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "marker-9c1d")
			assert.Contains(t, err.Error(), "reading Velero's backup location")
		})
	}
}

func TestAFailedReadWithoutOutputWrapsItsCause(t *testing.T) {
	cause := errors.New("connection reset")
	err := failedRead("reading the thing", "", cause)
	assert.ErrorIs(t, err, cause)
}

// failingCommands fails commands containing one of its keys, using the value
// as output and repeating it in the error like the SSH client. Other commands
// return "crd/backupstoragelocations.velero.io" successfully.
type failingCommands map[string]string

func (f failingCommands) Run(command string) (string, error) {
	for part, out := range f {
		if strings.Contains(command, part) {
			return out, fmt.Errorf("running %q on server: %w: %s", command, exitCodeError{code: 1}, out)
		}
	}
	return "crd/backupstoragelocations.velero.io", nil
}

func (f failingCommands) RunStdin(command string, _ io.Reader) (string, error) {
	return f.Run(command)
}

// TestOtherFailedReadsKeepTheirOutputOutOfTheError covers the remaining reads
// whose output can hold object data or a location's s3Url.
func TestOtherFailedReadsKeepTheirOutputOutOfTheError(t *testing.T) {
	const out = "partial https://key:marker-3d8a@s3.example.com" //nolint:gosec // a test value, not a credential
	runner := failingCommands{"get backupstoragelocation": out, "get pod": out, "get backups": out}
	move := KubectlMoveCluster{Server: runner}
	reads := map[string]func() error{
		"the backup location":   func() error { _, err := liveBackupLocation(runner); return err },
		"Velero's chart values": func() error { _, err := EnsureVeleroCoversNodes(failingCommands{"get helmchart": out}); return err },
		"the storage record":    func() error { _, err := readStorageRecord(failingCommands{"get configmap": out}); return err },
		"the access record":     func() error { _, _, err := ReadAccessRecord(failingCommands{"get configmap": out}); return err },
		"the node records":      func() error { _, err := ReadNodeRecords(failingCommands{"get configmap": out}); return err },
		"the nodes":             func() error { _, err := buildEgressExcepts(failingCommands{"get nodes": out}); return err },
		"a fenced node":         func() error { _, err := ReadFenceNode(failingCommands{"get node": out}, "worker-1"); return err },
		"one object": func() error {
			var pod struct{}
			_, err := move.getJSON("the pod", "kubectl -n app get pod web-0", &pod)
			return err
		},
		"a list": func() error { _, err := move.list("the backups", "kubectl -n velero get backups"); return err },
		"a Velero list": func() error {
			var backups []VeleroBackup
			return (&retainedCopyReader{cluster: move}).veleroList("the backups", "kubectl -n velero get backups", &backups)
		},
		"the restore helper settings": func() error { return configureRestoreHelper(failingCommands{"get configmap": out}) },
		"Velero's work in flight":     func() error { _, err := backupWorkInFlight(failingCommands{"get backups.velero.io": out}); return err },
		"the Velero HelmChart":        func() error { return checkVeleroChartInputs(failingCommands{"get helmchart velero": out}) },
		"Longhorn's state":            func() error { _, err := readLonghornSnapshot(failingCommands{"longhorn-system": out}); return err },
		"the global manager's lease": func() error {
			_, problem := globalLeaderProblem(failingCommands{"leases.coordination.k8s.io": out}, "")
			return errors.New(problem)
		},
		"the server nodes": func() error { _, err := ServerNodeNames(failingCommands{"get nodes": out}); return err },
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			err := read()
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "marker-3d8a")
			assert.Contains(t, err.Error(), "may hold secrets")
		})
	}
}

// TestFailedLogReadsKeepTheirOutputOutOfTheResult covers log reads whose
// callers filter or trim the text before showing it, so a partial read must not
// bypass them through the runner error.
func TestFailedLogReadsKeepTheirOutputOutOfTheResult(t *testing.T) {
	const partial = "KIPPER_INTERNAL partial marker-9c41 kipper-move-copied"
	failed := fmt.Errorf("running %q on server: %w: %s", "kubectl logs", exitCodeError{code: 1}, partial)

	t.Run("a Job's logs", func(t *testing.T) {
		client := &routedRunner{routes: []route{{contains: "get job", out: "1/"}, {contains: "logs job/", out: partial, err: failed}}}
		_, err := waitForJobLogs(client, "kipper-bucket-cleanup", "the bucket cleanup", time.Second)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "marker-9c41")
	})
	t.Run("a copy pod's log", func(t *testing.T) {
		server := &routedRunner{routes: []route{
			{contains: "get pod", out: `{"metadata":{"uid":"u-1"},"status":{"phase":"Succeeded"}}`},
			{contains: " logs ", out: partial, err: failed},
		}}
		result, err := KubectlMoveCluster{Server: server}.CopyResult("app", "kipper-move-copy")
		require.NoError(t, err)
		assert.NotContains(t, result.Log, "marker-9c41")
		assert.False(t, result.Succeeded, "a log that could not be read never proves the copy")
	})
	t.Run("the backup volume size", func(t *testing.T) {
		err := installInClusterMinIO(failingCommands{"minio-storage": partial}, "pw", "longhorn")
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "marker-9c41")
	})
}
