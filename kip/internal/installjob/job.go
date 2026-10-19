package installjob

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/getkipper/kipper/kip/internal/config"
)

// DefaultRoot is where install jobs live on the server, readable only by root.
const DefaultRoot = "/root/.kip/jobs"

// EndMarker starts the last line Follow prints, which carries the offset the
// stream reached and the job's phase.
const EndMarker = "--- kipper install end"

const (
	PhaseStarting    = "starting"
	PhaseRunning     = "running"
	PhaseSucceeded   = "succeeded"
	PhaseFailed      = "failed"
	PhaseInterrupted = "interrupted"
)

const (
	ActionStarted = "started"
	ActionAttach  = "attach"
	ActionCollect = "collect"
)

// Systemd starts, inspects and stops the unit an install runs in.
type Systemd interface {
	// Start runs argv as unit, appending its output to logPath. It returns an
	// error wrapping os.ErrExist when the unit already exists.
	Start(unit string, argv []string, logPath string) error
	Active(unit string) (bool, error)
	Stop(unit string) error
}

// State is what state.json records about a job.
type State struct {
	RunID      string    `json:"run_id"`
	KipVersion string    `json:"kip_version,omitempty"`
	Started    time.Time `json:"started"`
	Phase      string    `json:"phase"`
}

// Result holds the saved cluster entry, credential-free kubeconfig and admin
// email and password for the driver to collect. A failed step also includes
// a bug report link and a redacted error for the user to review and paste.
type Result struct {
	RunID         string          `json:"run_id"`
	Outcome       string          `json:"outcome"`
	AdminEmail    string          `json:"admin_email,omitempty"`
	Collected     bool            `json:"collected,omitempty"`
	Cluster       *config.Cluster `json:"cluster,omitempty"`
	Kubeconfig    string          `json:"kubeconfig,omitempty"`
	AdminPassword string          `json:"admin_password,omitempty"`
	IssueLink     string          `json:"issue_link,omitempty"`
	ErrorReport   string          `json:"error_report,omitempty"`
}

// Outcome is what a claim decided, for which run. KipVersion names the kip
// that started a job the driver attaches to or collects from.
type Outcome struct {
	Action     string `json:"action"`
	RunID      string `json:"run_id"`
	KipVersion string `json:"kip_version,omitempty"`
}

// Status is a job's state as a reader should act on it.
type Status struct {
	RunID       string
	Phase       string
	KipVersion  string
	Collectable bool
}

// UnitName returns the systemd unit name for an install job.
func UnitName(runID string) string { return "kipper-install-" + runID }

// Claim holds the job lock while choosing a running job to attach to, an
// uncollected result to return, or a new job to stage and start.
func Claim(root string, req Request, kipPath string, sd Systemd) (Outcome, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Outcome{}, err
	}
	if err := os.Chmod(root, 0o700); err != nil { //nolint:gosec // a directory: 0700 is owner-only, the execute bit lets root enter it
		return Outcome{}, err
	}
	unlock, err := lock(filepath.Join(root, "install.lock"))
	if err != nil {
		return Outcome{}, err
	}
	defer unlock()

	if current, err := readCurrent(root); err != nil {
		return Outcome{}, err
	} else if current != "" {
		if current == req.RunID {
			return Outcome{Action: ActionAttach, RunID: current}, nil
		}
		// Treat a missing state file as a stale current-job pointer.
		st, err := status(root, current, sd)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Outcome{}, err
		}
		if err == nil && (st.Phase == PhaseStarting || st.Phase == PhaseRunning) {
			return Outcome{Action: ActionAttach, RunID: current, KipVersion: st.KipVersion}, nil
		}
		if err == nil && st.Collectable {
			return Outcome{Action: ActionCollect, RunID: current, KipVersion: st.KipVersion}, nil
		}
	}

	dir := filepath.Join(root, req.RunID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return Outcome{}, fmt.Errorf("creating the job directory: %w", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Outcome{}, err
	}
	if err := writePrivate(filepath.Join(dir, "request.json"), body); err != nil {
		return Outcome{}, err
	}
	logPath := filepath.Join(dir, "install.log")
	if err := writePrivate(logPath, nil); err != nil {
		return Outcome{}, err
	}
	if err := WriteState(dir, State{RunID: req.RunID, KipVersion: req.KipVersion, Started: time.Now().UTC(), Phase: PhaseStarting}); err != nil {
		return Outcome{}, err
	}
	previous, _ := readCurrent(root)
	if err := writeAtomic(filepath.Join(root, "current"), []byte(req.RunID+"\n")); err != nil {
		return Outcome{}, err
	}
	// Satisfy Cobra's required --host flag even though the job reads the request.
	argv := []string{kipPath, "install", "--local", "--host", req.Host, "--request", filepath.Join(dir, "request.json")}
	if err := sd.Start(UnitName(req.RunID), argv, logPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return Outcome{Action: ActionAttach, RunID: req.RunID}, nil
		}
		return Outcome{}, fmt.Errorf("starting the install: %w", err)
	}
	if previous != "" && previous != req.RunID {
		// The previous job has finished; a request it never read can still
		// hold backup credentials.
		_ = os.Remove(filepath.Join(root, previous, "request.json"))
	}
	removeOldJobs(root, req.RunID, previous)
	removeOldKips(kipPath, time.Now())
	return Outcome{Action: ActionStarted, RunID: req.RunID}, nil
}

// ReadStatus reports the current job's status.
func ReadStatus(root string, sd Systemd) (Status, error) {
	current, err := readCurrent(root)
	if err != nil || current == "" {
		return Status{}, err
	}
	st, err := status(root, current, sd)
	// Match Claim: a missing state file makes the current-job pointer stale.
	if errors.Is(err, os.ErrNotExist) {
		return Status{}, nil
	}
	return st, err
}

// status asks systemd first and reads the state second, so a job that finished
// between the two reads is never mistaken for an interrupted one.
func status(root, runID string, sd Systemd) (Status, error) {
	active, err := sd.Active(UnitName(runID))
	if err != nil {
		return Status{}, err
	}
	dir := filepath.Join(root, runID)
	st, err := ReadState(dir)
	if err != nil {
		return Status{}, err
	}
	phase := st.Phase
	if !active && !terminal(phase) {
		phase = PhaseInterrupted
	}
	return Status{RunID: runID, Phase: phase, KipVersion: st.KipVersion, Collectable: uncollected(dir)}, nil
}

func terminal(phase string) bool { return phase == PhaseSucceeded || phase == PhaseFailed }

// Follow writes the job's log from offset to w until the log is drained and the
// job has finished or its unit is gone, then writes a final line with the offset
// reached and the phase. Only log bytes count towards the offset.
func Follow(root, runID string, offset int64, w io.Writer, sd Systemd, poll time.Duration) error {
	if err := CheckRunID(runID); err != nil {
		return err
	}
	logPath := filepath.Join(root, runID, "install.log")
	for {
		st, err := status(root, runID, sd)
		if err != nil {
			return err
		}
		n, err := copyFrom(logPath, offset, w)
		if err != nil {
			return err
		}
		offset += n
		if st.Phase != PhaseStarting && st.Phase != PhaseRunning {
			// One last read: the job may have written its final lines between
			// the status check and the copy above.
			n, err := copyFrom(logPath, offset, w)
			if err != nil {
				return err
			}
			offset += n
			_, err = fmt.Fprintf(w, "\n%s offset=%d phase=%s\n", EndMarker, offset, st.Phase)
			return err
		}
		time.Sleep(poll)
	}
}

func copyFrom(path string, offset int64, w io.Writer) (int64, error) {
	f, err := os.Open(path) //nolint:gosec // the job's own log under the root-only jobs directory
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	return io.Copy(w, f)
}

// CheckRunID validates an ID for use in directory and systemd unit names.
func CheckRunID(runID string) error {
	if !runIDPattern.MatchString(runID) {
		return fmt.Errorf("run id %q is not a plain name", runID)
	}
	return nil
}

// Ack marks a result collected and clears its password. Other clients can still
// import the cluster settings. Repeated acknowledgements are safe.
func Ack(root, runID string) error {
	if err := CheckRunID(runID); err != nil {
		return err
	}
	dir := filepath.Join(root, runID)
	r, err := ReadResult(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	r.AdminPassword = ""
	r.Collected = true
	return WriteResult(dir, r)
}

func uncollected(dir string) bool {
	r, err := ReadResult(dir)
	return err == nil && !r.Collected
}

const (
	CancelStopped   = "stopped"
	CancelDiscarded = "discarded"
	CancelIdle      = "idle"
)

// Cancel stops the current job or discards its uncollected result. A discarded
// result allows the next claim to start a new job.
func Cancel(root string, sd Systemd) (string, error) {
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return CancelIdle, nil
	}
	// Hold the claim lock until Stop returns to serialize cancellation
	// with new installs.
	unlock, err := lock(filepath.Join(root, "install.lock"))
	if err != nil {
		return "", err
	}
	defer unlock()
	current, err := readCurrent(root)
	if err != nil || current == "" {
		return CancelIdle, err
	}
	active, err := sd.Active(UnitName(current))
	if err != nil {
		return "", err
	}
	if active {
		return CancelStopped, sd.Stop(UnitName(current))
	}
	if !uncollected(filepath.Join(root, current)) {
		return CancelIdle, nil
	}
	if err := os.Remove(filepath.Join(root, current, "result.json")); err != nil {
		return "", err
	}
	return CancelDiscarded, nil
}

// WriteState writes state.json atomically.
func WriteState(dir string, st State) error {
	body, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "state.json"), body)
}

// ReadState reads state.json.
func ReadState(dir string) (State, error) {
	var st State
	body, err := os.ReadFile(filepath.Join(dir, "state.json")) //nolint:gosec // under the root-only jobs directory
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(body, &st)
	return st, err
}

// WriteResult writes result.json atomically with mode 0600.
func WriteResult(dir string, r Result) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "result.json"), body)
}

// ReadResult reads result.json.
func ReadResult(dir string) (Result, error) {
	var r Result
	body, err := os.ReadFile(filepath.Join(dir, "result.json")) //nolint:gosec // under the root-only jobs directory
	if err != nil {
		return r, err
	}
	err = json.Unmarshal(body, &r)
	return r, err
}

func readCurrent(root string) (string, error) {
	body, err := os.ReadFile(filepath.Join(root, "current")) //nolint:gosec // under the root-only jobs directory
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(body))
	if id != "" && !runIDPattern.MatchString(id) {
		return "", fmt.Errorf("the current job pointer holds %q, which is not a run id", id)
	}
	return id, nil
}

// removeOldJobs keeps the current and previous directories and any readable,
// uncollected results. Other directories with valid run IDs are removed.
func removeOldJobs(root, keep, previous string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || name == keep || name == previous || !runIDPattern.MatchString(name) {
			continue
		}
		if uncollected(filepath.Join(root, name)) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, name))
	}
}

// oldKip matches the kips drivers put on the server, named after their
// sha256, and the staging files an interrupted download or upload leaves.
var oldKip = regexp.MustCompile(`^kip-[0-9a-f]{64}(\.upload-[0-9a-f]+|\.[A-Za-z0-9]{6})?$`)

// KipDir holds the kips drivers put on the server, each named after its
// sha256. Cleanup touches no other directory.
var KipDir = "/usr/local/lib/kipper"

// KipLockName is the lock in KipDir that cleanup and every driver command that
// verifies, refreshes or publishes a kip take, so cleanup never removes a kip
// between a driver's check and its use.
const KipLockName = ".lock"

// kipGrace keeps a kip a driver placed or reused recently, since that driver
// may be about to claim with it.
const kipGrace = time.Hour

// removeOldKips removes the kips in KipDir beside keep, the one a new job runs,
// that no driver has placed or reused within kipGrace. It runs only when a job
// starts, so no other job is running from one of them, and does nothing for a
// keep outside KipDir.
func removeOldKips(keep string, now time.Time) {
	dir := filepath.Dir(keep)
	if dir != KipDir {
		return
	}
	unlock, err := lock(filepath.Join(dir, KipLockName))
	if err != nil {
		return
	}
	defer unlock()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == filepath.Base(keep) || !oldKip.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < kipGrace {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// writePrivate creates path with mode 0600 before writing to it, refusing to
// follow an existing file or symlink.
func writePrivate(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // a fixed name under the root-only jobs directory
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// writeAtomic writes body to a private temporary file and renames it over path.
func writeAtomic(path string, body []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
