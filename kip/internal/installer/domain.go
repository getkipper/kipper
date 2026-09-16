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
// be minutes from propagating. The lookup only asks whether each name
// resolves somewhere; it does not check that the answers point at the server.
// Free *.kipper.run names are skipped: the gateway owns that DNS. lookup is
// injected so tests can stub resolution; pass nil to use net.LookupHost.
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
	var lookupErr error
	for _, host := range hosts {
		if _, err := lookup(host); err != nil {
			missing = append(missing, host)
			if lookupErr == nil {
				lookupErr = err
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}

	msg := fmt.Sprintf("%s: DNS does not resolve yet", strings.Join(missing, ", "))
	if lookupErr != nil {
		msg += " (" + lookupErr.Error() + ")"
	}
	msg += ". cert-manager needs those names to resolve before certificates can issue."
	if wildcardCovers(domain, missing) {
		msg += fmt.Sprintf(" A wildcard A record (*.%s) covers these hosts and every app you deploy later. Where the zone already has a name at or above one of them, that host needs a record of its own.", domain)
	}
	return []string{msg}
}

// wildcardCovers reports whether a *.domain record would name every host in
// the set. Overrides are used verbatim, so a host outside the cluster domain
// falls outside that wildcard, and so does the domain apex: *.example.com
// answers for descendants of example.com, never for example.com itself. Pass
// the hosts the message names, so advice that holds for them survives an
// override that sits elsewhere and already resolves.
func wildcardCovers(domain string, hosts []string) bool {
	if domain == "" {
		return false
	}
	suffix := "." + domain
	for _, host := range hosts {
		if !strings.HasSuffix(host, suffix) {
			return false
		}
	}
	return true
}
