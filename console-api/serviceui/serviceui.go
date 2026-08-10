// Package serviceui is the single source of truth for which service
// types expose a browseable web UI and what hostname that UI is served
// on. The service reconciler, the share endpoints, and the REST info
// handler all resolve through here, so the routing, the minted link, and
// the connection panel can never disagree about the same URL.
package serviceui

import (
	"fmt"

	"github.com/getkipper/kipper/console-api/domain"
)

// Browseable reports whether a service type ships a web UI worth an
// Ingress. Today: mailhog (inbox at port 8025). Future entries must add
// their `ui` block to the reconciler's service catalog in the same
// change — the catalog consistency test enforces the pairing.
func Browseable(serviceType string) bool {
	return serviceType == "mailhog"
}

// Hostname returns the per-service UI hostname Kipper exposes, or ""
// when the cluster has no domain configured. The name-namespace prefix
// flattens with a hyphen so the result stays a single label under
// *.kipper.run — otherwise wildcard DNS and TLS would not cover the
// host. On custom-domain clusters the prefix becomes a real subdomain.
func Hostname(name, namespace, clusterDomain string) string {
	if clusterDomain == "" {
		return ""
	}
	return domain.SubdomainFor(fmt.Sprintf("%s-%s", name, namespace), clusterDomain)
}
