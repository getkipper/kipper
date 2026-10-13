package installer

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/localrun"
)

const stockPublicZone = `<?xml version="1.0" encoding="utf-8"?>
<zone>
  <short>Public</short>
  <description>For use in public areas.</description>
  <service name="ssh"/>
  <service name="dhcpv6-client"/>
  <service name="cockpit"/>
  <forward/>
</zone>
`

const stockInventory = "f firewalld.conf\nf lockdown-whitelist.xml\nf zones/public.xml\n"

// loadedFirewalld is what firewalldStateCommand prints once Kipper's
// configuration for SSH port 22 is loaded, in firewalld 1.x's format.
const loadedFirewalld = `== state
running
== default
public
== panic
no
== direct
== active-zones
kipper-pods
  sources: 10.42.0.0/16 10.43.0.0/16
public (default)
  interfaces: eth0
== policies
allow-host-ipv6 kipper-pods-in kipper-pods-out
== zone public
public (active)
  target: default
  icmp-block-inversion: no
  interfaces: eth0
  sources:
  services: dhcpv6-client
  ports: 22/tcp 80/tcp 443/tcp 6443/tcp
  protocols:
  forward: yes
  masquerade: no
  forward-ports:
  source-ports:
  icmp-blocks:
  rich rules:
== zone kipper-pods
kipper-pods (active)
  target: default
  icmp-block-inversion: no
  interfaces:
  sources: 10.42.0.0/16 10.43.0.0/16
  services:
  ports: 6443/tcp 10250/tcp 9100/tcp
  protocols:
  forward: yes
  masquerade: no
  forward-ports:
  source-ports:
  icmp-blocks:
  rich rules:
== policy allow-host-ipv6
allow-host-ipv6 (active)
  priority: -15000
  target: CONTINUE
  ingress-zones: ANY
  egress-zones: HOST
  services:
  ports:
  protocols:
  masquerade: no
  forward-ports:
  source-ports:
  icmp-blocks:
  rich rules:
	rule family="ipv6" icmp-type name="neighbour-advertisement" accept
	rule family="ipv6" icmp-type name="neighbour-solicitation" accept
== policy kipper-pods-in
kipper-pods-in (active)
  priority: -1
  target: ACCEPT
  ingress-zones: public
  egress-zones: kipper-pods
  services:
  ports:
  protocols:
  masquerade: no
  forward-ports:
  source-ports:
  icmp-blocks:
  rich rules:
== policy kipper-pods-out
kipper-pods-out (active)
  priority: -1
  target: ACCEPT
  ingress-zones: kipper-pods
  egress-zones: public
  services:
  ports:
  protocols:
  masquerade: yes
  forward-ports:
  source-ports:
  icmp-blocks:
  rich rules:
`

// firewalldHost simulates command responses and uses sh -n to check syntax.
// Commands are parsed but not executed.
type firewalldHost struct {
	t                      *testing.T
	commands               []string
	active                 bool
	claimed                bool
	inventory              string
	conf                   string
	publicXML              string
	ourFiles               map[string]string
	loaded                 string
	pending                string
	fresh                  []error
	freshCalls             int
	rolledBack             []string
	script                 string
	fail                   map[string]error
	runningDump, savedDump string
	ufwActive              bool
	failStart              string
}

func newFirewalldHost(t *testing.T) *firewalldHost {
	return &firewalldHost{
		t:         t,
		inventory: stockInventory,
		conf:      "DefaultZone=public\nFirewallBackend=nftables\n",
		publicXML: stockPublicZone,
		loaded:    loadedFirewalld,
	}
}

func (h *firewalldHost) Run(cmd string) (string, error) {
	h.commands = append(h.commands, cmd)
	if out, err := exec.Command("sh", "-n", "-c", cmd).CombinedOutput(); err != nil {
		h.t.Errorf("kip built a command no shell can parse: %s\n%s", cmd, out)
	}
	if h.failStart != "" && strings.HasPrefix(cmd, h.failStart) {
		return "", errors.New("exit status 1")
	}
	for match, err := range h.fail {
		if strings.Contains(cmd, match) {
			return "", err
		}
	}
	switch {
	case cmd == "cat /etc/os-release":
		return rocky10OSRelease, nil
	case strings.HasPrefix(cmd, "systemctl is-active firewalld"):
		if h.active {
			return "active\n", nil
		}
		return "inactive\n", nil
	case strings.HasPrefix(cmd, "test -s "+firewalldClaimPath):
		if h.claimed {
			return "claimed\n", nil
		}
		return "", nil
	case strings.HasPrefix(cmd, "sshd -T"):
		return "22\n", nil
	case strings.HasPrefix(cmd, "ufw status"):
		if h.ufwActive {
			return "active\n", nil
		}
		return "", nil
	case strings.HasPrefix(cmd, "test -s /etc/kipper/firewall-managed"):
		if h.ufwActive {
			return "claimed\n", nil
		}
		return "", nil
	case cmd == pendingRollbackCommand:
		return h.pending, nil
	case cmd == firewalldInventoryCommand:
		return h.inventory, nil
	case strings.HasPrefix(cmd, "cat /etc/firewalld/firewalld.conf"):
		return h.conf, nil
	case cmd == "cat /etc/firewalld/zones/public.xml" && h.ourFiles == nil:
		return h.publicXML, nil
	case strings.HasPrefix(cmd, "cat /etc/firewalld/"):
		return h.ourFiles[strings.TrimPrefix(cmd, "cat /etc/firewalld/")], nil
	case strings.Contains(cmd, "mktemp '/etc/kipper/firewalld-managed"):
		h.claimed = true
	case cmd == firewalldStateCommand:
		if !h.active {
			return "== state\nnot running\n", nil
		}
		return h.loaded, nil
	case strings.Contains(cmd, "systemctl enable --now firewalld"):
		h.active = true
	case cmd == firewalldDumpCommand(false):
		return h.runningDump, nil
	case cmd == firewalldDumpCommand(true):
		return h.savedDump, nil
	case strings.HasPrefix(cmd, "/bin/sh /etc/kipper/firewalld-rollback-"):
		mode := "full"
		if strings.HasSuffix(cmd, " files") {
			mode = "files"
		}
		h.rolledBack = append(h.rolledBack, mode)
	}
	return "", nil
}

func (h *firewalldHost) RunStdin(cmd string, stdin io.Reader) (string, error) {
	if strings.Contains(cmd, "firewalld-rollback-") {
		b, _ := io.ReadAll(stdin)
		h.script = string(b)
	}
	return h.Run(cmd)
}

func (h *firewalldHost) SurvivesSSHRateLimit() bool { return true }

func (h *firewalldHost) ConfirmFreshConnection() error {
	h.freshCalls++
	if h.freshCalls <= len(h.fresh) {
		return h.fresh[h.freshCalls-1]
	}
	return nil
}

// index returns the position of the first command containing s, or -1.
func (h *firewalldHost) index(s string) int {
	for i, c := range h.commands {
		if strings.Contains(c, s) {
			return i
		}
	}
	return -1
}

const commitMarker = ".committed && "

func TestConfigureFirewalldOnAFreshHost(t *testing.T) {
	h := newFirewalldHost(t)

	require.NoError(t, configureFirewalld(h, rhel10Host, false))

	steps := []string{
		"cp -a /etc/firewalld /etc/kipper/firewalld-backup-",
		"mktemp '/etc/kipper/firewalld-managed",
		"/etc/firewalld/zones/kipper-pods.xml",
		"firewall-offline-cmd --check-config",
		"systemd-run --quiet --unit=" + firewalldRollbackUnit + "-",
		"systemctl enable --now firewalld",
		commitMarker,
		"systemctl restart k3s",
	}
	last := -1
	for _, s := range steps {
		i := h.index(s)
		require.Greater(t, i, last, "%q must come after the step before it\n%s", s, strings.Join(h.commands, "\n"))
		last = i
	}
	assert.Equal(t, 2, h.freshCalls, "a fresh connection is checked before the change and after it")
	assert.Empty(t, h.rolledBack)
	assert.Equal(t, -1, h.index(firewalldDumpCommand(false)), "a stopped firewalld has no runtime to compare")
	assert.Less(t, h.index("firewalld-run"), h.index("cp -a /etc/firewalld"), "the run is reserved before anything changes")
	assert.Contains(t, h.commands[h.index("systemctl enable --now firewalld")], "flock 9", "the load waits for, and loses to, a rollback that began")
	assert.Contains(t, h.script, "systemctl disable --now firewalld")
}

// Matching rules alone cannot confirm that new SSH connections succeed.
func TestConfigureFirewalldRestoresTheFirewallWhenNewConnectionsFail(t *testing.T) {
	h := newFirewalldHost(t)
	h.fresh = []error{nil, errors.New("connection timed out")}

	err := configureFirewalld(h, rhel10Host, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "restored the previous firewall")
	assert.Equal(t, []string{"full"}, h.rolledBack)
	assert.Equal(t, -1, h.index(commitMarker))
}

func TestConfigureFirewalldRestoresTheFirewallWhenTheLoadedStateDiffers(t *testing.T) {
	h := newFirewalldHost(t)
	h.loaded = strings.Replace(loadedFirewalld, "public (default)\n", "trusted\n  interfaces: eth1\npublic (default)\n", 1)

	err := configureFirewalld(h, rhel10Host, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "zone trusted is active")
	assert.Equal(t, []string{"full"}, h.rolledBack)
	assert.Equal(t, 1, h.freshCalls, "no connection check vouches for a configuration that did not load as written")
}

func TestConfigureFirewalldRestoresTheFirewallWhenItWillNotLoad(t *testing.T) {
	h := newFirewalldHost(t)
	h.fail = map[string]error{"systemctl enable --now firewalld": errors.New("job failed")}

	err := configureFirewalld(h, rhel10Host, false)

	require.Error(t, err)
	assert.Equal(t, []string{"full"}, h.rolledBack)
}

// Before anything is loaded, only the saved files go back. Reloading would
// replace a running firewall's runtime rules with its saved ones.
func TestConfigureFirewalldRestoresOnlyTheFilesBeforeLoading(t *testing.T) {
	h := newFirewalldHost(t)
	h.active, h.claimed = true, true
	h.fail = map[string]error{"firewall-cmd --check-config": errors.New("INVALID_POLICY")}

	err := configureFirewalld(h, rhel10Host, false)

	require.Error(t, err)
	assert.Equal(t, []string{"files"}, h.rolledBack)
	assert.Equal(t, -1, h.index("firewall-cmd --reload"))
}

func TestConfigureFirewalldReportsAFailedRestore(t *testing.T) {
	h := newFirewalldHost(t)
	h.fresh = []error{nil, errors.New("connection timed out")}
	h.failStart = "/bin/sh /etc/kipper/firewalld-rollback-"

	err := configureFirewalld(h, rhel10Host, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "restoring the previous firewall failed too")
	assert.NotContains(t, err.Error(), "kip restored the previous firewall")
	assert.Equal(t, -1, h.index("systemctl stop kipper-firewalld-rollback-"), "the timer stays armed to try again")
}

func TestConfigureFirewalldRefusesAStoppedFirewallWithSavedChanges(t *testing.T) {
	h := newFirewalldHost(t)
	h.publicXML = strings.Replace(stockPublicZone, "<forward/>", `<rule family="ipv4"><source address="198.51.100.0/24"/><drop/></rule>`, 1)

	err := configureFirewalld(h, rhel10Host, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "public zone rule entry")
	assert.False(t, h.claimed)
	assert.Equal(t, -1, h.index("cp -a /etc/firewalld"), "nothing is changed")
}

// A claim covers only the files Kipper writes. Anything else in the saved
// configuration, such as a policy dropping SSH, stops the run.
func TestConfigureFirewalldRefusesExtraFilesUnderItsOwnClaim(t *testing.T) {
	h := newFirewalldHost(t)
	h.claimed = true
	h.inventory += "f zones/kipper-pods.xml\nf policies/block-ssh.xml\n"

	err := configureFirewalld(h, rhel10Host, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "policies/block-ssh.xml")
	assert.Equal(t, -1, h.index("cp -a /etc/firewalld"))
}

func TestConfigureFirewalldTrustsItsOwnClaimOnAStoppedFirewall(t *testing.T) {
	h := newFirewalldHost(t)
	h.claimed = true
	h.inventory += "f zones/kipper-pods.xml\nf zones/public.xml.old\nf policies/kipper-pods-out.xml\n"

	require.NoError(t, configureFirewalld(h, rhel10Host, false))
}

func TestConfigureFirewalldChangesNothingWhenASecondConnectionFailsAlready(t *testing.T) {
	h := newFirewalldHost(t)
	h.fresh = []error{errors.New("too many authentication failures")}

	err := configureFirewalld(h, rhel10Host, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "left the firewall alone")
	assert.Equal(t, -1, h.index("cp -a /etc/firewalld"))
	assert.False(t, h.claimed)
}

func TestConfigureFirewalldWaitsForAnEarlierRollback(t *testing.T) {
	h := newFirewalldHost(t)
	h.pending = "kipper-firewalld-rollback-0123456789ab.timer\n"

	err := configureFirewalld(h, rhel10Host, false)

	assert.ErrorContains(t, err, "still waiting to be rolled back")
	assert.Equal(t, -1, h.index("cp -a /etc/firewalld"))
}

func TestConfigureFirewalldReloadsItsOwnRunningFirewall(t *testing.T) {
	h := newFirewalldHost(t)
	h.active, h.claimed = true, true

	require.NoError(t, configureFirewalld(h, rhel10Host, false))

	assert.Less(t, h.index(firewalldDumpCommand(true)), h.index("cp -a /etc/firewalld"), "runtime and saved configuration are compared first")
	assert.Equal(t, -1, h.index("--runtime-to-permanent"), "kip never saves runtime rules on the operator's behalf")
	assert.Greater(t, h.index("firewall-cmd --reload"), h.index("systemd-run"))
	assert.Equal(t, -1, h.index("enable --now firewalld"))
	assert.Greater(t, h.index("firewall-cmd --check-config"), -1, "a running firewalld checks with firewall-cmd")
	assert.Greater(t, h.index("systemctl restart k3s"), h.index(commitMarker))
	assert.Contains(t, h.script, "firewall-cmd --reload")
}

// A reload restores the saved configuration only, so a running firewalld with
// runtime-only rules could not be brought back by a rollback.
func TestConfigureFirewalldRefusesRuntimeOnlyRules(t *testing.T) {
	h := newFirewalldHost(t)
	h.active, h.claimed = true, true
	h.runningDump = "== zone public\n  ports: 22/tcp 2222/tcp\n== direct\n"
	h.savedDump = "== zone public\n  ports: 22/tcp\n== direct\n"

	err := configureFirewalld(h, rhel10Host, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "differs from its saved one (zone public)")
	assert.Equal(t, -1, h.index("cp -a /etc/firewalld"))
	assert.Greater(t, h.index("rm -f /etc/kipper/firewalld-run"), h.index("firewalld-run"), "the reservation is given back")
}

func TestFirewalldDrift(t *testing.T) {
	same := "== zone public\npublic\n  ports: 22/tcp \n== direct\n"
	assert.Empty(t, firewalldDrift(same, strings.ReplaceAll(same, "22/tcp ", "22/tcp")), "trailing spaces are not drift")
	assert.Empty(t, firewalldDrift("== zone public\n  ports: 80/tcp 22/tcp\n", "== zone public\n  ports: 22/tcp 80/tcp\n"), "order within a field is not drift")
	assert.Equal(t, []string{"direct"}, firewalldDrift(same+"ipv4 filter INPUT 0 -p tcp --dport 443 -j DROP\n", same))
	assert.Equal(t, []string{"zone trusted"}, firewalldDrift(same+"== zone trusted\n", same))
	assert.Equal(t, []string{"policy office"}, firewalldDrift(same, same+"== policy office\n"))
}

func TestConfigureFirewalldRefusesAnUnclaimedRunningFirewall(t *testing.T) {
	h := newFirewalldHost(t)
	h.active = true

	err := configureFirewalld(h, rhel10Host, false)

	assert.ErrorContains(t, err, "left it alone")
	assert.False(t, h.claimed)
}

type localHost struct{ *firewalldHost }

func (localHost) ConfirmFreshConnection() error { return localrun.ErrNoConnection }

func TestConfigureFirewalldLocally(t *testing.T) {
	ours := wantFirewalld("22").files()

	t.Run("starts a stopped stock firewall", func(t *testing.T) {
		h := newFirewalldHost(t)
		require.NoError(t, configureFirewalld(localHost{h}, rhel10Host, false))
		assert.Greater(t, h.index(commitMarker), h.index("enable --now firewalld"))
	})

	t.Run("leaves its own firewall alone when it already matches", func(t *testing.T) {
		h := newFirewalldHost(t)
		h.active, h.claimed, h.ourFiles = true, true, ours
		require.NoError(t, configureFirewalld(localHost{h}, rhel10Host, false))
		assert.Equal(t, -1, h.index("--reload"))
		assert.Equal(t, -1, h.index("cp -a /etc/firewalld"))
	})

	t.Run("refuses to change a running firewall nobody outside can check", func(t *testing.T) {
		h := newFirewalldHost(t)
		h.active, h.claimed, h.ourFiles = true, true, ours
		h.loaded = strings.Replace(loadedFirewalld, "ports: 22/tcp", "ports: 2222/tcp", 1)
		err := configureFirewalld(localHost{h}, rhel10Host, false)
		assert.ErrorContains(t, err, "differs from kip's")
		assert.Equal(t, -1, h.index("--reload"))
	})

	t.Run("refuses when the saved files differ from the loaded ones", func(t *testing.T) {
		h := newFirewalldHost(t)
		h.active, h.claimed = true, true
		h.ourFiles = map[string]string{}
		for k, v := range ours {
			h.ourFiles[k] = v
		}
		h.ourFiles["zones/public.xml"] = stockPublicZone
		err := configureFirewalld(localHost{h}, rhel10Host, false)
		assert.ErrorContains(t, err, "saved zones/public.xml differs")
	})
}

func TestFirewalldMismatches(t *testing.T) {
	want := wantFirewalld("22")
	require.Empty(t, firewalldMismatches(loadedFirewalld, want))

	cases := map[string]struct{ old, new, problem string }{
		"extra public port":     {"ports: 22/tcp 80/tcp", "ports: 22/tcp 8080/tcp 80/tcp", "zone public ports"},
		"public rich rule":      {"  rich rules:\n== zone kipper-pods", "  rich rules:\n\trule family=\"ipv4\" service name=\"ssh\" drop\n== zone kipper-pods", "zone public rich rules"},
		"public target":         {"public (active)\n  target: default", "public (active)\n  target: ACCEPT", "zone public target"},
		"public protocol":       {"  protocols:\n  forward: yes\n  masquerade: no\n  forward-ports:\n  source-ports:\n  icmp-blocks:\n  rich rules:\n== zone kipper-pods", "  protocols: gre\n  forward: yes\n  masquerade: no\n  forward-ports:\n  source-ports:\n  icmp-blocks:\n  rich rules:\n== zone kipper-pods", "zone public protocols"},
		"missing pod source":    {"  sources: 10.42.0.0/16 10.43.0.0/16\n  services", "  sources: 10.42.0.0/16\n  services", "zone kipper-pods sources"},
		"policies on two lines": {"allow-host-ipv6 kipper-pods-in kipper-pods-out", "allow-host-ipv6 kipper-pods-in\nkipper-pods-out\nblock-ssh", "policy block-ssh is loaded"},
		"stock policy ingress":  {"  ingress-zones: ANY", "  ingress-zones: public", "policy allow-host-ipv6 ingress-zones"},
		"missing policy":        {"allow-host-ipv6 kipper-pods-in kipper-pods-out", "allow-host-ipv6 kipper-pods-out", "policy kipper-pods-in is not loaded"},
		"extra policy":          {"allow-host-ipv6 kipper-pods-in", "allow-host-ipv6 block-ssh kipper-pods-in", "policy block-ssh is loaded"},
		"policy target":         {"priority: -1\n  target: ACCEPT\n  ingress-zones: public", "priority: -1\n  target: REJECT\n  ingress-zones: public", "policy kipper-pods-in target"},
		"policy port":           {"egress-zones: public\n  services:\n  ports:\n", "egress-zones: public\n  services:\n  ports: 22/tcp\n", "policy kipper-pods-out ports"},
		"stock policy rule":     {"neighbour-solicitation\" accept", "neighbour-solicitation\" accept\n\trule family=\"ipv4\" port port=\"5432\" protocol=\"tcp\" accept", "allow-host-ipv6 has rule"},
		"stopped":               {"== state\nrunning", "== state\nnot running", "firewalld state"},
		"panic":                 {"== panic\nno", "== panic\nyes", "panic mode"},
		"panic query failed":    {"== panic\nno", "== panic\nFirewallD is not running", "panic mode"},
		"direct rule":           {"== direct\n", "== direct\nipv4 filter INPUT 0 -p tcp --dport 22 -j DROP\n", "direct configuration"},
		"policy priority":       {"kipper-pods-in (active)\n  priority: -1", "kipper-pods-in (active)\n  priority: -32768", "policy kipper-pods-in priority"},
		"other default zone":    {"== default\npublic", "== default\ntrusted", "default zone"},
		"source on public":      {"  interfaces: eth0\n== policies", "  interfaces: eth0\n  sources: 0.0.0.0/0\n== policies", "public zone has sources"},
		"pod zone not active":   {"kipper-pods\n  sources: 10.42.0.0/16 10.43.0.0/16\npublic", "public", "kipper-pods zone is not active"},
		"failed query":          {"== zone kipper-pods\nkipper-pods (active)", "== zone kipper-pods\nFAILED: firewall-cmd --info-zone=kipper-pods", "could not run"},
		"missing field":         {"public (active)\n  target: default\n  icmp-block-inversion: no", "public (active)\n  target: default", "does not report icmp-block-inversion"},
		"masquerade on public":  {"forward: yes\n  masquerade: no\n  forward-ports:\n  source-ports:\n  icmp-blocks:\n  rich rules:\n== zone kipper-pods", "forward: yes\n  masquerade: yes\n  forward-ports:\n  source-ports:\n  icmp-blocks:\n  rich rules:\n== zone kipper-pods", "zone public masquerade"},
		"forward port on pods":  {"  forward-ports:\n  source-ports:\n  icmp-blocks:\n  rich rules:\n== policy allow-host-ipv6", "  forward-ports: port=22:proto=tcp:toport=2222\n  source-ports:\n  icmp-blocks:\n  rich rules:\n== policy allow-host-ipv6", "zone kipper-pods forward-ports"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			state := strings.Replace(loadedFirewalld, tc.old, tc.new, 1)
			require.NotEqual(t, loadedFirewalld, state, "the case changes nothing")
			assert.Contains(t, strings.Join(firewalldMismatches(state, want), "\n"), tc.problem)
		})
	}
}

// firewalld 2.x adds fields and marks the default zone on its info line.
func TestFirewalldMismatchesReadsFirewalld2(t *testing.T) {
	state := strings.Replace(loadedFirewalld, "public (active)\n  target: default", "public (default, active)\n  target: default\n  ingress-priority: 0\n  egress-priority: 0", 1)
	assert.Empty(t, firewalldMismatches(state, wantFirewalld("22")))
}

func TestFirewalldSavedChanges(t *testing.T) {
	want := wantFirewalld("22")
	stock := firewalldSavedConfig{inventory: stockInventory, conf: "DefaultZone=public\n", public: stockPublicZone}
	assert.Empty(t, stock.changes(false, false, want))
	assert.Empty(t, firewalldSavedConfig{inventory: "f firewalld.conf\n"}.changes(false, false, want), "an image without a public zone file is stock")

	cases := map[string]struct {
		c       firewalldSavedConfig
		claimed bool
		want    string
	}{
		"extra zone":       {firewalldSavedConfig{inventory: stockInventory + "f zones/trusted.xml\n", public: stockPublicZone}, false, "zones/trusted.xml"},
		"symlink":          {firewalldSavedConfig{inventory: "f firewalld.conf\nl zones/public.xml\n"}, false, "zones/public.xml is not a regular file"},
		"custom service":   {firewalldSavedConfig{inventory: stockInventory + "f services/dhcpv6-client.xml\n", public: stockPublicZone}, true, "services/dhcpv6-client.xml"},
		"default zone":     {firewalldSavedConfig{inventory: stockInventory, conf: "DefaultZone=drop\n", public: stockPublicZone}, false, `default zone "drop"`},
		"zone drifting":    {firewalldSavedConfig{inventory: stockInventory, conf: "AllowZoneDrifting=yes\n", public: stockPublicZone}, false, "zone drifting"},
		"public port":      {firewalldSavedConfig{inventory: stockInventory, public: strings.Replace(stockPublicZone, "<forward/>", `<port port="8080" protocol="tcp"/>`, 1)}, false, "public zone port entry"},
		"public service":   {firewalldSavedConfig{inventory: stockInventory, public: strings.Replace(stockPublicZone, `"cockpit"`, `"http"`, 1)}, false, "public zone service http"},
		"public target":    {firewalldSavedConfig{inventory: stockInventory, public: strings.Replace(stockPublicZone, "<zone>", `<zone target="ACCEPT">`, 1)}, false, "public zone target ACCEPT"},
		"ours, unclaimed":  {firewalldSavedConfig{inventory: stockInventory + "f zones/kipper-pods.xml\n", public: stockPublicZone}, false, "zones/kipper-pods.xml"},
		"claimed symlinks": {firewalldSavedConfig{inventory: stockInventory + "l zones/kipper-pods.xml\n"}, true, "zones/kipper-pods.xml is not a regular file"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Contains(t, strings.Join(tc.c.changes(tc.claimed, false, want), "\n"), tc.want)
		})
	}
}

func TestFirewalldFiles(t *testing.T) {
	files := wantFirewalld("2222").files()

	for path, content := range files {
		var v any
		require.NoError(t, xml.Unmarshal([]byte(content), &v), path)
	}
	for _, p := range firewalldPolicies {
		assert.LessOrEqual(t, len(p.name), 17, "firewalld rejects policy names longer than 17 characters: %s", p.name)
	}
	public := files["zones/public.xml"]
	assert.Contains(t, public, `<port port="2222" protocol="tcp"/>`)
	assert.NotContains(t, public, `"ssh"`, "the SSH port is opened by number, whatever sshd listens on")
	assert.NotContains(t, public, "cockpit")

	pods := files["zones/kipper-pods.xml"]
	assert.Contains(t, pods, `<source address="10.42.0.0/16"/>`)
	assert.Contains(t, pods, `<port port="10250" protocol="tcp"/>`)
	assert.NotContains(t, pods, "target=", "the pod zone rejects what it does not list")
	assert.NotContains(t, pods, `port="22"`)

	assert.Contains(t, files["policies/kipper-pods-out.xml"], "<masquerade/>")
	assert.NotContains(t, files["policies/kipper-pods-in.xml"], "<masquerade/>")
}

// testRunID names the configuration attempt the rollback tests act as.
const testRunID = "0123456789ab"

// rollbackHost lays out a host's /etc under a temporary root and puts fake
// firewall-cmd, systemctl and flock commands on PATH, so the rollback script
// and the commit command run for real.
type rollbackHost struct {
	root, bin, calls string
	run              firewalldRun
}

func newRollbackHost(t *testing.T) *rollbackHost {
	t.Helper()
	root := t.TempDir()
	h := &rollbackHost{root: root, bin: filepath.Join(root, "bin"), calls: filepath.Join(root, "calls"), run: firewalldRun{id: testRunID, root: root}}
	for _, dir := range []string{h.bin, root + firewalldConfigDir + "/zones", root + firewalldStateDir} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	fake := func(name, body string) {
		//nolint:gosec // the fake commands have to be executable
		require.NoError(t, os.WriteFile(filepath.Join(h.bin, name), []byte("#!/bin/sh\necho \""+name+" $*\" >> "+h.calls+"\n"+body), 0o755))
	}
	require.NoError(t, os.WriteFile(h.run.reservation(), []byte(testRunID+"\n"), 0o600))
	fake("firewall-cmd", "exit ${FAKE_FIREWALL_CMD_EXIT:-0}\n")
	fake("systemctl", "exit ${FAKE_SYSTEMCTL_EXIT:-0}\n")
	//nolint:gosec // the fake commands have to be executable
	require.NoError(t, os.WriteFile(filepath.Join(h.bin, "flock"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	return h
}

func (h *rollbackHost) write(t *testing.T, rel, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(h.root+rel), 0o700))
	require.NoError(t, os.WriteFile(h.root+rel, []byte(content), 0o600))
}

func (h *rollbackHost) read(rel string) string {
	b, err := os.ReadFile(h.root + rel)
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

// backUp copies the current /etc/firewalld the way backUpFirewalld does.
func (h *rollbackHost) backUp(t *testing.T) {
	t.Helper()
	out, err := exec.Command("sh", "-c", "set -e; mkdir -p "+h.run.backup()+"; cp -a "+h.root+firewalldConfigDir+" "+h.run.backup()+"/etc; touch "+h.run.backup()+"/complete").CombinedOutput()
	require.NoError(t, err, string(out))
}

func (h *rollbackHost) sh(t *testing.T, env []string, script string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"-c", script, "sh"}, args...)...)
	cmd.Env = append([]string{"PATH=" + h.bin + ":/usr/bin:/bin"}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (h *rollbackHost) callLog() string {
	b, _ := os.ReadFile(h.calls)
	return string(b)
}

func TestFirewalldRollbackRestoresTheSavedFirewall(t *testing.T) {
	h := newRollbackHost(t)
	h.write(t, "/etc/firewalld/zones/public.xml", "before")
	h.backUp(t)
	h.write(t, "/etc/firewalld/zones/public.xml", "kipper's")
	h.write(t, "/etc/firewalld/zones/kipper-pods.xml", "kipper's")
	h.write(t, firewalldClaimPath, "claim")

	out, err := h.sh(t, nil, h.run.rollbackScript(true, false))

	require.NoError(t, err, out)
	assert.Equal(t, "before", h.read("/etc/firewalld/zones/public.xml"))
	assert.Equal(t, "<missing>", h.read("/etc/firewalld/zones/kipper-pods.xml"), "files the run added go too")
	assert.Equal(t, "<missing>", h.read(firewalldClaimPath), "a claim the run wrote goes with it")
	assert.Contains(t, h.callLog(), "firewall-cmd --reload")
	assert.NotEqual(t, "<missing>", h.read(strings.TrimPrefix(h.run.started(), h.root)))
}

func TestFirewalldRollbackOfFilesOnlyLeavesTheRunningFirewall(t *testing.T) {
	h := newRollbackHost(t)
	h.write(t, "/etc/firewalld/zones/public.xml", "before")
	h.backUp(t)
	h.write(t, "/etc/firewalld/zones/public.xml", "kipper's")

	out, err := h.sh(t, nil, h.run.rollbackScript(true, true), "files")

	require.NoError(t, err, out)
	assert.Equal(t, "before", h.read("/etc/firewalld/zones/public.xml"))
	assert.NotContains(t, h.callLog(), "firewall-cmd", "nothing is reloaded")
	assert.NotContains(t, h.callLog(), " firewalld\n", "firewalld is neither started nor stopped")
}

func TestFirewalldRollbackStopsAFirewallThatWasStopped(t *testing.T) {
	h := newRollbackHost(t)
	h.backUp(t)

	out, err := h.sh(t, nil, h.run.rollbackScript(false, true))

	require.NoError(t, err, out)
	assert.Contains(t, h.callLog(), "systemctl disable --now firewalld")
	assert.NotContains(t, h.callLog(), "--reload")
}

func TestFirewalldRollbackRefusesAnIncompleteBackup(t *testing.T) {
	h := newRollbackHost(t)
	h.write(t, "/etc/firewalld/zones/public.xml", "kipper's")
	require.NoError(t, os.MkdirAll(h.run.backup()+"/etc", 0o700))

	_, err := h.sh(t, nil, h.run.rollbackScript(true, false))

	require.Error(t, err)
	assert.Equal(t, "kipper's", h.read("/etc/firewalld/zones/public.xml"), "the live configuration is not removed")
	assert.Empty(t, h.callLog())
}

func TestFirewalldRollbackFailsWhenNothingReloads(t *testing.T) {
	h := newRollbackHost(t)
	h.backUp(t)

	h.write(t, firewalldClaimPath, "claim")

	_, err := h.sh(t, []string{"FAKE_FIREWALL_CMD_EXIT=1", "FAKE_SYSTEMCTL_EXIT=1"}, h.run.rollbackScript(true, false))

	assert.Error(t, err)
	assert.Equal(t, "claim", h.read(firewalldClaimPath), "the claim stays until the restore has worked")
}

// Exercise both marker orderings; the fake flock does not test concurrent locking.
func TestFirewalldCommitAndRollbackExcludeEachOther(t *testing.T) {
	t.Run("a committed run is never rolled back", func(t *testing.T) {
		h := newRollbackHost(t)
		h.write(t, "/etc/firewalld/zones/public.xml", "before")
		h.backUp(t)
		h.write(t, "/etc/firewalld/zones/public.xml", "kipper's")

		out, err := h.sh(t, nil, h.run.commitCommand())
		require.NoError(t, err, out)
		out, err = h.sh(t, nil, h.run.rollbackScript(true, false))
		require.NoError(t, err, out)

		assert.Equal(t, "kipper's", h.read("/etc/firewalld/zones/public.xml"))
		assert.Equal(t, "<missing>", h.read(strings.TrimPrefix(h.run.backup(), h.root)+"/complete"), "the commit clears the backups")
	})

	t.Run("a run whose rollback began cannot commit", func(t *testing.T) {
		h := newRollbackHost(t)
		h.backUp(t)
		out, err := h.sh(t, nil, h.run.rollbackScript(true, true))
		require.NoError(t, err, out)

		out, err = h.sh(t, nil, h.run.commitCommand())

		require.Error(t, err)
		assert.Contains(t, out, "already restored the previous firewall")
	})
}

func TestAuditFirewallRecognisesItsOwnFirewalld(t *testing.T) {
	h := newFirewalldHost(t)
	h.active, h.claimed = true, true

	audit, err := AuditFirewall(h)

	require.NoError(t, err)
	assert.True(t, audit.KipperManaged)
	assert.False(t, audit.BlocksConfiguration())
}

// A firewalld claim is recognized only on RHEL-family hosts.
func TestAuditFirewallIgnoresAFirewalldClaimOffTheRHELFamily(t *testing.T) {
	r := &storageHostRunner{replies: map[string]string{
		"systemctl is-active firewalld": "active\n",
		firewalldClaimPath:              "claimed\n",
	}}

	audit, err := AuditFirewall(r)

	require.NoError(t, err)
	assert.False(t, audit.KipperManaged, "the stub host reads as Debian")
}

func TestK3sAndFirewallStepOrder(t *testing.T) {
	k3s, firewall := installStep{name: "k3s"}, installStep{name: "firewall"}
	names := func(steps []installStep) []string {
		var n []string
		for _, s := range steps {
			n = append(n, s.name)
		}
		return n
	}

	assert.Equal(t, []string{"firewall", "k3s"}, names(k3sAndFirewallSteps(rhel10Host, k3s, &firewall)))
	assert.Equal(t, []string{"k3s", "firewall"}, names(k3sAndFirewallSteps(debianHost, k3s, &firewall)))
	assert.Equal(t, []string{"k3s"}, names(k3sAndFirewallSteps(rhel10Host, k3s, nil)))
}

func TestApplyFirewallPlanUsesFirewalldOnTheRHELFamily(t *testing.T) {
	h := newFirewalldHost(t)

	require.NoError(t, ApplyFirewallPlan(h, FirewallPlan{Configure: true}, false))

	assert.Greater(t, h.index("systemctl enable --now firewalld"), -1)
	assert.Equal(t, -1, h.index("-q ufw"), "ufw is never installed")
	assert.Equal(t, -1, h.index("ufw --force enable"))
}

func TestFirewalldLoadLosesToARollbackThatBegan(t *testing.T) {
	h := newRollbackHost(t)
	h.backUp(t)
	out, err := h.sh(t, nil, h.run.rollbackScript(false, true))
	require.NoError(t, err, out)
	before := h.callLog()

	out, err = h.sh(t, nil, h.run.locked("systemctl enable --now firewalld"))

	require.Error(t, err)
	assert.Contains(t, out, "already restored the previous firewall")
	assert.Equal(t, before, h.callLog(), "firewalld is not started over the restored configuration")
}

// Only one run changes the firewall at a time, and a run that died leaves a
// reservation the next one can take over once it is stale.
func TestFirewalldReservation(t *testing.T) {
	h := newRollbackHost(t)
	other := firewalldRun{id: "aaaaaaaaaaaa", root: h.root}

	out, err := h.sh(t, nil, h.run.reserveCommand())
	require.NoError(t, err, out)

	out, err = h.sh(t, []string{"FAKE_SYSTEMCTL_EXIT=1"}, other.reserveCommand())
	require.Error(t, err)
	assert.Contains(t, out, "another firewall change (0123456789ab) is in progress")

	_, err = h.sh(t, nil, other.releaseCommand())
	require.NoError(t, err)
	assert.Equal(t, "0123456789ab\n", h.read(strings.TrimPrefix(h.run.reservation(), h.root)), "a run releases only its own reservation")

	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(h.run.reservation(), old, old))
	_, err = h.sh(t, nil, other.reserveCommand())
	require.Error(t, err, "a stale reservation whose rollback timer still runs is not abandoned")

	out, err = h.sh(t, []string{"FAKE_SYSTEMCTL_EXIT=1"}, other.reserveCommand())
	require.NoError(t, err, out)
	assert.Equal(t, "aaaaaaaaaaaa\n", h.read(strings.TrimPrefix(h.run.reservation(), h.root)))
}

func TestFirewalldCommitClearsOnlyItsOwnRun(t *testing.T) {
	h := newRollbackHost(t)
	other := firewalldRun{id: "aaaaaaaaaaaa", root: h.root}
	h.backUp(t)
	h.write(t, strings.TrimPrefix(other.backup(), h.root)+"/complete", "")
	out, err := h.sh(t, nil, h.run.reserveCommand())
	require.NoError(t, err, out)

	out, err = h.sh(t, nil, h.run.commitCommand())

	require.NoError(t, err, out)
	assert.Equal(t, "<missing>", h.read(strings.TrimPrefix(h.run.backup(), h.root)+"/complete"))
	assert.Equal(t, "", h.read(strings.TrimPrefix(other.backup(), h.root)+"/complete"), "another run's backup stays")
	assert.Equal(t, "<missing>", h.read(strings.TrimPrefix(h.run.reservation(), h.root)), "the commit releases the reservation")
}

func TestFirewalldRollbackReleasesTheReservation(t *testing.T) {
	h := newRollbackHost(t)
	h.backUp(t)
	out, err := h.sh(t, nil, h.run.reserveCommand())
	require.NoError(t, err, out)

	out, err = h.sh(t, nil, h.run.rollbackScript(true, true))

	require.NoError(t, err, out)
	assert.Equal(t, "<missing>", h.read(strings.TrimPrefix(h.run.reservation(), h.root)))
}

// A fresh host has no /etc/kipper until the reservation makes it.
func TestFirewalldReservationOnAFreshHost(t *testing.T) {
	h := newRollbackHost(t)
	require.NoError(t, os.RemoveAll(h.root+firewalldStateDir))

	out, err := h.sh(t, nil, h.run.reserveCommand())

	require.NoError(t, err, out)
	assert.Equal(t, testRunID+"\n", h.read(strings.TrimPrefix(h.run.reservation(), h.root)))
}

// Run A's timer restored the firewall; run B then installed and kept its own.
// A rollback of A that runs after that must leave B's firewall alone.
func TestFirewalldFinishedRollbackIsTerminal(t *testing.T) {
	h := newRollbackHost(t)
	b := firewalldRun{id: "bbbbbbbbbbbb", root: h.root}
	h.write(t, "/etc/firewalld/zones/public.xml", "before")
	h.backUp(t)
	out, err := h.sh(t, nil, h.run.rollbackScript(false, false))
	require.NoError(t, err, out)

	out, err = h.sh(t, nil, b.reserveCommand())
	require.NoError(t, err, out)
	h.write(t, "/etc/firewalld/zones/public.xml", "run b's")
	h.write(t, firewalldClaimPath, "claim")
	out, err = h.sh(t, nil, b.commitCommand())
	require.NoError(t, err, out)
	calls := h.callLog()

	out, err = h.sh(t, nil, h.run.rollbackScript(false, false))

	require.NoError(t, err, out)
	assert.Contains(t, out, "no longer holds the reservation")
	assert.Equal(t, "run b's", h.read("/etc/firewalld/zones/public.xml"))
	assert.Equal(t, "claim", h.read(firewalldClaimPath))
	assert.Equal(t, calls, h.callLog(), "firewalld is not stopped under run B")
}

// A run whose stale reservation was taken over cannot change anything when it
// resumes, and its rollback restores nothing.
func TestFirewalldTakeoverShutsOutTheFormerOwner(t *testing.T) {
	h := newRollbackHost(t)
	b := firewalldRun{id: "bbbbbbbbbbbb", root: h.root}
	h.backUp(t)
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(h.run.reservation(), old, old))
	out, err := h.sh(t, []string{"FAKE_SYSTEMCTL_EXIT=1"}, b.reserveCommand())
	require.NoError(t, err, out)
	calls := h.callLog()

	out, err = h.sh(t, nil, h.run.locked("systemctl enable --now firewalld"))
	require.Error(t, err)
	assert.Contains(t, out, "took over from this one")

	out, err = h.sh(t, nil, h.run.commitCommand())
	require.Error(t, err, out)

	out, err = h.sh(t, nil, h.run.rollbackScript(true, false))
	require.NoError(t, err, out)
	assert.Contains(t, out, "no longer holds the reservation")
	assert.Equal(t, calls, h.callLog(), "nothing is started, reloaded or stopped")
}

func TestConfigureFirewalldReleasesTheReservationWhenTheClaimFails(t *testing.T) {
	h := newFirewalldHost(t)
	h.fail = map[string]error{"mktemp '/etc/kipper/firewalld-managed": errors.New("no space left on device")}

	err := configureFirewalld(h, rhel10Host, false)

	require.Error(t, err)
	assert.Equal(t, []string{"files"}, h.rolledBack, "the rollback script restores the files and releases the run")
}

func TestRollBackFirewalldSaysWhenAnotherRunOwnsTheFirewall(t *testing.T) {
	r := &storageHostRunner{replies: map[string]string{"/bin/sh ": "this firewall change no longer holds the reservation; nothing to restore\n"}}

	err := rollBackFirewalld(r, firewalldRun{id: testRunID}, true)

	assert.ErrorContains(t, err, "another firewall change holds the firewall now")
	assert.NotContains(t, err.Error(), "restored")
}

// Another run may change the host until this one holds the reservation, so
// nothing kip decides on is read before it.
func TestConfigureFirewalldReservesBeforeLookingAtTheHost(t *testing.T) {
	h := newFirewalldHost(t)

	require.NoError(t, configureFirewalld(h, rhel10Host, false))

	reserved := h.index("mkdir -p /etc/kipper && exec 9>")
	require.Greater(t, reserved, -1)
	for _, look := range []string{"systemctl is-active firewalld", "test -s " + firewalldClaimPath, firewalldInventoryCommand, pendingRollbackCommand} {
		assert.Greater(t, h.index(look), reserved, "%q is read before the run holds the host", look)
	}
	assert.Equal(t, 1, h.freshCalls-1, "the baseline connection check runs under the reservation too")
}

func TestConfigureFirewalldGivesTheReservationBackWhenItRefuses(t *testing.T) {
	for name, setup := range map[string]func(*firewalldHost){
		"unclaimed running firewall": func(h *firewalldHost) { h.active = true },
		"saved changes":              func(h *firewalldHost) { h.inventory += "f zones/trusted.xml\n" },
		"local, already matching": func(h *firewalldHost) {
			h.active, h.claimed, h.ourFiles = true, true, wantFirewalld("22").files()
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newFirewalldHost(t)
			setup(h)
			var runner FirewallRunner = h
			if name == "local, already matching" {
				runner = localHost{h}
			}

			_ = configureFirewalld(runner, rhel10Host, false)

			assert.Greater(t, h.index("then rm -f /etc/kipper/firewalld-run"), h.index("mkdir -p /etc/kipper && exec 9>"))
			assert.Equal(t, -1, h.index("cp -a /etc/firewalld"))
		})
	}
}

// An older install's ufw claim does not license firewalld on the same host.
func TestConfigureFirewalldRefusesAnActiveUFW(t *testing.T) {
	h := newFirewalldHost(t)
	h.ufwActive = true

	err := configureFirewalld(h, rhel10Host, false)

	assert.ErrorContains(t, err, "ufw is active on this host")
	assert.Equal(t, -1, h.index("cp -a /etc/firewalld"))
}

func TestAuditFirewallBlocksAHostRunningBothManagers(t *testing.T) {
	h := newFirewalldHost(t)
	h.active, h.claimed, h.ufwActive = true, true, true

	audit, err := AuditFirewall(h)

	require.NoError(t, err)
	assert.Equal(t, "firewalld and ufw", audit.Existing)
	assert.True(t, audit.BlocksConfiguration())
}

// A timer that already fired is not a rollback still to come.
func TestPendingRollbackIgnoresAnElapsedTimer(t *testing.T) {
	h := newRollbackHost(t)
	listing := "kipper-firewalld-rollback-aaaaaaaaaaaa.timer loaded active elapsed x\nkipper-firewalld-rollback-bbbbbbbbbbbb.timer loaded active waiting x\n"
	//nolint:gosec // the fake command has to be executable
	require.NoError(t, os.WriteFile(filepath.Join(h.bin, "systemctl"), []byte("#!/bin/sh\nprintf '"+strings.ReplaceAll(listing, "\n", "\\n")+"'\n"), 0o755))

	out, err := h.sh(t, nil, pendingRollbackCommand)

	require.NoError(t, err, out)
	assert.Equal(t, "kipper-firewalld-rollback-bbbbbbbbbbbb.timer\n", out)
}

func TestFirewalldRollbackTimerUnloadsOnceFired(t *testing.T) {
	h := newFirewalldHost(t)
	require.NoError(t, configureFirewalld(h, rhel10Host, false))
	assert.Contains(t, h.commands[h.index("systemd-run")], "--timer-property=RemainAfterElapse=no")
	assert.Contains(t, h.script, "systemctl stop kipper-firewalld-rollback-")
}
