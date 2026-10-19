package installer

import (
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const wireguardPort = "51820"

// FirewallMesh describes a host's role and firewall peers.
// The zero value represents a server without peers.
type FirewallMesh struct {
	Role  HostRole
	Peers []FirewallPeer
}

// FirewallPeer identifies another node by its traffic's source address.
type FirewallPeer struct {
	Address string
	Server  bool
}

// FirewallMeshFor builds the mesh for the node with selfMachineID from the
// node records. Records without a peer address are skipped, because their
// traffic source is not known yet. Peers are sorted lexicographically by address.
// A missing self record defaults to the server role.
func FirewallMeshFor(records []NodeRecord, selfMachineID string) FirewallMesh {
	mesh := FirewallMesh{Role: RoleServer}
	for _, r := range records {
		if r.MachineID == selfMachineID {
			if r.Role == NodeRoleWorker {
				mesh.Role = RoleWorker
			}
			continue
		}
		if r.PeerAddress == "" {
			continue
		}
		mesh.Peers = append(mesh.Peers, FirewallPeer{Address: r.PeerAddress, Server: r.Role == NodeRoleServer})
	}
	sort.Slice(mesh.Peers, func(i, j int) bool { return mesh.Peers[i].Address < mesh.Peers[j].Address })
	return mesh
}

func (m FirewallMesh) equal(o FirewallMesh) bool {
	return m.Role == o.Role && slices.Equal(m.Peers, o.Peers)
}

// validate requires canonical IPv4 addresses for interpolation into firewall commands.
func (m FirewallMesh) validate() error {
	for _, p := range m.Peers {
		addr, err := netip.ParseAddr(p.Address)
		if err != nil || !addr.Is4() || addr.String() != p.Address {
			return fmt.Errorf("peer address %q is not an IPv4 address", p.Address)
		}
	}
	return nil
}

// peerPorts lists ports for cluster networking and kubelet access from peers,
// plus node-exporter access from the server for monitoring.
func peerPorts(p FirewallPeer) []string {
	ports := []string{wireguardPort + "/udp", kubeletPort + "/tcp"}
	if p.Server {
		ports = append(ports, nodeExporterPort+"/tcp")
	}
	return ports
}

// ufwPeerComment identifies Kipper's peer rules for cleanup.
const ufwPeerComment = "kipper-peer"

func ufwPeerRules(mesh FirewallMesh) []string {
	var rules []string
	for _, p := range mesh.Peers {
		for _, port := range peerPorts(p) {
			number, proto, _ := strings.Cut(port, "/")
			rules = append(rules, fmt.Sprintf("ufw allow from %s to any port %s proto %s comment '%s'", p.Address, number, proto, ufwPeerComment))
		}
	}
	return rules
}

// staleUFWPeerRules lists the delete commands for kip's peer rules whose
// address or port is no longer in the mesh. It reads `ufw show added`, which
// prints each saved rule as the command that added it.
func staleUFWPeerRules(added string, mesh FirewallMesh) []string {
	want := map[string]bool{}
	for _, r := range ufwPeerRules(mesh) {
		want[r] = true
	}
	var stale []string
	for _, line := range strings.Split(added, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "ufw allow from ")
		if !ok || want[line] || !strings.HasSuffix(line, " comment '"+ufwPeerComment+"'") {
			continue
		}
		stale = append(stale, "ufw delete allow from "+rest)
	}
	return stale
}

// ufwServerOnlyRules lists the public ingress and API rules kip adds on servers.
// A host that was a server can still carry them, so a worker join removes them.
var ufwServerOnlyRules = []string{"ufw allow 80/tcp", "ufw allow 443/tcp", "ufw allow 6443/tcp"}

// staleUFWRoleRules returns commands to delete saved server port rules on workers,
// except those in sshPorts. Exact matches include rules added by an operator.
// Rules with source restrictions or other options stay. Servers return no commands.
func staleUFWRoleRules(added string, mesh FirewallMesh, sshPorts []string) []string {
	if mesh.Role != RoleWorker {
		return nil
	}
	present := map[string]bool{}
	for _, line := range strings.Split(added, "\n") {
		present[strings.TrimSpace(line)] = true
	}
	var stale []string
	for _, rule := range ufwServerOnlyRules {
		if present[rule] && !slices.Contains(sshPorts, strings.TrimSuffix(strings.TrimPrefix(rule, "ufw allow "), "/tcp")) {
			stale = append(stale, "ufw delete "+strings.TrimPrefix(rule, "ufw "))
		}
	}
	return stale
}

// ufwPeerRulesMissing lists peer rules whose access the live chain cannot confirm.
func ufwPeerRulesMissing(chain string, mesh FirewallMesh) []string {
	var missing []string
	for _, p := range mesh.Peers {
		for _, port := range peerPorts(p) {
			number, proto, _ := strings.Cut(port, "/")
			if !iptablesAllows(chain, p.Address, number, proto) {
				missing = append(missing, fmt.Sprintf("%s from %s", port, p.Address))
			}
		}
	}
	return missing
}

// iptablesAllows requires the first potentially matching rule in an iptables -S
// listing to be an unrestricted ACCEPT for the exact source, protocol and port.
func iptablesAllows(chain, source, port, proto string) bool {
	peer, err := netip.ParseAddr(source)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(chain, "\n") {
		r, ok := parseIPTablesRule(line)
		if !ok {
			continue
		}
		if !r.negated && !r.mayMatch(peer, port, proto) {
			continue
		}
		return r.target == "ACCEPT" && r.source == source+"/32" && r.proto == proto && r.dport == port && !r.restricted && !r.negated
	}
	return false
}

// mayMatch checks source, protocol and port selectors conservatively:
// unparseable sources and port ranges remain potential matches.
func (r iptablesRule) mayMatch(peer netip.Addr, port, proto string) bool {
	if r.source != "" {
		prefix, err := netip.ParsePrefix(r.source)
		if err == nil && !prefix.Contains(peer) {
			return false
		}
	}
	if r.proto != "" && r.proto != proto {
		return false
	}
	if r.dport != "" {
		low, high, isRange := strings.Cut(r.dport, ":")
		if !isRange {
			high = low
		}
		want, errWant := strconv.Atoi(port)
		lo, errLow := strconv.Atoi(low)
		hi, errHigh := strconv.Atoi(high)
		if errWant == nil && errLow == nil && errHigh == nil && (want < lo || want > hi) {
			return false
		}
	}
	return true
}

type iptablesRule struct {
	source, proto, dport, target string
	restricted                   bool
	negated                      bool
}

// parseIPTablesRule reads one "-A chain ..." line of `iptables -S`.
func parseIPTablesRule(line string) (iptablesRule, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "-A" {
		return iptablesRule{}, false
	}
	var r iptablesRule
	for i := 2; i < len(fields); i++ {
		next := ""
		if i+1 < len(fields) {
			next = fields[i+1]
		}
		switch fields[i] {
		case "-s":
			r.source, i = next, i+1
		case "-p":
			r.proto, i = next, i+1
		case "--dport":
			r.dport, i = next, i+1
		case "-j":
			r.target, i = next, i+1
		case "!":
			r.negated = true
		case "-m":
			// A protocol match module repeats -p; anything else restricts the rule.
			if next != r.proto {
				r.restricted = true
			}
			i++
		default:
			r.restricted = true
		}
	}
	return r, true
}

// Separate peer zones grant the server additional access to node-exporter.
const (
	firewalldPeerZone   = "kipper-peers"
	firewalldServerZone = "kipper-server"
)

// operatorPeerCommand checks peer access in an operator-managed firewalld
// and collects the checked zones in the shell variable peerzones.
func operatorPeerCommand(mesh FirewallMesh) string {
	var b strings.Builder
	for _, p := range mesh.Peers {
		// Use the route to the peer to infer its incoming interface when no
		// exact source binding exists; private traffic may use a separate interface.
		fmt.Fprintf(&b, `if pz=$(firewall-cmd --get-zone-of-source=%[1]s/32 2>/dev/null || firewall-cmd --get-zone-of-source=%[1]s 2>/dev/null) && [ -n "$pz" ]; then :; `+
			`else pdev=$(ip -o route get %[1]s 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "dev") {print $(i+1); exit}}'); `+
			`pz=$( { [ -n "$pdev" ] && firewall-cmd --get-zone-of-interface="$pdev"; } 2>/dev/null || firewall-cmd --get-default-zone); fi; peerzones="$peerzones $pz"; `, p.Address)
		for _, port := range peerPorts(p) {
			fmt.Fprintf(&b, `%s || echo MISSING-PEER:%s:%s; `, zoneOpensPort("$pz", port, ""), p.Address, port)
		}
		// A peer's own source zone replaces the external zone for it, so it
		// must also open the ports the role serves.
		if mesh.Role != RoleWorker {
			for _, s := range []struct{ port, service string }{{"80/tcp", "http"}, {"443/tcp", "https"}, {"6443/tcp", "kube-apiserver"}} {
				fmt.Fprintf(&b, `%s || echo MISSING-PEER:%s:%s; `, zoneOpensPort("$pz", s.port, s.service), p.Address, s.port)
			}
		}
	}
	return b.String()
}

// peerSourceList includes bare addresses and /32 prefixes for source comparisons.
func peerSourceList(mesh FirewallMesh) string {
	var list []string
	for _, p := range mesh.Peers {
		list = append(list, p.Address, p.Address+"/32")
	}
	return strings.Join(list, " ")
}
