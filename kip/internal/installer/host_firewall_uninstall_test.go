package installer

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// savedServerRules is `ufw show added` on a kip server with two rules for one peer and
// rules an operator added by hand.
const savedServerRules = `Added user rules (see 'ufw status' for running firewall):
ufw limit 22/tcp
ufw allow 80/tcp
ufw allow 443/tcp
ufw allow 6443/tcp
ufw allow from 10.42.0.0/16 to any port 10250 proto tcp
ufw allow from 10.42.0.0/16 to any port 9100 proto tcp
ufw allow from 192.0.2.11 to any port 51820 proto udp comment 'kipper-peer'
ufw allow from 192.0.2.11 to any port 10250 proto tcp comment 'kipper-peer'
ufw allow 8080/tcp
ufw allow from 203.0.113.5 to any port 5432 proto tcp
`

func TestUninstallUFWDeletesCoverOnlyKipsRules(t *testing.T) {
	assert.Equal(t, []string{
		"ufw delete allow 80/tcp",
		"ufw delete allow 443/tcp",
		"ufw delete allow 6443/tcp",
		"ufw delete allow from 10.42.0.0/16 to any port 10250 proto tcp",
		"ufw delete allow from 10.42.0.0/16 to any port 9100 proto tcp",
		"ufw delete allow from 192.0.2.11 to any port 51820 proto udp comment 'kipper-peer'",
		"ufw delete allow from 192.0.2.11 to any port 10250 proto tcp comment 'kipper-peer'",
	}, uninstallUFWDeletes(savedServerRules, []string{"22"}), "the SSH rule and the operator's rules stay")

	assert.NotContains(t, uninstallUFWDeletes(savedServerRules, []string{"22", "443"}), "ufw delete allow 443/tcp",
		"a port sshd listens on keeps its rule")
	assert.Equal(t, []string{
		"ufw delete allow 80/tcp",
		"ufw delete allow 443/tcp",
		"ufw delete allow 6443/tcp",
		"ufw delete allow from 10.42.0.0/16 to any port 9100 proto tcp",
		"ufw delete allow from 192.0.2.11 to any port 51820 proto udp comment 'kipper-peer'",
	}, uninstallUFWDeletes(savedServerRules, []string{"22", "10250"}), "sshd on the kubelet port keeps the pod and peer rules for it")
	assert.Contains(t, uninstallUFWDeletes(savedServerRules, []string{"51820"}), "ufw delete allow from 192.0.2.11 to any port 51820 proto udp comment 'kipper-peer'",
		"sshd listens on TCP, so a UDP rule never keeps it reachable")
	assert.Empty(t, uninstallUFWDeletes("Added user rules (see 'ufw status' for running firewall):\nufw limit 22/tcp\n", []string{"22"}),
		"a host with only the SSH rule has nothing to delete")
}

func TestRemoveKipperFirewallDeletesKipsUFWRulesAndKeepsSSH(t *testing.T) {
	host := &routedRunner{routes: []route{
		{contains: "firewall-managed", out: "claimed\n"},
		{contains: "sshd -T", out: "22\n"},
		{contains: "ufw show added", out: savedServerRules},
	}}

	acted, err := removeKipperFirewall(host)

	require.NoError(t, err)
	assert.True(t, acted)
	assert.True(t, host.ran(ufwTimeout+"ufw delete allow 6443/tcp"))
	assert.True(t, host.ran("ufw delete allow from 192.0.2.11 to any port 51820 proto udp comment 'kipper-peer'"))
	assert.True(t, host.ran(ufwForwardDropCommand("")), "routed traffic goes back to deny")
	for _, c := range host.commands {
		assert.NotContains(t, c, "22/tcp", "the SSH rule is never touched")
		assert.NotContains(t, c, "8080", "the operator's rule is never touched")
		assert.NotContains(t, c, "disable", "the firewall stays on")
		assert.NotContains(t, c, "reset", "the firewall stays on")
		assert.NotContains(t, c, "firewall-cmd", "firewalld is not kip's here")
	}
}

func TestRemoveKipperFirewallLeavesAFirewallKipDoesNotManageAlone(t *testing.T) {
	host := &routedRunner{routes: []route{{contains: "sshd -T", out: "22\n"}}}

	acted, err := removeKipperFirewall(host)

	require.NoError(t, err)
	assert.False(t, acted)
	assert.False(t, host.ran("ufw "), "an operator's ufw is left alone")
	assert.False(t, host.ran("firewall-cmd"), "an operator's firewalld is left alone")
}

func TestRemoveKipperFirewallLeavesAPendingFirewallChangeAlone(t *testing.T) {
	host := &routedRunner{routes: []route{
		{contains: "firewall-managed", out: "claimed\n"},
		{contains: "sshd -T", out: "22\n"},
		{contains: firewallChangePendingCommand(""), out: "pending\n"},
		{contains: "ufw show added", out: savedServerRules},
	}}

	_, err := removeKipperFirewall(host)

	require.Error(t, err, "a rollback still to run would bring the old rules back or race the removal")
	assert.False(t, host.ran("ufw "))
}

func TestFirewallChangePendingCommandOnAHost(t *testing.T) {
	for name, s := range map[string]stateScene{
		"nothing pending":         newStateScene(t, "firewall-managed"),
		"a ufw reservation":       newStateScene(t, "firewall-managed", "ufw-run"),
		"a firewalld reservation": newStateScene(t, "firewalld-managed", "firewalld-run"),
		"an active rollback unit": func() stateScene {
			s := newStateScene(t, "firewall-managed")
			s.activeUnit = firewalldRollbackUnit + "-abc123.timer loaded active waiting"
			return s
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			out := s.runScript(firewallChangePendingCommand(s.root))
			assert.Equal(t, name != "nothing pending", strings.TrimSpace(out) == "pending", out)
		})
	}
}

// runScript runs script with the scene's flock and systemctl stand-ins.
func (s stateScene) runScript(script string) string {
	s.t.Helper()
	for name, body := range map[string]string{
		"flock":     "exit 0",
		"systemctl": "echo '" + s.activeUnit + "'",
	} {
		//nolint:gosec // The command stand-ins must be executable.
		require.NoError(s.t, os.WriteFile(filepath.Join(s.bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	}
	cmd := exec.Command("sh", "-c", script) //nolint:gosec // The test runs a generated script against temporary files.
	cmd.Env = []string{"PATH=" + s.bin + ":/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	require.NoError(s.t, err, string(out))
	return string(out)
}

func TestRemoveKipperFirewallTouchesNothingWithoutSSHPorts(t *testing.T) {
	host := &routedRunner{routes: []route{
		{contains: "firewall-managed", out: "claimed\n"},
		{contains: "ufw show added", out: savedServerRules},
	}}

	_, err := removeKipperFirewall(host)

	require.Error(t, err, "without sshd's ports kip cannot tell which rule keeps SSH open")
	assert.False(t, host.ran("ufw delete"))
}

// firewalldDumpRoutes supplies runtime and saved configuration dumps for a running firewalld.
func firewalldDumpRoutes(running, saved string) []route {
	return []route{
		{contains: "firewall-cmd --state 2>/dev/null || true", out: "running\n"},
		{contains: `fc() { firewall-cmd --permanent "$@" 2>/dev/null; }`, out: saved},
		{contains: `fc() { firewall-cmd "$@" 2>/dev/null; }`, out: running},
	}
}

func TestRemoveKipperFirewallRemovesKipsFirewalldConfiguration(t *testing.T) {
	const dump = "== zone public\n  ports: 22/tcp 80/tcp 443/tcp 6443/tcp\n"
	host := &routedRunner{routes: append([]route{
		{contains: "firewalld-managed", out: "claimed\n"},
		{contains: "sshd -T", out: "22\n"},
	}, firewalldDumpRoutes(dump, dump)...)}

	acted, err := removeKipperFirewall(host)

	require.NoError(t, err)
	assert.True(t, acted)
	assert.True(t, host.ran(firewalldUninstallCommand("", []string{"22"})))
	assert.False(t, host.ran("ufw "), "ufw is not kip's here")
}

func TestRemoveKipperFirewallLeavesFirewalldWithRuntimeOnlyRulesAlone(t *testing.T) {
	host := &routedRunner{routes: append([]route{
		{contains: "firewalld-managed", out: "claimed\n"},
		{contains: "sshd -T", out: "2222\n"},
	}, firewalldDumpRoutes("== zone public\n  ports: 2222/tcp 80/tcp\n", "== zone public\n  ports: 22/tcp 80/tcp\n")...)}

	_, err := removeKipperFirewall(host)

	require.Error(t, err, "the reload would drop a rule that exists only at runtime, here the one for SSH")
	assert.Contains(t, err.Error(), "zone public")
	assert.False(t, host.ran("--delete-zone"))

	host = &routedRunner{routes: append([]route{
		{contains: "firewalld-managed", out: "claimed\n"},
		{contains: "sshd -T", out: "443\n"},
	}, firewalldDumpRoutes("== zone public\n  ports: 443/tcp 8080/tcp\n", "== zone public\n  ports: 443/tcp\n")...)}
	_, err = removeKipperFirewall(host)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the public ports 80/tcp, 6443/tcp by hand", "the advice never names a port sshd listens on")
}

func TestUninstallHostContinuesWhenTheFirewallRulesCannotBeRemoved(t *testing.T) {
	noUFWRetryDelay(t)
	host := &routedRunner{routes: []route{
		{contains: "firewall-managed", out: "claimed\n"},
		{contains: "sshd -T", out: "22\n"},
		{contains: "ufw show added", err: errors.New("ufw: command not found")},
	}}

	require.NoError(t, UninstallHost(host), "a firewall left as it was must not stop the wipe")

	assert.Equal(t, ufwAttempts, strings.Count(strings.Join(host.commands, "\n"), "ufw show added"), "ufw failures are retried as elsewhere")
	assert.True(t, host.ran("rm -rf /var/lib/longhorn"), "the data sweep still runs")
	assert.True(t, host.ran(clearKipperStateCommand("")), "kip's state is still cleared")
}

func TestUninstallHostRemovesKipsFirewallRules(t *testing.T) {
	host := &routedRunner{routes: []route{
		{contains: "firewall-managed", out: "claimed\n"},
		{contains: "sshd -T", out: "22\n"},
		{contains: "ufw show added", out: savedServerRules},
	}}

	require.NoError(t, UninstallHost(host))

	assert.True(t, host.ran("ufw delete allow 443/tcp"))
}

// gnuSed shadows BSD sed so scripts written for GNU sed's -i run on macOS.
func gnuSed(t *testing.T, bin string) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return
	}
	//nolint:gosec // The stand-in must be executable.
	require.NoError(t, os.WriteFile(filepath.Join(bin, "sed"),
		[]byte("#!/bin/sh\nif [ \"$1\" = -i ]; then shift; exec /usr/bin/sed -i '' \"$@\"; fi\nexec /usr/bin/sed \"$@\"\n"), 0o755))
}

func TestUFWForwardDropCommandOnAHost(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	gnuSed(t, bin)
	path := filepath.Join(root, "etc", "default", "ufw")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte("IPV6=yes\nDEFAULT_FORWARD_POLICY=\"ACCEPT\"\nDEFAULT_INPUT_POLICY=\"DROP\"\n"), 0o600))

	cmd := exec.Command("sh", "-c", ufwForwardDropCommand(root)) //nolint:gosec // The test runs a generated command against a temporary file.
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	saved, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "IPV6=yes\nDEFAULT_FORWARD_POLICY=\"DROP\"\nDEFAULT_INPUT_POLICY=\"DROP\"\n", string(saved))
	assert.NotContains(t, ufwForwardDropCommand(""), "ufw ", "ufw is not restarted, so the SSH session is never at risk")
}

// firewalldUninstallScene runs the firewalld removal script against stand-ins
// for firewall-cmd and firewall-offline-cmd that keep their state in files.
type firewalldUninstallScene struct {
	t                    *testing.T
	root, bin, log, data string
}

func newFirewalldUninstallScene(t *testing.T, running bool, zones, policies, publicPorts string) firewalldUninstallScene {
	t.Helper()
	s := firewalldUninstallScene{t: t, root: t.TempDir(), bin: t.TempDir()}
	s.log, s.data = filepath.Join(s.root, "calls.log"), filepath.Join(s.root, "fw")
	require.NoError(t, os.MkdirAll(s.data, 0o750))
	for name, body := range map[string]string{"zones": zones, "policies": policies, "public-ports": publicPorts} {
		require.NoError(t, os.WriteFile(filepath.Join(s.data, name), []byte(body+"\n"), 0o600))
	}
	if running {
		require.NoError(t, os.WriteFile(filepath.Join(s.data, "running"), nil, 0o600))
	}
	for _, rel := range []string{"/zones/kipper-pods.xml.old", "/policies/kipper-pods-in.xml.old", "/zones/public.xml.old", "/zones/operator.xml"} {
		path := filepath.Join(s.root, firewalldConfigDir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, nil, 0o600))
	}
	fake := `name=$(basename "$0"); [ "$1" = --permanent ] && shift
echo "$name $*" >> ` + s.log + `
d=` + s.data + `
case "$1" in
  --state) [ -e $d/running ] || exit 252 ;;
  --get-zones) cat $d/zones ;;
  --get-policies) cat $d/policies ;;
  --delete-zone=*) tr ' ' '\n' < $d/zones | grep -vx "${1#--delete-zone=}" | tr '\n' ' ' > $d/z; mv $d/z $d/zones ;;
  --delete-policy=*) tr ' ' '\n' < $d/policies | grep -vx "${1#--delete-policy=}" | tr '\n' ' ' > $d/z; mv $d/z $d/policies ;;
  --zone=public)
    case "$2" in
      --query-port=*) [ -e $d/query-fails ] && exit 2; grep -qx "${2#--query-port=}" $d/public-ports || { echo no; exit 1; }; echo yes ;;
      --remove-port=*) grep -vx "${2#--remove-port=}" $d/public-ports > $d/p; mv $d/p $d/public-ports ;;
    esac ;;
esac
exit 0`
	for _, name := range []string{"firewall-cmd", "firewall-offline-cmd"} {
		//nolint:gosec // The stand-ins must be executable.
		require.NoError(t, os.WriteFile(filepath.Join(s.bin, name), []byte("#!/bin/sh\n"+fake+"\n"), 0o755))
	}
	return s
}

func (s firewalldUninstallScene) run(sshPorts ...string) {
	s.t.Helper()
	out, err := s.try(sshPorts...)
	require.NoError(s.t, err, out)
}

func (s firewalldUninstallScene) try(sshPorts ...string) (string, error) {
	cmd := exec.Command("sh", "-c", firewalldUninstallCommand(s.root, sshPorts)) //nolint:gosec // The test runs a generated script against stand-ins.
	cmd.Env = []string{"PATH=" + s.bin + ":/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (s firewalldUninstallScene) state(name string) []string {
	b, err := os.ReadFile(filepath.Join(s.data, name))
	require.NoError(s.t, err)
	return strings.Fields(string(b))
}

func (s firewalldUninstallScene) calls() string {
	b, err := os.ReadFile(s.log)
	require.NoError(s.t, err)
	return string(b)
}

func (s firewalldUninstallScene) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(s.root, firewalldConfigDir, rel))
	return err == nil
}

func TestFirewalldUninstallCommandOnARunningServer(t *testing.T) {
	s := newFirewalldUninstallScene(t, true,
		"block dmz drop kipper-peers kipper-pods operator public trusted",
		"allow-host-ipv6 kipper-pods-in kipper-pods-out",
		"22/tcp\n80/tcp\n443/tcp\n6443/tcp\n8080/tcp")

	s.run("22")

	calls := s.calls()
	for _, want := range []string{
		"firewall-cmd --delete-policy=kipper-pods-out",
		"firewall-cmd --delete-policy=kipper-pods-in",
		"firewall-cmd --delete-zone=kipper-pods",
		"firewall-cmd --delete-zone=kipper-peers",
		"firewall-cmd --zone=public --remove-port=80/tcp",
		"firewall-cmd --zone=public --remove-port=443/tcp",
		"firewall-cmd --zone=public --remove-port=6443/tcp",
		"firewall-cmd --reload",
	} {
		assert.Contains(t, calls, want+"\n")
	}
	assert.Less(t, strings.Index(calls, "--delete-policy=kipper-pods-in"), strings.Index(calls, "--delete-zone=kipper-pods\n"),
		"the policies name the pod zone, so they go first")
	assert.NotContains(t, calls, "--delete-zone=kipper-server", "a zone that is not there is not deleted")
	assert.NotContains(t, calls, "allow-host-ipv6")
	assert.NotContains(t, calls, "22/tcp", "the SSH port is never touched")
	assert.NotContains(t, calls, "8080", "the operator's port is never touched")
	assert.NotContains(t, calls, "firewall-offline-cmd")
	assert.False(t, s.exists("zones/kipper-pods.xml.old"), "firewalld's copies of kip's files go too")
	assert.False(t, s.exists("policies/kipper-pods-in.xml.old"))
	assert.True(t, s.exists("zones/public.xml.old"), "the public zone's copy is firewalld's own")
	assert.True(t, s.exists("zones/operator.xml"))
	assert.Equal(t, []string{"block", "dmz", "drop", "operator", "public", "trusted"}, s.state("zones"))
	assert.Equal(t, []string{"allow-host-ipv6"}, s.state("policies"))
	assert.Equal(t, []string{"22/tcp", "8080/tcp"}, s.state("public-ports"))

	require.NoError(t, os.WriteFile(s.log, nil, 0o600))
	s.run("22")
	assert.NotContains(t, s.calls(), "--delete-", "a second run finds nothing left to delete")
	assert.NotContains(t, s.calls(), "--remove-port")
}

func TestFirewalldUninstallCommandStopsWhenAPortQueryFails(t *testing.T) {
	s := newFirewalldUninstallScene(t, true, "kipper-pods public", "kipper-pods-in", "22/tcp\n80/tcp")
	require.NoError(t, os.WriteFile(filepath.Join(s.data, "query-fails"), nil, 0o600))

	out, err := s.try("22")

	require.Error(t, err, "a failed query is an error, not a closed port")
	assert.Contains(t, out, "could not say whether port 80/tcp is open")
	assert.NotContains(t, s.calls(), "--reload")
}

func TestFirewalldUninstallCommandOnAStoppedFirewalld(t *testing.T) {
	s := newFirewalldUninstallScene(t, false,
		"block kipper-pods public",
		"kipper-pods-in kipper-pods-out",
		"22/tcp\n443/tcp")

	s.run("22", "443")

	calls := s.calls()
	assert.Contains(t, calls, "firewall-offline-cmd --delete-zone=kipper-pods\n", "a stopped firewalld is changed offline")
	assert.NotContains(t, calls, "--remove-port=443/tcp", "sshd listening on 443 keeps that port open")
	assert.NotContains(t, calls, "--reload", "a stopped firewalld is not started")
}

func TestFirewalldUninstallCommandKeepsTheTunnelPortForTheSession(t *testing.T) {
	s := newFirewalldUninstallScene(t, true, "kipper-pods public", "kipper-pods-in", "22/tcp\n443/udp")
	peers, err := RenderOpsPeers(9, 443, sampleDevices())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(s.root, opsDir), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(s.root, opsDir, "peers.conf"), []byte(peers), 0o600))

	s.run("22")

	assert.NotContains(t, s.calls(), "--remove-port=443/udp", "The uninstall may still run through the tunnel. The scheduled removal closes the port.")
	assert.Equal(t, []string{"22/tcp", "443/udp"}, s.state("public-ports"))
}
