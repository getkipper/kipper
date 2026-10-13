package installer

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ufwHost models a Kipper-managed firewall whose saved and live rules can diverge.
type ufwHost struct {
	saved   map[string]bool
	started bool
	live    map[string]bool
	// ruleLiveFails makes a rule command save its rule and then fail before
	// loading it, as a reload does during k3s netfilter churn.
	ruleLiveFails map[string]int
	// ruleFails makes a rule command fail before it saves anything.
	ruleFails map[string]int
	// startFails makes a start create its chains and fail before loading.
	startFails int
	// startLoadsNothing makes a start report success and load nothing.
	startLoadsNothing int
	// stopFails makes a stop fail and leave the chains in place.
	stopFails int
	// stopLeavesChains makes a stop report success and leave the chains.
	stopLeavesChains int
	// chainQueryFails makes asking whether the chains exist fail outright.
	chainQueryFails int
	ran             []string
}

func newUFWHost() *ufwHost {
	return &ufwHost{saved: map[string]bool{}, live: map[string]bool{}, ruleLiveFails: map[string]int{}, ruleFails: map[string]int{}}
}

var errUFW = errors.New("exit status 1: ERROR: Could not load logging rules")

func (h *ufwHost) Run(cmd string) (string, error) {
	h.ran = append(h.ran, cmd)
	switch {
	case strings.Contains(cmd, "/etc/os-release"):
		return "ID=debian\nVERSION_ID=13\n", nil
	case strings.Contains(cmd, "systemctl is-active firewalld"):
		return "inactive\n", nil
	case strings.Contains(cmd, "ufw status"):
		return "active\n", nil
	case strings.Contains(cmd, "test -s "+firewallClaimPath):
		return "claimed\n", nil
	case strings.Contains(cmd, "sshd -T"):
		return "22\n", nil
	case strings.Contains(cmd, "No chain/target/match"):
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
		return out, nil
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
			h.startFails--
			return "", errUFW
		}
		for r := range h.saved {
			h.live[r] = true
		}
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

// A rule that was saved but never loaded makes its retry a no-op; only a full
// reload from the saved rules gets SSH back into netfilter.
func TestConfigureFirewall_LoadsSavedRulesAfterAHalfAppliedRule(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	h.ruleLiveFails["ufw allow 22/tcp"] = 1

	require.NoError(t, configureFirewall(h, debianHost, false))

	assert.True(t, h.live["ufw allow 22/tcp"], "SSH is allowed in the loaded firewall, not only on disk")
}

func TestConfigureFirewall_ChecksTheLoadedRules(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	h.startLoadsNothing = 1

	require.NoError(t, configureFirewall(h, debianHost, false))

	assert.True(t, h.live["ufw allow 22/tcp"])
}

func TestConfigureFirewall_RetriesAFailedStart(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	h.startFails = 2

	require.NoError(t, configureFirewall(h, debianHost, false))

	assert.True(t, h.live["ufw allow 22/tcp"])
}

func TestConfigureFirewall_LeavesTheHostReachableWhenUFWWillNotLoad(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	h.startFails = 100

	err := configureFirewall(h, debianHost, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not active")
	assert.False(t, h.started, "ufw is off, so its default deny no longer applies")
}

// A confirmed stop ensures the next start loads all saved rules.
func TestConfigureFirewall_StartsOnlyAfterAConfirmedStop(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	for _, r := range ufwRules("22", false) {
		h.saved[r] = true
	}
	h.started = true
	h.live["ufw allow 22/tcp"] = true
	h.stopFails = ufwAttempts

	require.NoError(t, configureFirewall(h, debianHost, false))

	assert.True(t, h.live["ufw allow 80/tcp"], "the whole saved ruleset is loaded, not only what survived")
}

func TestConfigureFirewall_ChecksTheStopRemovedTheChains(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	for _, r := range ufwRules("22", false) {
		h.saved[r] = true
	}
	h.started = true
	h.live["ufw allow 22/tcp"] = true
	h.stopLeavesChains = 1

	require.NoError(t, configureFirewall(h, debianHost, false))

	assert.True(t, h.live["ufw allow 80/tcp"])
}

func TestConfigureFirewall_SaysWhenItCouldNotSwitchUFWOff(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	h.started = true
	h.startFails = 100
	h.stopFails = 100

	err := configureFirewall(h, debianHost, false)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "left not active")
	assert.Contains(t, err.Error(), "may refuse SSH")
}

func TestConfigureFirewall_ReportsARuleItCouldNotSave(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	h.ruleFails["ufw allow 80/tcp"] = 100

	err := configureFirewall(h, debianHost, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ufw allow 80/tcp")
	assert.True(t, h.live["ufw allow 22/tcp"], "the saved rules are loaded, so SSH stays reachable")
}

func TestConfigureFirewall_TreatsAFailedChainQueryAsUnknown(t *testing.T) {
	noUFWRetryDelay(t)
	h := newUFWHost()
	for _, r := range ufwRules("22", false) {
		h.saved[r] = true
	}
	h.started = true
	h.live["ufw allow 22/tcp"] = true
	h.stopLeavesChains = 100
	h.chainQueryFails = 100

	err := configureFirewall(h, debianHost, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "may refuse SSH")
}

func TestConfigureFirewall_RefusesAHostItCannotInstallPackagesOn(t *testing.T) {
	h := newUFWHost()

	err := configureFirewall(h, HostOS{ID: "arch"}, false)

	assert.ErrorContains(t, err, "arch")
	for _, cmd := range h.ran {
		assert.NotContains(t, cmd, "ufw ", "nothing touches the firewall")
	}
}
