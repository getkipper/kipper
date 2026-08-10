package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

// claimWrite identifies the command that lays the claim down. The audit reads
// the same path, so matching on the path alone would count a read as a write.
const claimWrite = "mkdir -p '/etc/kipper'"

// stubHost stands in for a host's answers to the audit. Each field is what the
// matching command prints. The stub records every command it is asked to run,
// in order, which is what the ordering of the claim against the ufw rules is
// checked against.
type stubHost struct {
	ufwStatus    string
	firewalldIs  string
	claimPresent bool
	// claimEmpty models the file a failed write leaves: it exists, so `test -f`
	// finds it, but it has no content, so `test -s` does not.
	claimEmpty    bool
	failOnCommand string
}

// dialStub puts a fake ssh on PATH and dials through it, so the tests drive a
// real *ssh.Client and the real command strings rather than a helper that
// agrees with them. Returns the client and the path of the recorded calls.
//
// Every command is also parsed by a real shell (`sh -n`, which parses without
// running), so a quoting mistake in a command Kipper builds fails a test here
// rather than on somebody's host. Nothing is executed: these commands install
// packages and rewrite firewalls.
func dialStub(t *testing.T, host stubHost) (*ssh.Client, string) {
	t.Helper()

	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	badSyntax := filepath.Join(dir, "bad-syntax")
	t.Cleanup(func() {
		out, err := os.ReadFile(badSyntax)
		if err == nil && len(out) > 0 {
			t.Errorf("kip built a command no shell can parse:\n%s", out)
		}
	})

	// How the modelled file answers whichever test the audit runs.
	claimArm := "exit 0"
	switch {
	case host.claimEmpty:
		claimArm = "case \"$remote\" in *'test -f'*) echo claimed ;; esac"
	case host.claimPresent:
		claimArm = "echo claimed"
	}
	fail := host.failOnCommand
	if fail == "" {
		// A pattern no command contains, so the stub fails nothing.
		fail = "__no_such_command__"
	}

	// The bare `true` probe is matched last. Kipper's audit commands end in
	// `|| true` themselves, so a leading `*' true')` arm answers them all with
	// silence and every audit reads as a fresh host.
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + calls + "\n" +
		// The remote command is the last argument. Parse it, never run it.
		"for remote; do :; done\n" +
		"sh -n -c \"$remote\" 2>/dev/null || printf '%s\\n' \"$remote\" >> " + badSyntax + "\n" +
		"case \"$*\" in\n" +
		"  *'" + fail + "'*) echo 'stub: refused' >&2; exit 1 ;;\n" +
		"  *'ufw status'*) echo '" + host.ufwStatus + "'; exit 0 ;;\n" +
		// The claim write names this path too, so it is answered first.
		"  *'mkdir -p '*) exit 0 ;;\n" +
		"  *'firewall-managed'*) " + claimArm + "; exit 0 ;;\n" +
		"  *'is-active firewalld'*) echo '" + host.firewalldIs + "'; exit 0 ;;\n" +
		"  *' true') exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	//nolint:gosec // a stub in the test's own temp dir has to be executable
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	c, err := ssh.Dial(ssh.Config{Host: "demo.example.com", User: "root"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(calls, nil, 0o600))
	return c, calls
}

func TestAuditFirewall(t *testing.T) {
	// The case the demo host was in: a failed install had already enabled ufw,
	// so the retry skipped the firewall step and kept the half-finished
	// ruleset. Kipper's own firewall is Kipper's to put right.
	t.Run("a firewall kip set up does not block reconfiguration", func(t *testing.T) {
		c, _ := dialStub(t, stubHost{ufwStatus: "active", claimPresent: true})

		audit, err := AuditFirewall(c)

		require.NoError(t, err)
		assert.Equal(t, "ufw", audit.Existing)
		assert.True(t, audit.KipperManaged)
		assert.False(t, audit.BlocksConfiguration(),
			"kip may bring its own half-applied ruleset back in line")
	})

	t.Run("an admin's firewall is left alone", func(t *testing.T) {
		c, _ := dialStub(t, stubHost{ufwStatus: "active", claimPresent: false})

		audit, err := AuditFirewall(c)

		require.NoError(t, err)
		assert.Equal(t, "ufw", audit.Existing)
		assert.False(t, audit.KipperManaged)
		assert.True(t, audit.BlocksConfiguration(),
			"rules kip did not write are never trampled")
	})

	// Kipper manages ufw and nothing else, so a claim left beside an active
	// firewalld describes a firewall that is no longer the one running.
	t.Run("a claim does not launder a firewall kip does not manage", func(t *testing.T) {
		c, _ := dialStub(t, stubHost{
			ufwStatus: "inactive", firewalldIs: "active", claimPresent: true,
		})

		audit, err := AuditFirewall(c)

		require.NoError(t, err)
		assert.Equal(t, "firewalld", audit.Existing)
		assert.False(t, audit.KipperManaged)
		assert.True(t, audit.BlocksConfiguration())
	})

	// Both managers running at once is the state a claim must not talk its way
	// out of: the claim can only ever describe the ufw ruleset, and Kipper has
	// no business reconfiguring a host while a firewalld it does not manage is
	// up.
	t.Run("a claimed ufw does not excuse an active firewalld beside it", func(t *testing.T) {
		c, _ := dialStub(t, stubHost{
			ufwStatus: "active", firewalldIs: "active", claimPresent: true,
		})

		audit, err := AuditFirewall(c)

		require.NoError(t, err)
		assert.Equal(t, "firewalld", audit.Existing)
		assert.False(t, audit.KipperManaged)
		assert.True(t, audit.BlocksConfiguration(),
			"a firewall manager kip cannot manage outranks kip's claim on another")
	})

	t.Run("a fresh host is configured", func(t *testing.T) {
		c, _ := dialStub(t, stubHost{ufwStatus: "inactive", firewalldIs: "inactive"})

		audit, err := AuditFirewall(c)

		require.NoError(t, err)
		assert.Empty(t, audit.Existing)
		assert.False(t, audit.BlocksConfiguration())
	})

	// Not being able to read the claim must answer "not ours", so an
	// unreadable host is treated exactly like an admin's own firewall.
	t.Run("an unreadable claim leaves the firewall alone", func(t *testing.T) {
		c, _ := dialStub(t, stubHost{
			ufwStatus: "active", claimPresent: true, failOnCommand: "firewall-managed",
		})

		audit, err := AuditFirewall(c)

		require.NoError(t, err)
		assert.False(t, audit.KipperManaged)
		assert.True(t, audit.BlocksConfiguration(), "an unanswerable question is not a yes")
	})
}

// The ordering the whole fix rests on. A claim written after the rules would
// leave any run that died in between unrecognizable, which is the bug being
// fixed with a smaller window. The claim shares the forward-policy command
// rather than preceding it, so what must hold is that it comes before the
// rules and the enable.
func TestConfigureFirewall_ClaimIsWrittenBeforeTheRulesAndTheEnable(t *testing.T) {
	c, calls := dialStub(t, stubHost{ufwStatus: "inactive", firewalldIs: "inactive"})

	require.NoError(t, configureFirewall(c, true))

	recorded, err := os.ReadFile(calls)
	require.NoError(t, err)
	run := string(recorded)

	claimAt := strings.Index(run, claimWrite)
	require.GreaterOrEqual(t, claimAt, 0, "the claim must be written at all")

	// DEFAULT_FORWARD_POLICY is not listed: the claim is sequenced behind it
	// inside one command, which
	// TestConfigureFirewall_SequencesTheClaimBehindTheFirstChange pins.
	// Everything after that command must come later.
	for _, mutation := range []string{"ufw default deny", "ufw limit", "ufw --force enable"} {
		at := strings.Index(run, mutation)
		require.GreaterOrEqual(t, at, 0, "expected %q to run", mutation)
		assert.Less(t, claimAt, at,
			"the claim must precede %q, or a run dying there leaves an unrecognizable host", mutation)
	}
}

// The decision both commands act on. It lives in one place so that `kip
// install` and `kip cluster harden` cannot answer the same host differently,
// which they previously did on the opt-out path.
func TestPlanFirewall(t *testing.T) {
	kippers := FirewallAudit{Existing: "ufw", KipperManaged: true}
	admins := FirewallAudit{Existing: "ufw"}
	fresh := FirewallAudit{}

	t.Run("configures a fresh host", func(t *testing.T) {
		step := PlanFirewall(fresh, true)
		assert.True(t, step.Configure)
		assert.Contains(t, step.Notice, "No existing host firewall")
		assert.Empty(t, step.FlagNotice)
	})

	t.Run("reconfigures a firewall kip set up", func(t *testing.T) {
		step := PlanFirewall(kippers, true)
		assert.True(t, step.Configure)
		assert.Contains(t, step.Notice, "set up by kip")
	})

	t.Run("never touches an admin's firewall", func(t *testing.T) {
		step := PlanFirewall(admins, true)
		assert.False(t, step.Configure)
		assert.Contains(t, step.Notice, "avoid trampling your rules")
	})

	// The notice describes the host. Announcing work that --firewall=false
	// then cancels is how the two commands came to disagree.
	t.Run("promises no work the opt-out cancels", func(t *testing.T) {
		step := PlanFirewall(kippers, false)
		assert.False(t, step.Configure)
		assert.NotContains(t, step.Notice, "Reapplying")
		assert.Contains(t, step.FlagNotice, "--firewall=false")
		assert.Contains(t, step.FlagNotice, "keeps the rules it already has")
	})

	t.Run("explains the opt-out on a fresh host", func(t *testing.T) {
		step := PlanFirewall(fresh, false)
		assert.False(t, step.Configure)
		assert.Contains(t, step.FlagNotice, "Your host has no firewall")
	})

	// An admin's firewall is not skipped by the flag; it was never kip's to
	// configure, and the notice already said so.
	t.Run("does not blame the flag for a host it may not touch", func(t *testing.T) {
		assert.Empty(t, PlanFirewall(admins, false).FlagNotice)
	})
}

// A claim that outlived a run which built nothing would vouch for whatever
// firewall an admin raised next, which is the misattribution this whole change
// exists to prevent, pointed the other way.
func TestConfigureFirewall_LeavesNoClaimWhenItBuildsNothing(t *testing.T) {
	c, calls := dialStub(t, stubHost{
		ufwStatus: "inactive", firewalldIs: "inactive", failOnCommand: "apt-get install",
	})

	err := configureFirewall(c, true)

	require.Error(t, err)
	recorded, readErr := os.ReadFile(calls)
	require.NoError(t, readErr)
	assert.NotContains(t, string(recorded), claimWrite,
		"a run that never reached ufw must not leave a claim behind")
}

// The caller's audit runs before host hardening and the k3s install, so the
// host can acquire a firewall in between.
func TestConfigureFirewall_StopsWhenAFirewallAppearedSinceTheAudit(t *testing.T) {
	c, calls := dialStub(t, stubHost{ufwStatus: "active", claimPresent: false})

	err := configureFirewall(c, true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "became active")

	recorded, readErr := os.ReadFile(calls)
	require.NoError(t, readErr)
	assert.NotContains(t, string(recorded), "ufw --force enable",
		"a firewall that appeared mid-install is an admin's, not kip's to take over")
	assert.NotContains(t, string(recorded), claimWrite,
		"and it must not be claimed either")
}

// A host kip cannot claim is one whose next audit would not recognize its
// firewall, so no rules are applied to it. The stub rejects the whole compound
// command rather than the claim half alone, so this pins the run stopping at a
// failed first command, not which half of it failed.
func TestConfigureFirewall_StopsWhenItCannotClaimTheHost(t *testing.T) {
	c, calls := dialStub(t, stubHost{
		ufwStatus: "inactive", firewalldIs: "inactive", failOnCommand: "firewall-managed",
	})

	err := configureFirewall(c, true)

	require.Error(t, err)
	// Either half of that one command can be the half that failed, so the
	// error has to name both rather than blame the policy edit.
	assert.Contains(t, err.Error(), "claiming the firewall")
	assert.Contains(t, err.Error(), "forward policy")

	recorded, readErr := os.ReadFile(calls)
	require.NoError(t, readErr)
	assert.NotContains(t, string(recorded), "ufw --force enable",
		"a firewall kip could not claim is one it must not enable")
}

// The claim is sequenced behind the first change to ufw in one command, so it
// is never reached unless the rest of that command succeeded. Were the claim
// its own command run first, a failure after it would leave a host carrying
// Kipper's name over a firewall Kipper never touched. This pins the sequencing;
// it is not a claim of atomicity, and nothing rolls the policy edit back.
func TestConfigureFirewall_SequencesTheClaimBehindTheFirstChange(t *testing.T) {
	c, calls := dialStub(t, stubHost{ufwStatus: "inactive", firewalldIs: "inactive"})

	require.NoError(t, configureFirewall(c, true))

	recorded, err := os.ReadFile(calls)
	require.NoError(t, err)

	var claimLine string
	for _, line := range strings.Split(string(recorded), "\n") {
		if strings.Contains(line, claimWrite) {
			claimLine = line
			break
		}
	}
	require.NotEmpty(t, claimLine, "the claim must be written")
	assert.Contains(t, claimLine, "DEFAULT_FORWARD_POLICY",
		"the claim must ride with the first ufw change, not run on its own")
}

// A probe that could not run is not a host without a firewall. Reading it that
// way would license kip to configure a host it never managed to look at.
func TestAuditFirewall_FailsWhenItCannotSeeTheHost(t *testing.T) {
	for _, probe := range []struct{ name, command string }{
		{"firewalld", "is-active firewalld"},
		{"ufw", "ufw status"},
	} {
		t.Run("an unreachable "+probe.name+" probe is an error, not an answer", func(t *testing.T) {
			c, _ := dialStub(t, stubHost{
				ufwStatus: "inactive", firewalldIs: "inactive", failOnCommand: probe.command,
			})

			_, err := AuditFirewall(c)

			require.Error(t, err, "a probe kip could not run must not read as no firewall")
		})
	}
}

// The decision is enforced where the host is changed. Both commands reach the
// work through here, so a caller that stopped consulting the plan still cannot
// configure a firewall the plan refused.
func TestApplyFirewallPlan(t *testing.T) {
	t.Run("configures when the plan says to", func(t *testing.T) {
		c, calls := dialStub(t, stubHost{ufwStatus: "inactive", firewalldIs: "inactive"})

		require.NoError(t, ApplyFirewallPlan(c, FirewallPlan{Configure: true}, true))

		recorded, err := os.ReadFile(calls)
		require.NoError(t, err)
		assert.Contains(t, string(recorded), "ufw --force enable")
	})

	t.Run("touches nothing when the plan says not to", func(t *testing.T) {
		c, calls := dialStub(t, stubHost{ufwStatus: "active", claimPresent: false})

		require.NoError(t, ApplyFirewallPlan(c, FirewallPlan{Configure: false}, true))

		recorded, err := os.ReadFile(calls)
		require.NoError(t, err)
		assert.NotContains(t, string(recorded), "ufw ",
			"a plan that refused the host must reach it with no ufw command at all")
	})
}

// `>` creates its destination before anything is written to it, so writing the
// note straight to the claim path would publish whatever prefix a failed write
// had already emitted. `test -s` rejects an empty file but reads a partial one
// as ownership, so a failed claim would silently become one.
func TestClaimCommand_PublishesByRenameSoAFailedWriteLeavesNothing(t *testing.T) {
	cmd := claimCommand(firewallClaimPath)

	// The `&&` is the guard, not decoration. A `;` here would publish whatever
	// prefix a failed write had already emitted, and `test -s` accepts a
	// non-empty partial file as readily as a complete one.
	assert.Contains(t, cmd, `> "$tmp" && mv "$tmp" '`+firewallClaimPath+`'`,
		"the move must be gated on the write succeeding")

	// A staged name of its own per run, or two concurrent runs share one file
	// and each publishes whatever the other had written into it.
	assert.Contains(t, cmd, "mktemp ", "each run stages under its own name")
	assert.Contains(t, cmd, `trap 'rm -f "$tmp"' EXIT`, "and clears it on any exit that did not publish")

	assert.NotContains(t, cmd, "> '"+firewallClaimPath+"'",
		"writing straight to the claim path publishes whatever a failed write emitted")
}

// An empty claim file is not a claim. A write that failed after `>` created the
// destination leaves exactly that, and reading it as ownership would hand kip
// an admin's firewall.
func TestAuditFirewall_DoesNotAcceptAnEmptyClaimFile(t *testing.T) {
	c, _ := dialStub(t, stubHost{ufwStatus: "active", claimEmpty: true})

	audit, err := AuditFirewall(c)

	require.NoError(t, err)
	assert.False(t, audit.KipperManaged, "an empty file records nothing")
	assert.True(t, audit.BlocksConfiguration())
}

// The command run by a real shell rather than parsed by one, which is the only
// way to know the note survives quoting and the staged file does not linger.
//
// The directory name carries a space and a `;` on purpose. Kipper claims one
// fixed path with no shell syntax in it, so nothing here reproduces production;
// what it pins is that the command is built by quoting its operands rather than
// by trusting them. Unquoted, the `;` ends the mkdir and the claim never
// arrives where it was asked for.
//
// Two things keep that safe while a guard is reverted, which is when this test
// is deliberately run against a broken command. The shell runs with its working
// directory set to the temp root, so a relative operand produced by word
// splitting (`mkdir -p <root>/kipper dir` makes `dir` a second operand) is
// created inside the root rather than in the checkout. And the only command the
// `;` can reach is `true`. A temp root that itself carried shell syntax would
// defeat both, so the test skips rather than build a command from one.
func TestClaimCommand_WritesTheNoteAndLeavesNoStagedFile(t *testing.T) {
	root := t.TempDir()
	if strings.ContainsAny(root, "'\"$`;&|<>()*?[]{}!#~ \t\n\\") {
		t.Skipf("temp root %q carries shell syntax, and this test builds a shell command from it", root)
	}
	claimPath := filepath.Join(root, "kipper dir; true", "firewall-managed")

	cmd := exec.Command("sh", "-c", claimCommand(claimPath))
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	written, err := os.ReadFile(claimPath)
	require.NoError(t, err, "the claim must be published at its final path")
	assert.Contains(t, string(written), "managed by Kipper")
	assert.Contains(t, string(written), "Delete this file to take the firewall over")

	leftovers, err := filepath.Glob(claimPath + ".*")
	require.NoError(t, err)
	assert.Empty(t, leftovers, "the staged file is moved, not copied, and never left behind")
}
