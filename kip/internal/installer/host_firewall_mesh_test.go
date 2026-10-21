package installer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var workerMesh = FirewallMesh{Role: RoleWorker, Peers: []FirewallPeer{
	{Address: "192.0.2.10", Server: true},
	{Address: "192.0.2.12"},
}}

var serverMesh = FirewallMesh{Role: RoleServer, Peers: []FirewallPeer{{Address: "192.0.2.11"}, {Address: "192.0.2.12"}}}

func TestFirewallMeshForSkipsItselfAndUnaddressedNodes(t *testing.T) {
	records := []NodeRecord{
		{Name: "server", MachineID: serverMachine, Role: NodeRoleServer, PeerAddress: "192.0.2.10"},
		{Name: "worker-1", MachineID: workerMachine, Role: NodeRoleWorker, PeerAddress: "192.0.2.12"},
		{Name: "worker-2", MachineID: "00000000000000000000000000000002", Role: NodeRoleWorker},
	}
	assert.Equal(t, FirewallMesh{Role: RoleWorker, Peers: []FirewallPeer{{Address: "192.0.2.10", Server: true}}}, FirewallMeshFor(records, workerMachine),
		"a worker gets the server, not itself and no unaddressed node")
	assert.Equal(t, FirewallMesh{Role: RoleServer, Peers: []FirewallPeer{{Address: "192.0.2.12"}}}, FirewallMeshFor(records, serverMachine))
}

func TestFirewallMeshRefusesAnAddressThatIsNotIPv4(t *testing.T) {
	for _, addr := range []string{"192.0.2.1/24", "2001:db8::1", "192.0.2.1; reboot", ""} {
		assert.Error(t, FirewallMesh{Peers: []FirewallPeer{{Address: addr}}}.validate(), addr)
	}
}

func TestUFWRulesOpenOnlySSHToTheWorldOnAWorker(t *testing.T) {
	rules := strings.Join(ufwRules([]string{"22"}, true, RoleWorker), "\n")
	for _, port := range []string{"80/tcp", "443/tcp", "6443/tcp"} {
		assert.NotContains(t, rules, "ufw allow "+port)
	}
	assert.Contains(t, rules, "ufw limit 22/tcp")
	assert.Contains(t, strings.Join(ufwRules([]string{"22"}, true, RoleServer), "\n"), "ufw allow 6443/tcp")
}

func TestUFWPeerRulesOpenNodeExporterToTheServerOnly(t *testing.T) {
	assert.Equal(t, []string{
		"ufw allow from 192.0.2.10 to any port 51820 proto udp comment 'kipper-peer'",
		"ufw allow from 192.0.2.10 to any port 10250 proto tcp comment 'kipper-peer'",
		"ufw allow from 192.0.2.10 to any port 9100 proto tcp comment 'kipper-peer'",
		"ufw allow from 192.0.2.12 to any port 51820 proto udp comment 'kipper-peer'",
		"ufw allow from 192.0.2.12 to any port 10250 proto tcp comment 'kipper-peer'",
	}, ufwPeerRules(workerMesh))
}

func TestStaleUFWPeerRulesDropsOnlyKipsDepartedRules(t *testing.T) {
	added := `Added user rules (see 'ufw status' for running firewall):
ufw limit 22/tcp
ufw allow 80/tcp
ufw allow from 10.42.0.0/16 to any port 10250 proto tcp
ufw allow from 192.0.2.10 to any port 51820 proto udp comment 'kipper-peer'
ufw allow from 192.0.2.10 to any port 9100 proto tcp comment 'kipper-peer'
ufw allow from 192.0.2.99 to any port 51820 proto udp comment 'kipper-peer'
ufw allow from 192.0.2.98 to any port 9100 proto tcp
`
	assert.Equal(t, []string{
		"ufw delete allow from 192.0.2.99 to any port 51820 proto udp comment 'kipper-peer'",
	}, staleUFWPeerRules(added, FirewallMesh{Role: RoleWorker, Peers: []FirewallPeer{{Address: "192.0.2.10", Server: true}}}))
	assert.Equal(t, []string{
		"ufw delete allow from 192.0.2.10 to any port 51820 proto udp comment 'kipper-peer'",
		"ufw delete allow from 192.0.2.10 to any port 9100 proto tcp comment 'kipper-peer'",
		"ufw delete allow from 192.0.2.99 to any port 51820 proto udp comment 'kipper-peer'",
	}, staleUFWPeerRules(added, FirewallMesh{}), "an empty mesh removes kip's peer rules and nothing else")
}

// TestStaleUFWRoleRulesDropTheServerPortsOnAWorker covers rules retained after
// uninstalling a server and reusing the host as a worker.
func TestStaleUFWRoleRulesDropTheServerPortsOnAWorker(t *testing.T) {
	added := `Added user rules (see 'ufw status' for running firewall):
ufw limit 22/tcp
ufw allow 80/tcp
ufw allow 443/tcp
ufw allow 6443/tcp
ufw allow 8080/tcp
ufw allow from 203.0.113.5 to any port 443 proto tcp
ufw allow from 10.42.0.0/16 to any port 10250 proto tcp
`
	assert.Equal(t, []string{"ufw delete allow 80/tcp", "ufw delete allow 443/tcp", "ufw delete allow 6443/tcp"},
		staleUFWRoleRules(added, workerMesh, []string{"22"}), "Public server port rules are removed. Port 8080 and the source-restricted 443 rule stay.")
	assert.Equal(t, []string{"ufw delete allow 80/tcp", "ufw delete allow 6443/tcp"},
		staleUFWRoleRules(added, workerMesh, []string{"443"}), "sshd listening on a server port keeps that port's rule")
	assert.Equal(t, []string{"ufw delete allow 80/tcp", "ufw delete allow 6443/tcp"},
		staleUFWRoleRules(added, workerMesh, []string{"22", "443"}), "sshd listening on 22 and 443 keeps the 443 rule")
	assert.Empty(t, staleUFWRoleRules(added, serverMesh, []string{"22"}), "a server keeps its ingress and API ports")
	assert.Empty(t, staleUFWRoleRules("Added user rules (see 'ufw status' for running firewall):\nufw limit 22/tcp\n", workerMesh, []string{"22"}),
		"There are no saved server port rules to delete.")
}

// TestConfigureUFWProtectsEverySSHPortOnAWorker covers sshd listening on 22 and
// 443, where the SSH rule must survive removal of saved server port rules.
func TestConfigureUFWProtectsEverySSHPortOnAWorker(t *testing.T) {
	delay := ufwRetryDelay
	ufwRetryDelay = 0
	t.Cleanup(func() { ufwRetryDelay = delay })
	routes := func(chain string) []route {
		return []route{
			{contains: "command -v ufw", out: "present\n"},
			{contains: "sshd -T", out: "22\n443\n"},
			{contains: "ufw show added", out: "ufw allow 22/tcp\nufw allow 80/tcp\nufw allow 443/tcp\nufw allow 6443/tcp\n"},
			{contains: "-N ufw-user-input", out: "absent\n"},
			{contains: "-S ufw-user-input", out: chain},
		}
	}
	ufw := &routedRunner{routes: routes("-A ufw-user-input -p tcp --dport 22 -j ACCEPT\n-A ufw-user-input -p tcp --dport 443 -j ACCEPT\n")}
	require.NoError(t, configureFirewall(ufw, HostOS{ID: "ubuntu", Family: FamilyDebian}, false, FirewallMesh{Role: RoleWorker}))
	assert.True(t, ufw.ran("ufw allow 22/tcp"))
	assert.True(t, ufw.ran("ufw allow 443/tcp"), "the second SSH port is opened like the first")
	assert.True(t, ufw.ran("ufw delete allow 80/tcp"))
	assert.True(t, ufw.ran("ufw delete allow 6443/tcp"))
	assert.False(t, ufw.ran("ufw delete allow 443/tcp"), "a port sshd listens on is never deleted")

	ufw = &routedRunner{routes: routes("-A ufw-user-input -p tcp --dport 22 -j ACCEPT\n")}
	err := configureFirewall(ufw, HostOS{ID: "ubuntu", Family: FamilyDebian}, false, FirewallMesh{Role: RoleWorker})
	require.Error(t, err, "a loaded firewall without the second SSH port must fail the change")
	assert.Contains(t, err.Error(), "SSH port 443")
}

func TestConfigureUFWKeepsTheOperatorTunnelPort(t *testing.T) {
	delay := ufwRetryDelay
	ufwRetryDelay = 0
	t.Cleanup(func() { ufwRetryDelay = delay })
	ufw := &routedRunner{routes: []route{
		{contains: "command -v ufw", out: "present\n"},
		{contains: "sshd -T", out: "22\n"},
		{contains: "/etc/kipper/ops/peers.conf", out: "443\n"},
		{contains: "ufw show added", out: "ufw allow 22/tcp\n"},
		{contains: "-N ufw-user-input", out: "absent\n"},
		{contains: "-S ufw-user-input", out: "-A ufw-user-input -p tcp --dport 22 -j ACCEPT\n"},
	}}
	require.NoError(t, configureFirewall(ufw, HostOS{ID: "ubuntu", Family: FamilyDebian}, false, FirewallMesh{Role: RoleWorker}))
	assert.True(t, ufw.ran("ufw allow 443/udp comment kipper-ops"), "a firewall change keeps the devices' tunnel open")
}

func TestUFWPeerRulesMissingReadsTheLiveChain(t *testing.T) {
	chain := `-N ufw-user-input
-A ufw-user-input -s 192.0.2.10/32 -p udp -m udp --dport 51820 -j ACCEPT
-A ufw-user-input -s 192.0.2.10/32 -p tcp -m tcp --dport 10250 -j ACCEPT
-A ufw-user-input -s 192.0.2.10/32 -p tcp -m tcp --dport 9100 -j ACCEPT
-A ufw-user-input -s 192.0.2.12/32 -p udp -m udp --dport 51820 -j ACCEPT
`
	assert.Equal(t, []string{"10250/tcp from 192.0.2.12"}, ufwPeerRulesMissing(chain, workerMesh))
}

func TestIPTablesAllowsNeedsAnUnshadowedUnrestrictedAccept(t *testing.T) {
	accept := "-A ufw-user-input -s 192.0.2.10/32 -p udp -m udp --dport 51820 -j ACCEPT"
	assert.True(t, iptablesAllows(accept, "192.0.2.10", "51820", "udp"))
	for name, chain := range map[string]string{
		"earlier source drop":   "-A ufw-user-input -s 192.0.2.10/32 -j DROP\n" + accept,
		"earlier port reject":   "-A ufw-user-input -p udp -m udp --dport 51820 -j REJECT\n" + accept,
		"destination-limited":   "-A ufw-user-input -s 192.0.2.10/32 -d 192.0.2.1/32 -p udp -m udp --dport 51820 -j ACCEPT",
		"interface-limited":     "-A ufw-user-input -i eth9 -s 192.0.2.10/32 -p udp -m udp --dport 51820 -j ACCEPT",
		"another source only":   "-A ufw-user-input -s 192.0.2.11/32 -p udp -m udp --dport 51820 -j ACCEPT",
		"earlier rate limiting": "-A ufw-user-input -p udp -m udp --dport 51820 -m conntrack --ctstate NEW -j ufw-user-limit\n" + accept,
		"earlier subnet drop":   "-A ufw-user-input -s 192.0.2.0/24 -j DROP\n" + accept,
		"earlier range deny":    "-A ufw-user-input -p udp -m udp --dport 50000:60000 -j DROP\n" + accept,
		"earlier negated drop":  "-A ufw-user-input ! -s 198.51.100.1/32 -p udp -j DROP\n" + accept,
	} {
		assert.False(t, iptablesAllows(chain, "192.0.2.10", "51820", "udp"), name)
	}
	assert.True(t, iptablesAllows("-A ufw-user-input -p tcp -m tcp --dport 22 -j ACCEPT\n"+accept, "192.0.2.10", "51820", "udp"),
		"a rule for another port does not shadow the peer rule")
	assert.True(t, iptablesAllows("-A ufw-user-input -s 198.51.100.0/24 -j DROP\n-A ufw-user-input -p udp -m udp --dport 60000:61000 -j DROP\n"+accept, "192.0.2.10", "51820", "udp"),
		"rules for other subnets and port ranges do not shadow it")
}

func TestConfigureUFWAddsPeerRulesAndChecksThemLoaded(t *testing.T) {
	delay := ufwRetryDelay
	ufwRetryDelay = 0
	t.Cleanup(func() { ufwRetryDelay = delay })
	ufw := &routedRunner{routes: []route{
		{contains: "command -v ufw", out: "present\n"},
		{contains: "sshd -T", out: "22\n"},
		{contains: "ufw show added", out: "ufw allow 443/tcp\nufw allow from 192.0.2.99 to any port 51820 proto udp comment 'kipper-peer'\n"},
		{contains: "-N ufw-user-input", out: "absent\n"},
		{contains: "-S ufw-user-input", out: "-A ufw-user-input -p tcp --dport 22 -j ACCEPT\n"},
	}}
	err := configureFirewall(ufw, HostOS{ID: "ubuntu", Family: FamilyDebian}, false, workerMesh)
	require.Error(t, err, "peer rules missing from the live chain must fail the change")
	assert.Contains(t, err.Error(), "51820/udp from 192.0.2.10")
	assert.True(t, ufw.ran("ufw allow from 192.0.2.12 to any port 10250 proto tcp comment 'kipper-peer'"))
	assert.True(t, ufw.ran("ufw delete allow from 192.0.2.99 to any port 51820 proto udp comment 'kipper-peer'"))
	assert.False(t, ufw.ran("ufw allow 443/tcp"), "a worker opens no ingress port")
	assert.True(t, ufw.ran("ufw delete allow 443/tcp"), "a former server's ingress rule is removed on a worker")
}

func TestWantFirewalldForAWorker(t *testing.T) {
	want := wantFirewalldFor([]string{"22"}, workerMesh)
	assert.Equal(t, []string{"22/tcp"}, want.publicPorts)
	require.Len(t, want.peerZones, 2)
	assert.Equal(t, firewalldSourceZone{name: firewalldPeerZone, sources: []string{"192.0.2.12"}, ports: []string{"22/tcp", "51820/udp", "10250/tcp"}}, want.peerZones[0])
	assert.Equal(t, firewalldSourceZone{name: firewalldServerZone, sources: []string{"192.0.2.10"}, ports: []string{"22/tcp", "51820/udp", "10250/tcp", "9100/tcp"}}, want.peerZones[1])
	files := want.files()
	assert.Contains(t, files["zones/kipper-server.xml"], `<source address="192.0.2.10"/>`)
	assert.Contains(t, files["zones/kipper-server.xml"], `<port port="9100" protocol="tcp"/>`)
	assert.Contains(t, files["zones/kipper-peers.xml"], `<port port="51820" protocol="udp"/>`)
}

// A peer's source zone replaces the public zone for that address, so it must
// still carry the public ports or a server would refuse a worker's API calls.
func TestFirewalldPeerZonesKeepThePublicPorts(t *testing.T) {
	want := wantFirewalldFor([]string{"22"}, serverMesh)
	require.Len(t, want.peerZones, 1)
	for _, p := range want.publicPorts {
		assert.Contains(t, want.peerZones[0].ports, p)
	}
	assert.NotContains(t, want.peerZones[0].ports, "9100/tcp", "workers do not scrape the server's node-exporter")
}

const serverPeerZoneState = `== zone kipper-peers
kipper-peers (active)
  target: default
  icmp-block-inversion: no
  interfaces:
  sources: 192.0.2.12 192.0.2.11
  services:
  ports: 22/tcp 80/tcp 443/tcp 6443/tcp 51820/udp 10250/tcp
  protocols:
  forward: yes
  masquerade: no
  forward-ports:
  source-ports:
  icmp-blocks:
  rich rules:
`

func serverLoadedWithPeers() string {
	return strings.Replace(loadedFirewalld, "public (default)\n", "kipper-peers\n  sources: 192.0.2.11 192.0.2.12\npublic (default)\n", 1) + serverPeerZoneState
}

func TestFirewalldMismatchesAcceptsTheLoadedPeerZone(t *testing.T) {
	assert.Empty(t, firewalldMismatches(serverLoadedWithPeers(), wantFirewalldFor([]string{"22"}, serverMesh)))
}

func TestFirewalldMismatchesCatchesPeerZoneDrift(t *testing.T) {
	want := wantFirewalldFor([]string{"22"}, serverMesh)
	missingPort := strings.Replace(serverLoadedWithPeers(), "51820/udp 10250/tcp", "51820/udp", 1)
	assert.Contains(t, strings.Join(firewalldMismatches(missingPort, want), "\n"), "zone kipper-peers ports")

	assert.Contains(t, strings.Join(firewalldMismatches(loadedFirewalld, want), "\n"), "kipper-peers zone is not active")

	assert.Contains(t, strings.Join(firewalldMismatches(serverLoadedWithPeers(), wantFirewalld([]string{"22"})), "\n"), "zone kipper-peers is active",
		"a peer zone nobody wants any more is drift too")
}

func TestFirewalldStateCommandQueriesEveryWantedZone(t *testing.T) {
	cmd := firewalldStateCommand(wantFirewalldFor([]string{"22"}, workerMesh))
	assert.Contains(t, cmd, "for z in public kipper-pods kipper-peers kipper-server;")
}

func TestFirewalldChangesAllowAClaimedPeerZoneThatIsNoLongerWanted(t *testing.T) {
	c := firewalldSavedConfig{inventory: stockInventory + "f zones/kipper-peers.xml\n", public: stockPublicZone}
	assert.Empty(t, c.changes(true, false, wantFirewalld([]string{"22"})))
	assert.NotEmpty(t, c.changes(false, false, wantFirewalld([]string{"22"})), "only kip's own claim covers its files")
}

func TestConfigureFirewalldRemovesPeerZonesNoLongerWanted(t *testing.T) {
	h := newFirewalldHost(t)
	require.NoError(t, configureFirewalld(h, rhel10Host, FirewallConsent{}, FirewallMesh{}))
	assert.GreaterOrEqual(t, h.index("rm -f /etc/firewalld/zones/kipper-peers.xml /etc/firewalld/zones/kipper-server.xml"), 0)
	assert.Less(t, h.index("rm -f /etc/firewalld/zones/kipper-peers.xml"), h.index("firewall-offline-cmd --check-config"))
}

func TestOperatorFirewalldReportsMissingPeerPorts(t *testing.T) {
	cmd := operatorFirewalldCommand([]string{"22"}, workerMesh)
	assert.Contains(t, cmd, "--get-zone-of-source=192.0.2.10/32")
	assert.Contains(t, cmd, "MISSING-PEER:192.0.2.10:9100/tcp")
	assert.NotContains(t, cmd, "MISSING-PEER:192.0.2.12:9100/tcp")
	assert.NotContains(t, cmd, "MISSING:443/tcp", "a worker needs no ingress port")
}

func TestAdoptionQuestionListsThePeers(t *testing.T) {
	q := FirewalldAdoption{Opened: []string{"22/tcp"}, Peers: openedToPeers(workerMesh)}.Question("192.0.2.12")
	assert.Contains(t, q, "opened to cluster node 192.0.2.10: 51820/udp, 10250/tcp, 9100/tcp")
}

func TestFirewalldFingerprintCoversThePeers(t *testing.T) {
	saved := firewalldSavedConfig{inventory: stockInventory}
	a := firewalldFingerprint("public", saved, []string{"22"}, serverMesh, "")
	assert.Equal(t, a, firewalldFingerprint("public", saved, []string{"22"}, serverMesh, ""))
	assert.NotEqual(t, a, firewalldFingerprint("public", saved, []string{"22"}, FirewallMesh{}, ""))
	assert.NotEqual(t, a, firewalldFingerprint("public", saved, []string{"22"}, serverMesh, "443/udp"), "consent covers the tunnel port")
	assert.NotEqual(t, a, firewalldFingerprint("public", saved, []string{"22"}, FirewallMesh{Role: RoleServer, Peers: serverMesh.Peers[:1]}, ""))
}
