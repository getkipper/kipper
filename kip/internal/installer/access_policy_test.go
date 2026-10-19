package installer

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func prefixes(t *testing.T, ps ...string) []netip.Prefix {
	t.Helper()
	var out []netip.Prefix
	for _, p := range ps {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}

func TestParseAccessPrefixAcceptsCanonicalPrefixesInBothFamilies(t *testing.T) {
	for _, in := range []string{"203.0.113.0/24", "198.51.100.7/32", "2001:db8::/48", "2001:db8:1:2::abcd/128"} {
		p, err := ParseAccessPrefix(in)
		require.NoError(t, err, in)
		assert.Equal(t, in, p.String())
	}
}

func TestParseAccessPrefixAcceptsABareAddressAsAHostPrefix(t *testing.T) {
	p, err := ParseAccessPrefix("198.51.100.7")
	require.NoError(t, err)
	assert.Equal(t, "198.51.100.7/32", p.String())
	p, err = ParseAccessPrefix("2001:db8::1")
	require.NoError(t, err)
	assert.Equal(t, "2001:db8::1/128", p.String())
}

func TestParseAccessPrefixRefusesNonCanonicalAndOpenPrefixes(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.7/24":   "203.0.113.0/24",
		"2001:db8::1/48":   "2001:db8::/48",
		"0.0.0.0/0":        "kip cluster access reset",
		"::/0":             "kip cluster access reset",
		"not-an-address":   "not an IP prefix",
		"fe80::1%eth0/64":  "not an IP prefix",
		"::ffff:192.0.2.4": "IPv4 address",
	} {
		_, err := ParseAccessPrefix(in)
		require.Error(t, err, in)
		assert.Contains(t, err.Error(), want, in)
	}
}

func serverIntent(t *testing.T) AccessIntent {
	t.Helper()
	return AccessIntent{
		Generation: 7,
		Admin:      prefixes(t, "203.0.113.0/24", "198.51.100.7/32", "2001:db8::/48"),
		Ports:      []int{22},
		Cluster4:   prefixes(t, "10.42.0.0/16", "192.0.2.11/32"),
	}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "access", name)
	if os.Getenv("KIP_UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o600))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(want), got)
}

func TestRenderAccessTableForTheServer(t *testing.T) {
	table, err := RenderAccessTable(serverIntent(t))
	require.NoError(t, err)
	golden(t, "server-table.nft", table)
}

func TestRenderAccessOperatorsForBothFamilies(t *testing.T) {
	ops, err := RenderAccessOperators(serverIntent(t))
	require.NoError(t, err)
	golden(t, "operators-mixed.nft", ops)
}

func TestRenderAccessTableForAWorkerAdmitsTheServerOnSSHOnly(t *testing.T) {
	in := serverIntent(t)
	in.Ports = []int{2222, 22}
	in.Server4 = netip.MustParseAddr("192.0.2.10")
	table, err := RenderAccessTable(in)
	require.NoError(t, err)
	golden(t, "worker-table.nft", table)
	assert.Contains(t, table, "tcp dport { 22, 2222 } ip saddr @server4 accept")
	assert.NotContains(t, table, "6443 } ip saddr @server4", "the server is never admitted on a worker's 6443 by server4")
}

func TestRenderAccessTableOnTheServerLeavesServer4Empty(t *testing.T) {
	table, err := RenderAccessTable(serverIntent(t))
	require.NoError(t, err)
	assert.Contains(t, table, "set server4 { type ipv4_addr; }\n", "workers are never admitted on the server's SSH")
	assert.Contains(t, table, "set joining4 { type ipv4_addr; }\n", "joining4 is always declared and empty in the file")
}

func TestRenderAccessOperatorsLeavesOutAFamilyWithoutPrefixes(t *testing.T) {
	in := serverIntent(t)
	in.Admin = prefixes(t, "2001:db8::/48")
	ops, err := RenderAccessOperators(in)
	require.NoError(t, err)
	assert.NotContains(t, ops, "admin4")
	assert.Contains(t, ops, "add element inet kipper_access admin6 { 2001:db8::/48 }\n")

	in.Admin = prefixes(t, "198.51.100.7/32")
	ops, err = RenderAccessOperators(in)
	require.NoError(t, err)
	assert.Equal(t, "# kipper-access generation 7\nadd element inet kipper_access admin4 { 198.51.100.7 }\n", ops)
}

func TestRenderAccessRefusesAnEmptyOperatorList(t *testing.T) {
	in := serverIntent(t)
	in.Admin = nil
	_, err := RenderAccessOperators(in)
	assert.ErrorContains(t, err, "no operator prefixes")
	_, err = RenderAccessTable(in)
	assert.ErrorContains(t, err, "no operator prefixes")
}

func TestRenderAccessCollapsesDuplicateAndContainedPrefixes(t *testing.T) {
	in := serverIntent(t)
	in.Admin = prefixes(t, "203.0.113.7/32", "203.0.113.0/24", "203.0.113.0/24", "198.51.100.7/32")
	in.Cluster4 = prefixes(t, "10.42.0.0/16", "10.42.3.4/32", "192.0.2.11/32", "192.0.2.11/32")
	ops, err := RenderAccessOperators(in)
	require.NoError(t, err)
	assert.Contains(t, ops, "admin4 { 198.51.100.7, 203.0.113.0/24 }", "nft refuses overlapping intervals")
	table, err := RenderAccessTable(in)
	require.NoError(t, err)
	assert.Contains(t, table, "elements = { 10.42.0.0/16, 192.0.2.11 }")
}

func TestRenderAccessDeduplicatesThePortSet(t *testing.T) {
	in := serverIntent(t)
	in.Ports = []int{6443, 22, 22}
	table, err := RenderAccessTable(in)
	require.NoError(t, err)
	assert.Contains(t, table, "tcp dport { 22, 6443 } counter drop", "nft rejects { 6443, 6443 }")
	assert.NotContains(t, table, "6443, 6443")
}

func TestRenderAccessRefusesSSHOnTheWebPorts(t *testing.T) {
	for _, port := range []int{80, 443} {
		in := serverIntent(t)
		in.Ports = []int{22, port}
		_, err := RenderAccessTable(in)
		assert.ErrorContains(t, err, "80 and 443 stay public")
	}
}

func TestRenderAccessRefusesAnIPv6ClusterNetwork(t *testing.T) {
	in := serverIntent(t)
	in.Cluster4 = prefixes(t, "fd00:42::/56")
	_, err := RenderAccessTable(in)
	assert.ErrorContains(t, err, "the access list supports IPv4 cluster networks. Operator prefixes may be IPv6")
}

func TestAccessTableHeaderGeneration(t *testing.T) {
	table, err := RenderAccessTable(serverIntent(t))
	require.NoError(t, err)
	gen, ok := AccessHeaderGeneration(table)
	assert.True(t, ok)
	assert.Equal(t, 7, gen)
	_, ok = AccessHeaderGeneration("")
	assert.False(t, ok)
	_, ok = AccessHeaderGeneration("# something else\n")
	assert.False(t, ok)
}

func peerRecord(name, id, role, state, peer string) NodeRecord {
	return NodeRecord{Name: name, MachineID: id, Host: name + ".example.com", Role: role, State: state, PeerAddress: peer}
}

const (
	serverID = "00000000000000000000000000000001"
	worker1  = "00000000000000000000000000000002"
	worker2  = "00000000000000000000000000000003"
	worker3  = "00000000000000000000000000000004"
)

func TestAccessPeersAdmitsEveryOtherLiveNode(t *testing.T) {
	records := []NodeRecord{
		peerRecord("server", serverID, NodeRoleServer, NodeStateReady, "192.0.2.10"),
		peerRecord("worker-1", worker1, NodeRoleWorker, NodeStateReady, "192.0.2.11"),
		peerRecord("worker-2", worker2, NodeRoleWorker, NodeStateRemoving, "192.0.2.12"),
		peerRecord("worker-3", worker3, NodeRoleWorker, NodeStateJoining, "192.0.2.13"),
	}
	peers, err := accessPeers(records, serverID, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"192.0.2.11", "192.0.2.12", "192.0.2.13"}, addrStrings(peers), "a removing node keeps reaching 6443 while it drains")

	peers, err = accessPeers(records, worker1, worker3)
	require.NoError(t, err)
	assert.Equal(t, []string{"192.0.2.10", "192.0.2.12"}, addrStrings(peers), "self and the excluded record contribute nothing")
}

func TestAccessPeersSkipsAWipingNode(t *testing.T) {
	records := []NodeRecord{
		peerRecord("server", serverID, NodeRoleServer, NodeStateReady, ""),
		peerRecord("worker-1", worker1, NodeRoleWorker, NodeStateWiping, ""),
	}
	peers, err := accessPeers(records, serverID, "")
	require.NoError(t, err)
	assert.Empty(t, peers)
}

func TestAccessPeersRefusesARecordWithoutAPeerAddress(t *testing.T) {
	records := []NodeRecord{
		peerRecord("server", serverID, NodeRoleServer, NodeStateReady, ""),
		peerRecord("worker-1", worker1, NodeRoleWorker, NodeStateReady, "192.0.2.11"),
	}
	_, err := accessPeers(records, worker1, "")
	assert.ErrorContains(t, err, "server")
	assert.ErrorContains(t, err, "kip node add")

	peers, err := accessPeers(records, serverID, "")
	require.NoError(t, err, "the server's own record needs no peer address")
	assert.Equal(t, []string{"192.0.2.11"}, addrStrings(peers))
}

func addrStrings(as []netip.Addr) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.String())
	}
	return out
}

func TestAccessListenerPortsUnionsSSHDAndTheSocket(t *testing.T) {
	ports, err := accessListenerPorts([]string{"22", "2222"}, "Listen=[::]:22 (Stream) 0.0.0.0:2200 (Stream)\n")
	require.NoError(t, err)
	assert.Equal(t, []int{22, 2200, 2222}, ports)

	ports, err = accessListenerPorts([]string{"22"}, "Listen=\n")
	require.NoError(t, err)
	assert.Equal(t, []int{22}, ports)

	ports, err = accessListenerPorts([]string{"22"}, "Listen=2022 (Stream)\n")
	require.NoError(t, err)
	assert.Equal(t, []int{22, 2022}, ports)
}

func TestAccessListenerPortsRefusesTheWebPorts(t *testing.T) {
	_, err := accessListenerPorts([]string{"22"}, "Listen=[::]:443 (Stream)\n")
	assert.ErrorContains(t, err, "443")
}

func TestParseSetElementsReadsEveryElementForm(t *testing.T) {
	for _, fixture := range []string{"set-admin4-1.0.2.json", "set-admin4-1.0.9.json", "set-admin4-1.1.json"} {
		t.Run(fixture, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", "access", fixture))
			require.NoError(t, err)
			got, err := parseSetElements(string(b))
			require.NoError(t, err)
			assert.True(t, sameAddressSet(got, prefixes(t, "203.0.113.0/24", "198.51.100.7/32")), "%v", got)
		})
	}
}

func TestSameAddressSetIgnoresHowAnIntervalIsWritten(t *testing.T) {
	ranges := `{"nftables":[{"metainfo":{}},{"set":{"family":"inet","name":"admin4","table":"kipper_access","type":"ipv4_addr","flags":["interval"],"elem":[{"range":["203.0.113.0","203.0.113.255"]},"198.51.100.7"]}}]}`
	got, err := parseSetElements(ranges)
	require.NoError(t, err)
	assert.True(t, sameAddressSet(got, prefixes(t, "203.0.113.0/25", "203.0.113.128/25", "198.51.100.7/32")))
	assert.False(t, sameAddressSet(got, prefixes(t, "203.0.113.0/24")), "a missing element is drift")
	assert.False(t, sameAddressSet(got, prefixes(t, "203.0.113.0/24", "198.51.100.7/32", "198.51.100.9/32")), "an extra element is drift")
}

func TestParseSetElementsOfAnEmptySet(t *testing.T) {
	got, err := parseSetElements(`{"nftables":[{"metainfo":{}},{"set":{"family":"inet","name":"joining4","table":"kipper_access","type":"ipv4_addr"}}]}`)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestParseSetElementsWithoutASet(t *testing.T) {
	_, err := parseSetElements(`{"nftables":[{"metainfo":{}}]}`)
	assert.Error(t, err)
	_, err = parseSetElements(`Error: No such file or directory`)
	assert.Error(t, err)
}

func TestParseChainShape(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "access", "chain-input.json"))
	require.NoError(t, err)
	shape, err := parseChainShape(string(b))
	require.NoError(t, err)
	assert.Equal(t, accessChainShape{Hook: "input", Priority: -10, Policy: "accept", Rules: 8, Dropped: 12}, shape)
	assert.True(t, shape.matches())

	shape.Priority = 0
	assert.False(t, shape.matches())
	_, err = parseChainShape(`{"nftables":[{"metainfo":{}}]}`)
	assert.Error(t, err)
}

func TestTheRenderedChainHasTheShapeShowExpects(t *testing.T) {
	table, err := RenderAccessTable(serverIntent(t))
	require.NoError(t, err)
	_, body, found := strings.Cut(table, "chain input {")
	require.True(t, found)
	rules := 0
	for _, line := range strings.Split(body, "\n")[2:] {
		if strings.TrimSpace(line) == "}" {
			break
		}
		rules++
	}
	assert.Equal(t, accessChainRules, rules)
}
