package installer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ufwStubs writes ufw and iptables stand-ins that keep the firewall state in
// files, so the guarded enable runs in a real shell.
func ufwStubs(t *testing.T, enableExit int, linked bool) (bin, state string) {
	t.Helper()
	bin, state = t.TempDir(), t.TempDir()
	before := "-N ufw-before-input\n-A ufw-before-input -j ufw-user-input\n"
	input := "-P INPUT DROP\n"
	if linked {
		before = "-N ufw-before-input\n-A ufw-before-input -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n-A ufw-before-input -j ufw-user-input\n"
		input = "-P INPUT DROP\n-A INPUT -j ufw-before-input\n"
	}
	ufw := "#!/bin/sh\necho \"$*\" >> " + state + "/ufw.log\n" +
		"case \"$2\" in enable) printf '%s' '" + input + "' > " + state + "/INPUT; printf '%s' '" + before + "' > " + state + "/before; exit " + strings.Repeat("1", enableExit) + strings.Repeat("0", 1-enableExit) + " ;;\n" +
		"disable) printf -- '-P INPUT ACCEPT\\n' > " + state + "/INPUT; rm -f " + state + "/before ;; esac\nexit 0\n"
	iptables := "#!/bin/sh\n[ \"$1\" = -w ] && shift 2\nif [ \"$1\" = -P ]; then echo \"policy $2 $3\" >> " + state + "/ufw.log; exit 0; fi\n" +
		"case \"$2\" in INPUT) cat " + state + "/INPUT ;; ufw-before-input) cat " + state + "/before 2>/dev/null || exit 1 ;; esac\n"
	//nolint:gosec // the stand-ins have to be executable
	require.NoError(t, os.WriteFile(filepath.Join(bin, "ufw"), []byte(ufw), 0o755))
	//nolint:gosec // the stand-ins have to be executable
	require.NoError(t, os.WriteFile(filepath.Join(bin, "iptables"), []byte(iptables), 0o755))
	//nolint:gosec // the stand-ins have to be executable
	require.NoError(t, os.WriteFile(filepath.Join(bin, "ip6tables"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	//nolint:gosec // the stand-ins have to be executable
	require.NoError(t, os.WriteFile(filepath.Join(bin, "timeout"), []byte("#!/bin/sh\nshift 3\nexec \"$@\"\n"), 0o755))
	return bin, state
}

func runGuardedEnable(t *testing.T, bin string) error {
	t.Helper()
	cmd := exec.Command("sh", "-c", enableUFW(t.TempDir()))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	return cmd.Run()
}

func TestGuardedEnableKeepsAFullyLoadedFirewall(t *testing.T) {
	bin, state := ufwStubs(t, 0, true)
	require.NoError(t, runGuardedEnable(t, bin))
	log, _ := os.ReadFile(filepath.Join(state, "ufw.log"))
	assert.NotContains(t, string(log), "disable")
}

// ufw's enable can fail after setting the default deny but before linking the
// chain that accepts established traffic, which also drops kip's own session.
func TestGuardedEnableSwitchesAHalfLoadedFirewallOffOnTheHost(t *testing.T) {
	for _, tc := range []struct {
		name   string
		exit   int
		linked bool
	}{
		{"enable failed with the chains unlinked", 1, false},
		{"enable reported success with the chains unlinked", 0, false},
		{"enable failed with the chains linked", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, state := ufwStubs(t, tc.exit, tc.linked)
			require.Error(t, runGuardedEnable(t, bin))
			log, _ := os.ReadFile(filepath.Join(state, "ufw.log"))
			assert.Contains(t, string(log), "--force disable")
			assert.Contains(t, string(log), "policy INPUT ACCEPT")
		})
	}
}

func TestConfigureFirewall_ArmsTheRollbackBeforeTheFirstChange(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	require.NoError(t, configureFirewall(h, debianHost, false, FirewallMesh{}))

	arm, firstChange := -1, -1
	for i, c := range h.ran {
		if arm < 0 && strings.Contains(c, "systemd-run") {
			arm = i
			assert.Contains(t, c, "--unit="+ufwRollbackUnit+"-")
			assert.Contains(t, c, "--on-active=180")
			assert.Contains(t, c, "/etc/kipper/ufw-", "the snapshot and rollback script live in kip's state directory")
			assert.Contains(t, c, "cp -p /etc/ufw/*.rules /etc/ufw/ufw.conf", "the firewall is snapshotted before the first change")
			assert.Contains(t, c, "rollback.sh")
			assert.Contains(t, c, `-name 'ufw-*.snapshot'`, "files of runs older than a day are pruned")
		}
		if firstChange < 0 && strings.Contains(c, "DEFAULT_FORWARD_POLICY") {
			firstChange = i
		}
	}
	require.GreaterOrEqual(t, arm, 0, "no rollback timer was armed")
	assert.Less(t, arm, firstChange)
}

func TestConfigureFirewall_CancelsTheRollbackOnlyAfterAFreshConnection(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	require.NoError(t, configureFirewall(h, debianHost, false, FirewallMesh{}))

	enable, fresh, cancel := -1, -1, -1
	for i, c := range h.ran {
		switch {
		case strings.Contains(c, "ufw --force enable"):
			enable = i
		case c == "fresh connection":
			fresh = i
		case strings.Contains(c, "systemctl stop "+ufwRollbackUnit):
			cancel = i
		}
	}
	require.GreaterOrEqual(t, cancel, 0, "the rollback timer was never canceled")
	assert.Less(t, enable, fresh)
	assert.Less(t, fresh, cancel)
}

func TestConfigureFirewall_RestoresThePreviousFirewallWhenNewConnectionsFail(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	h.freshFails = true
	err := configureFirewall(h, debianHost, false, FirewallMesh{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip restored the previous firewall")
	assert.Equal(t, 1, h.rolledBackNow)
	for _, c := range h.ran {
		assert.NotContains(t, c, "systemctl stop "+ufwRollbackUnit, "only a working fresh connection may commit")
	}
}

func TestConfigureFirewall_SaysWhenTheRestoreHadToSwitchUFWOff(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	h.freshFails, h.restoreFails = true, true
	err := configureFirewall(h, debianHost, false, FirewallMesh{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "attempted to switch ufw off")
	assert.Contains(t, err.Error(), "kip cluster harden")
}

func TestConfigureFirewall_LocalRunsRestoreTooWhenTheChangeFails(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	h.local = true
	h.startFails = 100
	err := configureFirewall(h, debianHost, false, FirewallMesh{})
	require.Error(t, err)
	assert.Equal(t, 1, h.rolledBackNow, "a failed local change is not committed")
	for _, c := range h.ran {
		assert.NotContains(t, c, "systemctl stop "+ufwRollbackUnit)
	}
}

func TestConfigureFirewall_ReportsARollbackThatAlreadyRan(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	h.rolledBack = true
	err := configureFirewall(h, debianHost, false, FirewallMesh{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rollback has begun")
}

var bareUFW = regexp.MustCompile(`(^|[^0-9a-z-])ufw (--force|allow|limit|default|delete|show|status|reload)`)

func TestConfigureFirewall_BoundsEveryUFWCommand(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	h.startFails = 1
	require.NoError(t, configureFirewall(h, debianHost, false, FirewallMesh{}))
	for _, c := range h.ran {
		assert.False(t, bareUFW.MatchString(strings.ReplaceAll(c, ufwTimeout+"ufw ", "")), "ufw without a timeout: %s", c)
	}
}

func TestConfigureFirewall_StopsChangingTheFirewallOnceTheRollbackRan(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	h.rollbackAfter = 3
	err := configureFirewall(h, debianHost, false, FirewallMesh{})
	require.Error(t, err)
	assert.ErrorIs(t, err, errUFWChangeRefused)
	refused := 0
	for _, c := range h.ran {
		if strings.HasPrefix(c, "(exec 9>") {
			refused++
		}
	}
	assert.LessOrEqual(t, refused, 4, "a refused change is not retried")
	assert.False(t, h.started, "nothing started ufw again after the rollback")
}

// hostScene is a host whose firewall state lives in files under a temporary
// root: ufw and iptables stand-ins read and write it, date reads a fake clock
// and sleep advances it. The rollback's own shell runs against it unchanged.
type hostScene struct {
	t                       *testing.T
	r                       ufwRollback
	bin, log, root          string
	savedUser, savedDefault string
}

const sceneUserRules = "-A ufw-user-input -p tcp --dport 22 -j ACCEPT\n-A ufw-user-input -s 192.0.2.11 -p udp --dport 51820 -j ACCEPT\n"

func newHostScene(t *testing.T, active bool) hostScene {
	t.Helper()
	s := newUnarmedHostScene(t, active, nil)
	require.NoError(t, s.arm())
	return s
}

// newUnarmedHostScene prepares a host before the snapshot. The optional prepare
// hook runs after initial setup and UFW activation when active is true.
func newUnarmedHostScene(t *testing.T, active bool, prepare func(s hostScene)) hostScene {
	t.Helper()
	root, bin := t.TempDir(), t.TempDir()
	log := filepath.Join(root, "calls.log")
	for _, d := range []string{firewalldStateDir, "/etc/ufw", "/etc/default"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o750))
	}
	files := map[string]string{
		"/etc/ufw/before.rules": "*filter\n-A ufw-before-input -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\nCOMMIT\n",
		"/etc/ufw/user.rules":   "*filter\n" + sceneUserRules + "COMMIT\n",
		"/etc/ufw/ufw.conf":     "ENABLED=yes\n",
		"/etc/default/ufw":      "IPV6=no\nDEFAULT_FORWARD_POLICY=\"DROP\"\n",
		"/clock":                "1000\n",
	}
	for name, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(body), 0o600))
	}
	st := root + "/state"
	ufw := `echo "ufw $*" >> ` + log + `
case "$1 $2" in
  "status "*)
    [ -e ` + root + `/status-fails ] && exit 1
    [ -e ` + root + `/status-garbled ] && { echo "ERROR: problem running ufw-init"; exit 0; }
    [ -e ` + root + `/status-warns ] && echo "WARN: '/etc/ufw/user.rules' is group writable" >&2
    if [ -e ` + st + `-active ]; then echo "Status: active"; else echo "Status: inactive"; fi ;;
  "--force enable")
    [ -e ` + root + `/enable-fails ] && exit 1
    touch ` + st + `-active
    printf -- '-P INPUT DROP\n-A INPUT -j ufw-before-input\n' > ` + st + `-INPUT
    { grep -- '^-A ufw-before-input' ` + root + `/etc/ufw/before.rules; echo '-A ufw-before-input -j ufw-user-input'; } > ` + st + `-before
    grep -- '^-[AN] operator-' ` + root + `/etc/ufw/before.rules > ` + st + `-custom; true
    grep -- '-A ufw-user-input' ` + root + `/etc/ufw/user.rules > ` + st + `-user
    printf -- '-P INPUT DROP\n-A INPUT -j ufw6-before-input\n' > ` + st + `6-INPUT
    [ -e ` + root + `/ipv6-unlinked ] || printf -- '-A ufw6-before-input -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n-A ufw6-before-input -j ufw6-user-input\n' > ` + st + `6-before
    grep -- '-A ufw6-user-input' ` + root + `/etc/ufw/user6.rules > ` + st + `6-user 2>/dev/null; true ;;
  "--force disable") rm -f ` + st + `-active ` + st + `-before ` + st + `-user ` + st + `6-before ` + st + `6-user; printf -- '-P INPUT ACCEPT\n' > ` + st + `-INPUT ;;
esac
exit 0`
	ipt := func(name, file, chain string) string {
		return `[ "$1" = -w ] && shift 2
if [ "$1" = -P ]; then echo "` + name + ` -P $2 $3" >> ` + log + `; sed -i.bak "1s/.*/-P $2 $3/" ` + st + file + `-INPUT 2>/dev/null; exit 0; fi
case "$2" in
  "") head -1 ` + st + file + `-INPUT 2>/dev/null; echo "-N ` + chain + `-before-input"; echo "-N ` + chain + `-user-input"; cat ` + st + file + `-custom ` + st + file + `-before ` + st + file + `-user 2>/dev/null; true ;;
  INPUT) cat ` + st + file + `-INPUT 2>/dev/null ;;
  ` + chain + `-before-input) cat ` + st + file + `-before 2>/dev/null || exit 1 ;;
  ` + chain + `-user-input) cat ` + st + file + `-user 2>/dev/null || exit 1 ;;
esac`
	}
	for name, body := range map[string]string{
		"flock":       "exit 0",
		"timeout":     `shift 3; exec "$@"`,
		"date":        `if [ "$1" = +%s ]; then cat ` + root + `/clock; else echo 2026-10-16T00:00:00Z; fi`,
		"sleep":       `echo "sleep $1" >> ` + log + `; echo $(( $(cat ` + root + `/clock) + $1 )) > ` + root + `/clock`,
		"systemctl":   `if [ "$1" = is-active ]; then [ -f ` + root + `/active ] && exit 0; exit 3; fi; echo "systemctl $*" >> ` + log,
		"systemd-run": `echo "systemd-run $*" >> ` + log,
		"ufw":         ufw,
		"iptables":    ipt("iptables", "", "ufw"),
		"ip6tables":   ipt("ip6tables", "6", "ufw6"),
	} {
		//nolint:gosec // the stand-ins have to be executable
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	}
	s := hostScene{t: t, r: ufwRollback{id: "abc123", root: root}, bin: bin, log: log, root: root}
	if active {
		require.NoError(t, s.run("ufw --force enable"))
	}
	if prepare != nil {
		prepare(s)
	}
	return s
}

// arm reserves and snapshots the firewall, as kip does before its first change.
func (s *hostScene) arm() error {
	s.savedUser, s.savedDefault = s.file("/etc/ufw/user.rules"), s.file("/etc/default/ufw")
	if err := s.run(s.r.reserveAndArmCommand()); err != nil {
		return err
	}
	return os.WriteFile(s.log, nil, 0o600)
}

func (s hostScene) write(name, body string) {
	s.t.Helper()
	require.NoError(s.t, os.MkdirAll(filepath.Dir(filepath.Join(s.root, name)), 0o750))
	require.NoError(s.t, os.WriteFile(filepath.Join(s.root, name), []byte(body), 0o600))
}

// reload loads the saved configuration, so the live rules match it.
func (s hostScene) reload() {
	s.t.Helper()
	require.NoError(s.t, s.run("ufw --force disable && ufw --force enable"))
}

func (s hostScene) failedOpen() bool {
	_, err := os.Stat(filepath.Join(s.root, firewalldStateDir, ufwFailOpenFile))
	return err == nil
}

func (s hostScene) run(command string) error {
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = []string{"PATH=" + s.bin + ":/usr/bin:/bin"}
	return cmd.Run()
}

func (s hostScene) calls() string {
	b, _ := os.ReadFile(s.log)
	return string(b)
}

func (s hostScene) file(name string) string {
	b, _ := os.ReadFile(filepath.Join(s.root, name))
	return string(b)
}

// advance moves the fake clock forward.
func (s hostScene) advance(seconds int) {
	s.t.Helper()
	now := strings.TrimSpace(s.file("/clock"))
	var n int
	_, err := fmt.Sscan(now, &n)
	require.NoError(s.t, err)
	require.NoError(s.t, os.WriteFile(filepath.Join(s.root, "clock"), []byte(fmt.Sprintf("%d\n", n+seconds)), 0o600))
}

// rollBack runs the rollback the timer would start.
func (s hostScene) rollBack() {
	s.t.Helper()
	require.NoError(s.t, s.run("sh "+s.r.script()))
}

// requirePriorFirewall checks that the firewall from before the change is
// active again with its rules, and that nothing opened the host.
func (s hostScene) requirePriorFirewall() {
	s.t.Helper()
	assert.FileExists(s.t, filepath.Join(s.root, "state-active"), "the previous firewall is active again")
	assert.Equal(s.t, s.savedUser, s.file("/etc/ufw/user.rules"), "with its own rules")
	assert.Equal(s.t, s.savedDefault, s.file("/etc/default/ufw"))
	assert.NotContains(s.t, s.calls(), "iptables -P INPUT ACCEPT", "nothing opened the host")
	assert.NoFileExists(s.t, filepath.Join(s.root, firewalldStateDir, ufwFailOpenFile))
}

func TestAnInterruptionRightAfterArmingKeepsTheWorkingFirewall(t *testing.T) {
	s := newHostScene(t, true)
	s.advance(ufwRollbackSeconds + 1)
	s.rollBack()
	s.requirePriorFirewall()
}

func TestAnInterruptionBetweenRulesRestoresThePreviousRules(t *testing.T) {
	s := newHostScene(t, true)
	require.NoError(t, s.run(s.r.locked("echo '-A ufw-user-input -p tcp --dport 6443 -j ACCEPT' >> "+s.root+"/etc/ufw/user.rules")))
	require.NoError(t, s.run(s.r.locked("sed -i.bak 's/DROP/ACCEPT/' "+s.root+"/etc/default/ufw")))
	s.advance(ufwRollbackSeconds + 1)
	s.rollBack()
	s.requirePriorFirewall()
}

func TestASlowWorkingUpdateIsNotCutOffAtTheFirstDeadline(t *testing.T) {
	s := newHostScene(t, true)
	s.advance(170)
	require.NoError(t, s.run(s.r.locked("true")), "a step renews the deadline")
	s.advance(30)
	s.rollBack()
	calls := s.calls()
	require.Contains(t, calls, "sleep 150", "the rollback waits for the renewed deadline")
	assert.Less(t, strings.Index(calls, "sleep 150"), strings.Index(calls, "ufw --force"))
	s.requirePriorFirewall()
}

func TestARollbackKeepsAFirewallThatWasOffSwitchedOff(t *testing.T) {
	s := newHostScene(t, false)
	s.advance(ufwRollbackSeconds + 1)
	s.rollBack()
	assert.NoFileExists(t, filepath.Join(s.root, "state-active"))
	assert.NoFileExists(t, filepath.Join(s.root, firewalldStateDir, ufwFailOpenFile), "an inactive firewall staying inactive is not a fail-open")
}

func TestARestoreThatFailsSwitchesUFWOffAndSaysSo(t *testing.T) {
	s := newHostScene(t, true)
	require.NoError(t, os.WriteFile(filepath.Join(s.root, "enable-fails"), nil, 0o600))
	s.advance(ufwRollbackSeconds + 1)
	s.rollBack()
	assert.Contains(t, s.calls(), "iptables -P INPUT ACCEPT")
	assert.True(t, strings.HasPrefix(s.file(firewalldStateDir+"/"+ufwFailOpenFile), "abc123 "), "the fail-open marker names the run")
}

func TestARollBackNowRestoresAtOnceAndReportsTheOutcome(t *testing.T) {
	s := newHostScene(t, true)
	cmd := exec.Command("sh", "-c", s.r.rollBackNowCommand())
	cmd.Env = []string{"PATH=" + s.bin + ":/usr/bin:/bin"}
	out, err := cmd.Output()
	require.NoError(t, err)
	assert.Equal(t, "restored", strings.TrimSpace(string(out)))
	s.requirePriorFirewall()
}

func TestARollbackRefusesEveryLaterChange(t *testing.T) {
	s := newHostScene(t, true)
	s.advance(ufwRollbackSeconds + 1)
	s.rollBack()
	require.NoError(t, os.WriteFile(s.log, nil, 0o600))
	require.Error(t, s.run(s.r.locked("ufw --force disable")))
	assert.NotContains(t, s.calls(), "ufw --force disable", "a change after the rollback must not run")
	assert.Error(t, s.run(s.r.commitCommand()), "a rolled-back change cannot be committed")
}

func TestACommittedChangeMakesALateRollbackHarmless(t *testing.T) {
	s := newHostScene(t, true)
	require.NoError(t, s.run(s.r.locked("ufw --force disable")))
	require.NoError(t, s.run(s.r.commitCommand()))
	require.NoError(t, os.WriteFile(s.log, nil, 0o600))
	s.advance(ufwRollbackSeconds + 1)
	_ = s.run("sh " + s.r.script())
	assert.NotContains(t, s.calls(), "ufw --force", "a committed change is never rolled back")
	assert.NotContains(t, s.calls(), "iptables -P INPUT ACCEPT")
}

func TestAStatusThatTimesOutIsNotReadAsNoFirewall(t *testing.T) {
	bin := t.TempDir()
	//nolint:gosec // the stand-in has to be executable
	require.NoError(t, os.WriteFile(filepath.Join(bin, "timeout"), []byte("#!/bin/sh\nexit 124\n"), 0o755))
	cmd := exec.Command("sh", "-c", ufwStatusCommand)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	assert.Error(t, cmd.Run())

	//nolint:gosec // the stand-in has to be executable
	require.NoError(t, os.WriteFile(filepath.Join(bin, "timeout"), []byte("#!/bin/sh\nexit 127\n"), 0o755))
	cmd = exec.Command("sh", "-c", ufwStatusCommand)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	assert.NoError(t, cmd.Run(), "a host without ufw has no ufw firewall")
}

func TestASecondRunCannotTakeAFirewallWhoseRollbackIsArmed(t *testing.T) {
	s := newHostScene(t, true)
	require.NoError(t, os.WriteFile(filepath.Join(s.root, "active"), nil, 0o600))
	other := ufwRollback{id: "def456", root: s.root}
	assert.Error(t, s.run(other.reserveAndArmCommand()), "the first run's rollback is still armed")

	require.NoError(t, os.Remove(filepath.Join(s.root, "active")))
	require.NoError(t, s.run(other.reserveAndArmCommand()), "a run whose timer is gone holds nothing")
	require.NoError(t, os.WriteFile(s.log, nil, 0o600))
	assert.Error(t, s.run(s.r.locked("ufw --force disable")), "the first run no longer owns the firewall")
	s.advance(ufwRollbackSeconds + 1)
	s.rollBack()
	assert.NotContains(t, s.calls(), "ufw --force", "the first run's rollback leaves the second run's firewall alone")
}

func TestARefusalIsToldApartFromAnOrdinaryFailure(t *testing.T) {
	r := ufwRollback{id: "abc123"}
	command := r.locked(enableUFWCommand)
	assert.NotContains(t, command, ufwRefusedMarker, "the command text must not carry the marker")

	ordinary := errors.New("running \"" + command + "\" on 192.0.2.10: exit status 1: ERROR: Could not load logging rules")
	_, err := refusedFrom("ERROR: Could not load logging rules\n", ordinary)
	assert.False(t, rolledBackDuring(err), "an ordinary failure is retried")

	_, err = refusedFrom(ufwRefusedMarker+"\n", ordinary)
	assert.True(t, rolledBackDuring(err))
}

func TestTheUFWAuditFailsWhenTheStatusReadTimesOut(t *testing.T) {
	run := func(timeoutBody string) (string, error) {
		bin := t.TempDir()
		//nolint:gosec // the stand-ins have to be executable
		require.NoError(t, os.WriteFile(filepath.Join(bin, "timeout"), []byte("#!/bin/sh\n"+timeoutBody+"\n"), 0o755))
		//nolint:gosec // the stand-ins have to be executable
		require.NoError(t, os.WriteFile(filepath.Join(bin, "ufw"), []byte("#!/bin/sh\necho 'Status: active'\n"), 0o755))
		cmd := exec.Command("sh", "-c", ufwStateCommand)
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	_, err := run("exit 124")
	assert.Error(t, err, "a status that timed out is not a host without ufw")
	_, err = run("exit 137")
	assert.Error(t, err, "a status read killed after its grace period is not a host without ufw")
	out, err := run("exit 127")
	assert.NoError(t, err)
	assert.Empty(t, out, "a host without ufw has no ufw firewall")
	out, err = run(`shift 3; exec "$@"`)
	assert.NoError(t, err)
	assert.Equal(t, "active", out)
}

func TestAClaimedUFWThatIsSwitchedOffIsStillKipManaged(t *testing.T) {
	h := &routedRunner{routes: []route{
		{contains: "is-active firewalld", out: "inactive\n"},
		{contains: "ufw status", out: "inactive\n"},
		{contains: "test -s " + firewallClaimPath, out: "claimed\n"},
	}}
	audit, err := AuditFirewall(h)
	require.NoError(t, err)
	assert.True(t, audit.KipperManaged, "a rollback that switched kip's ufw off must not make the host look unmanaged")
	assert.Equal(t, "", audit.Existing)
	assert.Contains(t, PlanFirewall(audit, true, false).Notice, "switched off. Enable firewall configuration to turn it back on")

	h = &routedRunner{routes: []route{
		{contains: "is-active firewalld", out: "inactive\n"},
		{contains: "ufw status", out: "inactive\n"},
	}}
	audit, err = AuditFirewall(h)
	require.NoError(t, err)
	assert.False(t, audit.KipperManaged, "without the claim an inactive ufw is nobody's")
}

func TestASwitchedOffUFWKeepsItsSavedSSHRateLimit(t *testing.T) {
	h := &routedRunner{routes: []route{
		{contains: "sshd -T", out: "22\n"},
		{contains: "ufw status", out: "Status: inactive\n"},
		{contains: "ufw show added", out: "Added user rules (see 'ufw status' for running firewall):\nufw limit 22/tcp\nufw allow 80/tcp\n"},
	}}
	limited, err := SSHRateLimited(h)
	require.NoError(t, err)
	assert.True(t, limited)
}

func TestArmingRefusesWhenTheUFWStatusCannotBeRead(t *testing.T) {
	for _, marker := range []string{"status-fails", "status-garbled"} {
		s := newUnarmedHostScene(t, true, func(s hostScene) { s.write(marker, "") })
		assert.Error(t, s.arm(), "%s: an unknown state must not be snapshotted as inactive", marker)
		assert.NoFileExists(t, filepath.Join(s.root, firewalldStateDir, "ufw-run"), "%s: nothing is reserved or changed", marker)
	}
}

func TestARestoreNowWithoutARecordedOutcomeIsNotReportedAsRestored(t *testing.T) {
	s := newHostScene(t, true)
	require.NoError(t, os.Remove(s.r.script()))
	cmd := exec.Command("sh", "-c", s.r.rollBackNowCommand())
	cmd.Env = []string{"PATH=" + s.bin + ":/usr/bin:/bin"}
	out, _ := cmd.Output()
	assert.Equal(t, "unknown", strings.TrimSpace(string(out)))
	err := s.r.rollBackNow(cannedOutput(string(out)))
	assert.ErrorContains(t, err, "could not confirm that the previous firewall was restored")
}

// cannedOutput answers every command with itself.
type cannedOutput string

func (o cannedOutput) Run(string) (string, error)                 { return string(o), nil }
func (o cannedOutput) RunStdin(string, io.Reader) (string, error) { return string(o), nil }

func TestARestoreChecksIPv6WhereUFWManagesIt(t *testing.T) {
	for _, tc := range []struct {
		name, rules6 string
		restored     bool
	}{
		{"SSH allowed on IPv6", "-A ufw6-user-input -p tcp --dport 22 -j ACCEPT\n", true},
		{"IPv6 chains not linked", "-A ufw6-user-input -p tcp --dport 22 -j ACCEPT\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newUnarmedHostScene(t, true, func(s hostScene) {
				s.write("/etc/default/ufw", "IPV6=yes\n")
				s.write("/proc/net/if_inet6", "")
				s.write("/etc/ufw/user6.rules", "*filter\n"+tc.rules6+"COMMIT\n")
				if tc.name == "IPv6 chains not linked" {
					s.write("ipv6-unlinked", "")
				}
				s.reload()
			})
			require.NoError(t, s.arm())
			s.advance(ufwRollbackSeconds + 1)
			s.rollBack()
			assert.Equal(t, !tc.restored, s.failedOpen())
		})
	}
}

func TestTheRuleCaptureProgramCanBeSingleQuoted(t *testing.T) {
	assert.NotContains(t, ufwReachableRules, "'", "the program is passed to awk in single quotes inside the rollback script")
}

func TestARestoreThatLoadsDifferentRulesThanTheWorkingOnesFailsClosed(t *testing.T) {
	s := newUnarmedHostScene(t, true, func(s hostScene) {
		// The saved before.rules no longer match what is loaded, as after an
		// edit that was never reloaded.
		s.write("/etc/ufw/before.rules", "*filter\n-A ufw-before-input -p tcp --dport 22 -j DROP\n-A ufw-before-input -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\nCOMMIT\n")
	})
	require.NoError(t, s.arm())
	s.advance(ufwRollbackSeconds + 1)
	s.rollBack()
	assert.True(t, s.failedOpen(), "rules that differ from the ones that admitted SSH are not a restore")
}

func TestARestoreComparesTheChainsUFWJumpsTo(t *testing.T) {
	live := "*filter\n:operator-x - [0:0]\n-N operator-x\n-A operator-x -j RETURN\n-A ufw-before-input -j operator-x\n-A ufw-before-input -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\nCOMMIT\n"
	s := newUnarmedHostScene(t, true, func(s hostScene) {
		s.write("/etc/ufw/before.rules", live)
		s.reload()
		s.write("/etc/ufw/before.rules", strings.Replace(live, "-A operator-x -j RETURN", "-A operator-x -p tcp --dport 22 -j DROP", 1))
	})
	require.NoError(t, s.arm())
	s.advance(ufwRollbackSeconds + 1)
	s.rollBack()
	assert.True(t, s.failedOpen(), "a chain the UFW chains jump to is part of what admitted SSH")
}

func TestARestoreKeepsAnAdminsSourceRestrictedSSHRule(t *testing.T) {
	s := newUnarmedHostScene(t, true, func(s hostScene) {
		// Preserve the administrator's rule and its position ahead of kip's rule.
		s.write("/etc/ufw/user.rules", "*filter\n-A ufw-user-input -s 192.0.2.0/24 -p tcp -m tcp --dport 22 -j ACCEPT\n-A ufw-user-input -p tcp -m tcp --dport 22 -j ACCEPT\nCOMMIT\n")
		s.reload()
	})
	require.NoError(t, s.arm())
	s.advance(ufwRollbackSeconds + 1)
	s.rollBack()
	s.requirePriorFirewall()
}

func TestArmingIgnoresUFWWarningsOnStderr(t *testing.T) {
	s := newUnarmedHostScene(t, true, func(s hostScene) { s.write("status-warns", "") })
	require.NoError(t, s.arm(), "a ufw warning is not an unknown state")
	assert.FileExists(t, filepath.Join(s.r.snapshot(), "was-active"))
}

func TestARolledBackFirstSetupIsNotLeftClaimedByKip(t *testing.T) {
	claim := firewallClaimPath
	s := newHostScene(t, false)
	require.NoError(t, s.run(s.r.locked("mkdir -p "+s.root+"/etc/kipper && echo kip > "+s.root+claim)))
	s.advance(ufwRollbackSeconds + 1)
	s.rollBack()
	assert.NoFileExists(t, filepath.Join(s.root, claim), "the host was not kip's before the change, so it is not kip's after the rollback")

	s = newUnarmedHostScene(t, true, func(s hostScene) { s.write(claim, "kip\n") })
	require.NoError(t, s.arm())
	s.advance(ufwRollbackSeconds + 1)
	s.rollBack()
	assert.FileExists(t, filepath.Join(s.root, claim), "a firewall that was kip's stays kip's")
}

func TestARestoreThatChangesTheInputPolicyIsNotReportedAsRestored(t *testing.T) {
	s := newUnarmedHostScene(t, true, func(s hostScene) {
		// A manual policy change allows traffic that the saved policy would drop.
		require.NoError(t, s.run("iptables -w 5 -P INPUT ACCEPT"))
	})
	require.NoError(t, s.arm())
	s.advance(ufwRollbackSeconds + 1)
	s.rollBack()
	assert.NotEqual(t, "restored\n", s.file(firewalldStateDir+"/ufw-abc123.outcome"), "reloading the saved deny changes what admitted SSH")
	assert.True(t, s.failedOpen())
}

func TestARestoreThatChangesTheIPv6InputPolicyIsNotReportedAsRestored(t *testing.T) {
	s := newUnarmedHostScene(t, true, func(s hostScene) {
		s.write("/etc/default/ufw", "IPV6=yes\n")
		s.write("/proc/net/if_inet6", "")
		s.write("/etc/ufw/user6.rules", "*filter\n-A ufw6-user-input -p tcp --dport 22 -j ACCEPT\nCOMMIT\n")
		s.reload()
		require.NoError(t, s.run("ip6tables -w 5 -P INPUT ACCEPT"))
	})
	require.NoError(t, s.arm())
	s.advance(ufwRollbackSeconds + 1)
	s.rollBack()
	assert.True(t, s.failedOpen(), "reloading the saved IPv6 deny changes what admitted SSH over IPv6")
}

// kip parses ufw's English status words, so an active firewall must be recognised
// even when the host's default locale translates ufw's status line.
func TestUFWStatusIsReadInEnglishOnATranslatedHost(t *testing.T) {
	bin := t.TempDir()
	//nolint:gosec // the stand-ins have to be executable
	require.NoError(t, os.WriteFile(filepath.Join(bin, "timeout"), []byte("#!/bin/sh\nshift 3\nexec \"$@\"\n"), 0o755))
	//nolint:gosec // the stand-ins have to be executable
	require.NoError(t, os.WriteFile(filepath.Join(bin, "ufw"), []byte("#!/bin/sh\n"+
		`if [ "$LC_ALL" = C ] && [ "$LANGUAGE" = C ]; then echo 'Status: active'; else echo 'Estado: activo'; fi`+"\n"), 0o755))
	cmd := exec.Command("sh", "-c", ufwStateCommand)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "LANG=es_ES.UTF-8", "LANGUAGE=es"}
	out, err := cmd.Output()
	require.NoError(t, err)
	assert.Equal(t, "active\n", string(out))
}
