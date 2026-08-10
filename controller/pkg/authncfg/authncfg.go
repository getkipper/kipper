// Package authncfg renders and fingerprints the kube-apiserver structured
// authentication configuration and validates the host and email inputs that
// flow into it. It is shared by kip (which writes the file over SSH) and
// console-api (which gates a domain cutover on the apiserver's active-config
// hash), so both sides render byte-identical content and compute the same
// hash.
package authncfg

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"

	"github.com/getkipper/kipper/controller/pkg/hostnames"
)

// UsernamePrefix and GroupsPrefix are prepended to every authenticated
// username and group. Only prefixed subjects can match an OIDC identity,
// which is what stops a compromised issuer or claim mapping from asserting a
// built-in name like system:masters. RBAC subjects carry the same prefixes.
const (
	UsernamePrefix = "oidc:"
	GroupsPrefix   = "oidc:"
)

// dnsNameRe matches an RFC 1123 DNS hostname. Every dex host is interpolated
// into shell (sed, curl, echo) and kubectl commands, so a value carrying a
// shell metacharacter must never reach a command string.
var dnsNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// emailRe is a conservative address check for the admin-binding subject
// inputs, which are also interpolated into kubectl commands.
var emailRe = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// ValidateDexHost rejects any host that is not a plain RFC 1123 DNS name of
// at most 253 characters. It is the injection gate for every host value that
// reaches a remote command.
func ValidateDexHost(host string) error {
	if len(host) == 0 || len(host) > 253 || !dnsNameRe.MatchString(host) {
		return fmt.Errorf("invalid dex host %q: must be a DNS hostname", host)
	}
	return nil
}

// ValidateAdminEmail rejects any email that is not a conservative address, so
// it is safe to interpolate into a kubectl subject argument.
func ValidateAdminEmail(email string) error {
	if len(email) == 0 || len(email) > 254 || !emailRe.MatchString(email) {
		return fmt.Errorf("invalid admin email %q", email)
	}
	return nil
}

// Render builds the AuthenticationConfiguration for one or more Dex issuer
// hosts. Multiple hosts produce a union authenticator set — the primitive a
// domain cutover uses to trust the old and new issuer at once and drain
// sessions across the flip. Callers must ValidateDexHost every host first.
// Render builds the API server's AuthenticationConfiguration for dexHosts.
//
// A gateway-fronted host is given caPEM as its issuer's trust anchor. It has to
// be: the cluster serves that host with its own hop certificate, which no public
// authority signed, so the API server verifies it against the cluster's CA or
// not at all. A custom domain is left without one, because it carries a real
// WebPKI certificate and pinning it to a cluster CA would break the moment
// cert-manager renewed it.
//
// The anchor is per issuer rather than global on purpose. A cutover config
// legitimately carries both kinds at once, and Go replaces the system pool when
// a root pool is set, so one global anchor would stop every WebPKI issuer
// verifying.
func Render(caPEM string, dexHosts ...string) string {
	var b strings.Builder
	b.WriteString("apiVersion: apiserver.config.k8s.io/v1\nkind: AuthenticationConfiguration\njwt:\n")
	for _, host := range dexHosts {
		fmt.Fprintf(&b, `  - issuer:
      url: https://%s/dex
`, host)
		if anchor := issuerAnchor(host, caPEM); anchor != "" {
			b.WriteString(anchor)
		}
		fmt.Fprintf(&b, `      audiences:
        - kipper-cli
      audienceMatchPolicy: MatchAny
    claimMappings:
      username:
        claim: email
        prefix: "%s"
      groups:
        claim: groups
        prefix: "%s"
      uid:
        claim: sub
    claimValidationRules:
      - expression: 'has(claims.email_verified) && claims.email_verified == true'
        message: 'email address must be verified by the identity provider'
`, UsernamePrefix, GroupsPrefix)
	}
	return b.String()
}

// issuerAnchor renders the inline certificateAuthority block for a host that
// needs one, and nothing at all for a host that does not.
func issuerAnchor(host, caPEM string) string {
	if caPEM == "" || !hostnames.IsKipperRun(host) {
		return ""
	}
	var b strings.Builder
	b.WriteString("      certificateAuthority: |\n")
	for _, line := range strings.Split(strings.TrimRight(caPEM, "\n"), "\n") {
		b.WriteString("        " + line + "\n")
	}
	return b.String()
}

// Hash is the fingerprint the apiserver publishes for its active
// authentication configuration (last_config_info metric): sha256 of the exact
// file bytes. Matching it proves the running apiserver loaded this content.
func Hash(content string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(content)))
}

// HostsFor returns the canonical union issuer-host order for a transition
// from one Dex host to another: [from, to], deduped. A move that keeps the
// Dex host (an override-only domain change) degenerates to a single entry, so
// staging becomes a no-op.
func HostsFor(from, to string) []string {
	if from == to || to == "" {
		return []string{from}
	}
	if from == "" {
		return []string{to}
	}
	return []string{from, to}
}

// activeHashRe extracts the hash label value from the apiserver metric line
// `apiserver_authentication_config_controller_last_config_info{...,hash="sha256:...",...} 1`.
var activeHashRe = regexp.MustCompile(`apiserver_authentication_config_controller_last_config_info\{[^}]*\bhash="(sha256:[0-9a-f]+)"`)

// ActiveHashMatches reports whether the apiserver's /metrics output shows the
// active authentication config hash equal to want. It parses the hash label
// exactly rather than substring-matching, so a hash appearing in an unrelated
// label or series can never produce a false match.
func ActiveHashMatches(metrics, want string) bool {
	for _, m := range activeHashRe.FindAllStringSubmatch(metrics, -1) {
		if m[1] == want {
			return true
		}
	}
	return false
}
