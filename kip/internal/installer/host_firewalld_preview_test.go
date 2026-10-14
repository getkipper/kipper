package installer

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stockPublicInfo is a running vendor public zone as firewall-cmd prints it.
const stockPublicInfo = `public (default, active)
  target: default
  icmp-block-inversion: no
  interfaces: eth0
  sources:
  services: cockpit dhcpv6-client ssh
  ports:
  protocols:
  forward: yes
  masquerade: no
  forward-ports:
  source-ports:
  icmp-blocks:
  rich rules:
`

func runningStockFirewalld(t *testing.T) *firewalldHost {
	h := newFirewalldHost(t)
	h.active = true
	h.publicInfo = stockPublicInfo
	return h
}

func TestPreviewFirewalldAdoptionOfAStockFirewall(t *testing.T) {
	h := runningStockFirewalld(t)

	a, err := PreviewFirewalldAdoption(h)

	require.NoError(t, err)
	assert.True(t, a.Possible, a.Reason)
	assert.ElementsMatch(t, []string{"service cockpit", "service ssh"}, a.Removed)
	assert.Equal(t, []string{"22/tcp", "80/tcp", "443/tcp", "6443/tcp"}, a.Opened)
	assert.Len(t, a.Fingerprint, 16)
	assert.Equal(t, -1, h.index("cp -a /etc/firewalld"), "a preview changes nothing")
	assert.Equal(t, -1, h.index("firewalld-run"), "a preview reserves nothing")
}

// A vendor zone that lives only in /usr/lib leaves no file in /etc; the
// running zone is what gets replaced, so that is what the preview reads.
func TestPreviewFirewalldAdoptionReadsTheRunningZone(t *testing.T) {
	h := runningStockFirewalld(t)
	h.inventory = "f firewalld.conf\n"
	h.publicInfo = strings.Replace(stockPublicInfo, "  ports:\n", "  ports: 8080/tcp\n", 1)
	h.publicInfo = strings.Replace(h.publicInfo, "  rich rules:\n", "  rich rules:\n\trule family=\"ipv4\" source address=\"198.51.100.0/24\" accept\n", 1)

	a, err := PreviewFirewalldAdoption(h)

	require.NoError(t, err)
	assert.Contains(t, a.Removed, "port 8080/tcp")
	assert.Contains(t, strings.Join(a.Removed, "\n"), `rich rule rule family="ipv4" source address="198.51.100.0/24" accept`)
}

func TestPreviewFirewalldAdoptionSaysWhyNot(t *testing.T) {
	t.Run("nothing to take over", func(t *testing.T) {
		h := newFirewalldHost(t)
		a, err := PreviewFirewalldAdoption(h)
		require.NoError(t, err)
		assert.False(t, a.Possible)
	})
	t.Run("other saved configuration", func(t *testing.T) {
		h := runningStockFirewalld(t)
		h.inventory += "f zones/office.xml\n"
		a, err := PreviewFirewalldAdoption(h)
		require.NoError(t, err)
		assert.False(t, a.Possible)
		assert.Contains(t, a.Reason, "zones/office.xml")
	})
	t.Run("runtime rules the saved configuration lacks", func(t *testing.T) {
		h := runningStockFirewalld(t)
		h.runningDump, h.savedDump = "== zone public\n  ports: 9000/tcp\n", "== zone public\n  ports:\n"
		a, err := PreviewFirewalldAdoption(h)
		require.NoError(t, err)
		assert.False(t, a.Possible)
		assert.Contains(t, a.Reason, "differs from its saved one (zone public)")
	})
}

func TestFirewalldFingerprintFollowsTheFirewall(t *testing.T) {
	h := runningStockFirewalld(t)
	first, err := PreviewFirewalldAdoption(h)
	require.NoError(t, err)
	again, err := PreviewFirewalldAdoption(h)
	require.NoError(t, err)
	assert.Equal(t, first.Fingerprint, again.Fingerprint)

	h.publicInfo = strings.Replace(stockPublicInfo, "  ports:\n", "  ports: 8443/tcp\n", 1)
	changed, err := PreviewFirewalldAdoption(h)
	require.NoError(t, err)
	assert.NotEqual(t, first.Fingerprint, changed.Fingerprint)
}

func TestFirewalldAdoptionQuestionSaysWhatChanges(t *testing.T) {
	q := FirewalldAdoption{Possible: true, Removed: []string{"service cockpit", "service ssh"}, Opened: []string{"22/tcp", "80/tcp", "443/tcp", "6443/tcp"}}.Question("203.0.113.10")

	assert.Contains(t, q, "firewalld is running on 203.0.113.10")
	assert.Contains(t, q, "removed: service cockpit, service ssh")
	assert.Contains(t, q, "opened to everyone: 22/tcp, 80/tcp, 443/tcp, 6443/tcp")
	assert.Contains(t, q, "three minutes")
	assert.NotContains(t, q, "stock", "takeover is also offered for a customised public zone")
	assert.NotContains(t, q, "operator-managed")
	for _, line := range strings.Split(q, "\n") {
		assert.LessOrEqual(t, len(line), 100, "the question must fit a terminal: %q", line)
	}
}

func TestPreviewFirewalldAdoptionReportsAFailedRead(t *testing.T) {
	h := runningStockFirewalld(t)
	h.fail = map[string]error{firewalldDumpCommand(true): errors.New("connection reset")}

	_, err := PreviewFirewalldAdoption(h)

	assert.ErrorContains(t, err, "reading the saved firewalld configuration")
}

func TestPreviewNamesEveryRestrictionTheTakeoverRemoves(t *testing.T) {
	h := runningStockFirewalld(t)
	info := strings.Replace(stockPublicInfo, "  icmp-blocks:\n", "  icmp-blocks: echo-request\n", 1)
	info = strings.Replace(info, "  icmp-block-inversion: no\n", "  icmp-block-inversion: yes\n", 1)
	info = strings.Replace(info, "  sources:\n", "  sources: 198.51.100.0/24\n", 1)
	info = strings.Replace(info, "  forward: yes\n", "  forward: no\n  ingress-priority: -10\n", 1)
	h.publicInfo = info

	a, err := PreviewFirewalldAdoption(h)

	require.NoError(t, err)
	all := strings.Join(a.Removed, "\n")
	for _, want := range []string{"ICMP block echo-request", "ICMP block inversion", "source 198.51.100.0/24", "forwarding off", "ingress-priority -10"} {
		assert.Contains(t, all, want)
	}
}

func TestPreviewNamesServicesTheTakeoverAdds(t *testing.T) {
	h := runningStockFirewalld(t)
	h.publicInfo = strings.Replace(stockPublicInfo, "  services: cockpit dhcpv6-client ssh\n", "  services: ssh\n", 1)

	a, err := PreviewFirewalldAdoption(h)

	require.NoError(t, err)
	assert.Contains(t, a.Opened, "service dhcpv6-client")

	stock, err := PreviewFirewalldAdoption(runningStockFirewalld(t))
	require.NoError(t, err)
	assert.NotContains(t, stock.Opened, "service dhcpv6-client", "a service already there is not news")
}
