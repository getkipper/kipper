package installer

import (
	"encoding/base64"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostScript answers each command and can fail one of them, which is what the
// rollback path needs: the restart succeeds and the node never comes back.
type hostScript struct {
	reply    func(command string) (string, error)
	commands []string
}

func (h *hostScript) Run(command string) (string, error) {
	h.commands = append(h.commands, command)
	if h.reply == nil {
		return "", nil
	}
	return h.reply(command)
}

func (h *hostScript) RunStdin(command string, _ io.Reader) (string, error) {
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
// Order is the whole safety argument here: the files the flags name have to
// exist before k3s restarts.
func (h *hostScript) indexOf(fragment string) int {
	for i, c := range h.commands {
		if strings.Contains(c, fragment) {
			return i
		}
	}
	return -1
}

var base64Payload = regexp.MustCompile(`printf %s '([A-Za-z0-9+/=]+)' \| base64 -d`)

// written returns the content the host was told to write, decoded from the
// command, so a test reads the bytes that would land on disk.
func (h *hostScript) written(t *testing.T) string {
	t.Helper()
	for _, c := range h.commands {
		if !strings.Contains(c, k3sConfigPath) {
			continue
		}
		if m := base64Payload.FindStringSubmatch(c); m != nil {
			decoded, err := base64.StdEncoding.DecodeString(m[1])
			require.NoError(t, err)
			return string(decoded)
		}
	}
	return ""
}

// currentConfig is what this version of kip installs.
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

// oldConfig is what a cluster installed before the API server was configured
// still carries: the three settings of the day and no arguments at all. Taken
// from a live cluster installed on 2026-07-18.
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

// hostRunningPolicy answers as a server whose audit policy already matches, so
// a test says which of the two restart reasons it is exercising.
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

	// The API server refuses to start when a flag names a file that is not
	// there, so both files must be written before the restart.
	restart := host.indexOf("systemctl restart k3s")
	assert.Less(t, host.indexOf(authnConfigPath), restart, "the authentication stub lands first")
	assert.Less(t, host.indexOf(auditPolicyPath), restart, "the audit policy lands first")
	assert.Less(t, host.indexOf("base64 -d"), restart, "the config is written before the restart")
}

// The block the repair appends and the block a fresh install writes are one
// constant. Two copies would drift, and the drift would be silent until an
// upgraded cluster behaved unlike an installed one.
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

	// The refusal tells the operator to add flags naming these files. An API
	// server pointed at a file that is not there does not start, so following
	// that advice has to be safe before it is given.
	assert.True(t, host.sent(authnConfigPath), "the authentication stub is in place before the operator is sent to add the flag")
	assert.True(t, host.sent(auditPolicyPath), "the audit policy is in place too")
}

// An upgrade may interrupt a control plane. It may not leave one down.
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

// A missing or unreadable config is not an invitation to write a fresh one:
// that file also disables traefik and points the cluster at its curated
// resolvers, and a config carrying only the arguments would silently undo both.
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

// YAML does not require a file to end in a newline, and splicing the block onto
// the last line produces a config k3s cannot read, found by restarting the
// control plane onto it.
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

// A comment is not configuration. Reading one as configuration skips the repair
// and leaves the cluster with no authenticator while the upgrade reports success.
func TestEnsureAPIServerConfigIgnoresArgumentsThatAreOnlyMentionedInComments(t *testing.T) {
	commented := "# kube-apiserver-arg:\n#   - \"authentication-config=/etc/rancher/k3s/authn-config.yaml\"\n" + oldConfig
	host := hostWithConfig(commented)

	changed, err := EnsureAPIServerConfig(host, nil)

	require.NoError(t, err)
	assert.True(t, changed, "a commented-out argument configures nothing, so the repair still has work to do")
	assert.Contains(t, host.written(t), k3sAPIServerArgs)
}

// Half the block is not the block. Treating it as current would leave a cluster
// authenticating operators with no audit trail of what they did.
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

// The file can hold a datastore endpoint or a node token, and mv replaces the
// inode, so an operator who tightened it must not find it world-readable
// because an upgrade rewrote it.
func TestEnsureAPIServerConfigKeepsTheFilesModeAndOwner(t *testing.T) {
	host := hostWithConfig(oldConfig)

	_, err := EnsureAPIServerConfig(host, nil)
	require.NoError(t, err)

	var write string
	for _, c := range host.commands {
		if strings.Contains(c, "base64 -d") {
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

// When the rollback cannot run, the operator is told the state is unknown and
// where the previous configuration is, rather than being assured of something
// this cannot know.
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

// The arguments the check looks for and the block the install writes are one
// definition, read back the same way k3s would read it.
func TestAPIServerArgListMatchesTheInstalledBlock(t *testing.T) {
	args := apiServerArgList()

	assert.Len(t, args, 6)
	assert.Contains(t, args, "authentication-config="+authnConfigPath)
	assert.Contains(t, args, "audit-policy-file="+auditPolicyPath)
}

// A run interrupted between the rename and the restart leaves the file current
// and the process not. Reading the file alone, every retry would agree there is
// nothing to do and the repair would never land.
func TestEnsureAPIServerConfigRestartsWhenTheFileIsCurrentButUnloaded(t *testing.T) {
	host := &hostScript{}
	host.reply = func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "cat "+k3sConfigPath):
			return currentConfig, nil
		case strings.Contains(cmd, "list-unit-files k3s.service"):
			return "1", nil
		case strings.Contains(cmd, "apiserver_authentication_config_controller_last_config_info"):
			return "0", nil // the running API server never loaded one
		}
		return "", nil
	}

	changed, err := EnsureAPIServerConfig(host, nil)

	require.NoError(t, err)
	assert.True(t, changed)
	assert.True(t, host.sent("systemctl restart k3s"), "the restart the interrupted run never performed")
	assert.Empty(t, host.written(t), "and the file it already holds is not rewritten")
}

// These are server flags. On an agent node there is no k3s.service to restart,
// so writing them there leaves a worker carrying control-plane configuration
// and a rollback that cannot run either.
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

// The API server reads the audit policy once, at startup. Writing a new one and
// stopping there leaves every already-repaired cluster running the old policy,
// which is exactly the fleet this change was for.
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

// Not knowing what the API server is running is not knowing it is behind. A
// healthy cluster whose metrics are briefly unreadable must not fail an upgrade
// that has nothing to do.
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

// The retry is where the risky restart actually happens, so it needs the same
// protection the first attempt has.
func TestConvergenceRestartRollsBackLikeTheFirstAttempt(t *testing.T) {
	host := &hostScript{}
	host.reply = func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "cat "+k3sConfigPath):
			return currentConfig, nil
		case strings.Contains(cmd, "list-unit-files k3s.service"):
			return "1", nil
		case strings.Contains(cmd, "apiserver_authentication_config_controller_last_config_info"):
			return "0", nil
		case strings.Contains(cmd, "[ -f "+k3sConfigBackupPath+" ]"):
			return "yes", nil // the interrupted run left one
		case strings.Contains(cmd, "kubectl get --raw /readyz"):
			return "", assert.AnError
		}
		return "", nil
	}

	_, err := EnsureAPIServerConfig(host, nil)

	require.Error(t, err)
	assert.True(t, host.sent("cp -p "+k3sConfigBackupPath), "the earlier configuration is put back")
}

// With no backup there is nothing to restore, and claiming otherwise would send
// an operator looking for a file that was never written.
func TestConvergenceRestartSaysSoWhenThereIsNothingToRestore(t *testing.T) {
	host := &hostScript{}
	host.reply = func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "cat "+k3sConfigPath):
			return currentConfig, nil
		case strings.Contains(cmd, "list-unit-files k3s.service"):
			return "1", nil
		case strings.Contains(cmd, "apiserver_authentication_config_controller_last_config_info"):
			return "0", nil
		case strings.Contains(cmd, "[ -f "+k3sConfigBackupPath+" ]"):
			return "no", nil
		case strings.Contains(cmd, "kubectl get --raw /readyz"):
			return "", assert.AnError
		}
		return "", nil
	}

	_, err := EnsureAPIServerConfig(host, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no earlier configuration to restore")
	assert.False(t, host.sent("cp -p "+k3sConfigBackupPath), "and no restore is attempted")
}
