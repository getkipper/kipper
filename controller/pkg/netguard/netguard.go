// Package netguard blocks server-side request forgery on outbound HTTP calls
// whose target host comes from user input (a configured git URL, a registry
// server). It refuses to connect to any non-public address, checked at the
// moment of connection against the resolved IP, so a hostname that resolves to
// an internal address — including via DNS rebinding — cannot reach cluster
// services or cloud metadata.
package netguard

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// nonGlobalCIDRs are IANA special-purpose ranges that net.IP's predicates miss
// but which are not globally reachable, so a public-only guard must reject
// them. This covers CGNAT, benchmarking, this-network, the IETF protocol and
// TEST-NET documentation blocks, reserved/future space, and the IPv6
// IPv4-compatible, well-known NAT64, local-use NAT64, and documentation
// prefixes — any of which could otherwise smuggle a private or internal target
// past the address-family predicates.
var nonGlobalCIDRs = func() []*net.IPNet {
	var nets []*net.IPNet
	for _, c := range []string{
		// IPv4
		"0.0.0.0/8",       // this-network
		"100.64.0.0/10",   // CGNAT (RFC 6598)
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1 (documentation)
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // TEST-NET-2 (documentation)
		"203.0.113.0/24",  // TEST-NET-3 (documentation)
		"240.0.0.0/4",     // reserved + broadcast
		// IPv6
		"::/96",          // IPv4-compatible (deprecated)
		"64:ff9b::/96",   // well-known NAT64 (RFC 6052)
		"64:ff9b:1::/48", // local-use NAT64 (RFC 8215)
		"2001:db8::/32",  // documentation
	} {
		if _, n, err := net.ParseCIDR(c); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}()

// IsPublicIP reports whether s is a routable public IP. It rejects loopback,
// private, link-local (including cloud metadata at 169.254.169.254), CGNAT,
// benchmarking, this-network, IPv4-compatible and NAT64 IPv6, unspecified, and
// multicast addresses.
func IsPublicIP(s string) bool {
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

// publicOnlyControl is a net.Dialer Control hook that runs after resolution,
// with the concrete IP:port about to be dialed, and refuses a non-public
// address. Because it inspects the resolved address, it defeats DNS rebinding.
func publicOnlyControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if !IsPublicIP(host) {
		return fmt.Errorf("refusing to connect to non-public address %s", host)
	}
	return nil
}

// Client returns an HTTP client for outbound requests to a user-supplied host.
// It never follows redirects (a 3xx must not bounce a credential-bearing
// request to a different host) and never connects to a non-public address.
func Client(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout, Control: publicOnlyControl}
	return &http.Client{
		Timeout:       timeout,
		Transport:     &http.Transport{DialContext: dialer.DialContext},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Dialer returns a dialer that refuses to connect to a non-public address, for
// a caller that needs the guard but its own redirect policy.
//
// Client above never follows a redirect at all, which is right for a probe that
// carries a credential to one address. A caller that must follow a hop — a git
// host answering a reference advertisement with its canonical URL — needs the
// same dial guard with a redirect rule of its own, and building one by hand
// would put the SSRF boundary in two places.
func Dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, Control: publicOnlyControl}
}
