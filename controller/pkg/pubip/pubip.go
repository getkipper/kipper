// Package pubip decides whether an address is a routable public IP that the
// kipper.run gateway will register a cluster at. The gateway enforces it on
// /register, and kip applies it before recording an address a cluster will later
// try to register with — one definition, so the CLI can never persist an address
// the gateway would refuse.
//
// This is deliberately separate from netguard, which guards OUTBOUND requests to
// user-supplied hosts and is stricter (it also refuses documentation and
// reserved ranges). The two answer different questions: "may we dial this?" and
// "can the internet reach a cluster here?".
package pubip

import "net"

// nonGlobalCIDRs are special-purpose ranges that net.IP's own predicates miss.
// CGNAT and benchmarking are non-global IPv4. 0.0.0.0/8 ("this network") can
// alias the local host on some stacks. ::/96 (IPv4-compatible IPv6, deprecated)
// and 64:ff9b::/96 (the NAT64 well-known prefix) can smuggle a private or
// loopback IPv4 target past the IPv6 predicates.
var nonGlobalCIDRs = func() []*net.IPNet {
	var nets []*net.IPNet
	for _, c := range []string{
		"100.64.0.0/10", "198.18.0.0/15", "0.0.0.0/8",
		"::/96", "64:ff9b::/96",
	} {
		if _, n, err := net.ParseCIDR(c); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}()

// IsPublic reports whether s is a routable public IP. It rejects loopback,
// private, link-local (including cloud metadata at 169.254.169.254), CGNAT,
// benchmarking, this-network, IPv4-compatible and NAT64 IPv6, unspecified, and
// multicast addresses, so a registration cannot turn the gateway proxy into an
// SSRF pivot to internal services.
func IsPublic(s string) bool {
	ip := net.ParseIP(s)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, n := range nonGlobalCIDRs {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// SameAddress reports whether two textual addresses denote the same IP. An
// address has more than one valid spelling (2001:db8::1 and
// 2001:0db8:0:0:0:0:0:1 are one address), so anything comparing addresses has to
// compare what they parse to rather than how they were written.
func SameAddress(a, b string) bool {
	parsed := net.ParseIP(a)
	return parsed != nil && parsed.Equal(net.ParseIP(b))
}
