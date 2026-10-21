package installer

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/localrun"
)

// ufwHost models a firewall whose ownership, saved rules and live rules can diverge.
type ufwHost struct {
	saved             map[string]bool
	started           bool
	live              map[string]bool
	ruleLiveFails     map[string]int
	ruleFails         map[string]int
	startFails        int
	startLoadsNothing int
	stopFails         int
	stopLeavesChains  int
	chainQueryFails   int
	listFails         int
	freshFails        bool
	fresh             int
	rolledBack        bool
	rollbackAfter     int
	restoreFails      bool
	local             bool
	rolledBackNow     int
	ran               []string
	unclaimed         bool
	claimWritten      bool
	osRelease         string
	// Empty sshListening defaults to a listening socket on port 22.
	sshListening string
	opsPort      string
	onRun        func(cmd string)
}

func (h *ufwHost) SurvivesSSHRateLimit() bool { return true }

func (h *ufwHost) ObservedAddress(string) (string, error) {
	return "", errors.New("no fresh connection in this test")
}

func (h *ufwHost) ConfirmFreshConnection() error {
	h.ran = append(h.ran, "fresh connection")
	if h.local {
		return localrun.ErrNoConnection
	}
	h.fresh++
	if h.freshFails && h.fresh > 1 {
		return errors.New("ssh: connect to host 192.0.2.10 port 22: Operation timed out")
	}
	return nil
}

func newUFWHost() *ufwHost {
	return &ufwHost{saved: map[string]bool{}, live: map[string]bool{}, ruleLiveFails: map[string]int{}, ruleFails: map[string]int{}}
}

var errUFW = errors.New("exit status 1: ERROR: Could not load logging rules")

func (h *ufwHost) Run(cmd string) (string, error) {
	h.ran = append(h.ran, cmd)
	if h.onRun != nil {
		h.onRun(cmd)
	}
	if strings.Contains(cmd, "rollback.sh; }; cat ") {
		h.rolledBackNow++
		h.rolledBack = true
		if h.restoreFails {
			return "failopen\n", nil
		}
		return "restored\n", nil
	}
	if strings.HasPrefix(cmd, "(exec 9>") {
		if h.rollbackAfter > 0 {
			h.rollbackAfter--
			if h.rollbackAfter == 0 {
				h.rolledBack, h.started, h.live = true, false, map[string]bool{}
			}
		}
		if h.rolledBack {
			// Include the command in the error to catch false refusal matches.
			return ufwRefusedMarker + "\n", errors.New("running " + cmd + ": exit status 1")
		}
		cmd = strings.TrimSuffix(cmd[strings.Index(cmd, ".deadline; ")+len(".deadline; "):], ")")
	}
	cmd = strings.ReplaceAll(cmd, ufwTimeout, "")
	cmd = strings.ReplaceAll(cmd, "iptables -w 5 ", "iptables ")
	switch {
	case strings.Contains(cmd, "systemd-run"):
		return "", nil
	case strings.Contains(cmd, "systemctl stop "+ufwRollbackUnit):
		return "", nil
	case strings.Contains(cmd, "list-units"):
		return "", nil
	case strings.Contains(cmd, "/etc/os-release"):
		if h.osRelease != "" {
			return h.osRelease, nil
		}
		return "ID=debian\nVERSION_ID=13\n", nil
	case strings.Contains(cmd, "DEFAULT_FORWARD_POLICY"):
		h.claimWritten = true
		return "", nil
	case strings.Contains(cmd, "systemctl is-active firewalld"):
		return "inactive\n", nil
	case strings.Contains(cmd, "ufw status"):
		return "active\n", nil
	case strings.Contains(cmd, "test -s "+firewallClaimPath):
		if h.unclaimed && !h.claimWritten {
			return "", nil
		}
		return "claimed\n", nil
	case strings.Contains(cmd, "sshd -T"):
		return "22\n", nil
	case strings.Contains(cmd, "/peers.conf"):
		return h.opsPort, nil
	case strings.Contains(cmd, "ss -Hltn"):
		if h.sshListening != "" {
			return h.sshListening, nil
		}
		return "LISTEN 0      4096         0.0.0.0:22        0.0.0.0:*\nLISTEN 0      4096            [::]:22           [::]:*\n", nil
	case strings.Contains(cmd, "-N ufw-user-input"):
		if h.chainQueryFails > 0 {
			h.chainQueryFails--
			return "unknown: iptables: Resource temporarily unavailable.\n", errors.New("exit status 1")
		}
		if h.started {
			return "present\n", nil
		}
		return "absent\n", nil
	case strings.Contains(cmd, "iptables -S ufw-user-input"):
		if !h.started {
			return "iptables: No chain/target/match by that name.\n", errors.New("exit status 1")
		}
		out := "-N ufw-user-input\n"
		if h.live["ufw allow 22/tcp"] {
			out += "-A ufw-user-input -p tcp -m tcp --dport 22 -j ACCEPT\n"
		}
		for _, rule := range slices.Sorted(maps.Keys(h.live)) {
			if f := strings.Fields(rule); len(f) >= 10 && f[2] == "from" && f[6] == "port" && f[8] == "proto" {
				source := f[3]
				if !strings.Contains(source, "/") {
					source += "/32"
				}
				out += fmt.Sprintf("-A ufw-user-input -s %s -p %s -m %s --dport %s -j ACCEPT\n", source, f[9], f[9], f[7])
			}
		}
		return out, nil
	case strings.Contains(cmd, "ufw --force enable"):
		if h.started {
			// ufw skips a start whose chains exist, loaded or not.
			return "", nil
		}
		h.started = true
		if h.startLoadsNothing > 0 {
			h.startLoadsNothing--
			return "", nil
		}
		if h.startFails > 0 {
			// Model successful recovery after a partial load fails.
			h.startFails--
			h.started, h.live = false, map[string]bool{}
			return "", errUFW
		}
		for r := range h.saved {
			h.live[r] = true
		}
		return "", nil
	case strings.Contains(cmd, "ufw --force disable"):
		if h.stopFails > 0 {
			h.stopFails--
			return "", errUFW
		}
		if h.stopLeavesChains > 0 {
			h.stopLeavesChains--
			return "", nil
		}
		h.started, h.live = false, map[string]bool{}
		return "", nil
	case strings.Contains(cmd, "ufw show added"):
		if h.listFails > 0 {
			h.listFails--
			return "", errUFW
		}
		return strings.Join(slices.Sorted(maps.Keys(h.saved)), "\n") + "\n", nil
	case strings.HasPrefix(cmd, "ufw delete "), strings.HasPrefix(cmd, "ufw route delete "):
		rule := strings.Replace(strings.TrimSuffix(cmd, " >/dev/null"), " delete ", " ", 1)
		delete(h.saved, rule)
		delete(h.live, rule)
		return "", nil
	case strings.HasPrefix(cmd, "ufw "):
		rule := strings.TrimSuffix(cmd, " >/dev/null")
		if h.ruleFails[rule] > 0 {
			h.ruleFails[rule]--
			return "", errUFW
		}
		if h.saved[rule] {
			// ufw skips a rule it has saved without looking at netfilter.
			return "", nil
		}
		h.saved[rule] = true
		if h.ruleLiveFails[rule] > 0 {
			h.ruleLiveFails[rule]--
			return "", errUFW
		}
		if h.started {
			h.live[rule] = true
		}
		return "", nil
	}
	return "", nil
}

func (h *ufwHost) RunStdin(cmd string, _ io.Reader) (string, error) { return h.Run(cmd) }

func noUFWRetryDelay(t *testing.T) {
	t.Helper()
	saved := ufwRetryDelay
	ufwRetryDelay = 0
	t.Cleanup(func() { ufwRetryDelay = saved })
}

// TestConfigureFirewall_LoadsSavedRulesAfterAHalfAppliedRule checks recovery
// when retrying a saved rule is a no-op. A full reload restores SSH in netfilter.
func TestConfigureFirewall_LoadsSavedRulesAfterAHalfAppliedRule(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	h.ruleLiveFails["ufw allow 22/tcp"] = 1

	require.NoError(t, configureFirewall(h, debianHost, false, FirewallConsent{}, FirewallMesh{}))

	assert.True(t, h.live["ufw allow 22/tcp"], "SSH is allowed in the loaded firewall, not only on disk")
}

func TestConfigureFirewall_ChecksTheLoadedRules(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	h.startLoadsNothing = 1

	require.NoError(t, configureFirewall(h, debianHost, false, FirewallConsent{}, FirewallMesh{}))

	assert.True(t, h.live["ufw allow 22/tcp"])
}

func TestConfigureFirewall_RetriesListingTheSavedRules(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	h.listFails = 2

	require.NoError(t, configureFirewall(h, debianHost, false, FirewallConsent{}, FirewallMesh{}))

	listings := 0
	for _, c := range h.ran {
		if strings.Contains(c, "ufw show added") {
			listings++
		}
	}
	assert.Equal(t, 3, listings, "two failed listings and the one that succeeded")
	assert.Equal(t, 0, h.rolledBackNow, "a transient listing failure does not roll the firewall back")
}

func TestConfigureFirewall_RetriesAFailedStart(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	h.startFails = 2

	require.NoError(t, configureFirewall(h, debianHost, false, FirewallConsent{}, FirewallMesh{}))

	assert.True(t, h.live["ufw allow 22/tcp"])
}

func TestConfigureFirewall_LeavesTheHostReachableWhenUFWWillNotLoad(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	h.startFails = 100

	err := configureFirewall(h, debianHost, false, FirewallConsent{}, FirewallMesh{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ufw failed to load its rules")
	assert.Contains(t, err.Error(), "kip restored the previous firewall")
	assert.Equal(t, 1, h.rolledBackNow, "a failed change restores the previous firewall at once")
	assert.False(t, h.started, "ufw is off, so its default deny no longer applies")
}

// TestConfigureFirewall_StartsOnlyAfterAConfirmedStop checks that a confirmed
// stop lets the next start load all saved rules.
func TestConfigureFirewall_StartsOnlyAfterAConfirmedStop(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	for _, r := range ufwRules([]string{"22"}, false, RoleServer) {
		h.saved[r] = true
	}
	h.started = true
	h.live["ufw allow 22/tcp"] = true
	h.stopFails = ufwAttempts

	require.NoError(t, configureFirewall(h, debianHost, false, FirewallConsent{}, FirewallMesh{}))

	assert.True(t, h.live["ufw allow 80/tcp"], "the whole saved ruleset is loaded, not only what survived")
}

func TestConfigureFirewall_ChecksTheStopRemovedTheChains(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	for _, r := range ufwRules([]string{"22"}, false, RoleServer) {
		h.saved[r] = true
	}
	h.started = true
	h.live["ufw allow 22/tcp"] = true
	h.stopLeavesChains = 1

	require.NoError(t, configureFirewall(h, debianHost, false, FirewallConsent{}, FirewallMesh{}))

	assert.True(t, h.live["ufw allow 80/tcp"])
}

func TestConfigureFirewall_SaysWhenItCouldNotSwitchUFWOff(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	h.started = true
	h.startFails = 100
	h.stopFails = 100

	err := configureFirewall(h, debianHost, false, FirewallConsent{}, FirewallMesh{})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "left not active")
	assert.Contains(t, err.Error(), "disabling ufw also failed")
}

func TestConfigureFirewall_ReportsARuleItCouldNotSave(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	h.ruleFails["ufw allow 80/tcp"] = 100

	err := configureFirewall(h, debianHost, false, FirewallConsent{}, FirewallMesh{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ufw allow 80/tcp")
	assert.True(t, h.live["ufw allow 22/tcp"], "the saved rules are loaded, so SSH stays reachable")
}

func TestConfigureFirewall_TreatsAFailedChainQueryAsUnknown(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	for _, r := range ufwRules([]string{"22"}, false, RoleServer) {
		h.saved[r] = true
	}
	h.started = true
	h.live["ufw allow 22/tcp"] = true
	h.stopLeavesChains = 100
	h.chainQueryFails = 100

	err := configureFirewall(h, debianHost, false, FirewallConsent{}, FirewallMesh{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "disabling ufw also failed")
}

func TestConfigureFirewall_RefusesAHostItCannotInstallPackagesOn(t *testing.T) {
	h := newUFWHost()

	err := configureFirewall(h, HostOS{ID: "arch"}, false, FirewallConsent{}, FirewallMesh{})

	assert.ErrorContains(t, err, "arch")
	for _, cmd := range h.ran {
		assert.NotContains(t, cmd, "ufw ", "nothing touches the firewall")
	}
}

func TestConfigureFirewall_RefreshesPackageListsBeforeInstallingUFW(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()

	require.NoError(t, configureFirewall(h, debianHost, false, FirewallConsent{}, FirewallMesh{}))

	var install string
	for _, cmd := range h.ran {
		if strings.Contains(cmd, "apt-get install") {
			install = cmd
			break
		}
	}
	assert.Contains(t, install, "apt-get update")
	assert.Contains(t, install, "apt-get install -y -qq ufw")
}
