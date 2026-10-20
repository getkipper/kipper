package installer

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	accessTable   = "inet kipper_access"
	accessAPIPort = 6443
	// accessChainRules is the number of rules in the rendered input chain
	// without operator devices. The device rule adds one.
	accessChainRules = 8
	accessHeader     = "# kipper-access generation "
)

// AccessIntent is everything one node's access table is rendered from.
// Server4 is the zero Addr on the server. Devices admits connections that
// arrive through the operator interface. WireGuard authenticates registered
// peers, and the kipper_ops table restricts their traffic.
type AccessIntent struct {
	Generation int
	Admin      []netip.Prefix
	Ports      []int
	Cluster4   []netip.Prefix
	Server4    netip.Addr
	Devices    bool
}

// ParseAccessPrefix reads a canonical operator prefix or a bare host address.
// It rejects non-canonical prefixes, IPv4-mapped IPv6 addresses, zones and
// the two prefixes that admit every address.
func ParseAccessPrefix(s string) (netip.Prefix, error) {
	if !strings.Contains(s, "/") {
		addr, err := netip.ParseAddr(s)
		if err != nil || addr.Zone() != "" {
			return netip.Prefix{}, fmt.Errorf("%q is not an IP prefix", s)
		}
		if addr.Is4In6() {
			return netip.Prefix{}, fmt.Errorf("%q is an IPv4 address written as IPv6. Write it as %s", s, addr.Unmap())
		}
		return netip.PrefixFrom(addr, addr.BitLen()), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q is not an IP prefix", s)
	}
	if p.Addr().Is4In6() {
		return netip.Prefix{}, fmt.Errorf("%q is an IPv4 address written as IPv6. Write it as an IPv4 prefix", s)
	}
	if p.Bits() == 0 {
		return netip.Prefix{}, fmt.Errorf("%s admits every address. 'kip cluster access reset' opens the ports", s)
	}
	if p.Masked().String() != s {
		return netip.Prefix{}, fmt.Errorf("%s has host bits set. The prefix is %s", s, p.Masked())
	}
	return p, nil
}

// RenderAccessTable renders table.nft: the chain and every set, with the
// operator sets empty. Loading it alone blocks new connections from operator
// prefixes while admitting the configured pods, peers and, when enabled, devices.
// Joining addresses must be added separately to the initially empty joining4 set.
// It rejects an empty operator list unless devices are enabled, missing or invalid
// SSH ports, SSH on 80 or 443, and IPv6 cluster addresses.
func RenderAccessTable(in AccessIntent) (string, error) {
	if err := in.validate(); err != nil {
		return "", err
	}
	ports := portList(append(append([]int(nil), in.Ports...), accessAPIPort))
	ssh := portList(in.Ports)
	server4 := ""
	if in.Server4.IsValid() {
		server4 = " elements = { " + in.Server4.String() + " }"
	}
	cluster4 := ""
	if c := collapse(in.Cluster4); len(c) > 0 {
		cluster4 = " elements = { " + prefixList(c) + " }"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s%d\n", accessHeader, in.Generation)
	fmt.Fprintf(&b, "table %s\ndelete table %s\ntable %s {\n", accessTable, accessTable, accessTable)
	b.WriteString("  set admin4 { type ipv4_addr; flags interval; }\n")
	b.WriteString("  set admin6 { type ipv6_addr; flags interval; }\n")
	fmt.Fprintf(&b, "  set cluster4 { type ipv4_addr; flags interval;%s }\n", cluster4)
	b.WriteString("  set joining4 { type ipv4_addr; }\n")
	fmt.Fprintf(&b, "  set server4 { type ipv4_addr;%s }\n", server4)
	b.WriteString("  chain input {\n")
	b.WriteString("    type filter hook input priority filter - 10; policy accept;\n")
	b.WriteString("    ct state established,related accept\n")
	b.WriteString("    iif \"lo\" accept\n")
	fmt.Fprintf(&b, "    tcp dport { %s } ip saddr @admin4 accept\n", ports)
	fmt.Fprintf(&b, "    tcp dport { %s } ip6 saddr @admin6 accept\n", ports)
	fmt.Fprintf(&b, "    tcp dport %d ip saddr @cluster4 accept\n", accessAPIPort)
	fmt.Fprintf(&b, "    tcp dport %d ip saddr @joining4 accept\n", accessAPIPort)
	fmt.Fprintf(&b, "    tcp dport { %s } ip saddr @server4 accept\n", ssh)
	if in.Devices {
		fmt.Fprintf(&b, "    %s tcp dport { %s } accept\n", accessDeviceMatch, ports)
	}
	fmt.Fprintf(&b, "    tcp dport { %s } counter drop\n", ports)
	b.WriteString("  }\n}\n")
	return b.String(), nil
}

// RenderAccessOperators renders operators.nft, which adds the operator
// prefixes to the table that table.nft declares. A family without prefixes
// has no line. It validates the intent with the same checks as RenderAccessTable.
func RenderAccessOperators(in AccessIntent) (string, error) {
	if err := in.validate(); err != nil {
		return "", err
	}
	var v4, v6 []netip.Prefix
	for _, p := range collapse(in.Admin) {
		if p.Addr().Is4() {
			v4 = append(v4, p)
		} else {
			v6 = append(v6, p)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s%d\n", accessHeader, in.Generation)
	if len(v4) > 0 {
		fmt.Fprintf(&b, "add element %s admin4 { %s }\n", accessTable, prefixList(v4))
	}
	if len(v6) > 0 {
		fmt.Fprintf(&b, "add element %s admin6 { %s }\n", accessTable, prefixList(v6))
	}
	return b.String(), nil
}

func (in AccessIntent) validate() error {
	if len(in.Admin) == 0 && !in.Devices {
		return errors.New("the access list has no operator prefixes. 'kip cluster access reset' opens the ports")
	}
	if len(in.Ports) == 0 {
		return errors.New("no SSH listener ports were found")
	}
	for _, p := range in.Ports {
		if p == 80 || p == 443 {
			return fmt.Errorf("sshd listens on port %d, and 80 and 443 stay public, so kip cannot restrict SSH on this host", p)
		}
		if p < 1 || p > 65535 {
			return fmt.Errorf("%d is not a port", p)
		}
	}
	for _, p := range in.Cluster4 {
		if !p.Addr().Is4() {
			return errors.New("the access list supports IPv4 cluster networks. Operator prefixes may be IPv6")
		}
	}
	if in.Server4.IsValid() && !in.Server4.Is4() {
		return fmt.Errorf("the server's peer address %s is not IPv4", in.Server4)
	}
	return nil
}

// collapse sorts prefixes, removing duplicates and prefixes inside another
// one, because nft refuses overlapping intervals in one set.
func collapse(ps []netip.Prefix) []netip.Prefix {
	sorted := append([]netip.Prefix(nil), ps...)
	for i := range sorted {
		sorted[i] = sorted[i].Masked()
	}
	sort.Slice(sorted, func(i, j int) bool {
		if c := sorted[i].Addr().Compare(sorted[j].Addr()); c != 0 {
			return c < 0
		}
		return sorted[i].Bits() < sorted[j].Bits()
	})
	var out []netip.Prefix
	for _, p := range sorted {
		if n := len(out); n > 0 && out[n-1].Contains(p.Addr()) && out[n-1].Bits() <= p.Bits() {
			continue
		}
		out = append(out, p)
	}
	return out
}

// prefixList writes host prefixes as bare addresses, as nft lists them.
func prefixList(ps []netip.Prefix) string {
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		if p.IsSingleIP() {
			parts = append(parts, p.Addr().String())
		} else {
			parts = append(parts, p.String())
		}
	}
	return strings.Join(parts, ", ")
}

func portList(ports []int) string {
	seen := map[int]bool{}
	var unique []int
	for _, p := range ports {
		if !seen[p] {
			seen[p] = true
			unique = append(unique, p)
		}
	}
	sort.Ints(unique)
	parts := make([]string, 0, len(unique))
	for _, p := range unique {
		parts = append(parts, strconv.Itoa(p))
	}
	return strings.Join(parts, ", ")
}

var accessHeaderPattern = regexp.MustCompile(`^# kipper-access generation ([0-9]{1,18})$`)

// AccessHeaderGeneration reads the generation from the first line of a
// rendered access file. The boolean is false if the header is missing, malformed
// or outside the range of int.
func AccessHeaderGeneration(file string) (int, bool) {
	line, _, _ := strings.Cut(file, "\n")
	m := accessHeaderPattern.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// accessPeers returns sorted IPv4 peer addresses for cluster4 to admit on port
// 6443, skipping the excluded machine ID and wiping nodes. It includes self so
// the server's pods can reach its advertised API address when provider NAT
// returns their traffic with that same address as the source. A missing self
// address is skipped. Other missing addresses and invalid or non-IPv4 addresses
// return an error.
func accessPeers(records []NodeRecord, self, exclude string) ([]netip.Addr, error) {
	var peers []netip.Addr
	for _, r := range records {
		if (exclude != "" && r.MachineID == exclude) || r.State == NodeStateWiping {
			continue
		}
		if r.MachineID == self && r.PeerAddress == "" {
			continue
		}
		if r.PeerAddress == "" {
			return nil, fmt.Errorf("node %s has no recorded peer address, so kip cannot tell which address to admit on 6443. Re-record it with 'kip node add %s'", r.Name, r.Host)
		}
		addr, err := netip.ParseAddr(r.PeerAddress)
		if err != nil || !addr.Is4() {
			return nil, fmt.Errorf("node %s has an invalid peer address %q", r.Name, r.PeerAddress)
		}
		peers = append(peers, addr)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Less(peers[j]) })
	return peers, nil
}

var socketPortPattern = regexp.MustCompile(`(?:^|[\s:=\]])([0-9]{1,5}) \(Stream\)`)

// accessListenerPorts unions sshd's configured ports with the stream ports
// of ssh.socket, as printed by systemctl show -p Listen. A listener on 80
// or 443 is refused.
func accessListenerPorts(sshd []string, socket string) ([]int, error) {
	seen := map[int]bool{}
	var ports []int
	add := func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%q is not a port", s)
		}
		if n == 80 || n == 443 {
			return fmt.Errorf("SSH listens on port %d, and 80 and 443 stay public, so kip cannot restrict SSH on this host", n)
		}
		if !seen[n] {
			seen[n] = true
			ports = append(ports, n)
		}
		return nil
	}
	for _, p := range sshd {
		if err := add(p); err != nil {
			return nil, err
		}
	}
	for _, m := range socketPortPattern.FindAllStringSubmatch(socket, -1) {
		if err := add(m[1]); err != nil {
			return nil, err
		}
	}
	sort.Ints(ports)
	return ports, nil
}

// addrRange is an inclusive range of addresses of one family.
type addrRange struct{ lo, hi netip.Addr }

// parseSetElements reads the elements of the one set in nft -j list set
// output as prefixes. A range that is not one prefix is split into prefixes.
func parseSetElements(nftJSON string) ([]netip.Prefix, error) {
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal([]byte(nftJSON), &doc); err != nil {
		return nil, fmt.Errorf("reading nft's set listing: %w", err)
	}
	for _, item := range doc.Nftables {
		raw, ok := item["set"]
		if !ok {
			continue
		}
		var set struct {
			Elem json.RawMessage `json:"elem"`
		}
		if err := json.Unmarshal(raw, &set); err != nil {
			return nil, fmt.Errorf("reading nft's set listing: %w", err)
		}
		if len(set.Elem) == 0 {
			return nil, nil
		}
		var elems []json.RawMessage
		if err := json.Unmarshal(set.Elem, &elems); err != nil {
			elems = []json.RawMessage{set.Elem}
		}
		var out []netip.Prefix
		for _, e := range elems {
			r, err := parseSetElement(e)
			if err != nil {
				return nil, err
			}
			out = append(out, rangePrefixes(r)...)
		}
		return out, nil
	}
	return nil, errors.New("nft listed no set")
}

func parseSetElement(raw json.RawMessage) (addrRange, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return addrRange{}, fmt.Errorf("nft listed %q, which is not an address", s)
		}
		return addrRange{a, a}, nil
	}
	var obj struct {
		Prefix *struct {
			Addr string `json:"addr"`
			Len  int    `json:"len"`
		} `json:"prefix"`
		Range []string `json:"range"`
		Elem  *struct {
			Val json.RawMessage `json:"val"`
		} `json:"elem"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return addrRange{}, fmt.Errorf("nft listed an element kip cannot read: %s", raw)
	}
	switch {
	case obj.Elem != nil:
		return parseSetElement(obj.Elem.Val)
	case obj.Prefix != nil:
		a, err := netip.ParseAddr(obj.Prefix.Addr)
		if err != nil {
			return addrRange{}, fmt.Errorf("nft listed prefix address %q", obj.Prefix.Addr)
		}
		p, err := a.Prefix(obj.Prefix.Len)
		if err != nil {
			return addrRange{}, fmt.Errorf("nft listed prefix %s/%d", obj.Prefix.Addr, obj.Prefix.Len)
		}
		return prefixRange(p), nil
	case len(obj.Range) == 2:
		lo, err1 := netip.ParseAddr(obj.Range[0])
		hi, err2 := netip.ParseAddr(obj.Range[1])
		if err1 != nil || err2 != nil || lo.BitLen() != hi.BitLen() || hi.Less(lo) {
			return addrRange{}, fmt.Errorf("nft listed range %v", obj.Range)
		}
		return addrRange{lo, hi}, nil
	}
	return addrRange{}, fmt.Errorf("nft listed an element kip cannot read: %s", raw)
}

func prefixRange(p netip.Prefix) addrRange {
	p = p.Masked()
	lo := p.Addr()
	hi := lo
	b := hi.AsSlice()
	for bit := p.Bits(); bit < len(b)*8; bit++ {
		b[bit/8] |= 0x80 >> (bit % 8)
	}
	hi, _ = netip.AddrFromSlice(b)
	return addrRange{lo, hi}
}

// rangePrefixes splits an inclusive range into the fewest prefixes.
func rangePrefixes(r addrRange) []netip.Prefix {
	var out []netip.Prefix
	lo := r.lo
	for {
		bits := lo.BitLen()
		for bits > 0 {
			p, _ := lo.Prefix(bits - 1)
			if p.Addr() != lo || prefixRange(p).hi.Compare(r.hi) > 0 {
				break
			}
			bits--
		}
		p := netip.PrefixFrom(lo, bits)
		out = append(out, p)
		end := prefixRange(p).hi
		if end.Compare(r.hi) >= 0 || !end.Next().IsValid() {
			return out
		}
		lo = end.Next()
	}
}

// mergedRanges converts prefixes into sorted, merged address ranges.
func mergedRanges(ps []netip.Prefix) []addrRange {
	rs := make([]addrRange, 0, len(ps))
	for _, p := range ps {
		rs = append(rs, prefixRange(p))
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].lo.Less(rs[j].lo) })
	var out []addrRange
	for _, r := range rs {
		if n := len(out); n > 0 && out[n-1].lo.BitLen() == r.lo.BitLen() {
			last := &out[n-1]
			if next := last.hi.Next(); r.lo.Compare(last.hi) <= 0 || (next.IsValid() && next == r.lo) {
				if r.hi.Compare(last.hi) > 0 {
					last.hi = r.hi
				}
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

// SameAccessAddresses reports whether a and b admit exactly the same
// addresses, however the prefixes are split or overlap.
func SameAccessAddresses(a, b []netip.Prefix) bool {
	return sameAddressSet(a, b)
}

func sameAddressSet(a, b []netip.Prefix) bool {
	x, y := mergedRanges(a), mergedRanges(b)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// admits reports whether any prefix contains addr.
func admits(ps []netip.Prefix, addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range ps {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// accessChainShape is what show compares about the live input chain.
type accessChainShape struct {
	Hook     string
	Priority int
	Policy   string
	Rules    int
	Dropped  int64
}

// accessDeviceMatch is the match of the device rule in table.nft.
const accessDeviceMatch = `iifname "` + opsInterface + `"`

// expectedAccessRules is the input chain's rule count for a committed
// table.nft: one more when it carries the device rule.
func expectedAccessRules(table string) int {
	if strings.Contains(table, accessDeviceMatch) {
		return accessChainRules + 1
	}
	return accessChainRules
}

func (s accessChainShape) matches(rules int) bool {
	return s.Hook == "input" && s.Priority == -10 && s.Policy == "accept" && s.Rules == rules
}

// parseChainShape reads the hook, priority, policy, rule count and drop
// counter of the chain in nft -j list chain output.
func parseChainShape(nftJSON string) (accessChainShape, error) {
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal([]byte(nftJSON), &doc); err != nil {
		return accessChainShape{}, fmt.Errorf("reading nft's chain listing: %w", err)
	}
	var shape accessChainShape
	found := false
	for _, item := range doc.Nftables {
		if raw, ok := item["chain"]; ok {
			var c struct {
				Hook   string `json:"hook"`
				Prio   int    `json:"prio"`
				Policy string `json:"policy"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return accessChainShape{}, fmt.Errorf("reading nft's chain listing: %w", err)
			}
			shape.Hook, shape.Priority, shape.Policy, found = c.Hook, c.Prio, c.Policy, true
		}
		if raw, ok := item["rule"]; ok {
			shape.Rules++
			var r struct {
				Expr []map[string]json.RawMessage `json:"expr"`
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				continue
			}
			var packets int64 = -1
			drop := false
			for _, e := range r.Expr {
				if c, ok := e["counter"]; ok {
					var counter struct {
						Packets int64 `json:"packets"`
					}
					if json.Unmarshal(c, &counter) == nil {
						packets = counter.Packets
					}
				}
				if _, ok := e["drop"]; ok {
					drop = true
				}
			}
			if drop && packets >= 0 {
				shape.Dropped = packets
			}
		}
	}
	if !found {
		return accessChainShape{}, errors.New("nft listed no chain")
	}
	return shape, nil
}
