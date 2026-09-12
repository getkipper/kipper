package installer

import (
	"fmt"
	"net"
	"strings"

	"github.com/getkipper/kipper/controller/pkg/hostnames"
)

// SubdomainFor returns the conventional hostname for a service prefix and
// cluster domain. It delegates to the shared hostnames package so the
// convention lives in exactly one place.
func SubdomainFor(prefix, domain string) string {
	return hostnames.SubdomainFor(prefix, domain)
}

// UIDomainFor returns the value for UI_DOMAIN: the base domain a post-login
// redirect or an SSO-code mint may target, beyond the console host. On a custom
// domain the console (console.example.com) and service UIs
// (mailhog-blog-test.example.com) are real subdomains of the cluster domain, so
// returning it lets one login reach every service UI.
//
// Free *.kipper.run clusters flatten every host to a single label under
// kipper.run (console--abc.kipper.run, mailhog-blog-test--abc.kipper.run), which
// are siblings rather than subdomains. Treating the apex as the allowed base
// would make every other tenant's host a valid redirect target, so return empty
// and leave per-host service-UI SSO off on those clusters until a
// cluster-label-scoped rule enables it.
func UIDomainFor(domain string) string {
	return hostnames.UIDomainFor(domain)
}

// CheckCustomDomainDNS looks up the three platform hosts for a custom-domain
// install (console, console-api, dex, honouring per-host overrides). Missing
// records become warnings — the install still proceeds, because DNS may only
// be minutes from propagating. Free *.kipper.run names are skipped: the
// gateway owns that DNS. lookup is injected so tests can stub resolution;
// pass nil to use net.LookupHost.
func CheckCustomDomainDNS(domain, consoleOverride, consoleAPIOverride, dexOverride string, lookup func(string) ([]string, error)) []string {
	domain = NormaliseDomain(domain)
	if domain == "" || claimsGatewayName(domain) {
		return nil
	}
	if lookup == nil {
		lookup = net.LookupHost
	}

	hosts := []string{
		pickHost(consoleOverride, "console", domain),
		pickHost(consoleAPIOverride, "console-api", domain),
		pickHost(dexOverride, "dex", domain),
	}

	var missing []string
	for _, host := range hosts {
		if _, err := lookup(host); err != nil {
			missing = append(missing, host)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	return []string{fmt.Sprintf(
		"%s %s not resolve yet. cert-manager needs those hosts to point at the server before certificates can issue. A wildcard A record (*.%s → the server) covers the platform hosts and every app you deploy later.",
		strings.Join(missing, ", "),
		dnsVerb(len(missing)),
		domain,
	)}
}

func dnsVerb(n int) string {
	if n == 1 {
		return "does"
	}
	return "do"
}
