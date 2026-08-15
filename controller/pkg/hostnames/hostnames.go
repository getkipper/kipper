// Package hostnames is the single source of truth for how Kipper derives the
// hostnames it serves the console, console-api, Dex, and per-app UIs on. Every
// module (kip CLI, console-api reconcilers, installer, gateway) derives hosts
// from here so the convention can never drift between them — drift between four
// copies of this logic is exactly what broke post-install reconfiguration.
package hostnames

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

// DerivedRouteSeparator joins a service prefix to a cluster label for free
// *.kipper.run subdomains. It is a double dash on purpose: the gateway forbids
// "--" in a registered cluster label, so a derived host (console--acme.kipper.run)
// can never collide with another cluster's registered name, which would let it
// shadow that cluster's console or login page.
const DerivedRouteSeparator = "--"

const kipperRunSuffix = ".kipper.run"

// GatewayDomain is the apex the free subdomains live under. It is the parent of
// every gateway-fronted host, so it is what a hop certificate's wildcard covers
// and what tells a kipper.run host from a custom domain.
const GatewayDomain = "kipper.run"

// MaxLabelLength is the DNS single-label length limit. A registrable cluster
// label must fit in one label because a *.kipper.run host is a single DNS label.
const MaxLabelLength = 63

// LabelPattern matches one DNS label: 1-63 lowercase-alphanumeric characters
// with internal hyphens. The gateway registration guard and the ClusterIdentity
// CRD both accept exactly this shape, so the registrable-label rule has one
// definition instead of a copy per component.
var LabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ReservedLabels are service and platform names that must never be registered
// as a standalone cluster label, so nobody can squat a login or platform
// hostname. Enforced by both the gateway registration guard and the
// ClusterIdentity CRD validation.
//
// The rule for what belongs here: a name that, standing alone under the gateway
// domain, reads as the platform's own rather than as somebody's cluster.
// login.kipper.run and status.kipper.run read that way; acme.kipper.run and
// lab.kipper.run do not, and stay registrable. Every entry costs an operator a
// name they might legitimately want, which is what keeps the list to names that
// meet the rule.
//
// The ClusterIdentity CRD carries the same list as a CEL literal, because a CEL
// rule cannot call into Go. A lockstep test in console-api compares the two, so
// an entry added here without the CRD regenerated fails the suite rather than
// leaving the API server accepting what the gateway refuses.
var ReservedLabels = map[string]bool{
	// Platform surface.
	"console": true, "console-api": true, "dex": true, "api": true,
	"www": true, "admin": true, "kipper": true, "register": true, "health": true,
	"status": true, "gateway": true,
	// Identity and sign-in, where impersonation pays best.
	"login": true, "logout": true, "auth": true, "sso": true,
	"oauth": true, "oidc": true, "account": true, "accounts": true, "signup": true,
	// Project presence, which a link is read against.
	"docs": true, "support": true, "help": true, "security": true,
	"billing": true, "blog": true,
	// Mail and nameserver conventions. A registration only steers HTTP at the
	// gateway, so holding one of these receives no mail and answers no query;
	// what it buys is a hostname that looks like the platform's infrastructure.
	"mail": true, "smtp": true, "mx": true, "ns1": true, "ns2": true, "webmail": true,
}

// LabelForIP returns the cluster label derived from a server's IPv4 address,
// which is the default name a free-tier install claims. Dots become hyphens
// because a *.kipper.run host is a single DNS label.
func LabelForIP(ip string) string {
	return strings.ReplaceAll(ip, ".", "-")
}

// IPShapedLabel reports whether a label is the dashed form of an IPv4 address.
// Such a label is registrable only by the address it names: otherwise anyone
// could hold the default name of a server they do not run, and pre-empt whoever
// installs on that address later.
//
// The answer is decided by round-tripping through LabelForIP rather than by a
// pattern, so the set this claims is exactly the set of default names and cannot
// drift from them. A label no address renders to is an ordinary name, free for
// anyone: 999-0-0-1 is nobody's default, so reserving it would cost a name and
// buy nothing.
func IPShapedLabel(label string) bool {
	ip := net.ParseIP(strings.ReplaceAll(label, "-", "."))
	return ip != nil && ip.To4() != nil && LabelForIP(ip.String()) == label
}

// ValidateClusterLabel reports why a label may not be registered as a cluster
// subdomain: it must match LabelPattern, must not contain DerivedRouteSeparator
// (reserved for derived per-cluster service routes), and must not be a
// ReservedLabel. Returns nil when the label is registrable.
func ValidateClusterLabel(label string) error {
	if !LabelPattern.MatchString(label) {
		return fmt.Errorf("label %q must be lowercase alphanumeric with optional hyphens, 1-%d characters", label, MaxLabelLength)
	}
	if strings.Contains(label, DerivedRouteSeparator) {
		return fmt.Errorf("label %q must not contain %q (reserved for per-cluster service routes)", label, DerivedRouteSeparator)
	}
	if ReservedLabels[label] {
		return fmt.Errorf("label %q is reserved", label)
	}
	return nil
}

// IsKipperRun reports whether a host is served through the kipper.run gateway.
// Gateway-fronted hosts get no cert-manager certificate (the HTTP-01 challenge
// would 404 at the gateway) and must fall through to the Traefik default store,
// which serves the pinned hop certificate the gateway verifies against.
func IsKipperRun(host string) bool {
	return strings.HasSuffix(host, kipperRunSuffix)
}

// SubdomainFor returns the hostname a service is served on for a cluster domain.
// On a free *.kipper.run domain the whole hostname is one DNS label, because the
// wildcard TLS certificate is single-level, so the prefix is glued to the
// cluster label with DerivedRouteSeparator (console--acme.kipper.run). On a
// custom domain the prefix is a normal dot-subdomain (console.example.com).
func SubdomainFor(prefix, domain string) string {
	if strings.HasSuffix(domain, kipperRunSuffix) {
		return prefix + DerivedRouteSeparator + domain
	}
	return prefix + "." + domain
}

// HostFor returns override when set, otherwise the conventional SubdomainFor
// host. This is the override-wins precedence used everywhere a per-service host
// can be pinned (a custom console domain, or a migrated host).
func HostFor(prefix, override, domain string) string {
	if override != "" {
		return override
	}
	return SubdomainFor(prefix, domain)
}

// UIDomainFor returns the value for UI_DOMAIN: the base domain a post-login
// redirect or an SSO-code mint may target, beyond the console host itself. On a
// custom domain the console and every service UI are real subdomains of the
// cluster domain, so returning it lets one login reach every service UI. On
// *.kipper.run every host is a sibling label under the shared apex, so treating
// the apex as the allowed base would make every other tenant's host a valid
// redirect target (an open-redirect/phishing vector) — return "" so only the
// console host is allowed, and per-host service-UI SSO stays off there until a
// cluster-label-scoped rule enables it.
func UIDomainFor(domain string) string {
	if domain == "kipper.run" || strings.HasSuffix(domain, kipperRunSuffix) {
		return ""
	}
	return domain
}
