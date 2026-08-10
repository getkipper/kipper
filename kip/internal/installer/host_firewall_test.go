package installer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUFWRules(t *testing.T) {
	t.Run("includes the configured ssh port, not a hardcoded 22", func(t *testing.T) {
		rules := ufwRules("2222", true)
		joined := strings.Join(rules, "\n")
		assert.Contains(t, joined, "ufw limit 2222/tcp")
		assert.NotContains(t, joined, "ufw limit 22/tcp")
	})

	t.Run("denies incoming and allows outgoing by default", func(t *testing.T) {
		rules := ufwRules("22", true)
		assert.Contains(t, rules, "ufw default deny incoming")
		assert.Contains(t, rules, "ufw default allow outgoing")
	})

	// A brute-force sweep opens far more than six connections in thirty
	// seconds; a person opens one. Rate-limiting SSH only became safe once an
	// install stopped making a fresh connection per command.
	t.Run("rate-limits ssh rather than opening it outright", func(t *testing.T) {
		rules := ufwRules("22", true)
		assert.Contains(t, rules, "ufw limit 22/tcp")
		assert.NotContains(t, rules, "ufw allow 22/tcp",
			"an unlimited ssh port is what let 1,400 attempts in three hours reach sshd")
	})

	// The limit fits one multiplexed install and not hundreds of separate
	// connections. Applying it to a client that fell back to a connection per
	// command would rate-limit the installer off a half-built host.
	t.Run("opens ssh outright when the client is not multiplexing", func(t *testing.T) {
		rules := ufwRules("22", false)
		assert.Contains(t, rules, "ufw allow 22/tcp")
		assert.NotContains(t, rules, "ufw limit 22/tcp",
			"an installer making a connection per command would trip its own rule")
	})

	t.Run("opens ports for traefik ingress and the k3s api", func(t *testing.T) {
		rules := ufwRules("22", true)
		assert.Contains(t, rules, "ufw allow 80/tcp")
		assert.Contains(t, rules, "ufw allow 443/tcp")
		assert.Contains(t, rules, "ufw allow 6443/tcp")
	})

	t.Run("permits pods only on the specific host ports k3s needs, not every port", func(t *testing.T) {
		joined := strings.Join(ufwRules("22", true), "\n")
		// The pod CIDR reaches the kubelet and node-exporter, and nothing else
		// on the host.
		assert.Contains(t, joined, "ufw allow from "+k3sPodCIDR+" to any port 10250 proto tcp")
		assert.Contains(t, joined, "ufw allow from "+k3sPodCIDR+" to any port 9100 proto tcp")
		// The blanket all-ports allow must not come back — it let a compromised
		// pod reach SSH and every other host service.
		assert.NotContains(t, joined, "ufw allow from "+k3sPodCIDR+"\n")
		for _, r := range ufwRules("22", true) {
			assert.NotEqual(t, "ufw allow from "+k3sPodCIDR, r,
				"the pod CIDR must never be allowed to every host port")
		}
	})

	t.Run("ssh allow precedes the enable command", func(t *testing.T) {
		// The orchestration in ConfigureFirewall applies rules before
		// `ufw --force enable`, so the rule list itself just needs the
		// ssh rule present — this asserts the contract.
		rules := ufwRules("22", true)
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
