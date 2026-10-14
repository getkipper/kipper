package installer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/localrun"
)

func TestPlanFirewallAdoptsOnlyWhenAsked(t *testing.T) {
	adoptable := FirewallAudit{Existing: "firewalld", Adoptable: true}

	assert.False(t, PlanFirewall(adoptable, true, false).Configure, "a running firewalld is never adopted on appearance")
	plan := PlanFirewall(adoptable, true, true)
	assert.True(t, plan.Configure)
	assert.True(t, plan.Adopt)
	assert.Contains(t, plan.Notice, "--adopt-firewall")
	assert.False(t, PlanFirewall(adoptable, false, true).Configure, "--firewall=false wins")
	assert.False(t, PlanFirewall(FirewallAudit{Existing: "ufw"}, true, true).Configure, "only firewalld can be adopted")
}

func TestAuditFirewallOffersToAdoptAnUnclaimedFirewalld(t *testing.T) {
	h := newFirewalldHost(t)
	h.active = true

	audit, err := AuditFirewall(h)

	require.NoError(t, err)
	assert.True(t, audit.Adoptable)
	assert.True(t, audit.BlocksConfiguration())
}

func TestConfigureFirewalldAdoptsWithConsent(t *testing.T) {
	h := newFirewalldHost(t)
	h.active = true
	h.publicXML = `<zone><service name="http"/></zone>`

	require.NoError(t, configureFirewalld(h, rhel10Host, FirewallConsent{Adopt: true}))

	assert.Less(t, h.index(firewalldDumpCommand(true)), h.index("cp -a /etc/firewalld"), "runtime and saved configuration are compared before the take-over")
	assert.Greater(t, h.index("firewall-cmd --reload"), h.index("systemd-run"))
	assert.Contains(t, h.script, "rm -f "+firewalldClaimPath, "a rollback returns the firewall to its owner")
}

func TestConfigureFirewalldRefusesToAdoptAroundOtherSavedRules(t *testing.T) {
	h := newFirewalldHost(t)
	h.active = true
	h.inventory += "f policies/office.xml\n"

	err := configureFirewalld(h, rhel10Host, FirewallConsent{Adopt: true})

	assert.ErrorContains(t, err, "policies/office.xml")
	assert.False(t, h.claimed)
}

func TestConfigureFirewalldRefusesToAdoptLocally(t *testing.T) {
	h := newFirewalldHost(t)
	h.active = true

	err := configureFirewalld(localHost{h}, rhel10Host, FirewallConsent{Adopt: true})

	assert.ErrorContains(t, err, "needs a check from outside the host")
	assert.Equal(t, -1, h.index("cp -a /etc/firewalld"))
}

func TestLocalClientUsesTheDriversConfirmation(t *testing.T) {
	asked := false
	client := localClient(Options{Local: true, ConfirmConnection: func() error { asked = true; return nil }})

	require.NoError(t, client.ConfirmFreshConnection())
	assert.True(t, asked)
	assert.ErrorIs(t, localClient(Options{Local: true}).ConfirmFreshConnection(), localrun.ErrNoConnection)
}

func TestRequireOperatorFirewalldRunsOnlyForALeftAloneFirewalld(t *testing.T) {
	r := &storageHostRunner{replies: map[string]string{"sshd -T": "22\n", "get-zone-of-source": "", "query": ""}}
	left := FirewallAudit{Existing: "firewalld", Adoptable: true}

	require.NoError(t, requireOperatorFirewalld(r, rhel10Host, FirewallAudit{}, FirewallPlan{Configure: true}))
	require.NoError(t, requireOperatorFirewalld(r, rhel10Host, left, FirewallPlan{Configure: true, Adopt: true}))
	assert.Empty(t, r.commands, "nothing is checked when kip configures the firewall itself")

	r.replies = map[string]string{"sshd -T": "22\n", "firewall-cmd": "MISSING:80/tcp\n"}
	err := requireOperatorFirewalld(r, rhel10Host, left, FirewallPlan{})
	assert.ErrorContains(t, err, "80/tcp open in zone public")

	assert.ErrorContains(t, err, "Update the rules manually")
	require.NoError(t, requireOperatorFirewalld(r, rhel10Host, FirewallAudit{Existing: "ufw", KipperManaged: true}, FirewallPlan{}), "a ufw host is not checked here")
}

func TestConfigureFirewalldWillNotAdoptBesideUFW(t *testing.T) {
	h := newFirewalldHost(t)
	h.active, h.ufwActive = true, true

	audit, err := AuditFirewall(h)
	require.NoError(t, err)
	assert.False(t, audit.Adoptable)
	assert.False(t, PlanFirewall(audit, true, true).Configure)

	err = configureFirewalld(h, rhel10Host, FirewallConsent{Adopt: true})
	assert.ErrorContains(t, err, "left it alone")
	assert.Equal(t, -1, h.index("cp -a /etc/firewalld"))
}

func TestRequireOperatorFirewalldHoldsKipsOwnFirewallToItsState(t *testing.T) {
	own := FirewallAudit{Existing: "firewalld", KipperManaged: true}
	h := newFirewalldHost(t)
	h.active, h.claimed = true, true

	require.NoError(t, requireOperatorFirewalld(h, rhel10Host, own, FirewallPlan{}))

	h.loaded = strings.Replace(loadedFirewalld, "ports: 22/tcp 80/tcp", "ports: 22/tcp", 1)
	assert.ErrorContains(t, requireOperatorFirewalld(h, rhel10Host, own, FirewallPlan{}), "has changed since")
}

// Without an ownership claim, even files named by kip belong to the operator.
func TestConfigureFirewalldRefusesToAdoptOverKipNamedFiles(t *testing.T) {
	h := newFirewalldHost(t)
	h.active = true
	h.inventory += "f policies/kipper-pods-out.xml\n"

	err := configureFirewalld(h, rhel10Host, FirewallConsent{Adopt: true})

	assert.ErrorContains(t, err, "policies/kipper-pods-out.xml")
	assert.Equal(t, -1, h.index("cp -a /etc/firewalld"))
}

func TestRequireOperatorFirewalldRefusesTwoManagersOnTheRHELFamily(t *testing.T) {
	r := &storageHostRunner{}
	both := FirewallAudit{Existing: "firewalld and ufw"}

	assert.ErrorContains(t, requireOperatorFirewalld(r, rhel10Host, both, FirewallPlan{}), "both running")
	assert.NoError(t, requireOperatorFirewalld(r, debianHost, both, FirewallPlan{}), "Debian hosts keep their notice")
}

func TestAdoptionConsentAsksWithThePreview(t *testing.T) {
	adoptable := FirewallAudit{Existing: "firewalld", Adoptable: true}
	h := runningStockFirewalld(t)
	asked := ""
	yes := func(q string) bool { asked = q; return true }

	consent, err := AdoptionConsent(h, "203.0.113.10", adoptable, true, false, yes)

	require.NoError(t, err)
	assert.True(t, consent.Adopt)
	assert.Len(t, consent.Fingerprint, 16, "a yes to a question is tied to what was shown")
	assert.Contains(t, asked, "removed: service")
	assert.Contains(t, asked, "203.0.113.10")
}

func TestAdoptionConsentWithoutAQuestion(t *testing.T) {
	adoptable := FirewallAudit{Existing: "firewalld", Adoptable: true}
	h := runningStockFirewalld(t)
	never := func(string) bool { t.Fatal("asked when it should not have"); return false }

	c, err := AdoptionConsent(h, "203.0.113.10", FirewallAudit{}, true, true, never)
	require.NoError(t, err)
	assert.Equal(t, FirewallConsent{Adopt: true}, c, "the flag needs no question and carries no fingerprint")

	c, _ = AdoptionConsent(h, "203.0.113.10", adoptable, true, false, nil)
	assert.False(t, c.Adopt, "no terminal, no flag, no consent")
	c, _ = AdoptionConsent(h, "203.0.113.10", adoptable, false, false, never)
	assert.False(t, c.Adopt, "--firewall=false is not asked")
	c, _ = AdoptionConsent(h, "203.0.113.10", FirewallAudit{Existing: "ufw"}, true, false, never)
	assert.False(t, c.Adopt, "only an adoptable firewalld is asked about")

	h.inventory += "f zones/office.xml\n"
	c, _ = AdoptionConsent(h, "203.0.113.10", adoptable, true, false, never)
	assert.False(t, c.Adopt, "a takeover the backend would refuse is not offered")

	no := func(string) bool { return false }
	c, _ = AdoptionConsent(runningStockFirewalld(t), "203.0.113.10", adoptable, true, false, no)
	assert.False(t, c.Adopt)
}

func TestAdoptionConsentForInstallOptions(t *testing.T) {
	adoptable := FirewallAudit{Existing: "firewalld", Adoptable: true}
	yes := func(string) bool { return true }

	c, err := adoptionConsent(runningStockFirewalld(t), Options{Firewall: true, Local: true, ConfirmAdoption: yes}, adoptable)
	require.NoError(t, err)
	assert.False(t, c.Adopt, "a local install is not asked")

	c, _ = adoptionConsent(runningStockFirewalld(t), Options{Firewall: true, AdoptFirewall: true, AdoptFingerprint: "0123456789abcdef"}, adoptable)
	assert.Equal(t, FirewallConsent{Adopt: true, Fingerprint: "0123456789abcdef"}, c, "a yes given to the driver keeps its fingerprint")
}

func TestConfigureFirewalldRefusesAFirewallThatChangedAfterTheYes(t *testing.T) {
	h := runningStockFirewalld(t)
	preview, err := PreviewFirewalldAdoption(h)
	require.NoError(t, err)

	h.publicInfo = strings.Replace(stockPublicInfo, "  ports:\n", "  ports: 8443/tcp\n", 1)
	err = configureFirewalld(h, rhel10Host, FirewallConsent{Adopt: true, Fingerprint: preview.Fingerprint})

	assert.ErrorContains(t, err, "changed after you agreed")
	assert.Equal(t, -1, h.index("cp -a /etc/firewalld"))
}

func TestConfigureFirewalldTakesOverTheFirewallThatWasShown(t *testing.T) {
	h := runningStockFirewalld(t)
	preview, err := PreviewFirewalldAdoption(h)
	require.NoError(t, err)

	require.NoError(t, configureFirewalld(h, rhel10Host, FirewallConsent{Adopt: true, Fingerprint: preview.Fingerprint}))
	assert.Greater(t, h.index("firewall-cmd --reload"), -1)
}

func TestHardenedStoppedFirewalldSaysAdoptionDoesNotApply(t *testing.T) {
	h := newFirewalldHost(t)
	h.publicXML = strings.Replace(stockPublicZone, `  <service name="cockpit"/>`+"\n", "", 1)

	err := configureFirewalld(h, rhel10Host, FirewallConsent{Adopt: true})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "public zone services")
	assert.Contains(t, err.Error(), "only takes over a running firewalld")
}

func TestTakeoverHint(t *testing.T) {
	assert.Contains(t, takeoverHint(runningStockFirewalld(t)), "run again with --adopt-firewall")

	h := runningStockFirewalld(t)
	h.inventory += "f zones/office.xml\n"
	hint := takeoverHint(h)
	assert.NotContains(t, hint, "--adopt-firewall")
	assert.Contains(t, hint, "zones/office.xml")
}

func TestFirewalldInventoryIsSorted(t *testing.T) {
	assert.Contains(t, firewalldInventoryCommand, "| LC_ALL=C sort", "the fingerprint must not depend on directory order")
}
