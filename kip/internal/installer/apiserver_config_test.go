package installer

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostScript records commands and supplies configurable responses.
type hostScript struct {
	reply    func(command string) (string, error)
	commands []string
	stdins   map[int]string
	lockHeld bool
}

func (h *hostScript) Run(command string) (string, error) {
	h.commands = append(h.commands, command)
	if strings.Contains(command, "mkdir "+apiServerLockPath) {
		if h.lockHeld {
			return lockHeldMarker + " 42\n", assert.AnError
		}
		return lockAcquiredMarker + "\n", nil
	}
	if h.reply == nil {
		return "", nil
	}
	return h.reply(command)
}

func (h *hostScript) RunStdin(command string, in io.Reader) (string, error) {
	b, _ := io.ReadAll(in)
	if h.stdins == nil {
		h.stdins = map[int]string{}
	}
	h.stdins[len(h.commands)] = string(b)
	return h.Run(command)
}

func (h *hostScript) sent(fragment string) bool {
	for _, c := range h.commands {
		if strings.Contains(c, fragment) {
			return true
		}
	}
	return false
}

// indexOf returns the position of the first command containing fragment, or -1.
func (h *hostScript) indexOf(fragment string) int {
	for i, c := range h.commands {
		if strings.Contains(c, fragment) {
			return i
		}
	}
	return -1
}

var base64Payload = regexp.MustCompile(`printf %s '([A-Za-z0-9+/=]+)' \| base64 -d`)

// written returns the first config write's content from stdin or a base64 payload,
// or an empty string if no matching write was recorded.
func (h *hostScript) written(t *testing.T) string {
	t.Helper()
	for i, c := range h.commands {
		if !strings.Contains(c, k3sConfigPath) {
			continue
		}
		if in, ok := h.stdins[i]; ok && strings.Contains(c, `cat > "$staged"`) {
			return in
		}
		if m := base64Payload.FindStringSubmatch(c); m != nil {
			decoded, err := base64.StdEncoding.DecodeString(m[1])
			require.NoError(t, err)
			return string(decoded)
		}
	}
	return ""
}

const currentConfig = `tls-san:
  - "203.0.113.10"
disable:
  - traefik
resolv-conf: /etc/rancher/k3s/resolv.conf
kube-apiserver-arg:
  - "authentication-config=/etc/rancher/k3s/authn-config.yaml"
  - "audit-policy-file=/etc/rancher/k3s/audit-policy.yaml"
  - "audit-log-path=/var/lib/rancher/k3s/server/logs/audit.log"
  - "audit-log-maxage=30"
  - "audit-log-maxbackup=10"
  - "audit-log-maxsize=100"
kubelet-arg:
  - "protect-kernel-defaults=true"
`

// oldConfig represents a config without API server arguments.
const oldConfig = `tls-san:
  - "203.0.113.10"
disable:
  - traefik
resolv-conf: /etc/rancher/k3s/resolv.conf
`

func hostWithConfig(config string) *hostScript {
	return &hostScript{reply: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "cat "+k3sConfigPath):
			return config, nil
		case strings.Contains(cmd, "list-unit-files k3s.service"):
			return "1", nil
		case strings.Contains(cmd, "apiserver_authentication_config_controller_last_config_info"):
			return "1", nil
		}
		return "", nil
	}}
}

// hostRunningPolicy lets tests control authentication status and policy changes independently.
func hostRunningPolicy(config string, authnLoaded, policyChanged bool) *hostScript {
	return &hostScript{reply: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "cat "+k3sConfigPath):
			return config, nil
		case strings.Contains(cmd, "list-unit-files k3s.service"):
			return "1", nil
		case strings.Contains(cmd, "apiserver_authentication_config_controller_last_config_info"):
			if authnLoaded {
				return "1", nil
			}
			return "0", nil
		case strings.Contains(cmd, auditPolicyPath) && strings.Contains(cmd, "base64 -d"):
			if policyChanged {
				return auditPolicyChangedMarker, nil
			}
			return "", nil
		}
		return "", nil
	}}
}

func TestEnsureAPIServerConfigLeavesACurrentClusterAlone(t *testing.T) {
	host := hostWithConfig(currentConfig)

	changed, err := EnsureAPIServerConfig(host, nil)

	require.NoError(t, err)
	assert.False(t, changed)
	assert.False(t, host.sent("systemctl restart k3s"),
		"an upgrade must not restart a control plane it has nothing to change")
	assert.Empty(t, host.written(t), "nothing may be written to a config that is already current")
}

func TestEnsureAPIServerConfigAddsTheArgumentsAndRestarts(t *testing.T) {
	host := hostWithConfig(oldConfig)

	changed, err := EnsureAPIServerConfig(host, nil)

	require.NoError(t, err)
	assert.True(t, changed)

	written := host.written(t)
	assert.Equal(t, oldConfig+k3sAPIServerArgs, written,
		"the repair appends the block and keeps every setting that was there")
	assert.Contains(t, written, "resolv-conf:", "the curated resolver survives")
	assert.Contains(t, written, "disable:", "traefik stays disabled")

	assert.True(t, host.sent("systemctl restart k3s"))
	assert.True(t, host.sent("kubectl get --raw /readyz"))
	assert.False(t, host.sent("kubectl wait --for=condition=Ready node --all"),
		"an unrelated agent being NotReady must not roll back a repaired control plane")

	// Referenced files must be prepared before restarting with their flags.
	restart := host.indexOf("systemctl restart k3s")
	assert.Less(t, host.indexOf(authnConfigPath), restart, "the authentication stub lands first")
	assert.Less(t, host.indexOf(auditPolicyPath), restart, "the audit policy lands first")
	assert.Less(t, host.indexOf("base64 -d"), restart, "the config is written before the restart")
}

func TestInstallAndRepairShareOneArgumentBlock(t *testing.T) {
	assert.Contains(t, k3sConfig, k3sAPIServerArgs)
}

func TestEnsureAPIServerConfigRefusesArgumentsItDidNotWrite(t *testing.T) {
	foreign := oldConfig + `kube-apiserver-arg:
  - "enable-admission-plugins=PodNodeSelector"
`
	host := hostWithConfig(foreign)

	_, err := EnsureAPIServerConfig(host, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "authentication-config=",
		"the refusal has to carry the lines the operator must add")
	assert.False(t, host.sent("systemctl restart k3s"))
	assert.Empty(t, host.written(t), "a config kip did not write is never rewritten")

	// Prepare referenced files before suggesting flags that use them.
	assert.True(t, host.sent(authnConfigPath), "the authentication stub is in place before the operator is sent to add the flag")
	assert.True(t, host.sent(auditPolicyPath), "the audit policy is in place too")
}

func TestEnsureAPIServerConfigRestoresTheConfigWhenTheNodeDoesNotComeBack(t *testing.T) {
	host := &hostScript{}
	host.reply = func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "cat "+k3sConfigPath):
			return oldConfig, nil
		case strings.Contains(cmd, "list-unit-files k3s.service"):
			return "1", nil
		case strings.Contains(cmd, "kubectl get --raw /readyz"):
			return "", assert.AnError
		}
		return "", nil
	}

	_, err := EnsureAPIServerConfig(host, nil)

	require.Error(t, err)
	assert.True(t, host.sent("cp -p "+k3sConfigBackupPath),
		"the backup must be copied back over the config it replaced")
	restores := host.indexOf("cp -p " + k3sConfigBackupPath)
	restarts := 0
	for i, c := range host.commands {
		if strings.Contains(c, "systemctl restart k3s") {
			restarts++
			if restarts == 2 {
				assert.Greater(t, i, restores, "k3s restarts again after the config is put back")
			}
		}
	}
	assert.Equal(t, 2, restarts, "the rollback restarts k3s on the restored config")
}

func TestEnsureAPIServerConfigRefusesWhenTheConfigIsMissing(t *testing.T) {
	host := hostWithConfig("")

	_, err := EnsureAPIServerConfig(host, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), k3sConfigPath)
	assert.False(t, host.sent("systemctl restart k3s"))
	assert.Empty(t, host.written(t))
}

func TestEnsureAPIServerConfigAnnouncesTheRestartBeforeItHappens(t *testing.T) {
	host := hostWithConfig(oldConfig)
	var said []string

	_, err := EnsureAPIServerConfig(host, func(m string) { said = append(said, m) })

	require.NoError(t, err)
	require.NotEmpty(t, said, "an operator watching an upgrade has to be told the control plane restarts")
	assert.Contains(t, strings.Join(said, "\n"), "restart")
}

func TestEnsureAPIServerConfigJoinsAConfigThatEndsWithoutANewline(t *testing.T) {
	host := hostWithConfig(strings.TrimRight(oldConfig, "\n"))

	changed, err := EnsureAPIServerConfig(host, nil)

	require.NoError(t, err)
	assert.True(t, changed)
	written := host.written(t)
	assert.Equal(t, oldConfig+k3sAPIServerArgs, written)
	assert.NotContains(t, written, "resolv.confkube-apiserver-arg")

	state, err := classifyAPIServerArgs(written)
	require.NoError(t, err)
	assert.Equal(t, apiServerArgsCurrent, state, "what gets written has to parse as the arguments it claims to add")
}

func TestEnsureAPIServerConfigIgnoresArgumentsThatAreOnlyMentionedInComments(t *testing.T) {
	commented := "# kube-apiserver-arg:\n#   - \"authentication-config=/etc/rancher/k3s/authn-config.yaml\"\n" + oldConfig
	host := hostWithConfig(commented)

	changed, err := EnsureAPIServerConfig(host, nil)

	require.NoError(t, err)
	assert.True(t, changed, "a commented-out argument configures nothing, so the repair still has work to do")
	assert.Contains(t, host.written(t), k3sAPIServerArgs)
}

func TestEnsureAPIServerConfigRefusesAPartialArgumentBlock(t *testing.T) {
	partial := oldConfig + `kube-apiserver-arg:
  - "authentication-config=/etc/rancher/k3s/authn-config.yaml"
`
	host := hostWithConfig(partial)

	_, err := EnsureAPIServerConfig(host, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "audit-policy-file=", "the refusal names what is missing")
	assert.NotContains(t, err.Error(), "authentication-config=",
		"an argument the block already sets must not be offered again; pasting it sets the flag twice")
	assert.False(t, host.sent("systemctl restart k3s"))
	assert.Empty(t, host.written(t))
}

func TestEnsureAPIServerConfigKeepsTheFilesModeAndOwner(t *testing.T) {
	host := hostWithConfig(oldConfig)

	_, err := EnsureAPIServerConfig(host, nil)
	require.NoError(t, err)

	var write string
	for _, c := range host.commands {
		if strings.Contains(c, `cat > "$staged"`) {
			write = c
		}
	}
	require.NotEmpty(t, write)
	assert.Contains(t, write, "readlink -f "+k3sConfigPath,
		"a config kept as a symlink is written through, not replaced by a plain file")
	assert.Contains(t, write, `stat -c %a "$target"`, "the mode is read from the file being replaced")
	assert.Contains(t, write, `chmod "$mode"`, "and applied to the file replacing it")
	assert.Contains(t, write, `chown "$owner"`)
	assert.Contains(t, write, `cp -p "$target" `+k3sConfigBackupPath, "the backup keeps them too")
}

func TestEnsureAPIServerConfigRefusesAConfigItCannotParse(t *testing.T) {
	host := hostWithConfig("tls-san:\n  - \"203.0.113.10\"\n  bad indentation: [\n")

	_, err := EnsureAPIServerConfig(host, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), k3sConfigPath)
	assert.False(t, host.sent("systemctl restart k3s"))
	assert.Empty(t, host.written(t))
}

func TestRollBackSaysTheStateIsUnknownWhenItCannotRestore(t *testing.T) {
	host := &hostScript{}
	host.reply = func(cmd string) (string, error) {
		if strings.Contains(cmd, "cp -p "+k3sConfigBackupPath) {
			return "", assert.AnError
		}
		return "", nil
	}

	err := rollBackK3sConfig(host, assert.AnError)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown")
	assert.Contains(t, err.Error(), k3sConfigBackupPath, "the operator is told where their previous configuration is")
}

func TestAPIServerArgListMatchesTheInstalledBlock(t *testing.T) {
	args := apiServerArgList()

	assert.Len(t, args, 6)
	assert.Contains(t, args, "authentication-config="+authnConfigPath)
	assert.Contains(t, args, "audit-policy-file="+auditPolicyPath)
}

func TestEnsureAPIServerConfigRestartsWhenTheFileIsCurrentButUnloaded(t *testing.T) {
	host := &hostScript{}
	host.reply = func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "cat "+k3sConfigPath):
			return currentConfig, nil
		case strings.Contains(cmd, "list-unit-files k3s.service"):
			return "1", nil
		case strings.Contains(cmd, "apiserver_authentication_config_controller_last_config_info"):
			return "0", nil
		}
		return "", nil
	}

	changed, err := EnsureAPIServerConfig(host, nil)

	require.NoError(t, err)
	assert.True(t, changed)
	assert.True(t, host.sent("systemctl restart k3s"), "the restart the interrupted run never performed")
	assert.Empty(t, host.written(t), "and the file it already holds is not rewritten")
}

func TestEnsureAPIServerConfigRefusesANodeRunningNoServer(t *testing.T) {
	for name, answer := range map[string]string{"no unit": "0", "no answer": ""} {
		t.Run(name, func(t *testing.T) {
			host := &hostScript{reply: func(cmd string) (string, error) {
				if strings.Contains(cmd, "list-unit-files k3s.service") {
					return answer, nil
				}
				return oldConfig, nil
			}}

			_, err := EnsureAPIServerConfig(host, nil)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "k3s server")
			assert.False(t, host.sent("systemctl restart k3s"))
			assert.Empty(t, host.written(t))
		})
	}
}

func TestEnsureAPIServerConfigRestartsWhenTheAuditPolicyChanged(t *testing.T) {
	host := hostRunningPolicy(currentConfig, true, true)

	changed, err := EnsureAPIServerConfig(host, nil)

	require.NoError(t, err)
	assert.True(t, changed)
	assert.True(t, host.sent("systemctl restart k3s"))
	assert.Empty(t, host.written(t), "the config file itself has nothing to change")
}

func TestEnsureAPIServerConfigLeavesACurrentServerAloneWhenNothingChanged(t *testing.T) {
	host := hostRunningPolicy(currentConfig, true, false)

	changed, err := EnsureAPIServerConfig(host, nil)

	require.NoError(t, err)
	assert.False(t, changed)
	assert.False(t, host.sent("systemctl restart k3s"))
}

func TestEnsureAPIServerConfigTolerateAnUnreadableAPIServer(t *testing.T) {
	host := &hostScript{}
	host.reply = func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "cat "+k3sConfigPath):
			return currentConfig, nil
		case strings.Contains(cmd, "list-unit-files k3s.service"):
			return "1", nil
		case strings.Contains(cmd, "apiserver_authentication_config_controller_last_config_info"):
			return "", assert.AnError
		}
		return "", nil
	}
	var said []string

	changed, err := EnsureAPIServerConfig(host, func(m string) { said = append(said, m) })

	require.NoError(t, err, "an unreadable metric is not a failed upgrade")
	assert.False(t, changed)
	assert.False(t, host.sent("systemctl restart k3s"), "and never a restart on a guess")
	assert.Contains(t, strings.Join(said, "\n"), "could not be asked")
}

func TestARestartOnSomeoneElsesConfigNeverRestoresTheBackup(t *testing.T) {
	host := &hostScript{}
	host.reply = func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "cat "+k3sConfigPath):
			return currentConfig, nil
		case strings.Contains(cmd, "list-unit-files k3s.service"):
			return "1", nil
		case strings.Contains(cmd, "apiserver_authentication_config_controller_last_config_info"):
			return "0", nil
		case strings.Contains(cmd, "kubectl get --raw /readyz"):
			return "", assert.AnError
		}
		return "", nil
	}

	_, err := EnsureAPIServerConfig(host, nil)

	require.Error(t, err)
	assert.False(t, host.sent("cp -p "+k3sConfigBackupPath),
		"a run that wrote no configuration has no configuration to put back")
	assert.Contains(t, err.Error(), "changed no k3s configuration")
	assert.Contains(t, err.Error(), k3sConfigBackupPath, "the operator is told what that file is")
	restarts := 0
	for _, c := range host.commands {
		if strings.Contains(c, "systemctl restart k3s") {
			restarts++
		}
	}
	assert.Equal(t, 1, restarts, "and k3s is not restarted a second time on a guess")
}

func TestAChangedPolicyRestartsEvenWhenTheServerCannotBeAsked(t *testing.T) {
	host := &hostScript{}
	host.reply = func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "cat "+k3sConfigPath):
			return currentConfig, nil
		case strings.Contains(cmd, "list-unit-files k3s.service"):
			return "1", nil
		case strings.Contains(cmd, auditPolicyPath) && strings.Contains(cmd, "base64 -d"):
			return auditPolicyChangedMarker, nil
		case strings.Contains(cmd, "apiserver_authentication_config_controller_last_config_info"):
			return "", assert.AnError
		}
		return "", nil
	}

	changed, err := EnsureAPIServerConfig(host, nil)

	require.NoError(t, err)
	assert.True(t, changed)
	assert.True(t, host.sent("systemctl restart k3s"),
		"the restart the policy needs cannot depend on a metric answering")
}

// unloadedServerAfterAnInterruptedWrite simulates an unloaded config whose
// readiness check fails until a restore command is sent. pending supplies the
// response for commands that check the pending checksum.
func unloadedServerAfterAnInterruptedWrite(pending string) *hostScript {
	host := &hostScript{}
	host.reply = func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, k3sConfigPendingPath) && strings.Contains(cmd, "sha256sum"):
			return pending + "\n", nil
		case strings.Contains(cmd, "cat "+k3sConfigPath):
			return currentConfig, nil
		case strings.Contains(cmd, "list-unit-files k3s.service"):
			return "1", nil
		case strings.Contains(cmd, "apiserver_authentication_config_controller_last_config_info"):
			return "0", nil
		case strings.Contains(cmd, "kubectl get --raw /readyz") && !restored(host):
			return "", assert.AnError
		}
		return "", nil
	}
	return host
}

func restored(h *hostScript) bool { return h.sent("cp -p " + k3sConfigBackupPath) }

func TestARetryRestoresTheConfigItsInterruptedRunWrote(t *testing.T) {
	host := unloadedServerAfterAnInterruptedWrite("ours")

	_, err := EnsureAPIServerConfig(host, nil)

	require.Error(t, err)
	assert.True(t, restored(host), "the interrupted run's own write is rolled back")
	assert.Contains(t, err.Error(), "previous configuration was restored")
	assert.True(t, host.sent("rm -f "+k3sConfigPendingPath), "a restored config is no longer a pending write")
}

func TestARetryLeavesAConfigEditedSinceTheInterruptedWrite(t *testing.T) {
	host := unloadedServerAfterAnInterruptedWrite("changed")

	_, err := EnsureAPIServerConfig(host, nil)

	require.Error(t, err)
	assert.False(t, restored(host))
	assert.Contains(t, err.Error(), "changed no k3s configuration")
}

func TestTheWriteMarksItsConfigPendingBeforeItLands(t *testing.T) {
	script := writeK3sConfigScript()

	marker := strings.Index(script, k3sConfigPendingPath)
	rename := strings.Index(script, `mv "$staged" "$target"`)
	require.GreaterOrEqual(t, marker, 0, "the write records what it wrote")
	assert.Less(t, marker, rename, "an interruption after the rename must still find the marker")
}

func TestASuccessfulRestartClearsThePendingWrite(t *testing.T) {
	host := hostWithConfig(oldConfig)

	_, err := EnsureAPIServerConfig(host, nil)

	require.NoError(t, err)
	assert.Greater(t, host.indexOf("rm -f "+k3sConfigPendingPath), host.indexOf("systemctl restart k3s"),
		"the marker is cleared once the server runs the new config")
}

func TestAPolicyRestartAlsoRestoresAnInterruptedConfigWrite(t *testing.T) {
	host := &hostScript{}
	host.reply = func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, k3sConfigPendingPath) && strings.Contains(cmd, "sha256sum") && !strings.Contains(cmd, "cp -p"):
			return "ours\n", nil
		case strings.Contains(cmd, "cat "+k3sConfigPath):
			return currentConfig, nil
		case strings.Contains(cmd, "list-unit-files k3s.service"):
			return "1", nil
		case strings.Contains(cmd, auditPolicyPath) && strings.Contains(cmd, "base64 -d"):
			return auditPolicyChangedMarker, nil
		case strings.Contains(cmd, "kubectl get --raw /readyz") && !restored(host):
			return "", assert.AnError
		}
		return "", nil
	}

	_, err := EnsureAPIServerConfig(host, nil)

	require.Error(t, err)
	assert.True(t, host.sent("cp -p "+auditPolicyBackupPath), "the policy goes back")
	assert.True(t, restored(host), "and so does the config the interrupted run wrote")
}

func TestTheWriteKeepsTheBackupOfAnUnconfirmedWrite(t *testing.T) {
	script := writeK3sConfigScript()

	guard := strings.Index(script, "cat "+k3sConfigPendingPath)
	backup := strings.Index(script, "cp -p \"$target\" "+k3sConfigBackupPath)
	require.GreaterOrEqual(t, guard, 0, "the backup refresh checks for an unconfirmed write")
	assert.Less(t, guard, backup)
}

func TestTheRollBackLeavesAConfigChangedSinceKipWroteIt(t *testing.T) {
	host := &hostScript{reply: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "[ -f "+k3sConfigBackupPath+" ]"):
			return "yes\n", nil
		case strings.Contains(cmd, "cp -p "+k3sConfigBackupPath):
			return "kipper-config-changed\n", assert.AnError
		}
		return "", nil
	}}

	err := rollBackK3sConfig(host, assert.AnError)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "changed since kip wrote it")
	assert.False(t, host.sent("systemctl restart k3s"), "nothing was restored, so there is nothing to restart onto")
	cp := host.commands[host.indexOf("cp -p "+k3sConfigBackupPath)]
	assert.Less(t, strings.Index(cp, k3sConfigPendingPath), strings.Index(cp, "cp -p "+k3sConfigBackupPath),
		"the check and the restore run in one command")
}

func TestTheRepairHoldsTheHostLockThroughout(t *testing.T) {
	host := hostWithConfig(oldConfig)

	_, err := EnsureAPIServerConfig(host, nil)

	require.NoError(t, err)
	acquire := host.indexOf("mkdir " + apiServerLockPath)
	release := len(host.commands) - 1
	require.GreaterOrEqual(t, acquire, 0, "the lock is taken")
	assert.Less(t, acquire, host.indexOf("cat "+k3sConfigPath), "before the config is read")
	assert.Contains(t, host.commands[release], "rm -rf "+apiServerLockPath, "the last command releases it")
	assert.Greater(t, release, host.indexOf("systemctl restart k3s"), "after the restart")
}

func TestTheRepairStopsWhileAnotherRunHoldsTheLock(t *testing.T) {
	host := hostWithConfig(oldConfig)
	host.lockHeld = true

	_, err := EnsureAPIServerConfig(host, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "another kip run")
	assert.Empty(t, host.written(t))
	assert.False(t, host.sent("systemctl restart k3s"))
	assert.False(t, host.sent("cat "+apiServerLockPath+"/owner"), "a lock it did not take is not released")
}

func TestTheRollBackNeedsTheMarker(t *testing.T) {
	host := &hostScript{}

	_ = rollBackK3sConfig(host, assert.AnError)

	cp := host.commands[host.indexOf("cp -p "+k3sConfigBackupPath)]
	assert.Contains(t, cp, "[ ! -f "+k3sConfigPendingPath+" ]", "a missing marker refuses the restore")
}

func TestTheLockNeedsAPositiveAnswer(t *testing.T) {
	for name, reply := range map[string]string{"empty": "", "banner only": "Welcome to the server\n"} {
		host := &stdinlessHost{reply: reply}
		_, _, err := lockAPIServerConfig(host)
		assert.Error(t, err, name)
	}
}

type stdinlessHost struct{ reply string }

func (h *stdinlessHost) Run(string) (string, error) { return h.reply, nil }
func (h *stdinlessHost) RunStdin(string, io.Reader) (string, error) {
	return h.reply, nil
}

func TestEveryRepairCommandIsFencedByTheLock(t *testing.T) {
	host := hostWithConfig(oldConfig)

	_, err := EnsureAPIServerConfig(host, nil)

	require.NoError(t, err)
	acquire := host.indexOf("mkdir " + apiServerLockPath)
	require.GreaterOrEqual(t, acquire, 0)
	for _, c := range host.commands[acquire+1 : len(host.commands)-1] {
		assert.Contains(t, c, lockLostMarker, "command ran without checking the lock: %.80q", c)
	}
}

// TestEveryFencedCommandRefreshesTheLock checks that command completion renews
// ownership so the expiry threshold applies to idle time between commands.
func TestEveryFencedCommandRefreshesTheLock(t *testing.T) {
	host := hostWithConfig(oldConfig)

	_, err := EnsureAPIServerConfig(host, nil)

	require.NoError(t, err)
	acquire := host.indexOf("mkdir " + apiServerLockPath)
	require.GreaterOrEqual(t, acquire, 0)
	for _, c := range host.commands[acquire+1 : len(host.commands)-1] {
		owner := strings.Index(c, apiServerLockPath+"/owner")
		refresh := strings.LastIndex(c, "touch -c "+apiServerLockPath)
		if !assert.Greater(t, refresh, owner, "command ended without refreshing the lock: %.80q", c) {
			continue
		}
		assert.Contains(t, c[refresh:], "exit $rc", "the refresh must not hide the command's exit status: %.80q", c)
	}
}

func TestADeadOwnersLockIsReclaimedWithinMinutes(t *testing.T) {
	host := hostWithConfig(oldConfig)

	_, _, err := lockAPIServerConfig(host)

	require.NoError(t, err)
	acquire := host.commands[host.indexOf("mkdir "+apiServerLockPath)]
	assert.LessOrEqual(t, apiServerLockTTL, 180, "a killed run must be recoverable within minutes")
	assert.Contains(t, acquire, "-gt "+strconv.Itoa(apiServerLockTTL)+" ]", "the reclaim threshold is the TTL")
	assert.Contains(t, acquire, "stat -c %Y "+apiServerLockPath, "idleness is read from the lock directory's mtime")
}

func TestTheRefusalNamesTheLockAge(t *testing.T) {
	host := hostWithConfig(oldConfig)
	host.lockHeld = true

	_, err := EnsureAPIServerConfig(host, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "42 seconds ago")
	assert.Contains(t, err.Error(), strconv.Itoa(apiServerLockTTL)+" seconds", "the operator learns how long a dead owner's lock lasts")
}

func TestTheReleaseHoldsTheGuard(t *testing.T) {
	host := hostWithConfig(oldConfig)

	_, err := EnsureAPIServerConfig(host, nil)

	require.NoError(t, err)
	release := host.commands[len(host.commands)-1]
	assert.Contains(t, release, "flock 9", "a takeover cannot slip between the owner check and the removal")
}

func TestTheConfigWriteKeepsTheConfigOutOfCommandsAndErrors(t *testing.T) {
	const token = "K10example-token::server:example"
	config := oldConfig + "token: " + token + "\n"
	host := &hostScript{reply: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, `cat > "$staged"`):
			return "", fmt.Errorf("running %q on host: %w", cmd, errors.New("exit status 1"))
		case strings.Contains(cmd, "cat "+k3sConfigPath):
			return config, nil
		case strings.Contains(cmd, "list-unit-files k3s.service"):
			return "1", nil
		case strings.Contains(cmd, "apiserver_authentication_config_controller_last_config_info"):
			return "1", nil
		}
		return "", nil
	}}

	_, err := EnsureAPIServerConfig(host, nil)

	require.Error(t, err)
	assert.Contains(t, host.written(t), token, "the config still carries its token")
	for _, c := range host.commands {
		assert.NotContains(t, c, token)
		assert.NotContains(t, c, base64.StdEncoding.EncodeToString([]byte(token))[:16])
	}
	assert.NotContains(t, err.Error(), token)
	assert.NotContains(t, err.Error(), "staged", "the failed write's command stays out of the error")
}

var errExitStatus = errors.New("exit status 1")

type sshStyleFailure struct{}

func (sshStyleFailure) Run(command string) (string, error) {
	return "", fmt.Errorf("running %q on 203.0.113.10: %w: Job for k3s.service failed", command, errExitStatus)
}

func (s sshStyleFailure) RunStdin(command string, _ io.Reader) (string, error) { return s.Run(command) }

func TestAFencedFailureNamesTheCommandRatherThanTheLockWrapper(t *testing.T) {
	runner := fencedRunner{base: sshStyleFailure{}, token: "0123456789abcdef0123456789abcdef"}

	_, err := runner.Run("systemctl restart k3s")

	require.Error(t, err)
	assert.Equal(t, `running "systemctl restart k3s" on 203.0.113.10: exit status 1: Job for k3s.service failed`, err.Error())
	assert.NotContains(t, err.Error(), runner.token)
	assert.ErrorIs(t, err, errExitStatus, "the runner's cause stays reachable")
}

// echoingFailure fails like a runner that formats the command unquoted and
// appends a shell error echoing part of it.
type echoingFailure struct{ token string }

func (e echoingFailure) Run(command string) (string, error) {
	return "", fmt.Errorf("running %s: %w: sh: 2: [: %s: unexpected operator", command, errExitStatus, e.token)
}

func (e echoingFailure) RunStdin(command string, _ io.Reader) (string, error) { return e.Run(command) }

func TestAFencedFailureNeverCarriesTheLockToken(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef"
	runner := fencedRunner{base: echoingFailure{token: token}, token: token}

	_, err := runner.Run("systemctl restart k3s")

	require.Error(t, err)
	assert.NotContains(t, err.Error(), token)
	assert.Contains(t, err.Error(), "systemctl restart k3s")
	assert.Contains(t, err.Error(), "unexpected operator", "the shell's reason stays")
	assert.ErrorIs(t, err, errExitStatus)
}
