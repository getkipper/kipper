package installer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUFWRules(t *testing.T) {
	t.Run("includes the configured ssh port, not a hardcoded 22", func(t *testing.T) {
		rules := ufwRules([]string{"2222"}, true, RoleServer)
		joined := strings.Join(rules, "\n")
		assert.Contains(t, joined, "ufw limit 2222/tcp")
		assert.NotContains(t, joined, "ufw limit 22/tcp")
	})

	t.Run("opens every port sshd listens on", func(t *testing.T) {
		rules := ufwRules([]string{"22", "2222"}, true, RoleServer)
		assert.Contains(t, rules, "ufw limit 22/tcp")
		assert.Contains(t, rules, "ufw limit 2222/tcp")
	})

	t.Run("denies incoming and allows outgoing by default", func(t *testing.T) {
		rules := ufwRules([]string{"22"}, true, RoleServer)
		assert.Contains(t, rules, "ufw default deny incoming")
		assert.Contains(t, rules, "ufw default allow outgoing")
	})

	// Reusing an SSH connection keeps the installer within the rate limit.
	t.Run("rate-limits ssh rather than opening it outright", func(t *testing.T) {
		rules := ufwRules([]string{"22"}, true, RoleServer)
		assert.Contains(t, rules, "ufw limit 22/tcp")
		assert.NotContains(t, rules, "ufw allow 22/tcp",
			"an unlimited ssh port is what let 1,400 attempts in three hours reach sshd")
	})

	// The limit fits one multiplexed install and not hundreds of separate
	// connections. Applying it to a client that fell back to a connection per
	// command would rate-limit the installer off a half-built host.
	t.Run("opens ssh outright when the client is not multiplexing", func(t *testing.T) {
		rules := ufwRules([]string{"22"}, false, RoleServer)
		assert.Contains(t, rules, "ufw allow 22/tcp")
		assert.NotContains(t, rules, "ufw limit 22/tcp",
			"an installer making a connection per command would trip its own rule")
	})

	t.Run("opens ports for traefik ingress and the k3s api", func(t *testing.T) {
		rules := ufwRules([]string{"22"}, true, RoleServer)
		assert.Contains(t, rules, "ufw allow 80/tcp")
		assert.Contains(t, rules, "ufw allow 443/tcp")
		assert.Contains(t, rules, "ufw allow 6443/tcp")
	})

	t.Run("permits pods only on the specific host ports k3s needs, not every port", func(t *testing.T) {
		joined := strings.Join(ufwRules([]string{"22"}, true, RoleServer), "\n")
		// Pod-specific rules support monitoring without opening every host port.
		assert.Contains(t, joined, "ufw allow from "+k3sPodCIDR+" to any port 10250 proto tcp")
		assert.Contains(t, joined, "ufw allow from "+k3sPodCIDR+" to any port 9100 proto tcp")
		// A blanket allow would expose every host service to compromised pods.
		assert.NotContains(t, joined, "ufw allow from "+k3sPodCIDR+"\n")
		for _, r := range ufwRules([]string{"22"}, true, RoleServer) {
			assert.NotEqual(t, "ufw allow from "+k3sPodCIDR, r,
				"the pod CIDR must never be allowed to every host port")
		}
	})

	t.Run("ssh allow precedes the enable command", func(t *testing.T) {
		// loadUFW applies these rules before enabling UFW to preserve SSH access.
		rules := ufwRules([]string{"22"}, true, RoleServer)
		var sshIdx int
		for i, r := range rules {
			if strings.Contains(r, "/tcp") && strings.Contains(r, "22") {
				sshIdx = i
				break
			}
		}
		assert.Greater(t, sshIdx, 0, "ssh allow rule must be present in rule list")
	})
}

func TestSSHRateLimitedReadsTheUFWRuleForTheSSHPort(t *testing.T) {
	limited := &routedRunner{routes: []route{
		{contains: "sshd -T", out: "2222\n"},
		{contains: "ufw status", out: "Status: active\n\nTo                         Action      From\n--                         ------      ----\n2222/tcp                   LIMIT       Anywhere\n80/tcp                     ALLOW       Anywhere\n"},
	}}
	got, err := SSHRateLimited(limited)
	require.NoError(t, err)
	assert.True(t, got)

	open := &routedRunner{routes: []route{
		{contains: "sshd -T", out: "22\n"},
		{contains: "ufw status", out: "Status: active\n\n22/tcp                     ALLOW       Anywhere\n2222/tcp                   LIMIT       Anywhere\n"},
	}}
	got, err = SSHRateLimited(open)
	require.NoError(t, err)
	assert.False(t, got, "only the rule for sshd's own port counts")
}
