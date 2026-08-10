// Package giturl canonicalizes git remote authorities so a credential can be
// bound to exactly one host and a crafted URL cannot smuggle a different host
// past that binding.
package giturl

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// CanonicalAuthority returns the canonical authority of a git remote (see
// Canonical). It is used where only the authority is needed (credential
// creation, config-time URL validation).
func CanonicalAuthority(raw string) (string, error) {
	authority, _, err := Canonical(raw)
	return authority, err
}

// Canonical parses a git remote (a full https URL or a bare host, which is
// assumed https) and returns both its canonical authority (the lowercased host,
// bracketed if it is an IPv6 literal, with a non-default port appended) and a
// canonical https URL rebuilt with that authority. Cloning from the canonical
// URL makes git derive exactly this authority for its credential request, so the
// credential-helper config scope, the expected-host env, and git's `host=`
// attribute all match on an exact string comparison. authority is "" when raw is
// a bare host with no path.
//
// It requires https and rejects userinfo, fragments, whitespace, and control
// characters, so a URL like https://user@evil.example/#github.com cannot bind a
// token meant for another host or inject into the credential helper.
func Canonical(raw string) (authority, canonicalURL string, err error) {
	if raw == "" {
		return "", "", fmt.Errorf("empty git url")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return "", "", fmt.Errorf("git url contains whitespace")
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return "", "", fmt.Errorf("git url contains a control character")
		}
	}
	// A bare host (no scheme) is taken as https; anything with a scheme must be
	// https, so http:// is rejected below rather than silently upgraded.
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("parsing git url: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", "", fmt.Errorf("git url must use https, got %q", u.Scheme)
	}
	if u.User != nil {
		return "", "", fmt.Errorf("git url must not contain userinfo")
	}
	if u.Fragment != "" {
		return "", "", fmt.Errorf("git url must not contain a fragment")
	}
	// A trailing dot is an absolute-FQDN marker, DNS-equivalent to the name
	// without it; drop it so github.com. and github.com canonicalize the same
	// and a credential's host binds regardless of which spelling was configured.
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return "", "", fmt.Errorf("git url has no host")
	}
	port := u.Port()

	switch {
	case port != "" && port != "443":
		// net.JoinHostPort brackets an IPv6 host, leaves a DNS host as host:port.
		authority = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"): // IPv6 literal, default port
		authority = "[" + host + "]"
	default:
		authority = host
	}

	// Rebuild the URL over the canonical authority so git's request matches.
	u.Scheme = "https"
	u.Host = authority
	u.User = nil
	u.Fragment = ""
	return authority, u.String(), nil
}
