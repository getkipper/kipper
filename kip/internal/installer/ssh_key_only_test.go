package installer

import (
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSSHDProbe(t *testing.T) {
	p, err := parseSSHDProbe("include\tok\npasswordauthentication yes\nkbdinteractiveauthentication no\npermitrootlogin yes\n")
	require.NoError(t, err)
	assert.True(t, p.includesDropIns)
	assert.Empty(t, p.overrides)
	assert.Equal(t, "yes", p.passwordAuth)
	assert.Equal(t, "yes", p.rootLogin)

	p, err = parseSSHDProbe("include\tmissing\noverride\t/etc/ssh/sshd_config:120\tPasswordAuthentication\tyes\npasswordauthentication no\nkbdinteractiveauthentication no\npermitrootlogin prohibit-password\n")
	require.NoError(t, err)
	assert.False(t, p.includesDropIns)
	assert.Equal(t, []string{"/etc/ssh/sshd_config:120 PasswordAuthentication yes"}, p.overrides)

	p, err = parseSSHDProbe("include\tok\noverride\t/etc/ssh/sshd_config.d/user overrides.conf:2\tpasswordauthentication\tyes\n" +
		"managed-extra\t/etc/ssh/sshd_config.d/00-kipper-key-only.conf:4\tauthorizedkeysfile\n" +
		"passwordauthentication no\nkbdinteractiveauthentication no\npermitrootlogin no\n")
	require.NoError(t, err)
	assert.Len(t, p.overrides, 1, "a filename with spaces does not shift the fields")
	assert.Len(t, p.managedExtras, 1)

	for _, permissive := range []string{
		"override\t/etc/ssh/sshd_config:9\tPasswordAuthentication\t\"yes\"",
		"override\t/etc/ssh/sshd_config:9\tAuthenticationMethods\tpublickey publickey,password",
		"override\t/etc/ssh/sshd_config:9\tAuthenticationMethods\tany",
		"override\t/etc/ssh/sshd_config:9\tKbdInteractiveAuthentication\tYes",
		"override\t/etc/ssh/sshd_config:9\tPasswordAuthentication\t'yes'",
	} {
		p, err = parseSSHDProbe(permissive + "\npasswordauthentication no\nkbdinteractiveauthentication no\npermitrootlogin no\n")
		require.NoError(t, err)
		assert.Len(t, p.overrides, 1, permissive)
	}
	p, err = parseSSHDProbe("override\t/etc/ssh/sshd_config:9\tAuthenticationMethods\tpublickey\npasswordauthentication no\nkbdinteractiveauthentication no\npermitrootlogin no\n")
	require.NoError(t, err)
	assert.Empty(t, p.overrides)

	p, err = parseSSHDProbe("include\tok\ninclude-other\t/etc/ssh/sshd_config:30\tInclude /etc/ssh/support.conf\npasswordauthentication no\nkbdinteractiveauthentication no\npermitrootlogin no\n")
	require.NoError(t, err)
	assert.Equal(t, []string{"/etc/ssh/sshd_config:30 Include /etc/ssh/support.conf"}, p.otherIncludes)

	_, err = parseSSHDProbe("sshd: command not found\n")
	assert.Error(t, err, "no effective settings means the probe failed")
}

func TestKeyOnlyDropInTightensButNeverLoosensRootLogin(t *testing.T) {
	assert.Contains(t, keyOnlyDropIn("yes"), "PermitRootLogin prohibit-password")
	// The drop-in always names the policy, because it may replace the file that
	// set a stricter one.
	for _, stricter := range []string{"no", "prohibit-password", "forced-commands-only"} {
		assert.Contains(t, keyOnlyDropIn(stricter), "PermitRootLogin "+stricter, stricter)
	}
	assert.Contains(t, keyOnlyDropIn("without-password"), "PermitRootLogin prohibit-password")
	for _, line := range []string{"PasswordAuthentication no", "KbdInteractiveAuthentication no"} {
		assert.Contains(t, keyOnlyDropIn("no"), line)
	}
}

// sshHost simulates the probe, the apply and the verification of one host.
type sshHost struct {
	probe        string
	baselineErr  error
	afterErr     error
	effective    string
	commands     []string
	stdins       []string
	freshChecks  int
	restoredHost bool
	applyErr     error
	passwordErr  error
}

func (h *sshHost) ConfirmPasswordRefused() error { return h.passwordErr }

func (h *sshHost) Run(cmd string) (string, error) {
	h.commands = append(h.commands, cmd)
	switch {
	case strings.Contains(cmd, sshdProbeMarker):
		return h.probe, nil
	case strings.HasPrefix(cmd, "echo verify"):
		return h.effective, nil
	case strings.HasPrefix(cmd, sshdRestoreMarker):
		h.restoredHost = true
	}
	return "", nil
}

func (h *sshHost) RunStdin(cmd string, in io.Reader) (string, error) {
	b, _ := io.ReadAll(in)
	h.stdins = append(h.stdins, string(b))
	out, err := h.Run(cmd)
	if strings.HasPrefix(cmd, sshdApplyMarker) && h.applyErr != nil {
		return out, h.applyErr
	}
	return out, err
}

func (h *sshHost) ConfirmFreshKeyOnlyConnection() error {
	h.freshChecks++
	if h.freshChecks == 1 {
		return h.baselineErr
	}
	return h.afterErr
}

const okProbe = "include\tok\npasswordauthentication yes\nkbdinteractiveauthentication yes\npermitrootlogin yes\n"
const keyOnlyEffective = "verify\npasswordauthentication no\nkbdinteractiveauthentication no\npermitrootlogin prohibit-password\n"

func (h *sshHost) index(part string) int {
	for i, c := range h.commands {
		if strings.Contains(c, part) {
			return i
		}
	}
	return -1
}

func TestEnsureKeyOnlySSH(t *testing.T) {
	h := &sshHost{probe: okProbe, effective: keyOnlyEffective}

	changed, err := EnsureKeyOnlySSH(h)

	require.NoError(t, err)
	assert.True(t, changed)
	require.Len(t, h.stdins, 1)
	assert.Contains(t, h.stdins[0], "PasswordAuthentication no", "the drop-in goes over stdin")
	assert.Contains(t, h.stdins[0], "PermitRootLogin prohibit-password")
	apply := h.index(sshdApplyMarker)
	require.GreaterOrEqual(t, apply, 0)
	assert.Less(t, strings.Index(h.commands[apply], "sshd -t"), strings.LastIndex(h.commands[apply], "systemctl reload"),
		"sshd validates the new config before it reloads")
	assert.Equal(t, 2, h.freshChecks, "a key-only connection is proven before and after")
	assert.Greater(t, h.index(sshdCommitMarker), apply, "the saved state goes only after verification")
	assert.False(t, h.restoredHost)
}

func TestEnsureKeyOnlySSHLeavesAKeyOnlyHostAlone(t *testing.T) {
	h := &sshHost{probe: "include\tok\npasswordauthentication no\nkbdinteractiveauthentication no\npermitrootlogin prohibit-password\n"}

	changed, err := EnsureKeyOnlySSH(h)

	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, -1, h.index(sshdApplyMarker))
}

func TestEnsureKeyOnlySSHRefuses(t *testing.T) {
	cases := map[string]*sshHost{
		"drop-ins not included first": {probe: "include\tmissing\npasswordauthentication yes\nkbdinteractiveauthentication no\npermitrootlogin yes\n"},
		"a Match block re-enables passwords": {probe: "include\tok\noverride\t/etc/ssh/sshd_config.d/60-support.conf:3\tPasswordAuthentication\tyes\n" +
			"passwordauthentication yes\nkbdinteractiveauthentication no\npermitrootlogin yes\n"},
		"an Include kip cannot follow": {probe: "include\tok\ninclude-other\t/etc/ssh/sshd_config:30\tInclude /etc/ssh/support.conf\n" +
			"passwordauthentication no\nkbdinteractiveauthentication no\npermitrootlogin no\n"},
		"the operator does not log in with a key": {probe: okProbe, baselineErr: errors.New("Permission denied (publickey)")},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := EnsureKeyOnlySSH(h)
			require.Error(t, err)
			assert.Equal(t, -1, h.index(sshdApplyMarker), "nothing is changed")
		})
	}
}

func TestEnsureKeyOnlySSHRestoresWhenVerificationFails(t *testing.T) {
	for name, h := range map[string]*sshHost{
		"no new key connection":                   {probe: okProbe, effective: keyOnlyEffective, afterErr: errors.New("connection refused")},
		"passwords still allowed":                 {probe: okProbe, effective: "verify\npasswordauthentication yes\nkbdinteractiveauthentication no\npermitrootlogin prohibit-password\n"},
		"the running sshd still offers passwords": {probe: okProbe, effective: keyOnlyEffective, passwordErr: errors.New("the server still offers password login")},
		"root login loosened":                     {probe: "include\tok\npasswordauthentication yes\nkbdinteractiveauthentication no\npermitrootlogin no\n", effective: keyOnlyEffective},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := EnsureKeyOnlySSH(h)
			require.Error(t, err)
			assert.True(t, h.restoredHost, "the previous config comes back at once")
			assert.Equal(t, -1, h.index(sshdCommitMarker))
		})
	}
}

func TestEnsureKeyOnlySSHRestoresWhenTheApplyFails(t *testing.T) {
	h := &sshHost{probe: okProbe, effective: keyOnlyEffective, applyErr: errors.New("reload failed")}

	_, err := EnsureKeyOnlySSH(h)

	require.Error(t, err)
	assert.True(t, h.restoredHost, "a change that may have landed is undone")
}

func TestEnsureKeyOnlySSHChecksTheRunningServerOfAKeyOnlyHost(t *testing.T) {
	h := &sshHost{
		probe:       "include\tok\npasswordauthentication no\nkbdinteractiveauthentication no\npermitrootlogin prohibit-password\n",
		passwordErr: errors.New("the server still offers password login"),
	}

	_, err := EnsureKeyOnlySSH(h)

	require.Error(t, err, "files that say key-only prove nothing about an sshd that was not reloaded")
	assert.Contains(t, err.Error(), "Reload sshd")
}

func TestEnsureKeyOnlySSHKeepsSettingsItDoesNotManage(t *testing.T) {
	h := &sshHost{probe: "include\tok\nmanaged-extra\t" + sshdKeyOnlyDropIn + ":4\tauthorizedkeysfile\n" + strings.TrimPrefix(okProbe, "include\tok\n")}

	_, err := EnsureKeyOnlySSH(h)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "authorizedkeysfile")
	assert.Equal(t, -1, h.index(sshdApplyMarker))
}

func TestShippedSSHDReportsProhibitPasswordAsWithoutPassword(t *testing.T) {
	h := &sshHost{probe: okProbe, effective: "verify\npasswordauthentication no\nkbdinteractiveauthentication no\npermitrootlogin without-password\n"}

	changed, err := EnsureKeyOnlySSH(h)

	require.NoError(t, err)
	assert.True(t, changed)
	assert.False(t, h.restoredHost)
}

func TestSSHDScriptsParse(t *testing.T) {
	// Check shell syntax separately because the fakes return canned output.
	for name, script := range map[string]string{"probe": sshdProbeScript, "apply": sshdApply, "restore": sshdRestore, "commit": sshdCommit, "verify": sshdVerify} {
		cmd := exec.Command("sh", "-n")
		cmd.Stdin = strings.NewReader(script)
		out, err := cmd.CombinedOutput()
		assert.NoError(t, err, "%s: %s", name, out)
	}
}
