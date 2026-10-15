package installer

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/getkipper/kipper/kip/internal/localrun"
)

const (
	ufwRollbackUnit = "kipper-ufw-rollback"
	// ufwRollbackSeconds is the rollback delay renewed before each locked command.
	// Rollback waits for the command holding the firewall lock to finish.
	ufwRollbackSeconds = 180
	// ufwTimeout allows 60 seconds per command, then a 10-second termination grace period.
	ufwTimeout = "timeout -k 10 60 "
	// ufwRefusedMarker identifies commands refused after rollback or loss of ownership.
	// Splitting it in shell commands avoids matching echoed command text.
	ufwRefusedMarker = "kip-ufw-change-refused"
	// ufwFailOpenFile records that a rollback could not restore the previous
	// firewall and attempted to disable UFW.
	ufwFailOpenFile = "ufw-failopen"
)

const ufwRefusedEcho = "printf '%s%s\\n' 'kip-ufw-change' '-refused' >&2"

// errUFWChangeRefused reports a command refused under the rollback lock.
var errUFWChangeRefused = errors.New("rollback has begun, or another run now owns the firewall")

// ufwOpenCommand attempts to restore host access by disabling UFW and setting
// IPv4 and IPv6 INPUT policies to ACCEPT, even if disabling UFW fails.
const ufwOpenCommand = ufwTimeout + "ufw --force disable >/dev/null 2>&1; iptables -w 5 -P INPUT ACCEPT; ip6tables -w 5 -P INPUT ACCEPT 2>/dev/null"

// ufwLinked builds a shell check that INPUT jumps to UFW's before-input chain, which accepts
// established traffic and jumps to the user rules. IPv6 is checked too when
// UFW manages it on this host.
func ufwLinked(root string) string {
	check := func(ipt, chain string) string {
		return fmt.Sprintf("%[1]s -w 5 -S INPUT | grep -q -- '-j %[2]s-before-input' && "+
			"%[1]s -w 5 -S %[2]s-before-input | grep -q -- '--ctstate RELATED,ESTABLISHED -j ACCEPT' && "+
			"%[1]s -w 5 -S %[2]s-before-input | grep -q -- '-j %[2]s-user-input'", ipt, chain)
	}
	return fmt.Sprintf("{ %s; } && { ! %s || { %s; }; }",
		check("iptables", "ufw"), ufwManagesIPv6(root), check("ip6tables", "ufw6"))
}

// ufwManagesIPv6 builds a shell check for configured and available IPv6 support.
func ufwManagesIPv6(root string) string {
	return fmt.Sprintf("{ grep -qs '^IPV6=yes' %[1]s/etc/default/ufw && [ -e %[1]s/proc/net/if_inet6 ]; }", root)
}

// ufwReachableRules prints, in their original order, the rules of every
// chain whose name starts with ufw and of every chain reachable from those
// through -j or -g, from an iptables -S listing on stdin.
const ufwReachableRules = `{ line[NR] = $0 }
$1 == "-N" { chain[$2] = 1 }
END {
  for (c in chain) if (c ~ /^ufw/) keep[c] = 1
  do {
    added = 0
    for (i = 1; i <= NR; i++) {
      n = split(line[i], f, " ")
      if (f[1] != "-A" || !(f[2] in keep)) continue
      for (j = 3; j < n; j++) if ((f[j] == "-j" || f[j] == "-g") && (f[j + 1] in chain) && !(f[j + 1] in keep)) { keep[f[j + 1]] = 1; added = 1 }
    }
  } while (added)
  for (i = 1; i <= NR; i++) { split(line[i], f, " "); if (f[1] == "-A" && (f[2] in keep)) print line[i] }
}`

// ufwLiveRules builds a command that prints the INPUT policy and rules in UFW
// chains and chains reachable from them. It includes IPv6 when UFW manages it
// and fails if either ruleset cannot be read.
func ufwLiveRules(root string) string {
	return fmt.Sprintf(`{ v4=$(iptables -w 5 -S) || exit 1; printf '%%s\n' "$v4" | grep '^-P INPUT '; printf '%%s\n' "$v4" | awk '%[1]s'; `+
		`if %[2]s; then v6=$(ip6tables -w 5 -S) || exit 1; printf '%%s\n' "$v6" | grep '^-P INPUT '; printf '%%s\n' "$v6" | awk '%[1]s'; fi; }`, ufwReachableRules, ufwManagesIPv6(root))
}

// enableUFW builds a command to start UFW and check its chains. Recovery runs
// in the same shell so it can attempt to restore access if SSH is blocked.
func enableUFW(root string) string {
	return `out=$(` + ufwTimeout + `ufw --force enable 2>&1); rc=$?; ` +
		`if [ "$rc" -eq 0 ] && ` + ufwLinked(root) + `; then exit 0; fi; ` +
		ufwOpenCommand + `; ` +
		`echo "ufw failed to start or load its required rules (exit $rc: $out). Attempted to switch ufw off" >&2; exit 1`
}

var enableUFWCommand = enableUFW("")

// ufwStatusCommand prints UFW status, or an empty line if UFW is missing.
// A timeout (124) or a kill (128 and above) is an error. Other exits are tolerated.
const ufwStatusCommand = `out=$(` + ufwTimeout + `ufw status 2>/dev/null); rc=$?; ` +
	`if [ "$rc" -eq 124 ] || [ "$rc" -ge 128 ]; then echo 'ufw did not report its status within 60 seconds' >&2; exit 1; fi; printf '%s\n' "$out"`

// ufwStateCommand extracts the status word while preserving errors from
// ufwStatusCommand that a pipeline into awk would hide.
const ufwStateCommand = `st=$(` + ufwStatusCommand + `) || { printf '%s\n' "$st" >&2; exit 1; }; printf '%s\n' "$st" | awk 'NR==1 {print $2}'`

// ufwRollback coordinates one UFW change through a host lock and reservation.
// It snapshots the firewall before the first change. If kip stops renewing
// the deadline or the change fails, the host attempts to restore that snapshot.
// Tests use root to redirect host paths into a temporary directory.
type ufwRollback struct {
	id, root string
	local    bool
}

func (r ufwRollback) unit() string          { return ufwRollbackUnit + "-" + r.id }
func (r ufwRollback) state(n string) string { return r.root + firewalldStateDir + "/" + n }
func (r ufwRollback) lock() string          { return r.state("ufw.lock") }
func (r ufwRollback) rolledBack() string    { return r.state("ufw-" + r.id + ".rolledback") }
func (r ufwRollback) committed() string     { return r.state("ufw-" + r.id + ".committed") }
func (r ufwRollback) deadline() string      { return r.state("ufw-" + r.id + ".deadline") }
func (r ufwRollback) snapshot() string      { return r.state("ufw-" + r.id + ".snapshot") }
func (r ufwRollback) script() string        { return r.snapshot() + "/rollback.sh" }
func (r ufwRollback) reservation() string   { return r.state("ufw-run") }
func (r ufwRollback) failOpen() string      { return r.state(ufwFailOpenFile) }
func (r ufwRollback) outcome() string       { return r.state("ufw-" + r.id + ".outcome") }

// owns builds a shell check for this run's firewall reservation.
func (r ufwRollback) owns() string {
	return fmt.Sprintf(`[ "$(cat %s 2>/dev/null)" = %s ]`, r.reservation(), r.id)
}

// renew builds a command to extend the deadline by ufwRollbackSeconds.
func (r ufwRollback) renew() string {
	return fmt.Sprintf("echo $(( $(date +%%s) + %d )) > %s", ufwRollbackSeconds, r.deadline())
}

// rollbackScript builds a script that restores the snapshot after the deadline.
// If restoration or rule checks fail, it attempts to disable UFW and records
// a fail-open marker. The marker does not confirm that disabling UFW succeeded.
func (r ufwRollback) rollbackScript() string {
	return strings.Join([]string{
		"while :; do",
		fmt.Sprintf("  exec 9>%s; flock 9 || exit 1", r.lock()),
		fmt.Sprintf("  [ -e %s ] && exit 0", r.committed()),
		fmt.Sprintf("  %s || exit 0", r.owns()),
		fmt.Sprintf("  now=$(date +%%s); dl=$(cat %s 2>/dev/null); dl=${dl:-0}", r.deadline()),
		`  [ "$now" -ge "$dl" ] && break`,
		`  flock -u 9; sleep $((dl - now))`,
		"done",
		fmt.Sprintf("touch %s; rm -f %s", r.rolledBack(), r.reservation()),
		// Restore prior ownership so a failed first setup leaves the firewall
		// under operator control.
		fmt.Sprintf("[ -e %s/was-claimed ] || rm -f %s%s", r.snapshot(), r.root, firewallClaimPath),
		fmt.Sprintf("if cp -p %[1]s/etc-ufw/* %[2]s/etc/ufw/ && cp -p %[1]s/default-ufw %[2]s/etc/default/ufw; then", r.snapshot(), r.root),
		fmt.Sprintf("  if [ -e %s/was-active ]; then", r.snapshot()),
		"    " + ufwTimeout + "ufw --force disable >/dev/null 2>&1",
		// Saved configuration can differ from live rules. Compare the restored
		// rules with those loaded before the change to detect that mismatch.
		"    if " + ufwTimeout + "ufw --force enable >/dev/null 2>&1 && " + ufwLinked(r.root) +
			" && " + ufwLiveRules(r.root) + " | cmp -s - " + r.snapshot() + "/live-rules; then echo restored > " + r.outcome() + "; exit 0; fi",
		"  else",
		"    " + ufwTimeout + "ufw --force disable >/dev/null 2>&1 && { echo restored > " + r.outcome() + "; exit 0; }",
		"  fi",
		"fi",
		ufwOpenCommand,
		fmt.Sprintf(`printf '%%s %%s\n' %s "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" > %s`, r.id, r.failOpen()),
		"echo failopen > " + r.outcome(),
	}, "\n") + "\n"
}

// locked wraps command in a shell that checks ownership under the host lock
// and renews the deadline. It refuses execution after rollback or ownership loss.
func (r ufwRollback) locked(command string) string {
	return fmt.Sprintf("(exec 9>%s && flock 9 || exit 1; if [ -e %s ] || ! %s; then %s; exit 1; fi; %s; %s)", r.lock(), r.rolledBack(), r.owns(), ufwRefusedEcho, r.renew(), command)
}

// reserveAndArmCommand builds a command to reserve and snapshot the firewall
// and schedule rollback under one lock. It can replace an existing reservation
// when both its timer and service are inactive.
func (r ufwRollback) reserveAndArmCommand() string {
	return fmt.Sprintf(`mkdir -p %[1]s && find %[1]s -maxdepth 1 \( -name 'ufw-*.committed' -o -name 'ufw-*.rolledback' -o -name 'ufw-*.deadline' -o -name 'ufw-*.outcome' -o -name 'ufw-*.snapshot' \) -mmin +1440 -exec rm -rf {} + && `+
		`(exec 9>%[2]s && flock 9 || exit 1; f=%[3]s; if [ -s "$f" ]; then other=$(cat "$f"); `+
		`case "$other" in *[!0-9a-f]*) echo "$f contains an invalid firewall run ID: $other" >&2; exit 1 ;; esac; `+
		`if systemctl is-active --quiet "%[4]s-$other.timer" || systemctl is-active --quiet "%[4]s-$other.service"; then echo "another firewall change ($other) is in progress. Run the command again in %[5]d seconds" >&2; exit 1; fi; fi; `+
		`rm -rf %[8]s && mkdir -p %[8]s/etc-ufw && cp -p %[9]s/etc/ufw/*.rules %[9]s/etc/ufw/ufw.conf %[8]s/etc-ufw/ && cp -p %[9]s/etc/default/ufw %[8]s/default-ufw || exit 1; `+
		`st=$(%[10]s 2>/dev/null) || { echo "reading the state of UFW failed: $st" >&2; exit 1; }; `+
		`case "$st" in "Status: active"*) touch %[8]s/was-active ;; "Status: inactive"*) ;; *) echo "UFW reported an unexpected state: $st" >&2; exit 1 ;; esac; `+
		`if [ -e %[8]s/was-active ]; then %[14]s > %[8]s/live-rules || exit 1; fi; `+
		`if [ -e %[9]s%[15]s ]; then touch %[8]s/was-claimed; fi; `+
		"cat > %[11]s <<'KIPEOF'\n%[12]sKIPEOF\n"+
		`%[13]s && printf '%%s\n' %[6]s > "$f" && systemd-run --quiet --unit=%[7]s --on-active=%[5]d --timer-property=RemainAfterElapse=no --timer-property=AccuracySec=1s /bin/sh %[11]s)`,
		r.state(""), r.lock(), r.reservation(), ufwRollbackUnit, ufwRollbackSeconds, r.id, r.unit(),
		r.snapshot(), r.root, ufwTimeout+"ufw status", r.script(), r.rollbackScript(), r.renew(), ufwLiveRules(r.root), firewallClaimPath)
}

// commitCommand builds a command to keep the change and release ownership
// under the lock. Its marker prevents a late rollback if stopping the timer
// fails. It also clears any earlier fail-open marker.
func (r ufwRollback) commitCommand() string {
	return r.locked(fmt.Sprintf("touch %s && rm -f %s %s %s", r.committed(), r.reservation(), r.deadline(), r.failOpen())) +
		fmt.Sprintf(" && { systemctl stop %s.timer >/dev/null 2>&1 || true; rm -rf %s; }", r.unit(), r.snapshot())
}

// rollBackNowCommand builds a command to request immediate rollback and print
// its recorded outcome, or "unknown" if no outcome can be read.
func (r ufwRollback) rollBackNowCommand() string {
	return "{ " + r.locked("echo 0 > "+r.deadline()) + fmt.Sprintf(" && sh %s; }; cat %s 2>/dev/null || echo unknown", r.script(), r.outcome())
}

// runner wraps commands with the host lock and ownership checks.
func (r ufwRollback) runner(client CommandRunner) CommandRunner { return lockedRunner{client, r} }

type lockedRunner struct {
	client CommandRunner
	r      ufwRollback
}

func (l lockedRunner) Run(command string) (string, error) {
	return refusedFrom(l.client.Run(l.r.locked(command)))
}

func (l lockedRunner) RunStdin(command string, in io.Reader) (string, error) {
	return refusedFrom(l.client.RunStdin(l.r.locked(command), in))
}

// refusedFrom wraps errUFWChangeRefused when failed command output contains
// the refusal marker. Checking output avoids matching commands quoted in errors.
func refusedFrom(out string, err error) (string, error) {
	if err != nil && strings.Contains(out, ufwRefusedMarker) {
		return out, fmt.Errorf("%w: %w", errUFWChangeRefused, err)
	}
	return out, err
}

// rolledBackDuring reports a refusal caused by rollback or loss of ownership.
func rolledBackDuring(err error) bool { return errors.Is(err, errUFWChangeRefused) }

// armUFWRollback reserves the firewall, snapshots it and schedules rollback
// after checking a fresh SSH connection. Local runners skip the connection check.
func armUFWRollback(client FirewallRunner) (ufwRollback, error) {
	baseline := client.ConfirmFreshConnection()
	local := errors.Is(baseline, localrun.ErrNoConnection)
	if baseline != nil && !local {
		return ufwRollback{}, fmt.Errorf("kip could not open a second connection to the host, so it left the firewall alone: %w", baseline)
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return ufwRollback{}, fmt.Errorf("naming the firewall rollback: %w", err)
	}
	r := ufwRollback{id: hex.EncodeToString(b), local: local}
	if _, err := client.Run(r.reserveAndArmCommand()); err != nil {
		return ufwRollback{}, fmt.Errorf("could not snapshot the firewall and schedule its rollback, so kip left the firewall alone: %w", err)
	}
	return r, nil
}

// ufwRepairHint names the commands that switch a kip-managed firewall back on.
const ufwRepairHint = "run the same kip command again after fixing the cause. Use 'kip cluster harden' to enable the server's firewall or 'kip node add <host>' for a worker"

// finish commits a successful change once a fresh SSH connection works, or at
// once for local runners. A failed change or connection triggers an immediate
// restoration attempt.
func (r ufwRollback) finish(client FirewallRunner, err error) error {
	if rolledBackDuring(err) {
		return fmt.Errorf("%w. Check the host's firewall state before retrying. 'kip status' reports failed restoration attempts on the server. To retry, %s", err, ufwRepairHint)
	}
	if err == nil && !r.local {
		if freshErr := client.ConfirmFreshConnection(); freshErr != nil {
			err = fmt.Errorf("new SSH connections failed with the new firewall: %w", freshErr)
		}
	}
	if err != nil {
		return errors.Join(err, r.rollBackNow(client))
	}
	if _, commitErr := refusedFrom(client.Run(r.commitCommand())); commitErr != nil {
		return fmt.Errorf("could not commit the firewall change (%w). If rollback is still armed, the host attempts restoration about %d seconds after kip's last step. To retry, %s", commitErr, ufwRollbackSeconds, ufwRepairHint)
	}
	return nil
}

// rollBackNow requests immediate restoration and returns an error describing
// the outcome, including successful restoration, for the caller to report.
func (r ufwRollback) rollBackNow(client CommandRunner) error {
	out, err := client.Run(r.rollBackNowCommand())
	switch strings.TrimSpace(out) {
	case "restored":
		return fmt.Errorf("kip restored the previous firewall. To retry, %s", ufwRepairHint)
	case "failopen":
		return fmt.Errorf("kip could not restore the previous firewall and attempted to switch ufw off. Check the host's firewall state. To retry, %s", ufwRepairHint)
	}
	if err == nil {
		err = errors.New("the restore did not record an outcome")
	}
	return fmt.Errorf("kip could not confirm that the previous firewall was restored (%v). If rollback is still armed, the host attempts restoration about %d seconds after kip's last step. Check the host's firewall state. To retry, %s", err, ufwRollbackSeconds, ufwRepairHint)
}

// UFWFailOpen returns the run ID and UTC timestamp, separated by a space, of
// a failed restoration that attempted to disable UFW. An empty result means
// the marker was absent or unreadable. Runner errors are returned. The marker
// is cleared on commit and does not describe the current firewall state.
func UFWFailOpen(client CommandRunner) (string, error) {
	out, err := client.Run("cat " + firewalldStateDir + "/" + ufwFailOpenFile + " 2>/dev/null || true")
	return strings.TrimSpace(out), err
}
