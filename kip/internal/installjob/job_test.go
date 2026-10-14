package installjob

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSystemd records units and lets a test decide whether one is active.
type fakeSystemd struct {
	started  map[string][]string
	active   map[string]bool
	stopped  []string
	failNext error
}

func newFakeSystemd() *fakeSystemd {
	return &fakeSystemd{started: map[string][]string{}, active: map[string]bool{}}
}

func (f *fakeSystemd) Start(unit string, argv []string, logPath string) error {
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return err
	}
	if _, exists := f.started[unit]; exists {
		return os.ErrExist
	}
	f.started[unit] = argv
	f.active[unit] = true
	return nil
}
func (f *fakeSystemd) Active(unit string) (bool, error) { return f.active[unit], nil }
func (f *fakeSystemd) Stop(unit string) error {
	f.stopped = append(f.stopped, unit)
	f.active[unit] = false
	return nil
}

func request(runID string) Request {
	return Request{Version: RequestVersion, RunID: runID, KipVersion: "0.29.0", Host: "203.0.113.10"}
}

func TestClaimStartsANewJob(t *testing.T) {
	root := t.TempDir()
	sd := newFakeSystemd()

	got, err := Claim(root, request("run-1"), "/usr/local/bin/kip", sd)

	require.NoError(t, err)
	assert.Equal(t, Outcome{Action: ActionStarted, RunID: "run-1"}, got)
	dir := filepath.Join(root, "run-1")
	for name, mode := range map[string]os.FileMode{"request.json": 0o600, "install.log": 0o600, "state.json": 0o600} {
		info, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err, name)
		assert.Equal(t, mode, info.Mode().Perm(), name)
	}
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	assert.Equal(t, []string{"/usr/local/bin/kip", "install", "--local", "--host", "203.0.113.10", "--request", filepath.Join(dir, "request.json")}, sd.started["kipper-install-run-1"],
		"kip install requires --host, so the unit passes the request's")
	st, err := ReadStatus(root, sd)
	require.NoError(t, err)
	assert.Equal(t, PhaseStarting, st.Phase)
}

func TestClaimAttachesToARunningJob(t *testing.T) {
	root := t.TempDir()
	sd := newFakeSystemd()
	_, err := Claim(root, request("run-1"), "/usr/local/bin/kip", sd)
	require.NoError(t, err)

	got, err := Claim(root, request("run-2"), "/usr/local/bin/kip", sd)

	require.NoError(t, err)
	assert.Equal(t, Outcome{Action: ActionAttach, RunID: "run-1", KipVersion: "0.29.0"}, got)
	assert.NotContains(t, sd.started, "kipper-install-run-2", "a second install never starts beside a running one")
}

func TestClaimCollectsAnUncollectedResult(t *testing.T) {
	root := t.TempDir()
	sd := newFakeSystemd()
	_, err := Claim(root, request("run-1"), "/usr/local/bin/kip", sd)
	require.NoError(t, err)
	dir := filepath.Join(root, "run-1")
	require.NoError(t, WriteState(dir, State{RunID: "run-1", Phase: PhaseSucceeded}))
	require.NoError(t, WriteResult(dir, Result{RunID: "run-1", Outcome: PhaseSucceeded, AdminPassword: "pw"}))
	sd.active["kipper-install-run-1"] = false

	got, err := Claim(root, request("run-2"), "/usr/local/bin/kip", sd)

	require.NoError(t, err)
	assert.Equal(t, Outcome{Action: ActionCollect, RunID: "run-1"}, got)
}

func TestClaimStartsAfterACollectedJob(t *testing.T) {
	root := t.TempDir()
	sd := newFakeSystemd()
	_, err := Claim(root, request("run-1"), "/usr/local/bin/kip", sd)
	require.NoError(t, err)
	require.NoError(t, WriteState(filepath.Join(root, "run-1"), State{RunID: "run-1", Phase: PhaseFailed}))
	sd.active["kipper-install-run-1"] = false

	got, err := Claim(root, request("run-2"), "/usr/local/bin/kip", sd)

	require.NoError(t, err)
	assert.Equal(t, Outcome{Action: ActionStarted, RunID: "run-2"}, got)
}

// Retrying the current run ID reuses the claim after a lost acknowledgement.
func TestClaimRetriedWithTheSameRunIDAttaches(t *testing.T) {
	root := t.TempDir()
	sd := newFakeSystemd()
	_, err := Claim(root, request("run-1"), "/usr/local/bin/kip", sd)
	require.NoError(t, err)

	got, err := Claim(root, request("run-1"), "/usr/local/bin/kip", sd)

	require.NoError(t, err)
	assert.Equal(t, Outcome{Action: ActionAttach, RunID: "run-1"}, got)
	assert.Len(t, sd.started, 1)
}

func TestReadStatusInfersAnInterruptedJob(t *testing.T) {
	root := t.TempDir()
	sd := newFakeSystemd()
	_, err := Claim(root, request("run-1"), "/usr/local/bin/kip", sd)
	require.NoError(t, err)
	require.NoError(t, WriteState(filepath.Join(root, "run-1"), State{RunID: "run-1", Phase: PhaseRunning}))
	sd.active["kipper-install-run-1"] = false

	st, err := ReadStatus(root, sd)

	require.NoError(t, err)
	assert.Equal(t, PhaseInterrupted, st.Phase, "a unit that is gone while the state says running was interrupted")
}

func TestFollowStreamsFromAnOffsetAndEnds(t *testing.T) {
	root := t.TempDir()
	sd := newFakeSystemd()
	_, err := Claim(root, request("run-1"), "/usr/local/bin/kip", sd)
	require.NoError(t, err)
	dir := filepath.Join(root, "run-1")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "install.log"), []byte("one\ntwo\nthree\n"), 0o600))
	require.NoError(t, WriteState(dir, State{RunID: "run-1", Phase: PhaseSucceeded}))
	sd.active["kipper-install-run-1"] = false

	var out bytes.Buffer
	require.NoError(t, Follow(root, "run-1", 4, &out, sd, time.Millisecond))

	assert.True(t, strings.HasPrefix(out.String(), "two\nthree\n"), out.String())
	assert.Contains(t, out.String(), EndMarker+" offset=14 phase=succeeded")
}

func TestFollowEndsWhenTheUnitDies(t *testing.T) {
	root := t.TempDir()
	sd := newFakeSystemd()
	_, err := Claim(root, request("run-1"), "/usr/local/bin/kip", sd)
	require.NoError(t, err)
	require.NoError(t, WriteState(filepath.Join(root, "run-1"), State{RunID: "run-1", Phase: PhaseRunning}))
	sd.active["kipper-install-run-1"] = false

	var out bytes.Buffer
	require.NoError(t, Follow(root, "run-1", 0, &out, sd, time.Millisecond))

	assert.Contains(t, out.String(), "phase=interrupted")
}

func TestAckRemovesThePasswordAndKeepsTheResult(t *testing.T) {
	root := t.TempDir()
	sd := newFakeSystemd()
	_, err := Claim(root, request("run-1"), "/usr/local/bin/kip", sd)
	require.NoError(t, err)
	sd.active["kipper-install-run-1"] = false
	for _, id := range []string{"run-1", "run-0"} {
		dir := filepath.Join(root, id)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, WriteState(dir, State{RunID: id, Phase: PhaseSucceeded}))
		require.NoError(t, WriteResult(dir, Result{RunID: id, Outcome: PhaseSucceeded, AdminPassword: "pw", Kubeconfig: "kc"}))
	}

	require.NoError(t, Ack(root, "run-1"))
	require.NoError(t, Ack(root, "run-1"), "repeating an acknowledgement is harmless")

	r, err := ReadResult(filepath.Join(root, "run-1"))
	require.NoError(t, err)
	assert.Empty(t, r.AdminPassword)
	assert.True(t, r.Collected)
	assert.Equal(t, "kc", r.Kubeconfig)
	other, err := ReadResult(filepath.Join(root, "run-0"))
	require.NoError(t, err)
	assert.Equal(t, "pw", other.AdminPassword, "an acknowledgement never touches another run")

	got, err := Claim(root, request("run-2"), "/usr/local/bin/kip", sd)
	require.NoError(t, err)
	assert.Equal(t, ActionStarted, got.Action)
}

func TestRemoveOldJobsKeepsOnlyUncollectedResults(t *testing.T) {
	root := t.TempDir()
	for id, r := range map[string]Result{"run-a": {Collected: true}, "run-b": {AdminPassword: "pw"}} {
		dir := filepath.Join(root, id)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, WriteResult(dir, r))
	}

	removeOldJobs(root, "run-z", "")

	_, err := os.Stat(filepath.Join(root, "run-a"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(root, "run-b"))
	assert.NoError(t, err)
}

func TestCancelStopsTheCurrentUnit(t *testing.T) {
	root := t.TempDir()
	sd := newFakeSystemd()
	_, err := Claim(root, request("run-1"), "/usr/local/bin/kip", sd)
	require.NoError(t, err)

	done, err := Cancel(root, sd)

	require.NoError(t, err)
	assert.Equal(t, CancelStopped, done)
	assert.Equal(t, []string{"kipper-install-run-1"}, sd.stopped)

	done, err = Cancel(root, sd)
	require.NoError(t, err)
	assert.Equal(t, CancelIdle, done, "nothing left to stop")
}

// A result nobody can collect would otherwise answer every later claim.
func TestCancelDiscardsAnUncollectedResult(t *testing.T) {
	root := t.TempDir()
	sd := newFakeSystemd()
	_, err := Claim(root, request("run-1"), "/usr/local/bin/kip", sd)
	require.NoError(t, err)
	sd.active["kipper-install-run-1"] = false
	require.NoError(t, WriteResult(filepath.Join(root, "run-1"), Result{RunID: "run-1", Outcome: PhaseFailed}))

	done, err := Cancel(root, sd)

	require.NoError(t, err)
	assert.Equal(t, CancelDiscarded, done)
	out, err := Claim(root, request("run-2"), "/usr/local/bin/kip", sd)
	require.NoError(t, err)
	assert.Equal(t, ActionStarted, out.Action)
}

func TestResultIsPrivate(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, WriteResult(dir, Result{RunID: "run-1", AdminPassword: "pw"}))

	info, err := os.Stat(filepath.Join(dir, "result.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	got, err := ReadResult(dir)
	require.NoError(t, err)
	assert.Equal(t, "pw", got.AdminPassword)
}

// A job that died before it read its request leaves the request behind, and a
// request can carry backup credentials; the next job removes it.
func TestClaimRemovesAnAbandonedRequest(t *testing.T) {
	root := t.TempDir()
	sd := newFakeSystemd()
	_, err := Claim(root, request("run-1"), "/usr/local/bin/kip", sd)
	require.NoError(t, err)
	sd.active["kipper-install-run-1"] = false

	_, err = Claim(root, request("run-2"), "/usr/local/bin/kip", sd)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(root, "run-1", "request.json"))
	assert.True(t, os.IsNotExist(err), "the abandoned request with its credentials is gone")
	_, err = os.Stat(filepath.Join(root, "run-2", "request.json"))
	assert.NoError(t, err, "the new job's request stays until its install reads it")
}

func TestClaimStartsWhenTheCurrentJobHasGone(t *testing.T) {
	root := t.TempDir()
	sd := newFakeSystemd()
	require.NoError(t, os.WriteFile(filepath.Join(root, "current"), []byte("run-gone\n"), 0o600))

	got, err := Claim(root, request("run-2"), "/usr/local/bin/kip", sd)

	require.NoError(t, err)
	assert.Equal(t, ActionStarted, got.Action)
}

func TestClaimAttachReportsTheRunningJobsVersion(t *testing.T) {
	root := t.TempDir()
	sd := newFakeSystemd()
	_, err := Claim(root, request("run-1"), "/usr/local/bin/kip", sd)
	require.NoError(t, err)
	sd.active["kipper-install-run-1"] = true
	other := request("run-2")
	other.KipVersion = "0.30.0"

	got, err := Claim(root, other, "/usr/local/bin/kip", sd)

	require.NoError(t, err)
	assert.Equal(t, ActionAttach, got.Action)
	assert.Equal(t, "0.29.0", got.KipVersion)
}

func TestFollowRefusesARunIDThatIsNotAPlainName(t *testing.T) {
	err := Follow(t.TempDir(), "../x", 0, io.Discard, newFakeSystemd(), time.Millisecond)
	assert.ErrorContains(t, err, "not a plain name")
	assert.ErrorContains(t, CheckRunID("../x"), "not a plain name")
	assert.NoError(t, CheckRunID("run-1"))
}

func TestCancelWithNoJobsYet(t *testing.T) {
	done, err := Cancel(filepath.Join(t.TempDir(), "jobs"), newFakeSystemd())
	require.NoError(t, err)
	assert.Equal(t, CancelIdle, done)
}

// Old kips go when a new install starts, except the one it runs and any a
// driver placed or reused within the hour, which may be about to claim.
// useKipDir points the managed kip directory at dir for one test.
func useKipDir(t *testing.T, dir string) {
	t.Helper()
	saved := KipDir
	KipDir = dir
	t.Cleanup(func() { KipDir = saved })
}

func TestRemoveOldKips(t *testing.T) {
	dir := t.TempDir()
	useKipDir(t, dir)
	now := time.Now()
	sum := func(c string) string { return strings.Repeat(c, 64) }
	files := map[string]time.Duration{
		"kip-" + sum("a"):                  -5 * time.Hour,    // the one this job runs, however old
		"kip-" + sum("b"):                  -2 * time.Hour,    // old
		"kip-" + sum("c"):                  -10 * time.Minute, // recently placed
		"kip-" + sum("d") + ".upload-1a2b": -3 * time.Hour,    // abandoned upload
		"kip-" + sum("e") + ".Ab12Cd":      -3 * time.Hour,    // abandoned download
		"kip":                              -5 * time.Hour,    // not one of ours
		"notes.txt":                        -5 * time.Hour,
	}
	for name, age := range files {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
		require.NoError(t, os.Chtimes(p, now.Add(age), now.Add(age)))
	}

	removeOldKips(filepath.Join(dir, "kip-"+sum("a")), now)

	left, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{"kip-" + sum("a"), "kip-" + sum("c"), "kip", "notes.txt", KipLockName}, names)
}

func TestClaimRemovesOldKipsWhenItStarts(t *testing.T) {
	root := t.TempDir()
	lib := t.TempDir()
	useKipDir(t, lib)
	old := filepath.Join(lib, "kip-"+strings.Repeat("b", 64))
	require.NoError(t, os.WriteFile(old, []byte("x"), 0o600))
	past := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(old, past, past))

	_, err := Claim(root, request("run-1"), filepath.Join(lib, "kip-"+strings.Repeat("a", 64)), newFakeSystemd())

	require.NoError(t, err)
	_, err = os.Stat(old)
	assert.True(t, os.IsNotExist(err))
}

// A kip run from anywhere else leaves its directory alone: only the directory
// drivers manage holds disposable kips.
func TestRemoveOldKipsOnlyInItsOwnDirectory(t *testing.T) {
	useKipDir(t, t.TempDir())
	elsewhere := t.TempDir()
	other := filepath.Join(elsewhere, "kip-"+strings.Repeat("b", 64))
	require.NoError(t, os.WriteFile(other, []byte("x"), 0o600))
	past := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(other, past, past))

	removeOldKips(filepath.Join(elsewhere, "kip-"+strings.Repeat("a", 64)), time.Now())

	_, err := os.Stat(other)
	assert.NoError(t, err)
}

// Cleanup waits for a driver that is verifying, refreshing or publishing a kip
// under the same lock.
func TestRemoveOldKipsWaitsForTheLock(t *testing.T) {
	dir := t.TempDir()
	useKipDir(t, dir)
	old := filepath.Join(dir, "kip-"+strings.Repeat("b", 64))
	require.NoError(t, os.WriteFile(old, []byte("x"), 0o600))
	past := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(old, past, past))
	unlock, err := lock(filepath.Join(dir, KipLockName))
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		removeOldKips(filepath.Join(dir, "kip-"+strings.Repeat("a", 64)), time.Now())
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	_, err = os.Stat(old)
	assert.NoError(t, err, "nothing is removed while a driver holds the lock")

	unlock()
	<-done
	_, err = os.Stat(old)
	assert.True(t, os.IsNotExist(err))
}

func TestReadStatusTreatsAGoneJobAsNone(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "current"), []byte("run-gone\n"), 0o600))

	st, err := ReadStatus(root, newFakeSystemd())

	require.NoError(t, err)
	assert.Empty(t, st.RunID)
}
