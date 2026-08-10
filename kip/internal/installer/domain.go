package installer

import "github.com/getkipper/kipper/controller/pkg/hostnames"

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
