package mirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type rcloneCall struct {
	args []string
	env  []string
}

// fakeRclone records its calls and writes the given lines to stderr, one
// slice per call.
type fakeRclone struct {
	calls  []rcloneCall
	output [][]string
	errs   []error
}

func (f *fakeRclone) run(_ context.Context, args, env []string, stderr io.Writer) error {
	i := len(f.calls)
	f.calls = append(f.calls, rcloneCall{args: args, env: env})
	if i < len(f.output) {
		for _, line := range f.output[i] {
			_, _ = fmt.Fprintln(stderr, line)
		}
	}
	if i < len(f.errs) {
		return f.errs[i]
	}
	return nil
}

const finalStats = `{"level":"notice","msg":"1.2 MiB / 1.2 MiB, 100%","source":"accounting/stats.go:549","stats":{"bytes":1258291,"checks":1200,"deletes":2,"elapsedTime":12.5,"errors":0,"transfers":12,"totalBytes":1258291},"time":"2026-10-20T03:00:12Z"}`

func runMirror(t *testing.T, spec Spec, source, target Store, rc *fakeRclone) (Summary, string, error) {
	t.Helper()
	var log bytes.Buffer
	sum, err := Run(context.Background(), spec, source, target, rc.run, &log, testNow)
	return sum, log.String(), err
}

func TestRunSyncsThenChecks(t *testing.T) {
	rc := &fakeRclone{output: [][]string{{`{"level":"notice","msg":"progress","stats":{"bytes":1}}`, "not json", finalStats}}}
	sum, log, err := runMirror(t, validSpec(), readOnlySource(), lockedTarget(), rc)
	require.NoError(t, err)

	require.Len(t, rc.calls, 2)
	assert.Equal(t, []string{
		"sync", "src:app", "dst:app-backup", "--checksum", "--fast-list", "--s3-no-check-bucket",
		"--exclude", CanaryFilter, "--transfers", "8", "--checkers", "16", "--retries", "5",
		"--low-level-retries", "20", "--use-json-log", "--stats-log-level", "NOTICE",
		"--stats-one-line", "--stats", "5m",
	}, rc.calls[0].args)
	assert.Equal(t, []string{
		"check", "src:app", "dst:app-backup", "--one-way", "--checksum", "--fast-list",
		"--exclude", CanaryFilter, "--use-json-log",
	}, rc.calls[1].args)

	env := strings.Join(rc.calls[0].env, "\n")
	assert.Equal(t, "RCLONE_CONFIG=", rc.calls[0].env[0], "rclone reads no configuration file")
	assert.NotContains(t, env, "RCLONE_CONFIG_SRC_OBJECT_LOCK")
	for _, want := range []string{
		"RCLONE_CONFIG_DST_OBJECT_LOCK_MODE=COMPLIANCE", "RCLONE_CONFIG_DST_OBJECT_LOCK_RETAIN_UNTIL_DATE=30d",
		"RCLONE_CONFIG_DST_UPLOAD_CUTOFF=16M",
		"RCLONE_CONFIG_SRC_TYPE=s3", "RCLONE_CONFIG_SRC_PROVIDER=Other", "RCLONE_CONFIG_SRC_ENV_AUTH=false",
		"RCLONE_CONFIG_SRC_ENDPOINT=https://src.example.com", "RCLONE_CONFIG_SRC_REGION=r1",
		"RCLONE_CONFIG_SRC_ACCESS_KEY_ID=AK1", "RCLONE_CONFIG_SRC_SECRET_ACCESS_KEY=SK1",
		"RCLONE_CONFIG_DST_ENDPOINT=https://dst.example.com", "RCLONE_CONFIG_DST_REGION=r2",
		"RCLONE_CONFIG_DST_ACCESS_KEY_ID=AK2", "RCLONE_CONFIG_DST_SECRET_ACCESS_KEY=SK2",
	} {
		assert.Contains(t, env, want)
	}

	assert.Equal(t, "passed", sum.Preflight)
	require.NotNil(t, sum.Sync)
	assert.Equal(t, Stats{Bytes: 1258291, Checks: 1200, Transfers: 12, Deletes: 2, ElapsedSeconds: 12.5}, *sum.Sync)
	assert.Equal(t, "passed", sum.Check)
	assert.Equal(t, testNow, sum.Started)
	assert.Contains(t, log, "not json", "rclone's output is passed through to the job log")
	assert.Contains(t, log, "canary delete refused")
	assert.NotContains(t, log, "SK1")
}

func TestRunPassesTheMaxDelete(t *testing.T) {
	spec := validSpec()
	spec.MaxDelete = 0
	rc := &fakeRclone{}
	_, _, err := runMirror(t, spec, readOnlySource(), lockedTarget(), rc)
	require.NoError(t, err)
	assert.Equal(t, []string{"--max-delete", "0"}, rc.calls[0].args[len(rc.calls[0].args)-2:])
}

func TestRunStopsWhenThePreflightFails(t *testing.T) {
	target := lockedTarget()
	target.lock.Mode = "GOVERNANCE"
	rc := &fakeRclone{}
	sum, _, err := runMirror(t, validSpec(), readOnlySource(), target, rc)

	require.Error(t, err)
	assert.Empty(t, rc.calls, "nothing is copied into an unprotected target")
	assert.True(t, strings.HasPrefix(sum.Preflight, "failed: target object lock: "), sum.Preflight)
	assert.Nil(t, sum.Sync)
}

func TestRunReportsAFailedSync(t *testing.T) {
	rc := &fakeRclone{
		output: [][]string{{
			`{"level":"error","msg":"Got fatal error on delete: --max-delete threshold reached","object":"docs/a.txt"}`,
			`{"level":"error","msg":"Fatal error received - not attempting retries"}`,
			finalStats,
		}},
		errs: []error{errors.New("exit status 7")},
	}
	sum, _, err := runMirror(t, validSpec(), readOnlySource(), lockedTarget(), rc)

	require.Error(t, err)
	assert.Len(t, rc.calls, 1, "no check after a failed sync")
	assert.Equal(t, "exit status 7: Got fatal error on delete: --max-delete threshold reached (last error: Fatal error received - not attempting retries)", sum.SyncError)
	require.NotNil(t, sum.Sync)
	assert.Equal(t, int64(2), sum.Sync.Deletes)
}

type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitError) ExitCode() int { return int(e) }

func TestRunMarksAFatalRcloneErrorAsFatal(t *testing.T) {
	rc := &fakeRclone{errs: []error{exitError(7)}}
	_, _, err := runMirror(t, validSpec(), readOnlySource(), lockedTarget(), rc)
	require.ErrorIs(t, err, ErrFatal, "a --max-delete breach must not be retried")

	rc = &fakeRclone{errs: []error{exitError(1)}}
	_, _, err = runMirror(t, validSpec(), readOnlySource(), lockedTarget(), rc)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrFatal))

	rc = &fakeRclone{errs: []error{nil, exitError(7)}}
	_, _, err = runMirror(t, validSpec(), readOnlySource(), lockedTarget(), rc)
	require.ErrorIs(t, err, ErrFatal)
}

func TestRunReportsAFailedCheck(t *testing.T) {
	rc := &fakeRclone{
		output: [][]string{nil, {`{"level":"error","msg":"report.pdf: file not in dst:app-backup"}`, `{"level":"error","msg":"1 differences found"}`}},
		errs:   []error{nil, errors.New("exit status 1")},
	}
	sum, _, err := runMirror(t, validSpec(), readOnlySource(), lockedTarget(), rc)

	require.Error(t, err)
	assert.Equal(t, "failed: exit status 1: report.pdf: file not in dst:app-backup (last error: 1 differences found)", sum.Check)

	rc = &fakeRclone{
		output: [][]string{nil, {`{"level":"error","msg":"2 differences found"}`}},
		errs:   []error{nil, errors.New("exit status 1")},
	}
	sum, _, _ = runMirror(t, validSpec(), readOnlySource(), lockedTarget(), rc)
	assert.Equal(t, "failed: exit status 1: 2 differences found", sum.Check)

	rc = &fakeRclone{errs: []error{nil, errors.New("exit status 1")}}
	sum, _, _ = runMirror(t, validSpec(), readOnlySource(), lockedTarget(), rc)
	assert.Equal(t, "failed: exit status 1", sum.Check)
}

func TestSummaryFitsATerminationMessage(t *testing.T) {
	sum := Summary{Preflight: "failed: x: " + strings.Repeat("y", 10000), SyncError: strings.Repeat("z", 10000)}
	data := sum.Encode()
	assert.LessOrEqual(t, len(data), 4096)

	back, err := DecodeSummary(data)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(back.Preflight, "failed: x: yyy"))
}
