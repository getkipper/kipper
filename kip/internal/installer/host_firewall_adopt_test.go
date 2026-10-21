package installer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyUFWRules is the ruleset kip saved before it recorded ownership: plain
// SSH, the public server ports and blanket access from the pod and Service
// networks to every host port.
var legacyUFWRules = []string{
	"ufw allow 22/tcp",
	"ufw allow 80/tcp",
	"ufw allow 443/tcp",
	"ufw allow 6443/tcp",
	"ufw allow from 10.42.0.0/16",
	"ufw allow from 10.43.0.0/16",
}

func unrecordedUFWHost(rules ...string) *ufwHost {
	h := newUFWHost()
	h.unclaimed = true
	h.started = true
	for _, r := range rules {
		h.saved[r], h.live[r] = true, true
	}
	return h
}

func TestAuditFirewallOffersToAdoptAnUnrecordedUFW(t *testing.T) {
	audit, err := AuditFirewall(unrecordedUFWHost())
	require.NoError(t, err)
	assert.True(t, audit.Adoptable, "kip may offer to take over a UFW it did not record")
	assert.True(t, audit.BlocksConfiguration(), "without consent the firewall stays as it is")

	claimed := newUFWHost()
	audit, err = AuditFirewall(claimed)
	require.NoError(t, err)
	assert.False(t, audit.Adoptable, "kip's own firewall needs no takeover")

	rhel := unrecordedUFWHost()
	rhel.osRelease = "ID=rocky\nVERSION_ID=10.0\n"
	audit, err = AuditFirewall(rhel)
	require.NoError(t, err)
	assert.False(t, audit.Adoptable, "kip uses firewalld on RHEL-family hosts, so it never takes over UFW there")

	unknown := unrecordedUFWHost()
	unknown.osRelease = "ID=arch\n"
	audit, err = AuditFirewall(unknown)
	require.NoError(t, err)
	assert.False(t, audit.Adoptable, "kip takes over UFW only on Ubuntu and Debian")
}

func TestPlanFirewallNamesTheTakeoverFlag(t *testing.T) {
	audit, err := AuditFirewall(unrecordedUFWHost())
	require.NoError(t, err)

	assert.Contains(t, PlanFirewall(audit, true, false).Notice, "--adopt-firewall")
	assert.NotContains(t, PlanFirewall(FirewallAudit{Existing: "ufw"}, true, false).Notice, "--adopt-firewall",
		"a firewall kip cannot take over gets no hint")
}

func TestForeignUFWRules(t *testing.T) {
	want := append(append([]string{}, ufwRules([]string{"22"}, true, RoleServer)...), "ufw allow 443/udp comment kipper-ops")
	added := strings.Join(append(append([]string{"Added user rules (see 'ufw status' for running firewall):"}, legacyUFWRules...),
		"ufw limit 2222/tcp",
		"ufw allow 443/udp comment 'kipper-ops'",
		"ufw allow from 10.42.0.0/16 to any port 10250 proto tcp",
		"ufw allow 8080/tcp",
		"ufw route allow in on eth1 out on eth0",
		"ufw allow 2222/tcp comment 'ssh'",
	), "\n")

	foreign := foreignUFWRules(added, want, []string{"22", "2222"})

	assert.Equal(t, []string{
		"ufw allow from 10.42.0.0/16",
		"ufw allow from 10.43.0.0/16",
		"ufw allow 8080/tcp",
		"ufw route allow in on eth1 out on eth0",
	}, foreign, "SSH rules for configured ports, kip's rules and its quoted tunnel rule stay")

	assert.Equal(t, "ufw delete allow from 10.42.0.0/16", ufwDeleteCommand("ufw allow from 10.42.0.0/16"))
	assert.Equal(t, "ufw route delete allow in on eth1 out on eth0", ufwDeleteCommand("ufw route allow in on eth1 out on eth0"))

	mesh := FirewallMesh{Role: RoleServer, Peers: []FirewallPeer{{Address: "10.0.0.2"}}}
	peers := ufwPeerRules(mesh)
	require.NotEmpty(t, peers)
	assert.Empty(t, foreignUFWRules(peers[0]+"\nufw allow 22/tcp\n", ufwWantedRules([]string{"22"}, mesh, nil), []string{"22"}),
		"kip's quoted peer rules stay")
	assert.Empty(t, foreignUFWRules("ufw allow 443/udp comment 'kipper-ops'\n", ufwWantedRules([]string{"22"}, FirewallMesh{}, []string{"ufw allow 443/udp comment kipper-ops"}), []string{"22"}),
		"the tunnel rule kip adds unquoted stays when ufw prints it quoted")

	assert.Equal(t, []string{"ufw allow 2222/tcp comment 'web'"}, foreignUFWRules("ufw allow 2222/tcp comment 'web'\n", want, []string{"22"}),
		"a commented rule for a port sshd does not use goes")

	worker := foreignUFWRules("ufw allow 22/tcp\nufw allow 80/tcp\n", ufwRules([]string{"22"}, true, RoleWorker), []string{"22"})
	assert.Equal(t, []string{"ufw allow 80/tcp"}, worker, "a worker serves no public ingress")
}

func TestUFWRuleDeletable(t *testing.T) {
	for _, rule := range []string{
		"ufw allow 8080/tcp",
		"ufw allow from 10.42.0.0/16",
		"ufw allow from 2001:db8::/32 to any port 22 proto tcp",
		"ufw allow 6000:6007/tcp",
		"ufw allow in on eth0 to any port 80,443 proto tcp",
		"ufw allow 'Nginx Full'",
		"ufw allow from 192.0.2.0/24 to any app 'OpenSSH'",
		"ufw allow 8080/tcp comment 'web server'",
		"ufw deny log-all from 198.51.100.7",
		"ufw route allow in on eth1 out on eth0",
	} {
		assert.True(t, ufwRuleDeletable(rule), rule)
	}
	for _, rule := range []string{
		"ufw allow 8080/tcp comment 'Bob's laptop'",
		"ufw allow 8080/tcp comment 'it's Bob's'",
		"ufw allow 8080/tcp comment 'a;b'",
		"ufw allow 8080/tcp comment '$(id)'",
		"ufw allow 8080/tcp comment \"web\"",
		"ufw allow 8080/tcp; id",
	} {
		assert.False(t, ufwRuleDeletable(rule), rule)
	}
}

func TestPreviewUFWAdoption(t *testing.T) {
	h := unrecordedUFWHost(legacyUFWRules...)

	preview, err := PreviewUFWAdoption(h, FirewallMesh{})

	require.NoError(t, err)
	require.True(t, preview.Possible, preview.Reason)
	assert.Equal(t, []string{"allow from 10.42.0.0/16", "allow from 10.43.0.0/16"}, preview.Removed)
	assert.Equal(t, []string{"22/tcp", "80/tcp", "443/tcp", "6443/tcp"}, preview.Opened)
	assert.NotEmpty(t, preview.Fingerprint)
	question := preview.Question("203.0.113.10")
	assert.Contains(t, question, "UFW")
	assert.Contains(t, question, "allow from 10.42.0.0/16")
	assert.Contains(t, question, "10250/tcp")

	h.saved["ufw allow 8080/tcp"] = true
	changed, err := PreviewUFWAdoption(h, FirewallMesh{})
	require.NoError(t, err)
	assert.NotEqual(t, preview.Fingerprint, changed.Fingerprint, "consent covers the rules that were shown")

	ours, err := PreviewUFWAdoption(newUFWHost(), FirewallMesh{})
	require.NoError(t, err)
	assert.False(t, ours.Possible)

	quoted := unrecordedUFWHost("ufw allow 22/tcp", "ufw allow 8080/tcp comment 'Bob's laptop'")
	refused, err := PreviewUFWAdoption(quoted, FirewallMesh{})
	require.NoError(t, err)
	assert.False(t, refused.Possible)
	assert.Contains(t, refused.Reason, "Bob's laptop")

	moved := unrecordedUFWHost(legacyUFWRules...)
	moved.sshListening = "LISTEN 0 4096 0.0.0.0:2222 0.0.0.0:*\n"
	refused, err = PreviewUFWAdoption(moved, FirewallMesh{})
	require.NoError(t, err)
	assert.False(t, refused.Possible)
	assert.Contains(t, refused.Reason, "nothing listens")
}

func TestConfigureUFWAdoptsAnUnrecordedFirewall(t *testing.T) {
	noUFWRetryDelay(t)

	t.Run("replaces the stale rules with kip's", func(t *testing.T) {
		h := unrecordedUFWHost(append(legacyUFWRules, "ufw allow 8080/tcp")...)

		require.NoError(t, configureFirewall(h, debianHost, false, FirewallConsent{Adopt: true}, FirewallMesh{}))

		assert.True(t, h.claimWritten, "the firewall is kip's from now on")
		assert.False(t, h.saved["ufw allow from 10.42.0.0/16"], "pods no longer reach every host port")
		assert.False(t, h.saved["ufw allow from 10.43.0.0/16"])
		assert.False(t, h.saved["ufw allow 8080/tcp"], "the preview names every rule that goes")
		assert.True(t, h.saved["ufw allow 22/tcp"], "SSH stays open throughout")
		assert.True(t, h.saved["ufw allow from 10.42.0.0/16 to any port 10250 proto tcp"])
		assert.True(t, h.live["ufw allow from 10.42.0.0/16 to any port 9100 proto tcp"])
	})

	t.Run("adds kip's SSH rule before it deletes anything", func(t *testing.T) {
		h := unrecordedUFWHost("ufw allow OpenSSH", "ufw allow from 10.42.0.0/16")

		require.NoError(t, configureFirewall(h, debianHost, false, FirewallConsent{Adopt: true}, FirewallMesh{}))

		added, deleted := -1, -1
		for i, cmd := range h.ran {
			if added < 0 && strings.Contains(cmd, "ufw allow 22/tcp") {
				added = i
			}
			if deleted < 0 && strings.Contains(cmd, "ufw delete ") {
				deleted = i
			}
		}
		require.GreaterOrEqual(t, deleted, 0)
		assert.Less(t, added, deleted)
		assert.False(t, h.saved["ufw allow OpenSSH"])
	})

	t.Run("a worker keeps its peers and drops the server's public ports", func(t *testing.T) {
		h := unrecordedUFWHost(legacyUFWRules...)

		require.NoError(t, configureFirewall(h, debianHost, false, FirewallConsent{Adopt: true}, workerMesh))

		for _, rule := range ufwPeerRules(workerMesh) {
			assert.True(t, h.saved[rule], rule)
		}
		for _, rule := range []string{"ufw allow 80/tcp", "ufw allow 443/tcp", "ufw allow 6443/tcp", "ufw allow from 10.42.0.0/16"} {
			assert.False(t, h.saved[rule], rule)
		}
		assert.True(t, h.saved["ufw allow 22/tcp"])
	})

	t.Run("deletes route rules with ufw route delete", func(t *testing.T) {
		h := unrecordedUFWHost("ufw allow 22/tcp", "ufw route allow in on eth1 out on eth0")

		require.NoError(t, configureFirewall(h, debianHost, false, FirewallConsent{Adopt: true}, FirewallMesh{}))

		assert.False(t, h.saved["ufw route allow in on eth1 out on eth0"])
	})

	t.Run("stops when a rule appears after the check", func(t *testing.T) {
		h := unrecordedUFWHost(legacyUFWRules...)
		h.onRun = func(cmd string) {
			if strings.Contains(cmd, "DEFAULT_FORWARD_POLICY") {
				h.saved["ufw allow 8443/tcp"] = true
			}
		}

		err := configureFirewall(h, debianHost, false, FirewallConsent{Adopt: true}, FirewallMesh{})

		assert.ErrorContains(t, err, "8443/tcp")
		assert.True(t, h.saved["ufw allow 8443/tcp"], "only rules that were checked are deleted")
		assert.True(t, h.rolledBack, "the previous firewall comes back")
	})

	t.Run("stops when the tunnel's port changes after the check", func(t *testing.T) {
		h := unrecordedUFWHost(legacyUFWRules...)
		h.onRun = func(cmd string) {
			if strings.Contains(cmd, "DEFAULT_FORWARD_POLICY") {
				h.opsPort = "51821\n"
			}
		}

		err := configureFirewall(h, debianHost, false, FirewallConsent{Adopt: true}, FirewallMesh{})

		assert.ErrorContains(t, err, "tunnel")
		assert.False(t, h.saved["ufw allow 51821/udp comment kipper-ops"], "a port the check did not see is never opened")
		assert.True(t, h.rolledBack)
	})

	t.Run("refuses while sshd listens elsewhere than configured", func(t *testing.T) {
		h := unrecordedUFWHost(append(legacyUFWRules, "ufw allow 2222/tcp")...)
		h.sshListening = "LISTEN 0 4096 0.0.0.0:2222 0.0.0.0:*\n"

		err := configureFirewall(h, debianHost, false, FirewallConsent{Adopt: true}, FirewallMesh{})

		assert.ErrorContains(t, err, "nothing listens")
		assert.False(t, h.claimWritten)
		assert.True(t, h.saved["ufw allow 2222/tcp"])
	})

	t.Run("refuses a rule it cannot delete safely", func(t *testing.T) {
		h := unrecordedUFWHost("ufw allow 22/tcp", "ufw allow 8080/tcp comment 'Bob's laptop'")

		err := configureFirewall(h, debianHost, false, FirewallConsent{Adopt: true}, FirewallMesh{})

		assert.ErrorContains(t, err, "Bob's laptop")
		assert.False(t, h.claimWritten)
	})

	t.Run("leaves it alone without consent", func(t *testing.T) {
		h := unrecordedUFWHost(legacyUFWRules...)

		err := configureFirewall(h, debianHost, false, FirewallConsent{}, FirewallMesh{})

		assert.ErrorContains(t, err, "left it alone")
		assert.False(t, h.claimWritten)
		assert.True(t, h.saved["ufw allow from 10.42.0.0/16"])
	})

	t.Run("refuses rules that changed after the preview", func(t *testing.T) {
		h := unrecordedUFWHost(legacyUFWRules...)
		preview, err := PreviewUFWAdoption(h, FirewallMesh{})
		require.NoError(t, err)
		h.saved["ufw allow 8080/tcp"] = true

		err = configureFirewall(h, debianHost, false, FirewallConsent{Adopt: true, Fingerprint: preview.Fingerprint}, FirewallMesh{})

		assert.ErrorContains(t, err, "changed after you agreed")
		assert.False(t, h.claimWritten)
		assert.True(t, h.saved["ufw allow 8080/tcp"])
	})

	t.Run("needs a check from outside the host", func(t *testing.T) {
		h := unrecordedUFWHost(legacyUFWRules...)
		h.local = true

		err := configureFirewall(h, debianHost, false, FirewallConsent{Adopt: true}, FirewallMesh{})

		assert.ErrorContains(t, err, "outside the host")
		assert.False(t, h.claimWritten)
	})

	t.Run("a firewall kip owns keeps the operator's extra rules", func(t *testing.T) {
		h := newUFWHost()
		h.saved["ufw allow 8080/tcp"] = true

		require.NoError(t, configureFirewall(h, debianHost, false, FirewallConsent{}, FirewallMesh{}))

		assert.True(t, h.saved["ufw allow 8080/tcp"], "only a takeover replaces the ruleset")
	})
}

func TestAdoptionConsentPreviewsUFW(t *testing.T) {
	h := unrecordedUFWHost(legacyUFWRules...)
	audit, err := AuditFirewall(h)
	require.NoError(t, err)
	var asked string

	consent, err := AdoptionConsent(h, "203.0.113.10", audit, true, false, func(q string) bool { asked = q; return true }, FirewallMesh{})

	require.NoError(t, err)
	assert.True(t, consent.Adopt)
	assert.NotEmpty(t, consent.Fingerprint)
	assert.Contains(t, asked, "allow from 10.43.0.0/16")
	assert.Contains(t, PlanFirewall(audit, true, true).Notice, "UFW")
}

func TestAuditFirewallKeepsAUFWWhoseHostItCannotIdentify(t *testing.T) {
	h := unrecordedUFWHost()
	h.osRelease = "\n"

	audit, err := AuditFirewall(h)

	require.NoError(t, err, "eligibility for a takeover never fails the audit")
	assert.Equal(t, "ufw", audit.Existing)
	assert.False(t, audit.Adoptable)
}
