package installer

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// accessScene is a host whose kernel table, units and clock live in files
// under a temporary root. Stand-ins for nft, systemctl, systemd-run, date,
// sleep, sync and timeout read and write them, and flock takes real locks,
// so the access scripts run unchanged in a local shell.
type accessScene struct {
	t              *testing.T
	root, bin, log string
	r              accessRun
	observed       map[string]string
	observedErr    error
	ran            []string
}

const fakeNft = `echo "nft $*" >> LOG
# Simulate a translated missing-table error unless the command sets the C locale.
absent() {
  if [ "$LC_ALL" = C ]; then echo "Error: No such file or directory" >&2; else echo "Fehler: Datei oder Verzeichnis nicht gefunden" >&2; fi
}
check=; file=
while [ $# -gt 0 ]; do
  case "$1" in
    -c) check=1 ;;
    -j) ;;
    -f) file=$2; shift ;;
    *) break ;;
  esac
  shift
done
if [ -n "$file" ]; then
  [ -e ROOT/nft-slow ] && /bin/sleep "$(cat ROOT/nft-slow)"
  if [ "$file" = - ]; then input=$(cat); else
    [ -r "$file" ] || { echo "Error: Could not open file \"$file\"" >&2; exit 1; }
    input=$(cat "$file")
  fi
  printf '%s\n' "$input" | grep '^include' >> ROOT/includes
  body=$(printf '%s\n' "$input" | while IFS= read -r line; do
    case "$line" in
      (include*) f=$(printf '%s' "$line" | sed 's/^include "\(.*\)"$/\1/'); if [ -r "$f" ]; then cat "$f"; else echo "@@unreadable $f"; fi ;;
      (*) printf '%s\n' "$line" ;;
    esac
  done)
  case "$body" in *"@@unreadable"*) echo "Error: Could not process rule: No such file or directory" >&2; exit 1 ;; esac
  if [ -s ROOT/nft-rejects ]; then
    while IFS= read -r bad; do
      case "$body" in *"$bad"*) echo "Error: syntax error ($bad)" >&2; exit 1 ;; esac
    done < ROOT/nft-rejects
  fi
  [ -n "$check" ] && exit 0
  printf '%s\n' "$body" > ROOT/kernel
  { printf '%s\n' "$body"; echo ----; } >> ROOT/loads
  exit 0
fi
case "$1 $2" in
  "list table") [ -e ROOT/kernel ] || { absent; exit 1; } ;;
  "delete table") [ -e ROOT/nft-delete-fails ] && { echo "Error: Device or resource busy" >&2; exit 1; }
    [ -e ROOT/kernel ] || { absent; exit 1; }; rm -f ROOT/kernel ;;
  "add element") shift 2; echo "add element $*" >> ROOT/kernel ;;
esac
exit 0`

const fakeSystemctl = `echo "systemctl $*" >> LOG
case "$1" in
  is-enabled) shift; [ "$1" = --quiet ] && shift; if [ -e "ROOT/enabled-$1" ]; then echo enabled; exit 0; fi; echo disabled; exit 1 ;;
  enable) touch "ROOT/enabled-$2" ;;
  disable) rm -f "ROOT/enabled-$2" ;;
  is-active) shift; [ "$1" = --quiet ] && shift; if [ -e "ROOT/active-$1" ]; then echo active; exit 0; fi; echo inactive; exit 3 ;;
  list-units) cat ROOT/units 2>/dev/null ;;
  stop) rm -f "ROOT/active-$2" ;;
  show) cat ROOT/socket 2>/dev/null || echo "Listen=" ;;
esac
exit 0`

const fakeSystemdRun = `echo "systemd-run $*" >> LOG
wait=
while [ $# -gt 0 ]; do case "$1" in --wait) wait=1 ;; --*) ;; *) break ;; esac; shift; done
[ -n "$wait" ] && exec "$@"
exit 0`

// fakeFlock takes real flock(2) locks on an inherited descriptor, which
// outlive the stand-in because the shell keeps the descriptor open.
const fakeFlock = `exec perl -e '
use Fcntl qw(:flock);
my ($un, $fd, $nb, $sh, $w) = (0, undef, 0, 0, undef);
while (@ARGV) { my $a = shift; if ($a eq "-u") { $un = 1 } elsif ($a eq "-n") { $nb = 1 } elsif ($a eq "-s") { $sh = 1 } elsif ($a eq "-w") { $w = shift } else { $fd = $a } }
open(my $fh, ">>&=", $fd) or die "fd $fd: $!";
if ($un) { flock($fh, LOCK_UN) or exit 1; exit 0 }
my $mode = $sh ? LOCK_SH : LOCK_EX;
if (defined $w || $nb) { my $end = time + ($w // 0); until (flock($fh, $mode | LOCK_NB)) { exit 1 if time >= $end; select(undef, undef, undef, 0.05) } exit 0 }
flock($fh, $mode) or exit 1; exit 0' -- "$@"`

func newAccessScene(t *testing.T) *accessScene {
	t.Helper()
	root, bin := t.TempDir(), t.TempDir()
	log := filepath.Join(root, "calls.log")
	for _, d := range []string{"/etc/kipper", "/etc/systemd/system", "/usr/local/bin"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o750))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "clock"), []byte("1000\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, accessK3sBinary), nil, 0o600))
	stubs := map[string]string{
		"nft":         fakeNft,
		"systemctl":   fakeSystemctl,
		"systemd-run": fakeSystemdRun,
		"flock":       fakeFlock,
		"timeout":     `shift 3; exec "$@"`,
		"date":        `if [ "$1" = +%s ]; then cat ROOT/clock; else /bin/date "$@"; fi`,
		"sleep":       `echo "sleep $1" >> LOG; echo $(( $(cat ROOT/clock) + $1 )) > ROOT/clock`,
		"sync":        `echo "sync $*" >> LOG`,
		"restorecon":  `echo "restorecon $*" >> LOG`,
	}
	for name, body := range stubs {
		body = strings.ReplaceAll(strings.ReplaceAll(body, "ROOT", root), "LOG", log)
		//nolint:gosec // the stand-ins have to be executable
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	}
	return &accessScene{t: t, root: root, bin: bin, log: log, r: accessRun{id: "abc123", root: root},
		observed: map[string]string{"4": "198.51.100.7"}}
}

func (s *accessScene) shell(command string, stdin io.Reader) (string, error) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = []string{"PATH=" + s.bin + ":/usr/bin:/bin"}
	cmd.Stdin = stdin
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (s *accessScene) Run(command string) (string, error) {
	s.ran = append(s.ran, command)
	if command == "cat /etc/os-release" {
		return "ID=ubuntu\nVERSION_ID=\"24.04\"\n", nil
	}
	return s.shell(command, nil)
}

func (s *accessScene) RunStdin(command string, stdin io.Reader) (string, error) {
	s.ran = append(s.ran, command)
	return s.shell(command, stdin)
}

func (s *accessScene) SurvivesSSHRateLimit() bool    { return true }
func (s *accessScene) ConfirmFreshConnection() error { return nil }
func (s *accessScene) ObservedAddress(family string) (string, error) {
	s.ran = append(s.ran, "observe IPv"+family)
	if s.observedErr != nil {
		return "", s.observedErr
	}
	if a, ok := s.observed[family]; ok {
		return a, nil
	}
	return "", errors.New("no route")
}

func (s *accessScene) path(name string) string { return filepath.Join(s.root, name) }
func (s *accessScene) file(name string) string {
	b, _ := os.ReadFile(s.path(name))
	return string(b)
}
func (s *accessScene) exists(name string) bool {
	_, err := os.Lstat(s.path(name))
	return err == nil
}
func (s *accessScene) write(name, body string) {
	s.t.Helper()
	require.NoError(s.t, os.MkdirAll(filepath.Dir(s.path(name)), 0o750))
	//nolint:gosec // name is a fixed path inside the test's own temp dir
	require.NoError(s.t, os.WriteFile(s.path(name), []byte(body), 0o600))
}
func (s *accessScene) calls() string { return s.file("calls.log") }
func (s *accessScene) reject(substring string) {
	s.t.Helper()
	f, err := os.OpenFile(s.path("nft-rejects"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(s.t, err)
	_, err = f.WriteString(substring + "\n")
	require.NoError(s.t, err)
	require.NoError(s.t, f.Close())
}

func (s *accessScene) intent(gen int, admin ...string) AccessIntent {
	return AccessIntent{Generation: gen, Admin: prefixes(s.t, admin...), Ports: []int{22}, Cluster4: prefixes(s.t, "10.42.0.0/16")}
}

// commit installs generation 7 as the committed pair, as a finished apply
// leaves it.
func (s *accessScene) commit(admin ...string) {
	s.t.Helper()
	table, err := RenderAccessTable(s.intent(7, admin...))
	require.NoError(s.t, err)
	ops, err := RenderAccessOperators(s.intent(7, admin...))
	require.NoError(s.t, err)
	s.write(accessDir+"/table.nft", table)
	s.write(accessDir+"/operators.nft", ops)
	s.write(accessDir+"/access.sh", accessScript(s.root))
}

// arm reserves the host and writes gen as the candidate pair.
func (s *accessScene) arm(gen int, admin ...string) error {
	table, err := RenderAccessTable(s.intent(gen, admin...))
	require.NoError(s.t, err)
	ops, err := RenderAccessOperators(s.intent(gen, admin...))
	require.NoError(s.t, err)
	_, err = s.shell(s.r.reserveAndArmCommand(table, ops, gen), nil)
	return err
}

func (s *accessScene) script(args string) (string, error) {
	out, err := s.shell("sh -x "+s.path(accessDir+"/access.sh")+" "+args, nil)
	if os.Getenv("KIP_DEBUG") != "" {
		s.t.Log(out)
	}
	return out, err
}

func (s *accessScene) kernelGeneration() string {
	k := s.file("kernel")
	if k == "" {
		return "none"
	}
	line, _, _ := strings.Cut(k, "\n")
	return strings.TrimPrefix(line, accessHeader)
}

func (s *accessScene) kernelAdmitsOperators() bool {
	return strings.Contains(s.file("kernel"), "add element inet kipper_access admin")
}

func (s *accessScene) advance(seconds int) {
	s.t.Helper()
	var n int
	_, err := fmt.Sscan(strings.TrimSpace(s.file("clock")), &n)
	require.NoError(s.t, err)
	s.write("clock", strconv.Itoa(n+seconds)+"\n")
}

func TestAccessLoadLoadsThePairAsTwoIncludesAndClearsClosed(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	s.write(accessDir+"/closed", "")
	_, err := s.script("load")
	require.NoError(t, err)
	assert.Equal(t, "7", s.kernelGeneration())
	assert.True(t, s.kernelAdmitsOperators())
	assert.Equal(t, "include \""+s.path(accessDir+"/table.nft")+"\"\ninclude \""+s.path(accessDir+"/operators.nft")+"\"\n", s.file("includes"))
	assert.False(t, s.exists(accessDir+"/closed"))
}

func TestAccessLoadFallsBackToTheTableAloneWhenOperatorsAreRejected(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	s.reject("add element")
	_, err := s.script("load")
	require.NoError(t, err)
	assert.Equal(t, "7", s.kernelGeneration())
	assert.False(t, s.kernelAdmitsOperators(), "closed: every operator refused, the cluster still admitted")
	assert.True(t, s.exists(accessDir+"/closed"))
}

func TestAccessLoadWithoutOperatorsLoadsTheTableClosed(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	require.NoError(t, os.Remove(s.path(accessDir+"/operators.nft")))
	_, err := s.script("load")
	require.NoError(t, err)
	assert.Equal(t, "7", s.kernelGeneration())
	assert.True(t, s.exists(accessDir+"/closed"))
}

func TestAccessLoadOfAnUnreadableTableNeverLoadsTheOperatorLinesAlone(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	require.NoError(t, os.Remove(s.path(accessDir+"/table.nft")))
	require.NoError(t, os.Symlink(s.path("nowhere"), s.path(accessDir+"/table.nft")))
	_, err := s.script("load")
	assert.Error(t, err)
	assert.False(t, s.exists("kernel"), "nothing reached the kernel")
	assert.Empty(t, s.file("loads"))
}

func TestAccessLoadThatFailsKeepsTheReservationAndCandidates(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	s.write(accessDir+"/run", "abc123\n")
	s.write(accessDir+"/next-table.nft", "candidate")
	s.reject("generation 7")
	_, err := s.script("load")
	assert.Error(t, err)
	assert.True(t, s.exists(accessDir+"/run"), "the timer still owns the change")
	assert.True(t, s.exists(accessDir+"/next-table.nft"))

	require.NoError(t, os.Remove(s.path("nft-rejects")))
	_, err = s.script("load")
	require.NoError(t, err)
	assert.False(t, s.exists(accessDir+"/run"), "a successful load cancels the reservation")
	assert.False(t, s.exists(accessDir+"/next-table.nft"))
}

func TestAccessCandidateLoadsTheCandidatePairUnderTheLock(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	require.NoError(t, s.arm(8, "198.51.100.0/24"))
	_, err := s.script("candidate abc123")
	require.NoError(t, err)
	assert.Equal(t, "8", s.kernelGeneration())
	assert.Contains(t, s.file("includes"), "include \""+s.path(accessDir+"/next-table.nft")+"\"\ninclude \""+s.path(accessDir+"/next-operators.nft")+"\"")
	assert.Equal(t, "1180\n", s.file(accessDir+"/abc123.deadline"), "the deadline is renewed before the load")
}

func TestAccessCandidateRefusesAfterARollbackOrWithoutTheReservation(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	require.NoError(t, s.arm(8, "198.51.100.0/24"))
	s.write(accessDir+"/abc123.rolledback", "")
	out, err := s.script("candidate abc123")
	assert.Error(t, err)
	assert.Contains(t, out, accessRefusedMarker)
	assert.False(t, s.exists("kernel"))

	require.NoError(t, os.Remove(s.path(accessDir+"/abc123.rolledback")))
	s.write(accessDir+"/run", "def456\n")
	out, err = s.script("candidate abc123")
	assert.Error(t, err)
	assert.Contains(t, out, accessRefusedMarker)

	require.NoError(t, os.Remove(s.path(accessDir+"/run")))
	_, err = s.script("candidate abc123")
	assert.Error(t, err)
}

// A rollback that starts while the candidate load runs waits for it, so the
// restore always lands after a late candidate.
func TestAccessRollbackWaitsForARunningCandidateLoad(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	require.NoError(t, s.arm(8, "198.51.100.0/24"))
	s.write("nft-slow", "1")
	load := exec.Command("sh", s.path(accessDir+"/access.sh"), "candidate", "abc123")
	load.Env = []string{"PATH=" + s.bin + ":/usr/bin:/bin"}
	require.NoError(t, load.Start())
	time.Sleep(300 * time.Millisecond)
	require.NoError(t, os.Remove(s.path("nft-slow")))
	s.write(accessDir+"/abc123.deadline", "0\n")
	out, err := s.shell("sh "+s.path(accessDir+"/access.sh")+" rollback abc123", nil)
	require.NoError(t, err, out)
	require.NoError(t, load.Wait())
	loads := strings.Split(s.file("loads"), "----\n")
	require.Len(t, loads, 3)
	assert.Contains(t, loads[0], "generation 8", "the candidate landed first")
	assert.Contains(t, loads[1], "generation 7", "and the restore after it")
	assert.Equal(t, "7", s.kernelGeneration())
	assert.Equal(t, "restored\n", s.file(accessDir+"/abc123.outcome"))
}

func TestAccessRollbackRestoresTheCommittedPair(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	require.NoError(t, s.arm(8, "198.51.100.0/24"))
	_, err := s.script("candidate abc123")
	require.NoError(t, err)
	s.advance(accessRollbackSeconds + 1)
	_, err = s.script("rollback abc123")
	require.NoError(t, err)
	assert.Equal(t, "7", s.kernelGeneration())
	assert.True(t, s.kernelAdmitsOperators())
	assert.Equal(t, "restored\n", s.file(accessDir+"/abc123.outcome"))
	assert.True(t, s.exists(accessDir+"/abc123.rolledback"))
	assert.False(t, s.exists(accessDir+"/run"))
	assert.False(t, s.exists(accessDir+"/next-table.nft"))
}

func TestAccessRollbackClosesOnTheCommittedTableWhenItsOperatorsAreRejected(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	require.NoError(t, s.arm(8, "198.51.100.0/24"))
	s.reject("203.0.113.0/24")
	s.advance(accessRollbackSeconds + 1)
	_, err := s.script("rollback abc123")
	require.NoError(t, err)
	assert.Equal(t, "7", s.kernelGeneration())
	assert.False(t, s.kernelAdmitsOperators())
	assert.Equal(t, "closed\n", s.file(accessDir+"/abc123.outcome"))
}

func TestAccessRollbackClosesOnTheCandidateTableWhenTheCommittedOneNoLongerLoads(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	require.NoError(t, s.arm(8, "198.51.100.0/24"))
	_, err := s.script("candidate abc123")
	require.NoError(t, err)
	s.reject("generation 7")
	s.advance(accessRollbackSeconds + 1)
	_, err = s.script("rollback abc123")
	require.NoError(t, err)
	assert.Equal(t, "8", s.kernelGeneration())
	assert.False(t, s.kernelAdmitsOperators(), "the candidate's table alone, closed for the new topology")
	assert.Equal(t, "closed\n", s.file(accessDir+"/abc123.outcome"))
	assert.True(t, s.exists(accessDir+"/closed"))
}

func TestAccessRollbackThatLoadsNothingLeavesTheCandidateAndSaysSo(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	require.NoError(t, s.arm(8, "198.51.100.0/24"))
	_, err := s.script("candidate abc123")
	require.NoError(t, err)
	s.reject("kipper-access generation")
	s.advance(accessRollbackSeconds + 1)
	_, err = s.script("rollback abc123")
	assert.Error(t, err)
	assert.Equal(t, "8", s.kernelGeneration(), "the requested list stays live, never wider")
	assert.True(t, s.kernelAdmitsOperators())
	assert.Equal(t, "failed\n", s.file(accessDir+"/abc123.outcome"))
	assert.True(t, s.exists(accessDir+"/failed"))
	assert.False(t, s.exists(accessDir+"/next-table.nft"))
}

func TestAccessRollbackOfAFirstApplyRemovesTheTable(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	require.NoError(t, s.arm(1, "198.51.100.0/24"))
	_, err := s.script("candidate abc123")
	require.NoError(t, err)
	s.advance(accessRollbackSeconds + 1)
	_, err = s.script("rollback abc123")
	require.NoError(t, err)
	assert.False(t, s.exists("kernel"))
	assert.Equal(t, "removed\n", s.file(accessDir+"/abc123.outcome"))
}

func TestAccessRollbackOfAFirstApplyThatCannotDeleteTheTableSaysSo(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	require.NoError(t, s.arm(1, "198.51.100.0/24"))
	_, err := s.script("candidate abc123")
	require.NoError(t, err)
	s.write("nft-delete-fails", "")
	s.advance(accessRollbackSeconds + 1)
	_, err = s.script("rollback abc123")
	require.Error(t, err)
	assert.True(t, s.exists("kernel"), "the candidate is still live")
	assert.Equal(t, "failed\n", s.file(accessDir+"/abc123.outcome"))
	assert.True(t, s.exists(accessDir+"/failed"))
}

func TestAccessRollbackOfAFirstApplyTreatsAnUnreadableTableAsAFailure(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	require.NoError(t, s.arm(1, "198.51.100.0/24"))
	nft := filepath.Join(s.bin, "nft")
	//nolint:gosec // the stand-in has to be executable
	require.NoError(t, os.WriteFile(nft, []byte("#!/bin/sh\necho 'Error: Operation not permitted' >&2\nexit 1\n"), 0o755))
	s.advance(accessRollbackSeconds + 1)
	_, err := s.script("rollback abc123")
	require.Error(t, err)
	assert.Equal(t, "failed\n", s.file(accessDir+"/abc123.outcome"))
}

func TestAccessRollbackOfAFirstApplyThatNeverLoadedIsRemovedInAnyLocale(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	require.NoError(t, s.arm(1, "198.51.100.0/24"))
	s.advance(accessRollbackSeconds + 1)
	_, err := s.script("rollback abc123")
	require.NoError(t, err)
	assert.Equal(t, "removed\n", s.file(accessDir+"/abc123.outcome"))
}

func TestAccessRollbackLeavesACommittedOrForeignRunAlone(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	require.NoError(t, s.arm(8, "198.51.100.0/24"))
	s.write(accessDir+"/abc123.committed", "")
	s.advance(accessRollbackSeconds + 1)
	_, err := s.script("rollback abc123")
	require.NoError(t, err)
	assert.False(t, s.exists(accessDir+"/abc123.outcome"))

	require.NoError(t, os.Remove(s.path(accessDir+"/abc123.committed")))
	s.write(accessDir+"/run", "def456\n")
	_, err = s.script("rollback abc123")
	require.NoError(t, err)
	assert.False(t, s.exists(accessDir+"/abc123.outcome"))
}

func TestAccessRollbackWaitsForTheDeadline(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	require.NoError(t, s.arm(8, "198.51.100.0/24"))
	s.advance(100)
	_, err := s.script("rollback abc123")
	require.NoError(t, err)
	assert.Contains(t, s.calls(), "sleep 80")
	assert.Equal(t, "restored\n", s.file(accessDir+"/abc123.outcome"))
}

func TestAccessLockedRefusesAfterARollbackOrWithoutTheReservation(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	require.NoError(t, s.arm(8, "198.51.100.0/24"))
	_, err := s.shell(s.r.locked("touch "+s.path("ran")), nil)
	require.NoError(t, err)
	require.True(t, s.exists("ran"))

	require.NoError(t, os.Remove(s.path("ran")))
	s.write(accessDir+"/abc123.rolledback", "")
	out, err := s.shell(s.r.locked("touch "+s.path("ran")), nil)
	assert.Error(t, err)
	assert.Contains(t, out, accessRefusedMarker)
	assert.False(t, s.exists("ran"))

	require.NoError(t, os.Remove(s.path(accessDir+"/abc123.rolledback")))
	require.NoError(t, os.Remove(s.path(accessDir+"/run")))
	_, err = s.shell(s.r.locked("touch "+s.path("ran")), nil)
	assert.Error(t, err)
	assert.False(t, s.exists("ran"))
	assert.NotContains(t, s.r.locked("true"), accessRefusedMarker, "the command text never carries the marker")
}

func TestAccessReserveRefusesAnOlderGenerationAndAcceptsAnEqualOne(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"table.nft", "operators.nft"} {
		s := newAccessScene(t)
		s.commit("203.0.113.0/24")
		s.write(accessDir+"/"+file, "# kipper-access generation 9\n")
		err := s.arm(8, "198.51.100.0/24")
		require.Error(t, err, file)
		assert.False(t, s.exists(accessDir+"/run"), file)
		assert.NoError(t, s.arm(9, "198.51.100.0/24"), "%s: node add and remove re-render under the record's generation", file)
	}
}

func TestAccessReserveRefusesAHostWithoutK3s(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	require.NoError(t, os.Remove(s.path(accessK3sBinary)))
	table, _ := RenderAccessTable(s.intent(1, "198.51.100.0/24"))
	ops, _ := RenderAccessOperators(s.intent(1, "198.51.100.0/24"))
	out, err := s.shell(s.r.reserveAndArmCommand(table, ops, 1), nil)
	assert.Error(t, err)
	assert.Contains(t, out, "k3s is not installed on this host")
	assert.False(t, s.exists(accessDir+"/run"))
}

func TestAccessReserveRefusesWhileAnotherChangeIsActive(t *testing.T) {
	t.Parallel()
	for _, unit := range []string{"kipper-access-rollback-def456.timer", "kipper-access-rollback-def456.service", "kipper-access-load-def456.service"} {
		s := newAccessScene(t)
		s.write("units", unit+" loaded active waiting\n")
		err := s.arm(1, "198.51.100.0/24")
		assert.Error(t, err, unit)
		assert.False(t, s.exists(accessDir+"/run"), unit)
	}
}

func TestAccessReserveSweepsMarkersOlderThanADay(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.write(accessDir+"/old.outcome", "restored\n")
	s.write(accessDir+"/new.outcome", "restored\n")
	old := time.Now().Add(-25 * time.Hour)
	require.NoError(t, os.Chtimes(s.path(accessDir+"/old.outcome"), old, old))
	require.NoError(t, s.arm(1, "198.51.100.0/24"))
	assert.False(t, s.exists(accessDir+"/old.outcome"))
	assert.True(t, s.exists(accessDir+"/new.outcome"))
}

func TestAccessReserveInstallsTheUnitAndArmsTheTimer(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	require.NoError(t, s.arm(1, "198.51.100.0/24"))
	info, err := os.Stat(s.path(accessDir))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	for _, f := range []string{"next-table.nft", "next-operators.nft", "access.sh", "run", "abc123.deadline"} {
		info, err := os.Stat(s.path(accessDir + "/" + f))
		require.NoError(t, err, f)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), f)
	}
	assert.Equal(t, accessUnit(), s.file(accessUnitPath))
	unit, err := os.Stat(s.path(accessUnitPath))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), unit.Mode().Perm(), "The unit must be readable by everyone to avoid systemd warnings.")
	assert.True(t, s.exists("enabled-"+accessUnitName))
	calls := s.calls()
	assert.Contains(t, calls, "sync -f "+s.path(accessDir))
	assert.Less(t, strings.Index(calls, "systemctl enable"), strings.Index(calls, "sync -f"), "the unit is enabled before the candidates are synced")
	assert.Contains(t, calls, "systemd-run --quiet --unit=kipper-access-rollback-abc123 --on-active=180 --timer-property=RemainAfterElapse=no --timer-property=AccuracySec=1s /bin/sh "+s.path(accessDir+"/access.sh")+" rollback abc123")
	assert.Less(t, strings.Index(calls, "sync -f"), strings.Index(calls, "systemd-run"))

	require.NoError(t, os.WriteFile(s.log, nil, 0o600))
	s.r = accessRun{id: "def456", root: s.root}
	require.NoError(t, s.arm(1, "198.51.100.0/24"))
	assert.NotContains(t, s.calls(), "daemon-reload", "an unchanged unit is not reloaded")
}

func TestAccessReserveStopsWhenAFileCannotBeWritten(t *testing.T) {
	for _, name := range []string{"access.sh", "next-table.nft", "next-operators.nft"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newAccessScene(t)
			s.commit("203.0.113.0/24")
			before := s.file(accessDir + "/access.sh")
			s.write(accessDir+"/access.sh", before)
			require.NoError(t, os.MkdirAll(s.path(accessDir+"/"+name+".kip-new"), 0o700))
			require.Error(t, s.arm(8, "198.51.100.0/24"))
			assert.Equal(t, before, s.file(accessDir+"/access.sh"), "the script stays as it was")
			assert.False(t, s.exists(accessDir+"/run"), "no reservation is taken")
			assert.NotContains(t, s.calls(), "systemd-run", "no rollback is armed")
		})
	}
}

func TestWriteIfChangedChecksTheWrittenSize(t *testing.T) {
	assert.Contains(t, writeIfChanged("/x", "abc\n", "", ""), "[ \"$(wc -c < /x.kip-new)\" -eq 4 ] || exit 1")
}

func TestAccessReserveMakesAnUnchangedPrivateUnitReadable(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.write(accessUnitPath, accessUnit())
	require.NoError(t, s.arm(1, "198.51.100.0/24"))
	unit, err := os.Stat(s.path(accessUnitPath))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), unit.Mode().Perm())
}

func TestAccessReserveMakesTheUnitReadableBeforeSystemdReloads(t *testing.T) {
	cmd := accessRun{id: "abc123"}.reserveAndArmCommand("t", "o", 1)
	chmod := strings.Index(cmd, "chmod 0644 "+accessUnitPath+".kip-new")
	require.GreaterOrEqual(t, chmod, 0)
	assert.Less(t, chmod, strings.Index(cmd, "daemon-reload"))
}

func TestAccessReserveOpensTheLockForAppend(t *testing.T) {
	r := accessRun{id: "abc123"}
	assert.Contains(t, r.reserveAndArmCommand("t", "o", 1), "exec 9>>"+accessLock)
	assert.Contains(t, r.locked("true"), "exec 9>>"+accessLock)
	assert.Contains(t, accessScript(""), "exec 9>>"+accessLock)
	assert.NotContains(t, r.reserveAndArmCommand("t", "o", 1), "exec 9>"+accessLock)
}

func TestAccessCommitRenamesOperatorsThenTableThenSyncs(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	require.NoError(t, s.arm(8, "198.51.100.0/24"))
	cmd := s.r.commitCommand()
	ops := strings.Index(cmd, "next-operators.nft")
	table := strings.Index(cmd, "next-table.nft")
	sync := strings.Index(cmd, "sync -f")
	assert.Less(t, ops, table)
	assert.Less(t, table, sync)
	s.write(accessDir+"/closed", "")
	_, err := s.shell(cmd, nil)
	require.NoError(t, err)
	gen, _ := AccessHeaderGeneration(s.file(accessDir + "/table.nft"))
	assert.Equal(t, 8, gen)
	gen, _ = AccessHeaderGeneration(s.file(accessDir + "/operators.nft"))
	assert.Equal(t, 8, gen)
	assert.True(t, s.exists(accessDir+"/abc123.committed"))
	assert.False(t, s.exists(accessDir+"/run"))
	assert.False(t, s.exists(accessDir+"/closed"))
	assert.Contains(t, s.calls(), "systemctl stop kipper-access-rollback-abc123.timer")
	assert.Contains(t, s.calls(), "restorecon -R "+s.path(accessDir))

	s.advance(accessRollbackSeconds + 1)
	_, err = s.script("rollback abc123")
	require.NoError(t, err)
	assert.False(t, s.exists(accessDir+"/abc123.outcome"), "a committed change is never rolled back")
}

func TestAccessCommitAfterARollbackIsRefused(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	require.NoError(t, s.arm(8, "198.51.100.0/24"))
	s.advance(accessRollbackSeconds + 1)
	_, err := s.script("rollback abc123")
	require.NoError(t, err)
	_, err = s.shell(s.r.commitCommand(), nil)
	assert.Error(t, err)
	gen, _ := AccessHeaderGeneration(s.file(accessDir + "/table.nft"))
	assert.Equal(t, 7, gen, "a candidate that is not live is never committed")
}

func TestAccessRemovalStopsUnitsFirstAndLeavesTheLock(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	require.NoError(t, s.arm(8, "198.51.100.0/24"))
	_, err := s.script("load")
	require.NoError(t, err)
	s.write("units", "kipper-access-rollback-abc123.timer loaded active waiting\nkipper-access-load-def456.service loaded activating start\n")
	require.NoError(t, os.WriteFile(s.log, nil, 0o600))
	_, err = s.shell(removeAccessCommand(s.root), nil)
	require.NoError(t, err)
	calls := s.calls()
	assert.Contains(t, calls, "systemctl stop kipper-access-rollback-abc123.timer")
	assert.Contains(t, calls, "systemctl stop kipper-access-load-def456.service")
	assert.Less(t, strings.Index(calls, "stop kipper-access-load"), strings.Index(calls, "disable"))
	assert.Less(t, strings.Index(calls, "disable"), strings.Index(calls, "nft delete table"))
	assert.False(t, s.exists("kernel"))
	assert.False(t, s.exists(accessDir))
	assert.False(t, s.exists(accessUnitPath))
	assert.False(t, s.exists("enabled-"+accessUnitName))
	assert.True(t, s.exists(accessLock), "waiters keep the same lock inode")

	_, err = s.shell(removeAccessCommand(s.root), nil)
	assert.NoError(t, err, "removing a removed guard is not an error")
}

func TestAccessRemovalReportsAnUnexpectedNftError(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.write(accessDir+"/table.nft", "x")
	nft := filepath.Join(s.bin, "nft")
	//nolint:gosec // the stand-in has to be executable
	require.NoError(t, os.WriteFile(nft, []byte("#!/bin/sh\necho 'Error: Operation not permitted' >&2\nexit 1\n"), 0o755))
	_, err := s.shell(removeAccessCommand(s.root), nil)
	assert.Error(t, err)
	assert.True(t, s.exists(accessDir+"/table.nft"), "the files stay while the table may still be live")
}

func TestAccessRemovalOnAHostThatNeverHadKipperState(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	require.NoError(t, os.RemoveAll(s.path("/etc/kipper")))
	_, err := s.shell(removeAccessCommand(s.root), nil)
	require.NoError(t, err)
	assert.False(t, s.exists("/etc/kipper"), "removal creates no state")
}

func TestAccessRemovalWithoutNftRefusesWhileTheGuardHasFiles(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	require.NoError(t, os.Remove(filepath.Join(s.bin, "nft")))
	out, err := s.shell(removeAccessCommand(s.root), nil)
	require.Error(t, err)
	assert.Contains(t, out, "Install nftables")
	assert.True(t, s.exists(accessDir+"/table.nft"))

	require.NoError(t, os.RemoveAll(s.path(accessDir)))
	_, err = s.shell(removeAccessCommand(s.root), nil)
	assert.NoError(t, err, "a host that never had a guard needs no nft")
}

func TestClearKipperStateKeepsTheAccessLock(t *testing.T) {
	assert.Contains(t, clearKipperStateCommand(""), "! -name access.lock")
}

func TestApplyAccessGuardCommitsAfterTheChecks(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	outcome, err := s.apply(8, "198.51.100.0/24")
	require.NoError(t, err)
	assert.Equal(t, AccessCommitted, outcome)
	gen, _ := AccessHeaderGeneration(s.file(accessDir + "/operators.nft"))
	assert.Equal(t, 8, gen)
	assert.Equal(t, "8", s.kernelGeneration())

	check, reserve, load, observe, readyz, commit := -1, -1, -1, -1, -1, -1
	for i, c := range s.ran {
		switch {
		case strings.Contains(c, "nft -c -f -"):
			check = i
		case strings.Contains(c, "systemd-run --quiet --unit="+accessRollbackUnit):
			reserve = i
		case strings.HasPrefix(c, "systemd-run --wait --pipe --quiet --collect --unit="+accessLoadUnit+"-"):
			load = i
			assert.Contains(t, c, " candidate ")
		case c == "observe IPv4":
			observe = i
		case c == "readyz IPv4":
			readyz = i
		case strings.Contains(c, "next-operators.nft "+s.path(accessDir)+"/operators.nft"):
			commit = i
		}
	}
	assert.True(t, check >= 0 && check < reserve, "nft -c before anything is written")
	assert.True(t, reserve < load)
	assert.True(t, load < observe)
	assert.True(t, observe < readyz)
	assert.True(t, readyz < commit, "the renames come after every check")
	assert.Contains(t, s.ran[observe-1], "/bin/sh -c 'true'", "the deadline is renewed before each check")
	assert.Contains(t, s.ran[readyz-1], "/bin/sh -c 'true'")
	for _, c := range s.ran[commit:] {
		assert.NotContains(t, c, "restart")
	}
}

func (s *accessScene) apply(gen int, admin ...string) (string, error) {
	return ApplyAccessGuard(AccessApply{
		Node: s, Base: s, Label: "server", Intent: s.intent(gen, admin...), Families: []string{"4"},
		ReadyzOn: []string{"4"}, Readyz: func(f string) error { s.ran = append(s.ran, "readyz IPv"+f); return s.readyzErr() },
		Out: io.Discard, root: s.root,
	})
}

func (s *accessScene) readyzErr() error {
	if s.exists("readyz-fails") {
		return errors.New("connection timed out")
	}
	return nil
}

func TestApplyAccessGuardRollsBackWhenNewConnectionsFail(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	s.observedErr = errors.New("connection timed out")
	outcome, err := s.apply(8, "198.51.100.0/24")
	require.Error(t, err)
	assert.Equal(t, AccessRestored, outcome)
	assert.Contains(t, err.Error(), "more than one outgoing address")
	assert.Contains(t, err.Error(), "kip restored the previous access table")
	assert.Equal(t, "7", s.kernelGeneration())
	gen, _ := AccessHeaderGeneration(s.file(accessDir + "/table.nft"))
	assert.Equal(t, 7, gen)
}

func TestApplyAccessGuardRollsBackWhenTheSessionIsNotAdmitted(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	s.observed["4"] = "192.0.2.99"
	outcome, err := s.apply(8, "198.51.100.0/24")
	require.Error(t, err)
	assert.Equal(t, AccessRestored, outcome)
	assert.Contains(t, err.Error(), "192.0.2.99")
}

func TestApplyAccessGuardRollsBackWhenTheAPIStopsAnswering(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	s.write("readyz-fails", "")
	outcome, err := s.apply(8, "198.51.100.0/24")
	require.Error(t, err)
	assert.Equal(t, AccessRestored, outcome)
	assert.Equal(t, "7", s.kernelGeneration())
}

func TestApplyAccessGuardFirstApplyRollsBackToNoTable(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.observedErr = errors.New("connection timed out")
	outcome, err := s.apply(1, "198.51.100.0/24")
	require.Error(t, err)
	assert.Equal(t, AccessRemoved, outcome)
	assert.False(t, s.exists("kernel"))
}

func TestApplyAccessGuardStopsWhenNftRejectsTheRender(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.reject("table inet kipper_access {")
	_, err := s.apply(1, "198.51.100.0/24")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the live access table was left unchanged")
	assert.False(t, s.exists(accessDir+"/run"))
}

func TestApplyAccessGuardRefusesWhileNftablesServiceIsEnabled(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.write("enabled-nftables.service", "")
	_, err := s.apply(1, "198.51.100.0/24")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "systemctl disable --now nftables")
	assert.False(t, s.exists(accessDir))
}

func TestApplyAccessGuardReportsALoadRefusedAfterAUnitRestart(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	// The boot unit ran in the middle of the change: the committed pair is
	// live and the reservation is gone.
	s.observedErr = nil
	outcome, err := ApplyAccessGuard(AccessApply{
		Node: restartingNode{s}, Base: s, Label: "server", Intent: s.intent(8, "198.51.100.0/24"), Families: []string{"4"},
		Out: io.Discard, root: s.root,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "restarted during the change")
	assert.Equal(t, AccessRefused, outcome, "nothing of this run was committed")
	assert.Equal(t, "7", s.kernelGeneration())
}

// restartingNode runs the boot unit's load just before the candidate load.
type restartingNode struct{ *accessScene }

func (n restartingNode) Run(command string) (string, error) {
	if strings.HasPrefix(command, "systemd-run --wait") {
		if out, err := n.script("load"); err != nil {
			return out, err
		}
	}
	return n.accessScene.Run(command)
}

func TestAccessStateReadsTheHost(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	st, err := parseAccessState(mustShell(t, s, accessStateCommand(s.root)))
	require.NoError(t, err)
	assert.False(t, st.HasTable())
	assert.Equal(t, AccessNodeHeader{Name: "server"}, st.Header("server"))

	s.commit("203.0.113.0/24", "2001:db8::/48")
	s.write("enabled-"+accessUnitName, "")
	s.write("active-"+accessUnitName, "")
	_, err = s.script("load")
	require.NoError(t, err)
	st, err = parseAccessState(mustShell(t, s, accessStateCommand(s.root)))
	require.NoError(t, err)
	assert.True(t, st.HasTable())
	assert.True(t, st.Live)
	assert.Equal(t, "enabled", st.UnitEnabled)
	assert.Equal(t, "active", st.UnitActive)
	assert.Equal(t, AccessNodeHeader{Name: "server", Generation: 7, Present: true, Sources: 2}, st.Header("server"))
	assert.Equal(t, "", st.Problem())

	s.write(accessDir+"/closed", "")
	st, _ = parseAccessState(mustShell(t, s, accessStateCommand(s.root)))
	assert.Contains(t, st.Problem(), "booted closed and refuses every operator address")
}

func TestLeftoverAccessWarningNamesATableWithoutARecord(t *testing.T) {
	st := committedState(t, "203.0.113.0/24")
	state := st.Table + accessStateSeparator + "\n" + st.Operators + accessStateSeparator + "\nlive\n"
	host := &routedRunner{routes: []route{{contains: "cat /etc/kipper/access/table.nft", out: state}}}
	assert.Contains(t, LeftoverAccessWarning(host), "restricts SSH and 6443 to one source")

	host.routes = append([]route{{contains: "get configmap kipper-access", out: accessRecordJSON(t, sampleAccessRecord())}}, host.routes...)
	assert.Empty(t, LeftoverAccessWarning(host), "a cluster with a record owns its table")

	assert.Empty(t, LeftoverAccessWarning(&routedRunner{}), "a host without a table")
}

func TestAccessStateProblems(t *testing.T) {
	ok := AccessHostState{Table: "# kipper-access generation 3\n", Live: true, UnitActive: "active"}
	assert.Equal(t, "", ok.Problem())
	for want, st := range map[string]AccessHostState{
		"access table missing":              {},
		"access service failed at boot":     {Table: ok.Table, UnitActive: "failed"},
		"booted closed":                     {Table: ok.Table, Live: true, Closed: true},
		"last change could not be restored": {Table: ok.Table, Live: true, Failed: true},
		"missing from the kernel":           {Table: ok.Table, UnitActive: "active"},
	} {
		assert.Contains(t, st.Problem(), want)
	}
}

func TestAddJoiningAddressesDeduplicatesUnderTheLockAlone(t *testing.T) {
	r := &routedRunner{}
	require.NoError(t, AddJoiningAddresses(r, []netip.Addr{
		netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("203.0.113.20"), netip.MustParseAddr("203.0.113.20"),
	}))
	require.Len(t, r.commands, 1)
	assert.Contains(t, r.commands[0], "nft add element inet kipper_access joining4 { 10.0.0.5, 203.0.113.20 }")
	assert.Contains(t, r.commands[0], "flock -w 120 9")
	assert.NotContains(t, r.commands[0], "run", "no reservation and no deadline")
}

func TestJoinerAddressesAreItsInterfacesAndItsExternalAddress(t *testing.T) {
	joiner := &routedRunner{routes: []route{{contains: "ip -o -4 addr show", out: "2: eth0    inet 10.0.0.5/24 brd 10.0.0.255 scope global eth0\n3: eth1    inet 203.0.113.20/32 scope global eth1\n"}}}
	addrs, err := JoinerAddresses(joiner, "203.0.113.20")
	require.NoError(t, err)
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("203.0.113.20"), netip.MustParseAddr("203.0.113.20")}, addrs)

	_, err = JoinerAddresses(&routedRunner{routes: []route{{contains: "ip -o", err: errors.New("broken pipe")}}}, "203.0.113.20")
	require.Error(t, err)
}

func TestAccessIntentForAWorkerAdmitsTheServerOnSSH(t *testing.T) {
	records := []NodeRecord{
		peerRecord("server", serverID, NodeRoleServer, NodeStateReady, "192.0.2.10"),
		peerRecord("worker-1", worker1, NodeRoleWorker, NodeStateReady, "192.0.2.11"),
	}
	pod := netip.MustParsePrefix("10.42.0.0/16")
	in, err := AccessIntentFor(3, prefixes(t, "198.51.100.0/24"), []int{22}, pod, records, worker1, "")
	require.NoError(t, err)
	assert.Equal(t, "192.0.2.10", in.Server4.String())
	assert.Equal(t, prefixes(t, "10.42.0.0/16", "192.0.2.10/32", "192.0.2.11/32"), in.Cluster4)

	in, err = AccessIntentFor(3, prefixes(t, "198.51.100.0/24"), []int{22}, pod, records, serverID, "")
	require.NoError(t, err)
	assert.False(t, in.Server4.IsValid(), "the server admits no worker on its SSH ports")
	assert.Equal(t, prefixes(t, "10.42.0.0/16", "192.0.2.10/32", "192.0.2.11/32"), in.Cluster4)
}

// TestAccessIntentForAServerAdmitsItsOwnExternalAddressOnTheAPI covers the rule
// needed when a public-mode server's pods reach its advertised external API
// address through provider NAT and their traffic returns with that address as
// the source.
func TestAccessIntentForAServerAdmitsItsOwnExternalAddressOnTheAPI(t *testing.T) {
	records := []NodeRecord{
		{Name: "server", MachineID: serverID, Role: NodeRoleServer, State: NodeStateReady, Mode: NetworkModePublic, PeerAddress: "203.0.113.10"},
		{Name: "worker-1", MachineID: worker1, Role: NodeRoleWorker, State: NodeStateReady, Mode: NetworkModePublic, PeerAddress: "203.0.113.11"},
	}
	pod := netip.MustParsePrefix("10.42.0.0/16")
	in, err := AccessIntentFor(3, prefixes(t, "198.51.100.0/24"), []int{22}, pod, records, serverID, "")
	require.NoError(t, err)
	assert.Contains(t, in.Cluster4, netip.MustParsePrefix("203.0.113.10/32"))

	table, err := RenderAccessTable(in)
	require.NoError(t, err)
	assert.Contains(t, table, "set cluster4 { type ipv4_addr; flags interval; elements = { 10.42.0.0/16, 203.0.113.10, 203.0.113.11 } }")
	assert.Contains(t, table, "tcp dport 6443 ip saddr @cluster4 accept")
}

func mustShell(t *testing.T, s *accessScene, command string) string {
	t.Helper()
	out, err := s.shell(command, nil)
	require.NoError(t, err, out)
	return out
}

func liveFixture(t *testing.T, sets map[string][]string, chain accessChainShape, check string) AccessLive {
	t.Helper()
	live := AccessLive{Sets: map[string][]netip.Prefix{}, Chain: chain, CommittedCheck: check}
	for name, ps := range sets {
		live.Sets[name] = prefixes(t, ps...)
	}
	return live
}

func committedState(t *testing.T, admin ...string) AccessHostState {
	t.Helper()
	in := AccessIntent{Generation: 7, Admin: prefixes(t, admin...), Ports: []int{22}, Cluster4: prefixes(t, "10.42.0.0/16", "192.0.2.11/32")}
	table, err := RenderAccessTable(in)
	require.NoError(t, err)
	ops, err := RenderAccessOperators(in)
	require.NoError(t, err)
	return AccessHostState{Table: table, Operators: ops, Live: true, UnitEnabled: "enabled", UnitActive: "active"}
}

var goodChain = accessChainShape{Hook: "input", Priority: -10, Policy: "accept", Rules: accessChainRules}

func TestAccessDriftOfAMatchingHostIsEmpty(t *testing.T) {
	st := committedState(t, "203.0.113.0/24", "2001:db8::/48")
	live := liveFixture(t, map[string][]string{
		"admin4": {"203.0.113.0/25", "203.0.113.128/25"}, "admin6": {"2001:db8::/48"},
		"cluster4": {"10.42.0.0/16", "192.0.2.11/32"}, "joining4": nil, "server4": nil,
	}, goodChain, "")
	assert.Empty(t, AccessDrift(st, live))
}

func TestAccessDriftNamesEveryDifference(t *testing.T) {
	st := committedState(t, "203.0.113.0/24")
	st.UnitEnabled = "disabled"
	st.NftablesService = true
	live := liveFixture(t, map[string][]string{
		"admin4": {"203.0.113.0/24", "198.51.100.9/32"}, "admin6": nil,
		"cluster4": {"10.42.0.0/16"}, "joining4": {"203.0.113.20/32"}, "server4": nil,
	}, accessChainShape{Hook: "input", Priority: 0, Policy: "accept", Rules: 7}, "Error: syntax error")
	drift := strings.Join(AccessDrift(st, live), "\n")
	assert.Contains(t, drift, "admin4")
	assert.Contains(t, drift, "live admin4 holds { 198.51.100.9, 203.0.113.0/24 } but the committed file lists { 203.0.113.0/24 }")
	assert.Contains(t, drift, "cluster4")
	assert.Contains(t, drift, "live joining4 holds 1 addresses from a join that did not finish. 'kip cluster access apply' or a reboot clears them")
	assert.Contains(t, drift, "priority 0")
	assert.Contains(t, drift, "the installed nft rejects the committed files")
	assert.Contains(t, drift, "not enabled")
	assert.Contains(t, drift, "nftables.service")
}

func TestAccessDriftOfAClosedHostExpectsNoOperators(t *testing.T) {
	st := committedState(t, "203.0.113.0/24")
	st.Closed = true
	live := liveFixture(t, map[string][]string{"cluster4": {"10.42.0.0/16", "192.0.2.11/32"}}, goodChain, "")
	drift := strings.Join(AccessDrift(st, live), "\n")
	assert.Contains(t, drift, "booted closed")
	assert.NotContains(t, drift, "admin4", "a closed boot is reported once, not as set drift")
}

func TestAccessDriftOfAMissingTable(t *testing.T) {
	st := committedState(t, "203.0.113.0/24")
	st.Live = false
	drift := AccessDrift(st, AccessLive{})
	require.Len(t, drift, 1)
	assert.Contains(t, drift[0], "missing from the kernel")
}

func TestReadAccessLiveParsesTheListing(t *testing.T) {
	s := newAccessScene(t)
	t.Parallel()
	chain, err := os.ReadFile(filepath.Join("testdata", "access", "chain-input.json"))
	require.NoError(t, err)
	set, err := os.ReadFile(filepath.Join("testdata", "access", "set-admin4-1.0.9.json"))
	require.NoError(t, err)
	nft := "#!/bin/sh\ncase \"$*\" in\n" +
		"  '-j list set inet kipper_access admin4') cat " + s.path("set.json") + " ;;\n" +
		"  '-j list set inet kipper_access '*) echo '{\"nftables\":[{\"set\":{\"name\":\"x\"}}]}' ;;\n" +
		"  '-j list chain inet kipper_access input') cat " + s.path("chain.json") + " ;;\n" +
		"  '-c -f -') cat >/dev/null; echo 'Error: syntax error' >&2; exit 1 ;;\nesac\n"
	s.write("set.json", string(set))
	s.write("chain.json", string(chain))
	//nolint:gosec // the stand-in has to be executable
	require.NoError(t, os.WriteFile(filepath.Join(s.bin, "nft"), []byte(nft), 0o755))
	s.commit("203.0.113.0/24")
	live, err := parseAccessLive(mustShell(t, s, accessLiveCommand(s.root)))
	require.NoError(t, err)
	assert.True(t, sameAddressSet(live.Sets["admin4"], prefixes(t, "203.0.113.0/24", "198.51.100.7/32")))
	assert.Empty(t, live.Sets["joining4"])
	assert.Equal(t, int64(12), live.Chain.Dropped)
	assert.Contains(t, live.CommittedCheck, "syntax error")
}

// observeFunc is an AddressObserver for one path to the node.
type observeFunc func(family string) (string, error)

func (f observeFunc) ObservedAddress(family string) (string, error) { return f(family) }

func (s *accessScene) applyWithTunnel(gen int, families []string, tunnel observeFunc, admin ...string) (string, error) {
	in := s.intent(gen, admin...)
	in.Devices = true
	return ApplyAccessGuard(AccessApply{
		Node: s, Base: s, Label: "server", Intent: in, Families: families,
		Tunnel: observeFunc(func(f string) (string, error) { s.ran = append(s.ran, "tunnel IPv"+f); return tunnel(f) }),
		Device: netip.MustParseAddr("10.99.0.129"),
		Out:    io.Discard, root: s.root,
	})
}

func TestApplyAccessGuardChecksTheDeviceTunnel(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	outcome, err := s.applyWithTunnel(8, []string{"4"}, func(string) (string, error) { return "10.99.0.129", nil }, "198.51.100.0/24")
	require.NoError(t, err)
	assert.Equal(t, AccessCommitted, outcome)
	assert.Contains(t, s.ran, "observe IPv4", "the direct path is checked too")
	assert.Contains(t, s.ran, "tunnel IPv4")
}

func TestApplyAccessGuardWithOnlyTheTunnelLeftCommits(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	outcome, err := s.applyWithTunnel(8, nil, func(string) (string, error) { return "10.99.0.129", nil })
	require.NoError(t, err)
	assert.Equal(t, AccessCommitted, outcome)
	assert.NotContains(t, s.ran, "observe IPv4")
}

func TestApplyAccessGuardRollsBackWhenTheTunnelFails(t *testing.T) {
	t.Parallel()
	for name, tunnel := range map[string]observeFunc{
		"no connection":   func(string) (string, error) { return "", errors.New("handshake did not complete") },
		"another address": func(string) (string, error) { return "198.51.100.7", nil },
	} {
		s := newAccessScene(t)
		s.commit("203.0.113.0/24")
		outcome, err := s.applyWithTunnel(8, []string{"4"}, tunnel, "198.51.100.0/24")
		require.Error(t, err, name)
		assert.Equal(t, AccessRestored, outcome, name)
		assert.Contains(t, err.Error(), "device tunnel", name)
		assert.Equal(t, "7", s.kernelGeneration(), name)
	}
}

func TestReadOpsRevokedKeys(t *testing.T) {
	t.Parallel()
	s := newAccessScene(t)
	keys, err := readOpsRevokedKeys(s, s.root)
	require.NoError(t, err)
	assert.Empty(t, keys, "no file is no revoked key")
	s.write(opsDir+"/revoked-keys", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=\n\n  \nnot a key\n")
	keys, err = readOpsRevokedKeys(s, s.root)
	require.NoError(t, err)
	assert.Equal(t, []string{"AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="}, keys)
}
