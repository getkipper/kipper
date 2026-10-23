package installer

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

// SessionStarter starts a command with caller-managed input and output.
// The caller must close Stdin, drain Stdout and call Wait to release resources.
type SessionStarter interface {
	Start(command string) (*ssh.Held, error)
}

// LeaseServer is the server connection a node lease is taken and held on.
type LeaseServer interface {
	CommandRunner
	SessionStarter
}

// ErrNodeLeaseLost reports that this run no longer holds the node lease, so
// the command it was about to run did not start.
var ErrNodeLeaseLost = errors.New("this kip run no longer holds the node lifecycle lease, so it stopped before changing anything else. Run the command again")

const (
	leaseAcquired = "kipper-node-lease-acquired"
	leaseHeld     = "kipper-node-lease-held"
	leaseBusy     = "kipper-node-lease-busy"
	leaseLost     = "kipper-node-lease-lost"
	leaseHolding  = "kipper-node-lease-holding"
	leaseHostBusy = "kipper-node-host-busy"
	leaseFinish   = "finishing: "
	leaseLostExit = 76
	leaseBusyExit = 75
)

var (
	leaseHeartbeat = 15 * time.Second
	leaseWaitSecs  = 300
	leaseQuietSecs = 60

	leaseMachineIDFile = "/etc/machine-id"
)

var leasePurposeUnsafe = regexp.MustCompile(`[^A-Za-z0-9 .:_/-]`)

// NodeLease is this run's hold on the cluster's node lifecycle lease. One kip
// command that changes nodes, cluster access or Longhorn holds it at a time.
// Commands sent through Run or RunStdin on its Server and Host wrappers check
// ownership and hold a lock that the next owner waits for. Hosts remember
// each server's newest generation and refuse older ones. Lease ownership ends
// on reboot, while generation counters persist across reboots.
type NodeLease struct {
	server   LeaseServer
	dir      string
	stateDir string
	gen      string
	serverID string
	held     *ssh.Held

	stop     chan struct{}
	ended    chan struct{}
	mu       sync.Mutex
	released bool
}

// TakeNodeLease takes the node lifecycle lease on the server. It refuses when
// another run holds it, and waits up to five minutes for commands an earlier,
// interrupted run left running on the server. purpose names the run in the
// refusal another run sees. Call Release when finished. Heartbeats keep the
// lease alive, and the server releases it after 60 seconds without input,
// allowing up to five more minutes for outstanding commands to finish.
func TakeNodeLease(server LeaseServer, purpose string) (*NodeLease, error) {
	return takeNodeLease(server, "/run", "/var/lib", purpose)
}

func takeNodeLease(server LeaseServer, dir, stateDir, purpose string) (*NodeLease, error) {
	purpose = leasePurposeUnsafe.ReplaceAllString(purpose, "")
	held, err := server.Start(leaseScript(dir, stateDir, purpose))
	if err != nil {
		return nil, fmt.Errorf("taking the node lifecycle lease: %w", err)
	}
	stdout := bufio.NewReader(held.Stdout)
	line, readErr := stdout.ReadString('\n')
	fields := strings.Fields(line)
	if len(fields) != 3 || fields[0] != leaseAcquired || !machineIDPattern.MatchString(fields[2]) {
		_ = held.Stdin.Close()
		rest, _ := io.ReadAll(stdout)
		waitErr := held.Wait()
		switch {
		case strings.HasPrefix(line, leaseHeld):
			who := strings.TrimSpace(strings.TrimPrefix(line, leaseHeld))
			if stopped, ok := strings.CutPrefix(who, leaseFinish); ok {
				return nil, fmt.Errorf("an earlier kip command (%s) was stopped and is waiting up to %d seconds for its last step on the server to finish. Run this command again after that", stopped, leaseWaitSecs)
			}
			if who == "" {
				who = "an earlier kip command"
			}
			return nil, fmt.Errorf("another kip command that changes nodes, cluster access or Longhorn is running on this cluster (%s). Wait for it to finish, or stop it, then run this command again", who)
		case strings.HasPrefix(line, leaseBusy):
			return nil, fmt.Errorf("a command from an interrupted kip node run is still running on the server after %d seconds%s. Run this command again once it has finished", leaseWaitSecs, stillRunning(string(rest)))
		}
		return nil, fmt.Errorf("taking the node lifecycle lease: unexpected response %q: %w", strings.TrimSpace(line), errors.Join(readErr, waitErr))
	}
	l := &NodeLease{server: server, dir: dir, stateDir: stateDir, gen: fields[1], serverID: fields[2], held: held,
		stop: make(chan struct{}), ended: make(chan struct{})}
	go func() {
		_, _ = io.Copy(io.Discard, stdout)
		close(l.ended)
	}()
	go keepAlive(held.Stdin, l.stop)
	return l, nil
}

// stillRunning formats the process lines a busy response lists.
func stillRunning(out string) string {
	var procs []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			procs = append(procs, line)
		}
	}
	if len(procs) == 0 {
		return ""
	}
	return " (" + strings.Join(procs, "; ") + ")"
}

// keepAlive writes a heartbeat line to w until stop closes or a write fails.
// The server side ends a session after leaseQuietSecs without one.
func keepAlive(w io.Writer, stop <-chan struct{}) {
	t := time.NewTicker(leaseHeartbeat)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if _, err := io.WriteString(w, "ping\n"); err != nil {
				return
			}
		}
	}
}

// Release closes the lease input and waits for the SSH session to exit. The
// server allows up to five minutes for outstanding commands before clearing
// ownership. It is safe to call Release more than once.
func (l *NodeLease) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	close(l.stop)
	_ = l.held.Stdin.Close()
	select {
	case <-l.ended:
	case <-time.After(time.Duration(leaseWaitSecs+30) * time.Second):
	}
	_ = l.held.Wait()
}

// lost reports whether the server has already ended the lease.
func (l *NodeLease) lost() bool {
	select {
	case <-l.ended:
		return true
	default:
		return false
	}
}

// Server wraps the server connection base so Run and RunStdin check lease
// ownership and hold a shared lock until their remote processes exit. They
// return ErrNodeLeaseLost if ownership has ended before the command starts.
// Other FirewallRunner methods are forwarded to base.
func (l *NodeLease) Server(base FirewallRunner) FirewallRunner {
	return leaseRunner{FirewallRunner: base, lease: l, fence: l.serverFence}
}

// Host wraps the worker connection base so Run and RunStdin hold the server
// lease and serialise commands on the host. They wait up to five minutes for
// the host lock and return an error if it stays busy. They return
// ErrNodeLeaseLost if the lease has ended or the host has seen a newer
// generation from this server. Other FirewallRunner methods are forwarded
// to base.
func (l *NodeLease) Host(base FirewallRunner) FirewallRunner {
	return leaseRunner{FirewallRunner: base, lease: l, fence: l.hostFence, hold: true}
}

// serverFence builds a script that runs command only while this run owns
// the lease. The command inherits the shared ops lock, so the next owner
// waits for its processes that keep the lock descriptor open.
func (l *NodeLease) serverFence(command string) string {
	return fmt.Sprintf(`exec 8>%[1]s/kipper-node-ops.lock && flock -s 8 || exit 1
[ "$(cat %[1]s/kipper-node-lifecycle.owner 2>/dev/null)" = %[2]s ] || { echo %[3]s; exit %[4]d; }
(
%[5]s
)`, l.dir, l.gen, leaseLost, leaseLostExit, command)
}

// hostFence builds a script that waits for the host lock and refuses a
// command if the host has seen a newer generation from this server.
func (l *NodeLease) hostFence(command string) string {
	return fmt.Sprintf(`exec 8>%[1]s/kipper-node-host.lock || exit 1
flock -w %[2]d 8 || { echo %[3]s; exit %[4]d; }
mkdir -p %[1]s/kipper-node-lifecycle.d || exit 1
seen=$(cat %[1]s/kipper-node-lifecycle.d/%[5]s 2>/dev/null)
case "$seen" in ''|*[!0-9]*) ;; *) [ "$seen" -le %[6]s ] || { echo %[7]s; exit %[8]d; } ;; esac
echo %[6]s > %[1]s/kipper-node-lifecycle.d/%[5]s.tmp && mv -f %[1]s/kipper-node-lifecycle.d/%[5]s.tmp %[1]s/kipper-node-lifecycle.d/%[5]s || exit 1
(
%[9]s
)`, l.stateDir, leaseWaitSecs, leaseHostBusy, leaseBusyExit, l.serverID, l.gen, leaseLost, leaseLostExit, command)
}

// holdServer keeps a fenced command open on the server until the returned
// function is called or this client stops sending heartbeats. Another run
// cannot take the lease while the hold is open.
func (l *NodeLease) holdServer() (func(), error) {
	held, err := l.server.Start(l.serverFence("echo " + leaseHolding + "\n" + heartbeatLoop()))
	if err != nil {
		return nil, err
	}
	stdout := bufio.NewReader(held.Stdout)
	line, _ := stdout.ReadString('\n')
	stop := make(chan struct{})
	go keepAlive(held.Stdin, stop)
	end := func() {
		close(stop)
		_ = held.Stdin.Close()
		_, _ = io.Copy(io.Discard, stdout)
		_ = held.Wait()
	}
	if strings.TrimSpace(line) != leaseHolding {
		end()
		return nil, ErrNodeLeaseLost
	}
	return end, nil
}

// leaseRunner runs every command behind a lease fence. A host runner also
// keeps a server hold open for the length of each command.
type leaseRunner struct {
	FirewallRunner
	lease *NodeLease
	fence func(string) string
	hold  bool
}

func (r leaseRunner) Run(command string) (string, error) {
	return r.run(func(fenced string) (string, error) { return r.FirewallRunner.Run(fenced) }, command)
}

func (r leaseRunner) RunStdin(command string, stdin io.Reader) (string, error) {
	return r.run(func(fenced string) (string, error) { return r.FirewallRunner.RunStdin(fenced, stdin) }, command)
}

func (r leaseRunner) run(send func(string) (string, error), command string) (string, error) {
	if r.lease.lost() {
		return "", ErrNodeLeaseLost
	}
	if r.hold {
		end, err := r.lease.holdServer()
		if err != nil {
			return "", err
		}
		defer end()
	}
	fenced := r.fence(command)
	out, err := send(fenced)
	return fencedResult(out, err, fenced, command)
}

// fencedResult maps fence refusals to errors and uses the caller's command
// in other error messages, preserving the underlying error.
func fencedResult(out string, err error, fenced, command string) (string, error) {
	if err == nil {
		return out, nil
	}
	switch {
	case strings.Contains(out, leaseLost):
		return "", ErrNodeLeaseLost
	case strings.Contains(out, leaseHostBusy):
		return "", fmt.Errorf("a command from an interrupted kip node run is still running on this host after %d seconds. Run this command again once it has finished", leaseWaitSecs)
	}
	return out, fenceHidden{err: err, fenced: fenced, command: command}
}

// fenceHidden is a command's error with the fence script in its message
// replaced by the command itself.
type fenceHidden struct {
	err             error
	fenced, command string
}

func (e fenceHidden) Error() string {
	return strings.Replace(e.err.Error(), strconv.Quote(e.fenced), strconv.Quote(e.command), 1)
}

func (e fenceHidden) Unwrap() error { return e.err }

// readLine builds a shell substitution that reads one line from stdin, or
// returns empty output at EOF or after leaseQuietSecs. The shell reads one
// byte at a time so subsequent lines remain available to the next call.
func readLine() string {
	return fmt.Sprintf(`$(timeout %d sh -c 'IFS= read -r l && printf %%s "$l"' 7>&- 8>&-)`, leaseQuietSecs)
}

// heartbeatLoop builds a shell loop that ends on an empty line, EOF or
// leaseQuietSecs without a complete line.
func heartbeatLoop() string {
	return `while [ -n "` + readLine() + `" ]; do :; done`
}

// opsHolders builds a script listing processes with the ops lock file open,
// filtering out fence shells. flock(1) exits after acquiring the lock, so
// open descriptors identify the remaining holders when /proc/locks names
// the exited process.
func opsHolders(dir string) string {
	return fmt.Sprintf(`{ for f in /proc/[0-9]*/fd/*; do
  [ "$(readlink "$f" 2>/dev/null)" = %[1]s/kipper-node-ops.lock ] && p=${f#/proc/} && echo "${p%%%%/*}"
done | sort -u | while read -r p; do
  ps -o etime=,args= -p "$p" 2>/dev/null | grep -v -e kipper-node- -e ' flock ' | cut -c1-120
done; } 7>&- 8>&-`, dir)
}

// leaseScript builds a script that holds the lease while heartbeat input
// continues. It ignores HUP and PIPE and discards stderr after acquisition
// so a dropped connection can still reach cleanup. Cleanup waits up to
// leaseWaitSecs for outstanding commands before clearing ownership. Stdout
// stays open so EOF tells the client that the lease has ended.
func leaseScript(dir, stateDir, purpose string) string {
	return fmt.Sprintf(`trap '' HUP PIPE
id=$(cat %[10]s 2>/dev/null)
exec 7>%[1]s/kipper-node-lifecycle.lock || exit 1
if ! flock -n 7; then echo %[2]s "$(cat %[1]s/kipper-node-lifecycle.who 2>/dev/null)"; exit %[3]d; fi
exec 8>%[1]s/kipper-node-ops.lock || exit 1
if ! flock -w %[4]d 8; then echo %[5]s
%[9]s
exit %[3]d; fi
last=$(cat %[11]s/kipper-node-lifecycle.gen 2>/dev/null)
case "$last" in ''|*[!0-9]*) last=0 ;; esac
gen=$(date +%%s%%N)
[ "$gen" -gt "$last" ] 2>/dev/null || gen=$((last + 1))
echo "$gen" > %[11]s/kipper-node-lifecycle.gen && echo "$gen" > %[1]s/kipper-node-lifecycle.owner || exit 1
printf '%%s\n' %[6]s > %[1]s/kipper-node-lifecycle.who
flock -u 8
echo %[7]s "$gen" "$id"
exec 2>/dev/null
%[8]s
printf 'finishing: %%s\n' %[6]s > %[1]s/kipper-node-lifecycle.who
flock -w %[4]d 8
rm -f %[1]s/kipper-node-lifecycle.owner %[1]s/kipper-node-lifecycle.who`,
		dir, leaseHeld, leaseBusyExit, leaseWaitSecs, leaseBusy, shellQuote(purpose), leaseAcquired, heartbeatLoop(), opsHolders(dir), leaseMachineIDFile, stateDir)
}
