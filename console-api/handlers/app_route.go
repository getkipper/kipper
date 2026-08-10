package handlers

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
	"github.com/getkipper/kipper/console-api/domain"
)

// lookupHost is indirected so tests can stub DNS resolution.
var lookupHost = net.LookupHost

type routeRequest struct {
	Host         string   `json:"host,omitempty"`          // explicit host, or empty to auto-generate
	Path         string   `json:"path,omitempty"`          // path prefix (default "/")
	RedirectFrom []string `json:"redirect_from,omitempty"` // hostnames that 301 to the route host
}

type routeResponse struct {
	Host         string      `json:"host"`
	Path         string      `json:"path"`
	RedirectFrom []string    `json:"redirect_from"`
	URL          string      `json:"url"`
	Enabled      bool        `json:"enabled"`
	Health       RouteHealth `json:"health"`
}

// GetRoute returns the current route for an app.
// GET /api/v1/projects/{name}/apps/{app}/route
func (a *Apps) GetRoute(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: appName}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, routeResponse{})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	resp := routeResponse{}
	if appCR.Spec.Route != nil {
		resp.Enabled = true
		resp.Path = appCR.Spec.Route.Path
		if resp.Path == "" {
			resp.Path = "/"
		}
		resp.RedirectFrom = appCR.Spec.Route.RedirectFrom
		// Resolve the effective host the same way SetRoute and the
		// Routes list do, so an App with Spec.Route set but Host empty
		// (created via direct CR apply or pre-derive write) still
		// shows its implicit host, URL and health in the Settings tab.
		resp.Host = a.resolveRouteHost(ctx, project, appName, appCR.Spec.Route.Host)
		if resp.Host != "" {
			resp.URL = "https://" + resp.Host + resp.Path
			resp.Health = routeHealth(ctx, a.Client, project, appName, resp.Host)
		}
	}

	respondJSON(w, http.StatusOK, resp)
}

// resolveRouteHost returns the effective host for an app's route. An
// explicit Spec.Route.Host wins. When the spec leaves it empty and the
// cluster has a Domain configured, the host is derived the same way the
// App reconciler does: app[-env].clusterDomain, joined by a hyphen for
// kipper.run subdomains and by a dot for real custom domains.
func (a *Apps) resolveRouteHost(ctx context.Context, namespace, appName, specHost string) string {
	if specHost != "" {
		return specHost
	}
	if a.Domain == "" {
		return ""
	}
	env := ""
	if ns, err := a.Client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{}); err == nil {
		env = ns.Labels["kipper.run/environment"]
	}
	prefix := controllers.AppHostPrefix(appName, env)
	return domain.SubdomainFor(prefix, a.Domain)
}

// SetRoute creates or updates the route for an app. If host is empty,
// an auto-generated kipper.run subdomain is used.
// PUT /api/v1/projects/{name}/apps/{app}/route
func (a *Apps) SetRoute(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	var req routeRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: appName}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found", appName))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	host := a.resolveRouteHost(ctx, project, appName, req.Host)

	path := req.Path
	if path == "" {
		path = "/"
	}

	// The console sends the full route state, so the list replaces what is
	// stored, the same way host and path do. Entries are only tidied here;
	// the reconciler decides whether each host can actually be served. The
	// count is checked up front because the CRD rejects a longer list and
	// that would otherwise surface as an opaque 500.
	redirectFrom := make([]string, 0, len(req.RedirectFrom))
	for _, h := range req.RedirectFrom {
		if h = strings.TrimSpace(h); h != "" {
			redirectFrom = append(redirectFrom, h)
		}
	}
	if len(redirectFrom) == 0 {
		redirectFrom = nil
	}
	if len(redirectFrom) > kipperv1.MaxRedirectFromHosts {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("at most %d redirect domains are supported per route", kipperv1.MaxRedirectFromHosts))
		return
	}

	if appCR.Spec.Route == nil {
		appCR.Spec.Route = &kipperv1.AppRoute{}
	}
	appCR.Spec.Route.Host = host
	appCR.Spec.Route.Path = path
	appCR.Spec.Route.RedirectFrom = redirectFrom

	if err := a.CRClient.Update(ctx, &appCR); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update route")
		return
	}

	url := ""
	if host != "" {
		url = "https://" + host + path
	}

	resp := routeResponse{
		Host:         host,
		Path:         path,
		RedirectFrom: redirectFrom,
		URL:          url,
		Enabled:      true,
	}
	if host != "" {
		resp.Health = routeHealth(ctx, a.Client, project, appName, host)
	}

	respondJSON(w, http.StatusOK, resp)
}

// DNSStatus values returned by GetRouteDNSStatus.
const (
	dnsStatusOK         = "ok"
	dnsStatusMismatch   = "mismatch"
	dnsStatusUnresolved = "unresolved"
	// dnsStatusGateway: the host is a *.kipper.run subdomain served by the
	// shared kipper.run gateway. Traffic resolves to the gateway, not to
	// the cluster nodes, so node-IP comparison would be a false negative.
	dnsStatusGateway = "gateway"
	// dnsStatusWildcard: the host is a subdomain of the operator's own
	// cluster domain (CLUSTER_DOMAIN, e.g. example.com). Coverage comes
	// from the wildcard A record the operator set up at install time, so
	// no per-app DNS step is needed. The user can still force a lookup
	// via ?verify=true.
	dnsStatusWildcard = "wildcard"
	dnsStatusDisabled = "disabled"
)

// kipperRunGatewayDomain is the suffix the shared kipper.run gateway
// matches. Traffic to anything ending in .kipper.run is forwarded by that
// gateway, regardless of what CLUSTER_DOMAIN this cluster was installed
// with.
const kipperRunGatewayDomain = "kipper.run"

type dnsStatusResponse struct {
	Hostname    string   `json:"hostname"`
	Status      string   `json:"status"`
	Message     string   `json:"message"`
	ExpectedIPs []string `json:"expected_ips"`
	ResolvedIPs []string `json:"resolved_ips"`
}

// GetRouteDNSStatus tells the user whether the public route's hostname is
// reaching this cluster. Three categories:
//
//   - *.kipper.run: routed through the shared kipper.run gateway, no
//     per-app DNS to verify. Reported as "gateway".
//   - Subdomain of the operator's own CLUSTER_DOMAIN: covered by the
//     wildcard A record set up at install time. Reported as "wildcard"
//     unless the caller passes ?verify=true to force a lookup.
//   - Anything else: full net.LookupHost against the cluster node IPs.
//
// GET /api/v1/projects/{name}/apps/{app}/route/dns-status
// GET .../route/dns-status?verify=true   (force lookup for wildcard)
func (a *Apps) GetRouteDNSStatus(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")
	forceVerify := r.URL.Query().Get("verify") == "true"

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: appName}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found", appName))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	if appCR.Spec.Route == nil {
		respondJSON(w, http.StatusOK, dnsStatusResponse{
			Status:  dnsStatusDisabled,
			Message: "No public route configured for this app.",
		})
		return
	}

	// Resolve the effective host the same way GetRoute and the Routes
	// list do, so DNS status stays consistent for routes whose host is
	// derived from CLUSTER_DOMAIN.
	host := a.resolveRouteHost(ctx, project, appName, appCR.Spec.Route.Host)
	if host == "" {
		respondJSON(w, http.StatusOK, dnsStatusResponse{
			Status:  dnsStatusDisabled,
			Message: "No public route configured for this app.",
		})
		return
	}

	if isKipperRunGateway(host) {
		respondJSON(w, http.StatusOK, dnsStatusResponse{
			Hostname: host,
			Status:   dnsStatusGateway,
			Message:  "Free kipper.run subdomain. Traffic is routed through the shared kipper.run gateway, no DNS setup needed.",
		})
		return
	}

	if !forceVerify && a.Domain != "" && isClusterDomainSubdomain(host, a.Domain) {
		respondJSON(w, http.StatusOK, dnsStatusResponse{
			Hostname: host,
			Status:   dnsStatusWildcard,
			Message:  fmt.Sprintf("Covered by your wildcard A record for *.%s. No per-app DNS setup needed.", a.Domain),
		})
		return
	}

	expected, err := clusterIngressIPs(ctx, a.Client)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to read cluster nodes")
		return
	}

	resolved, lookupErr := lookupHost(host)
	resp := dnsStatusResponse{
		Hostname:    host,
		ExpectedIPs: expected,
		ResolvedIPs: resolved,
	}

	switch {
	case lookupErr != nil || len(resolved) == 0:
		resp.Status = dnsStatusUnresolved
		switch {
		case len(expected) == 1:
			resp.Message = fmt.Sprintf("%s does not resolve. At your DNS provider, add an A record for %s pointing to %s.", host, host, expected[0])
		case len(expected) > 1:
			resp.Message = fmt.Sprintf("%s does not resolve. At your DNS provider, add an A record for %s pointing to one of: %s.", host, host, strings.Join(expected, ", "))
		default:
			resp.Message = fmt.Sprintf("%s does not resolve.", host)
		}
	case ipsIntersect(resolved, expected):
		resp.Status = dnsStatusOK
		resp.Message = fmt.Sprintf("%s resolves to this cluster.", host)
	default:
		resp.Status = dnsStatusMismatch
		if len(expected) == 1 {
			resp.Message = fmt.Sprintf("%s resolves to %s. At your DNS provider, change the A record to point to %s.", host, strings.Join(resolved, ", "), expected[0])
		} else {
			resp.Message = fmt.Sprintf("%s resolves to %s. At your DNS provider, change the A record to point to one of: %s.", host, strings.Join(resolved, ", "), strings.Join(expected, ", "))
		}
	}

	respondJSON(w, http.StatusOK, resp)
}

// isKipperRunGateway is true for any *.kipper.run host. The check looks
// at the suffix only — matches must be strict subdomains, so a literal
// "kipper.run" or "fakekipper.run" is rejected.
func isKipperRunGateway(host string) bool {
	return strings.HasSuffix(host, "."+kipperRunGatewayDomain)
}

// isClusterDomainSubdomain is true when the host is a subdomain of the
// operator's CLUSTER_DOMAIN (the value passed to `kip install --domain`).
// kipper.run-style auto-generated names join with a double dash, real
// custom domains join with a dot — both forms count as covered by the
// wildcard.
func isClusterDomainSubdomain(host, clusterDomain string) bool {
	if host == clusterDomain {
		return true
	}
	return strings.HasSuffix(host, "."+clusterDomain) || strings.HasSuffix(host, "--"+clusterDomain)
}

func ipsIntersect(a, b []string) bool {
	set := make(map[string]struct{}, len(b))
	for _, ip := range b {
		set[ip] = struct{}{}
	}
	for _, ip := range a {
		if _, ok := set[ip]; ok {
			return true
		}
	}
	return false
}

// DeleteRoute removes the route from an app, deleting its Ingress.
// DELETE /api/v1/projects/{name}/apps/{app}/route
func (a *Apps) DeleteRoute(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: appName}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, routeResponse{})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	appCR.Spec.Route = nil

	if err := a.CRClient.Update(ctx, &appCR); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update app")
		return
	}

	respondJSON(w, http.StatusOK, routeResponse{})
}
