//go:build !windows

package installer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

// shellHost runs lease scripts with the machine's own sh, flock and timeout,
// so these tests exercise the real locking rather than a model of it.
type shellHost struct {
	mu    sync.Mutex
	procs []*exec.Cmd
}

// command starts each command in its own session, as sshd does.
func (h *shellHost) command(command string) *exec.Cmd {
	cmd := exec.Command("sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}

func (h *shellHost) Run(command string) (string, error) {
	out, err := h.command(command).CombinedOutput()
	return string(out), err
}

func (h *shellHost) RunStdin(command string, stdin io.Reader) (string, error) {
	cmd := h.command(command)
	cmd.Stdin = stdin
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (h *shellHost) Start(command string) (*ssh.Held, error) {
	cmd := h.command(command)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.procs = append(h.procs, cmd)
	h.mu.Unlock()
	return &ssh.Held{Stdin: stdin, Stdout: stdout, Wait: cmd.Wait}, nil
}

func (h *shellHost) SurvivesSSHRateLimit() bool    { return true }
func (h *shellHost) ConfirmFreshConnection() error { return nil }
func (h *shellHost) ObservedAddress(string) (string, error) {
	return "", errors.New("no fresh connection in this test")
}

// killFirst kills the process group of the first session started, which is
// what a server-side kill of an abandoned lease session does.
func (h *shellHost) killFirst() {
	h.mu.Lock()
	defer h.mu.Unlock()
	_ = syscall.Kill(-h.procs[0].Process.Pid, syscall.SIGKILL)
}

func needLockTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"flock", "timeout"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed; the lease tests run on Linux", tool)
		}
	}
}

type leaseCluster struct {
	server        *shellHost
	dir, stateDir string
	serverRunner  FirewallRunner
	hostRunner    FirewallRunner
}

func newLeaseCluster(t *testing.T) leaseCluster {
	t.Helper()
	needLockTools(t)
	idFile := filepath.Join(t.TempDir(), "machine-id")
	require.NoError(t, os.WriteFile(idFile, []byte("0123456789abcdef0123456789abcdef\n"), 0o600))
	old := leaseMachineIDFile
	leaseMachineIDFile = idFile
	t.Cleanup(func() { leaseMachineIDFile = old })
	h := &shellHost{}
	return leaseCluster{server: h, dir: t.TempDir(), stateDir: t.TempDir(), serverRunner: h, hostRunner: &shellHost{}}
}

func (c leaseCluster) take(t *testing.T, purpose string) *NodeLease {
	t.Helper()
	l, err := takeNodeLease(c.server, c.dir, c.stateDir, purpose)
	require.NoError(t, err)
	t.Cleanup(l.Release)
	return l
}

// abandon ends the lease session the way a client that died does: its
// connection closes, but the run's Go side never calls Release.
func abandon(l *NodeLease) {
	_ = l.held.Stdin.Close()
	<-l.ended
	_ = l.held.Wait()
}

func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestNodeLeaseRefusesASecondRunAndNamesTheFirst(t *testing.T) {
	c := newLeaseCluster(t)
	c.take(t, "kip node remove worker-1")

	_, err := takeNodeLease(c.server, c.dir, c.stateDir, "kip node add 192.0.2.10")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "another kip node or cluster access command is running on this cluster (kip node remove worker-1)")
}

func TestNodeLeaseIsFreeAgainAfterRelease(t *testing.T) {
	c := newLeaseCluster(t)
	first := c.take(t, "first")
	first.Release()

	second := c.take(t, "second")
	assert.Greater(t, second.gen, first.gen, "generations only grow")
}

func TestAFencedCommandKeepsItsOutputAndExitStatus(t *testing.T) {
	c := newLeaseCluster(t)
	l := c.take(t, "run")

	out, err := l.Server(c.serverRunner).Run("echo hello; exit 3")
	require.Error(t, err)
	assert.Equal(t, "hello\n", out)
	var exit *exec.ExitError
	require.True(t, errors.As(err, &exit))
	assert.Equal(t, 3, exit.ExitCode())

	out, err = l.Server(c.serverRunner).RunStdin("cat", strings.NewReader("payload"))
	require.NoError(t, err)
	assert.Equal(t, "payload", out)
}

func TestAStaleRunCannotStartACommandAfterATakeover(t *testing.T) {
	c := newLeaseCluster(t)
	stale := c.take(t, "stale")
	abandon(stale)
	c.take(t, "next")

	marker := filepath.Join(c.dir, "stale-ran")
	_, err := stale.Server(c.serverRunner).Run("touch " + marker)
	assert.ErrorIs(t, err, ErrNodeLeaseLost)

	// The server must also refuse stale commands because a client may not
	// have noticed that its lease session has ended.
	_, err = c.serverRunner.Run(stale.serverFence("touch " + marker))
	require.Error(t, err)
	_, err = c.hostRunner.Run(stale.hostFence("true"))
	require.NoError(t, err, "the host lock alone does not know the owner")
	_, err = leaseRunner{FirewallRunner: c.hostRunner, lease: &NodeLease{server: c.server, dir: c.dir, stateDir: c.stateDir, gen: stale.gen, ended: make(chan struct{})}, fence: stale.hostFence, hold: true}.Run("touch " + marker)
	assert.ErrorIs(t, err, ErrNodeLeaseLost, "a stale run's host command needs a server hold, which it cannot get")
	assert.NoFileExists(t, marker)
}

func TestTheLeaseEndsOnlyAfterTheRunsLastCommand(t *testing.T) {
	c := newLeaseCluster(t)
	l := c.take(t, "run")
	done := filepath.Join(c.dir, "done")
	go func() { _, _ = l.Server(c.serverRunner).Run("sleep 1; touch " + done) }()
	eventually(t, func() bool { return lockHeld(c.dir, "kipper-node-ops.lock") }, "the command never started")

	abandon(l)
	assert.FileExists(t, done, "the lease ended before the command it covered")
}

func TestTakingTheLeaseWaitsForAKilledRunsCommand(t *testing.T) {
	c := newLeaseCluster(t)
	l := c.take(t, "killed")
	done := filepath.Join(c.dir, "done")
	go func() { _, _ = l.Server(c.serverRunner).Run("sleep 2; touch " + done) }()
	eventually(t, func() bool { return lockHeld(c.dir, "kipper-node-ops.lock") }, "the command never started")

	c.server.killFirst()
	eventually(t, l.lost, "the client did not notice its lease session was killed")
	_, err := l.Server(c.serverRunner).Run("true")
	assert.ErrorIs(t, err, ErrNodeLeaseLost)

	next := c.take(t, "next")
	assert.FileExists(t, done, "the next run took over while the killed run's command was still running")
	_, err = next.Server(c.serverRunner).Run("true")
	assert.NoError(t, err)
}

func TestTakingTheLeaseGivesUpOnACommandThatNeverEnds(t *testing.T) {
	c := newLeaseCluster(t)
	old := leaseWaitSecs
	leaseWaitSecs = 1
	t.Cleanup(func() { leaseWaitSecs = old })

	l := c.take(t, "killed")
	go func() { _, _ = l.Server(c.serverRunner).Run("sleep 4") }()
	eventually(t, func() bool { return lockHeld(c.dir, "kipper-node-ops.lock") }, "the command never started")
	c.server.killFirst()

	_, err := takeNodeLease(c.server, c.dir, c.stateDir, "next")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "still running on the server")
	if _, lookErr := exec.LookPath("ps"); lookErr == nil {
		assert.Contains(t, err.Error(), "sleep 4", "the refusal names what is still running")
	}
}

func TestTheLockOutlivesAFenceWhoseShellWasKilled(t *testing.T) {
	c := newLeaseCluster(t)
	l := c.take(t, "first")
	started := filepath.Join(c.dir, "started")
	wrapper := exec.Command("sh", "-c", l.serverFence("touch "+started+"; sleep 2; true"))
	require.NoError(t, wrapper.Start())
	eventually(t, func() bool { _, err := os.Stat(started); return err == nil }, "the command never started")
	require.NoError(t, wrapper.Process.Kill())
	_ = wrapper.Wait()
	killed := time.Now()
	c.server.killFirst()

	c.take(t, "next")
	assert.Greater(t, time.Since(killed), time.Second, "the next run took over while the command's sleep was still running")
}

func TestAHostRefusesACommandOlderThanOneItHasRun(t *testing.T) {
	c := newLeaseCluster(t)
	stale := c.take(t, "stale")
	staleCommand := stale.hostFence("touch " + filepath.Join(c.stateDir, "stale-ran"))
	stale.Release()

	next := c.take(t, "next")
	_, err := next.Host(c.hostRunner).Run("true")
	require.NoError(t, err)

	// The stale run's command was already on its way when the lease changed hands.
	out, err := c.hostRunner.Run(staleCommand)
	require.Error(t, err)
	assert.Contains(t, out, leaseLost)
	assert.NoFileExists(t, filepath.Join(c.stateDir, "stale-ran"))
}

func TestAHostKeepsEachServersNewestGeneration(t *testing.T) {
	c := newLeaseCluster(t)
	stale := c.take(t, "stale")
	staleCommand := stale.hostFence("touch " + filepath.Join(c.stateDir, "stale-ran"))
	stale.Release()
	next := c.take(t, "next")
	_, err := next.Host(c.hostRunner).Run("true")
	require.NoError(t, err)

	// The host then joins another cluster, whose server has its own generations.
	other := &NodeLease{stateDir: c.stateDir, serverID: "fedcba9876543210fedcba9876543210", gen: "1"}
	_, err = c.hostRunner.Run(other.hostFence("true"))
	require.NoError(t, err, "a generation from another server does not block this one")

	out, err := c.hostRunner.Run(staleCommand)
	require.Error(t, err)
	assert.Contains(t, out, leaseLost, "the old cluster's stale command stays refused")
	assert.NoFileExists(t, filepath.Join(c.stateDir, "stale-ran"))
}

func TestAHostCommandsErrorNamesTheCommandNotTheFence(t *testing.T) {
	c := newLeaseCluster(t)
	l := c.take(t, "run")
	_, err := l.Host(&quotingHost{c.hostRunner}).Run("exit 3")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `running "exit 3"`)
	assert.NotContains(t, err.Error(), "flock")
	var exit *exec.ExitError
	assert.True(t, errors.As(err, &exit), "the exit status stays reachable")
}

// quotingHost wraps errors the way the SSH client does.
type quotingHost struct{ FirewallRunner }

func (h *quotingHost) Run(command string) (string, error) {
	out, err := h.FirewallRunner.Run(command)
	if err != nil {
		return out, fmt.Errorf("running %q on 192.0.2.11: %w", command, err)
	}
	return out, nil
}

func TestAStoppedRunThatIsFinishingSaysSo(t *testing.T) {
	c := newLeaseCluster(t)
	l := c.take(t, "kip node remove worker-1")
	go func() { _, _ = l.Server(c.serverRunner).Run("sleep 2") }()
	eventually(t, func() bool { return lockHeld(c.dir, "kipper-node-ops.lock") }, "the command never started")
	_ = l.held.Stdin.Close()
	eventually(t, func() bool {
		who, _ := os.ReadFile(filepath.Join(c.dir, "kipper-node-lifecycle.who"))
		return strings.HasPrefix(string(who), leaseFinish)
	}, "the session never started its release")

	_, err := takeNodeLease(c.server, c.dir, c.stateDir, "next")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "(kip node remove worker-1) was stopped and is waiting")
}

func TestAQuietLeaseEnds(t *testing.T) {
	c := newLeaseCluster(t)
	oldQuiet, oldBeat := leaseQuietSecs, leaseHeartbeat
	leaseQuietSecs, leaseHeartbeat = 1, time.Hour
	t.Cleanup(func() { leaseQuietSecs, leaseHeartbeat = oldQuiet, oldBeat })

	l := c.take(t, "asleep")
	eventually(t, l.lost, "the lease outlived its heartbeat")
	_, err := l.Server(c.serverRunner).Run("true")
	assert.ErrorIs(t, err, ErrNodeLeaseLost)
	c.take(t, "next")
}

func TestTheHeartbeatKeepsTheLease(t *testing.T) {
	c := newLeaseCluster(t)
	oldQuiet, oldBeat := leaseQuietSecs, leaseHeartbeat
	leaseQuietSecs, leaseHeartbeat = 1, 200*time.Millisecond
	t.Cleanup(func() { leaseQuietSecs, leaseHeartbeat = oldQuiet, oldBeat })

	l := c.take(t, "awake")
	time.Sleep(2500 * time.Millisecond)
	assert.False(t, l.lost())
	_, err := l.Server(c.serverRunner).Run("true")
	assert.NoError(t, err)
}

func TestAHostCommandHoldsOffTheNextOwner(t *testing.T) {
	c := newLeaseCluster(t)
	old := leaseWaitSecs
	leaseWaitSecs = 1
	t.Cleanup(func() { leaseWaitSecs = old })

	l := c.take(t, "first")
	go func() { _, _ = l.Host(c.hostRunner).Run("sleep 3") }()
	eventually(t, func() bool { return lockHeld(c.stateDir, "kipper-node-host.lock") }, "the host command never started")
	c.server.killFirst()

	_, err := takeNodeLease(c.server, c.dir, c.stateDir, "next")
	require.Error(t, err, "the server hold must keep the lease from changing hands mid-command")
	assert.Contains(t, err.Error(), "still running on the server")
}

func TestHostCommandsOfTwoRunsDoNotOverlap(t *testing.T) {
	c := newLeaseCluster(t)
	log := filepath.Join(c.stateDir, "log")
	first := c.take(t, "first")
	// A host command whose client has gone keeps running on the host.
	script := first.hostFence("echo start-a >> " + log + "; sleep 1; echo end-a >> " + log)
	bg := exec.Command("sh", "-c", script)
	require.NoError(t, bg.Start())
	eventually(t, func() bool { return lockHeld(c.stateDir, "kipper-node-host.lock") }, "the host command never started")
	first.Release()

	second := c.take(t, "second")
	_, err := second.Host(c.hostRunner).Run("echo b >> " + log)
	require.NoError(t, err)
	_ = bg.Wait()
	got, err := os.ReadFile(log)
	require.NoError(t, err)
	assert.Equal(t, "start-a\nend-a\nb\n", string(got))
}

func TestHostCommandsReportABusyHost(t *testing.T) {
	c := newLeaseCluster(t)
	old := leaseWaitSecs
	leaseWaitSecs = 1
	t.Cleanup(func() { leaseWaitSecs = old })
	l := c.take(t, "run")
	bg := exec.Command("sh", "-c", l.hostFence("sleep 3"))
	require.NoError(t, bg.Start())
	t.Cleanup(func() { _ = bg.Process.Kill(); _ = bg.Wait() })
	eventually(t, func() bool { return lockHeld(c.stateDir, "kipper-node-host.lock") }, "the host command never started")

	_, err := l.Host(c.hostRunner).Run("true")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "still running on this host")
}

func TestThePurposeCannotBreakTheScript(t *testing.T) {
	c := newLeaseCluster(t)
	c.take(t, "x'; touch /tmp/kipper-should-not-exist; echo '$(id)")
	_, err := takeNodeLease(c.server, c.dir, c.stateDir, "next")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "uid=")
	assert.NoFileExists(t, "/tmp/kipper-should-not-exist")
}

// lockHeld reports whether some process holds the lock file name in dir.
func lockHeld(dir, name string) bool {
	out, _ := exec.Command("sh", "-c", "flock -n "+filepath.Join(dir, name)+" true && echo free").CombinedOutput()
	return !bytes.Contains(out, []byte("free"))
}
