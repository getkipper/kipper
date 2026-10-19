package mirror

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// ErrFatal marks a run that retrying cannot fix, such as a sync stopped by
// --max-delete. The Job ends on FatalExitCode instead of retrying.
var ErrFatal = errors.New("fatal")

// FatalExitCode is rclone's exit code for a fatal error, which the mirror
// passes on.
const FatalExitCode = 7

// Runner runs rclone with args and extra environment, sending its log
// output to stderr.
type Runner func(ctx context.Context, args, env []string, stderr io.Writer) error

// Stats are the totals rclone reports at the end of a sync.
type Stats struct {
	Bytes          int64   `json:"bytes"`
	Checks         int64   `json:"checks"`
	Transfers      int64   `json:"transfers"`
	Deletes        int64   `json:"deletes"`
	Errors         int64   `json:"errors"`
	ElapsedSeconds float64 `json:"elapsedTime"`
}

// Summary is the outcome of one run. The job writes it as its termination
// message, where status reads it back.
type Summary struct {
	Started   time.Time `json:"started"`
	Preflight string    `json:"preflight"`
	Sync      *Stats    `json:"sync,omitempty"`
	SyncError string    `json:"syncError,omitempty"`
	Check     string    `json:"check,omitempty"`
}

// summaryFieldLimit bounds each message before JSON encoding to reduce
// truncation by Kubernetes' 4096-byte termination-message limit.
const summaryFieldLimit = 1000

// Encode returns JSON, truncating messages longer than 1000 bytes and adding
// an ellipsis. JSON escaping can still exceed the termination-message limit.
func (s Summary) Encode() []byte {
	s.Preflight = shorten(s.Preflight)
	s.SyncError = shorten(s.SyncError)
	s.Check = shorten(s.Check)
	data, _ := json.Marshal(s)
	return data
}

// DecodeSummary parses a termination message written by Encode.
func DecodeSummary(data []byte) (Summary, error) {
	var s Summary
	err := json.Unmarshal(data, &s)
	return s, err
}

func shorten(s string) string {
	if len(s) <= summaryFieldLimit {
		return s
	}
	return s[:summaryFieldLimit] + "..."
}

// Run runs the preflight and, only when it passes, an rclone sync followed
// by an rclone check. It writes progress to log and returns the summary
// together with the first failure. A failed sync skips the check.
// Rclone errors with FatalExitCode wrap ErrFatal to prevent Job retries.
func Run(ctx context.Context, spec Spec, source, target Store, rclone Runner, log io.Writer, now time.Time) (Summary, error) {
	sum := Summary{Started: now}

	res := Preflight(ctx, spec, source, target, now)
	for _, c := range res.Checks {
		mark := "✔"
		if !c.Passed {
			mark = "✖"
		}
		_, _ = fmt.Fprintf(log, "preflight %s %s: %s\n", mark, c.Name, c.Detail)
	}
	if !res.Passed() {
		f := res.Failure()
		sum.Preflight = "failed: " + f.Name + ": " + f.Detail
		return sum, fmt.Errorf("preflight failed at %s: %s", f.Name, f.Detail)
	}
	sum.Preflight = "passed"

	env := RemoteEnv(spec, res.LockDays)
	sync := &logWriter{out: log}
	err := rclone(ctx, SyncArgs(spec), env, sync)
	sync.flush()
	sum.Sync = sync.stats
	if sum.Sync == nil {
		sum.Sync = &Stats{}
	}
	if err != nil {
		sum.SyncError = sync.describe(err)
		return sum, fmt.Errorf("rclone sync: %w", fatal(err))
	}

	check := &logWriter{out: log}
	err = rclone(ctx, CheckArgs(spec), env, check)
	check.flush()
	if err != nil {
		sum.Check = "failed: " + check.describe(err)
		return sum, fmt.Errorf("rclone check: %w", fatal(err))
	}
	sum.Check = "passed"
	return sum, nil
}

// fatal adds ErrFatal to an rclone error that exited with FatalExitCode.
func fatal(err error) error {
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) && exit.ExitCode() == FatalExitCode {
		return fmt.Errorf("%w: %w", ErrFatal, err)
	}
	return err
}

// SyncArgs returns the rclone sync command line. The canary is excluded on
// both sides, so it is neither copied nor deleted.
func SyncArgs(spec Spec) []string {
	args := []string{
		"sync", "src:" + spec.Source.Bucket, "dst:" + spec.Target.Bucket,
		"--checksum", "--fast-list", "--s3-no-check-bucket", "--exclude", CanaryFilter,
		"--transfers", "8", "--checkers", "16", "--retries", "5", "--low-level-retries", "20",
		"--use-json-log", "--stats-log-level", "NOTICE", "--stats-one-line", "--stats", "5m",
	}
	if spec.MaxDelete >= 0 {
		args = append(args, "--max-delete", strconv.Itoa(spec.MaxDelete))
	}
	return args
}

// CheckArgs returns a one-way rclone check command, excluding the canary
// prefix. It compares checksums where available and otherwise compares sizes.
func CheckArgs(spec Spec) []string {
	return []string{
		"check", "src:" + spec.Source.Bucket, "dst:" + spec.Target.Bucket,
		"--one-way", "--checksum", "--fast-list", "--exclude", CanaryFilter, "--use-json-log",
	}
}

// RemoteEnv defines the rclone remotes src and dst through environment
// variables, so no configuration file holding the keys is written. An empty
// RCLONE_CONFIG keeps rclone's configuration in memory.
//
// Uploads to dst carry an explicit compliance lock of lockDays days, using
// Preflight's retention value. This makes rclone compute Content-MD5 even
// when the source has no checksum, as required for locked uploads. Rclone
// buffers these single-part uploads in memory, so a 16 MiB cutoff sends
// larger objects as multipart uploads.
func RemoteEnv(spec Spec, lockDays int) []string {
	env := []string{"RCLONE_CONFIG="}
	for _, side := range []struct {
		name string
		r    Remote
	}{{"SRC", spec.Source}, {"DST", spec.Target}} {
		prefix := "RCLONE_CONFIG_" + side.name + "_"
		env = append(env,
			prefix+"TYPE=s3",
			prefix+"PROVIDER=Other",
			prefix+"ENV_AUTH=false",
			prefix+"ENDPOINT=https://"+side.r.Endpoint,
			prefix+"REGION="+side.r.Region,
			prefix+"ACCESS_KEY_ID="+side.r.AccessKeyID,
			prefix+"SECRET_ACCESS_KEY="+side.r.SecretAccessKey,
		)
	}
	return append(env,
		"RCLONE_CONFIG_DST_OBJECT_LOCK_MODE=COMPLIANCE",
		"RCLONE_CONFIG_DST_OBJECT_LOCK_RETAIN_UNTIL_DATE="+strconv.Itoa(lockDays)+"d",
		"RCLONE_CONFIG_DST_UPLOAD_CUTOFF=16M",
	)
}

// logWriter passes rclone's JSON log through. It keeps the stats of the
// last line that carried them, and the first and last error messages.
type logWriter struct {
	out        io.Writer
	pending    []byte
	stats      *Stats
	firstError string
	lastError  string
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.pending = append(w.pending, p...)
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			break
		}
		w.line(w.pending[:i+1])
		w.pending = w.pending[i+1:]
	}
	return len(p), nil
}

// describe joins err with rclone's logged errors. The first one usually names
// the cause and the last one sums up the run.
func (w *logWriter) describe(err error) string {
	switch {
	case w.firstError == "":
		return err.Error()
	case w.lastError == w.firstError:
		return err.Error() + ": " + w.firstError
	}
	return err.Error() + ": " + w.firstError + " (last error: " + w.lastError + ")"
}

func (w *logWriter) flush() {
	if len(w.pending) > 0 {
		w.line(append(w.pending, '\n'))
		w.pending = nil
	}
}

func (w *logWriter) line(line []byte) {
	_, _ = w.out.Write(line)
	var entry struct {
		Level string `json:"level"`
		Msg   string `json:"msg"`
		Stats *Stats `json:"stats"`
	}
	if json.Unmarshal(line, &entry) != nil {
		return
	}
	if entry.Stats != nil {
		w.stats = entry.Stats
	}
	if entry.Level == "error" || entry.Level == "critical" {
		w.lastError = strings.TrimSpace(entry.Msg)
		if w.firstError == "" {
			w.firstError = w.lastError
		}
	}
}
