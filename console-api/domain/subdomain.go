package domain

import "github.com/getkipper/kipper/controller/pkg/hostnames"

// SubdomainFor returns the conventional hostname for a service prefix and
// cluster domain. It delegates to the shared hostnames package so the
// convention lives in exactly one place across the CLI, controllers, and
// gateway.
func SubdomainFor(prefix, clusterDomain string) string {
	return hostnames.SubdomainFor(prefix, clusterDomain)
}

// ConsoleHost returns the console hostname, honoring an admin override when
// present. Pass the value of CONSOLE_DOMAIN as override (empty when unset).
func ConsoleHost(override, clusterDomain string) string {
	return hostnames.HostFor("console", override, clusterDomain)
}

// ConsoleAPIHost returns the console-api hostname, honoring an admin override
// when present. Pass the value of CONSOLE_API_DOMAIN as override.
func ConsoleAPIHost(override, clusterDomain string) string {
	return hostnames.HostFor("console-api", override, clusterDomain)
}

// DexHost returns the Dex hostname, honoring an admin override when present.
// Pass the value of DEX_DOMAIN as override.
func DexHost(override, clusterDomain string) string {
	return hostnames.HostFor("dex", override, clusterDomain)
}
