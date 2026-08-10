package domain

import "strings"

// DomainClass categorises a route host relative to a cluster's base domain, so
// migration can decide whether the host follows the app to the target (a custom
// domain), stays derived on both clusters (a platform subdomain), or is handled
// by the shared gateway (a kipper.run subdomain).
type DomainClass string

const (
	// DomainClassGateway is a free *.kipper.run subdomain served by the shared
	// kipper.run gateway rather than a cluster ingress.
	DomainClassGateway DomainClass = "gateway"
	// DomainClassPlatform is the cluster's own auto-derived subdomain for the
	// app. It is Kipper's to hand out, so each cluster derives its own.
	DomainClassPlatform DomainClass = "platform"
	// DomainClassCustom is a domain the user owns and pointed at the app.
	DomainClassCustom DomainClass = "custom"
)

// NormalizeHost lowercases a hostname and drops a single trailing root dot so
// DNS-equivalent names compare equal.
func NormalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// IsGatewayHost reports whether host is a strict *.kipper.run subdomain. The
// check is on the suffix only, so a literal "kipper.run" or "fakekipper.run"
// is rejected.
func IsGatewayHost(host string) bool {
	return strings.HasSuffix(NormalizeHost(host), ".kipper.run")
}

// AppRoutePrefix returns the service prefix Kipper derives an app's host from:
// the app name, suffixed with -<env> when the app's namespace carries an
// environment label. This is the prefix fed to SubdomainFor, kept here so the
// convention has one definition across the route handler and migration.
func AppRoutePrefix(appName, env string) string {
	if env != "" {
		return appName + "-" + env
	}
	return appName
}

// ClassifyHost categorises host against the host the cluster would auto-assign
// this app, derivedHost (= SubdomainFor(AppRoutePrefix(app, env), sourceBase)).
// An exact, normalised match means the host is the platform subdomain; anything
// else the user owns. Suffix membership is deliberately not used: when the base
// domain is the user's own domain, a real custom domain (app.hrportal.eu) is a
// suffix of the base yet must be treated as custom.
func ClassifyHost(host, derivedHost string) DomainClass {
	switch {
	case IsGatewayHost(host):
		return DomainClassGateway
	case NormalizeHost(host) == NormalizeHost(derivedHost):
		return DomainClassPlatform
	default:
		return DomainClassCustom
	}
}

// TargetEquivalent returns the host the target cluster would auto-assign an app
// with the given derived prefix, i.e. the coexist URL a platform subdomain lands
// on after migration. Returns "" when prefix or targetBase is empty.
func TargetEquivalent(prefix, targetBase string) string {
	if prefix == "" || targetBase == "" {
		return ""
	}
	return SubdomainFor(prefix, targetBase)
}
