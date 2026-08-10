package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/domain"
)

// A route's hostname is a cluster-wide property, but an Ingress is unique only
// per (name, namespace), so nothing stopped a workload in one project from
// declaring another project's hostname and having Traefik route that host to
// it. Even a distinct path would capture a subset of the victim's traffic,
// because Traefik prefers the more specific PathPrefix.
//
// Ownership is therefore at the HOST level and belongs to the first namespace
// to claim it: the claim is a ConfigMap whose NAME is derived from the
// canonical host, so etcd's name uniqueness makes exactly one namespace win.
// A workload whose namespace does not own the host is refused; workloads in the
// OWNING namespace share the host freely (the path-based routing feature).
//
// A claim is STICKY: it is held by the owning namespace for that namespace's
// whole lifetime and is never explicitly released. Releasing a claim while its
// namespace is alive would be unsafe — a scan that looked "unused" could race a
// workload the same project is about to serve, and once freed another project
// could take the host mid-flight. Instead an abandoned host is reclaimed
// lazily: reserveHost takes over a claim whose owner namespace no longer exists.
// A live owner (including one whose namespace is still terminating) is never
// displaced, so two projects can never serve the host at once; the takeover only
// happens once the old project, and its Ingresses, are fully gone.
//
// The cost is that abandoning a host (deleting the app, changing its route, or
// deleting the project) leaves the claim ConfigMap behind until some project
// reserves that host again. Nothing reads the full claim set — every lookup is a
// Get by name — so these leftovers cost only a little etcd storage. Reaping
// claims whose owner namespace is gone is a possible future janitor, not a
// correctness requirement.
const (
	routeClaimNamespace = "kipper-system"
	routeClaimPrefix    = "route-claim-"

	routeClaimLabel          = "kipper.run/route-claim"
	routeOwnerNamespaceLabel = "kipper.run/route-owner-namespace"

	fnNamespaceLabel = "kipper.run/fn-namespace"
)

// platformIngressNamespaces are the namespaces whose Ingress rule hosts are the
// authoritative platform serving set. console and console-api live in
// kipper-system; Dex lives in its own namespace. Reading them covers
// CONSOLE_DOMAIN/DEX_DOMAIN overrides and both sides of a ClusterIdentity domain
// transition, so a tenant may not claim any host the platform actually serves.
var platformIngressNamespaces = []string{"kipper-system", "dex"}

// reservedPlatformPrefixes are the subdomains the platform serves under the
// cluster domain. A tenant route may never claim one, or it could hijack the
// console, its API, the identity provider, or dashboards.
var reservedPlatformPrefixes = []string{"console", "console-api", "dex", "grafana", "api", "registry"}

// canonicalRouteHost normalizes a hostname so case- and trailing-dot variants
// can't each hold a separate claim for the same effective host.
func canonicalRouteHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

// routeClaimName derives the claim object's name from the canonical host. The
// hash keeps it DNS-safe and fixed-length regardless of the host.
func routeClaimName(host string) string {
	sum := sha256.Sum256([]byte(canonicalRouteHost(host)))
	return routeClaimPrefix + hex.EncodeToString(sum[:])[:48]
}

// hostReservedForPlatform reports whether host is one the platform serves, so a
// tenant route may not claim it. It layers three guards:
//   - the conventional subdomains of the configured cluster domain (the standard
//     hosts, reserved even before their Ingress exists);
//   - every host the ClusterIdentity singleton declares — spec host overrides,
//     the active serving set, and both sides of an in-flight or rolled-back
//     domain transition — which is authoritative and covers hosts declared but
//     not yet serving;
//   - any host a live platform Ingress in kipper-system or dex already serves,
//     as defence in depth against drift between the declaration and reality.
func hostReservedForPlatform(ctx context.Context, reader client.Reader, host, clusterDomain string) (bool, error) {
	canon := canonicalRouteHost(host)
	if isReservedPlatformSubdomain(canon, clusterDomain) {
		return true, nil
	}
	declared, err := hostDeclaredByClusterIdentity(ctx, reader, canon)
	if err != nil {
		return false, err
	}
	if declared {
		return true, nil
	}
	for _, ns := range platformIngressNamespaces {
		var ingresses networkingv1.IngressList
		if err := reader.List(ctx, &ingresses, client.InNamespace(ns)); err != nil {
			return false, fmt.Errorf("listing platform ingresses in %s: %w", ns, err)
		}
		for i := range ingresses.Items {
			for _, rule := range ingresses.Items[i].Spec.Rules {
				if canonicalRouteHost(rule.Host) == canon {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// isReservedPlatformSubdomain matches the conventional platform subdomains of
// the configured cluster domain (and the apex).
func isReservedPlatformSubdomain(host, clusterDomain string) bool {
	if clusterDomain == "" {
		return false
	}
	host = canonicalRouteHost(host)
	if host == canonicalRouteHost(clusterDomain) {
		return true
	}
	for _, prefix := range reservedPlatformPrefixes {
		if host == canonicalRouteHost(domain.SubdomainFor(prefix, clusterDomain)) {
			return true
		}
	}
	return false
}

// hostDeclaredByClusterIdentity reports whether canonHost matches any host the
// ClusterIdentity singleton declares as a platform serving host. This is the
// authoritative serving set: it reserves override and transition hosts the
// moment they are declared, before their Ingress is published. canonHost must
// already be canonical.
func hostDeclaredByClusterIdentity(ctx context.Context, reader client.Reader, canonHost string) (bool, error) {
	var ci kipperv1.ClusterIdentity
	err := reader.Get(ctx, types.NamespacedName{Name: ClusterIdentityName}, &ci)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading cluster identity: %w", err)
	}
	for _, base := range clusterIdentityBaseDomains(&ci) {
		if isReservedPlatformSubdomain(canonHost, base) {
			return true, nil
		}
	}
	for _, h := range clusterIdentityExplicitHosts(&ci) {
		if canonicalRouteHost(h) == canonHost {
			return true, nil
		}
	}
	return false, nil
}

// clusterIdentityBaseDomains collects every base domain whose conventional
// subdomains the platform reserves: the current spec domain and the domains of
// every steady/transition identity the status still records, including both the
// source and the target of an in-flight transition.
func clusterIdentityBaseDomains(ci *kipperv1.ClusterIdentity) []string {
	domains := []string{ci.Spec.Domain}
	for _, s := range clusterIdentitySteadyIdentities(ci) {
		domains = append(domains, s.Domain)
	}
	return domains
}

// clusterIdentityExplicitHosts collects every explicit per-service host the
// ClusterIdentity declares: spec overrides, the active serving set, the steady
// and rollback identities, and both sides of an in-flight transition (source and
// target). The transition target matters because after the issuer flip a spec
// edit may land, so status.transition.to/toIdentity, not live spec, is the
// authoritative target.
func clusterIdentityExplicitHosts(ci *kipperv1.ClusterIdentity) []string {
	var hosts []string
	hosts = append(hosts, identityHostValues(ci.Spec.Hosts)...)
	hosts = append(hosts, resolvedHostValues(ci.Status.ActiveHosts)...)
	for _, s := range clusterIdentitySteadyIdentities(ci) {
		hosts = append(hosts, identityHostValues(s.Hosts)...)
	}
	if t := ci.Status.Transition; t != nil {
		hosts = append(hosts, resolvedHostValues(t.From)...)
		hosts = append(hosts, resolvedHostValues(t.To)...)
	}
	return hosts
}

// clusterIdentitySteadyIdentities returns every SteadyIdentity the ClusterIdentity
// records: the current and previous steady state and both sides of an in-flight
// transition.
func clusterIdentitySteadyIdentities(ci *kipperv1.ClusterIdentity) []*kipperv1.SteadyIdentity {
	candidates := []*kipperv1.SteadyIdentity{ci.Status.Steady, ci.Status.LastSteady}
	if t := ci.Status.Transition; t != nil {
		candidates = append(candidates, t.FromIdentity, t.ToIdentity)
	}
	var out []*kipperv1.SteadyIdentity
	for _, s := range candidates {
		if s != nil {
			out = append(out, s)
		}
	}
	return out
}

func identityHostValues(h *kipperv1.IdentityHosts) []string {
	if h == nil {
		return nil
	}
	return []string{h.Console, h.ConsoleAPI, h.Dex}
}

func resolvedHostValues(h *kipperv1.ResolvedHosts) []string {
	if h == nil {
		return nil
	}
	return []string{h.Console, h.ConsoleAPI, h.Dex}
}

// reserveHost claims host for namespace. It returns owned=true when namespace
// holds the claim (fresh, already theirs, or taken over from a deleted project)
// and owned=false when another live namespace owns it. Reads use the uncached
// reader so an ownership decision is never made from a stale cache; the create
// is authoritative (AlreadyExists means someone else won).
//
// Because a claim is never explicitly released, a claim can outlive its owning
// project. So when an existing claim is owned by a namespace that no longer
// exists, this namespace takes it over: that is the only way an abandoned host
// becomes available again, and it is safe because a live owner is never
// displaced (a terminating namespace still exists, so it is not taken over until
// its Ingresses are gone too).
func reserveHost(ctx context.Context, reader client.Reader, writer client.Client, namespace, host string) (owned bool, err error) {
	claimName := routeClaimName(host)
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: routeClaimNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kipper",
				routeClaimLabel:                "true",
				routeOwnerNamespaceLabel:       namespace,
			},
		},
		Data: map[string]string{"host": canonicalRouteHost(host), "owner": namespace},
	}

	var existing corev1.ConfigMap
	getErr := reader.Get(ctx, types.NamespacedName{Name: claimName, Namespace: routeClaimNamespace}, &existing)
	if apierrors.IsNotFound(getErr) {
		createErr := writer.Create(ctx, desired)
		if apierrors.IsAlreadyExists(createErr) {
			if err := reader.Get(ctx, types.NamespacedName{Name: claimName, Namespace: routeClaimNamespace}, &existing); err != nil {
				return false, err
			}
			return resolveClaimOwnership(ctx, reader, writer, &existing, namespace)
		}
		if createErr != nil {
			return false, createErr
		}
		return true, nil
	}
	if getErr != nil {
		return false, getErr
	}
	return resolveClaimOwnership(ctx, reader, writer, &existing, namespace)
}

// resolveClaimOwnership decides whether namespace owns an existing claim. It
// owns the claim outright if it is already the recorded owner. Otherwise, if the
// recorded owner namespace still exists, that is a live owner and namespace is
// refused; if the recorded owner is gone, the claim is stale and namespace takes
// it over with an optimistic update. A concurrent takeover loses on conflict and
// re-evaluates on its next reconcile, so exactly one namespace wins.
func resolveClaimOwnership(ctx context.Context, reader client.Reader, writer client.Client, existing *corev1.ConfigMap, namespace string) (bool, error) {
	if existing.Data["owner"] == namespace {
		return true, nil
	}
	ownerExists, err := namespaceExists(ctx, reader, existing.Data["owner"])
	if err != nil {
		return false, err
	}
	if ownerExists {
		return false, nil
	}
	if existing.Data == nil {
		existing.Data = map[string]string{}
	}
	existing.Data["owner"] = namespace
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	existing.Labels[routeOwnerNamespaceLabel] = namespace
	if err := writer.Update(ctx, existing); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("taking over stale route claim %s: %w", existing.Name, err)
	}
	return true, nil
}

// namespaceExists reports whether a namespace of the given name exists. A
// terminating namespace still exists. A read error other than NotFound is
// surfaced so a claim is never taken over on uncertain state.
func namespaceExists(ctx context.Context, reader client.Reader, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	var ns corev1.Namespace
	err := reader.Get(ctx, types.NamespacedName{Name: name}, &ns)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading namespace %s: %w", name, err)
	}
	return true, nil
}

// resolveNamespaceEnvironment reads the environment label a project-environment
// namespace carries, used to derive an app's implicit per-environment host. A
// missing namespace means it is gone, so env is empty; any other read error is
// surfaced so the caller retries rather than acting on incomplete data.
func resolveNamespaceEnvironment(ctx context.Context, reader client.Reader, namespace string) (string, error) {
	var ns corev1.Namespace
	err := reader.Get(ctx, types.NamespacedName{Name: namespace}, &ns)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading namespace %s: %w", namespace, err)
	}
	return ns.Labels["kipper.run/environment"], nil
}

// redirectFromRefusal records a redirectFrom host a route cannot serve and
// why, phrased to complete "host %q is %s".
type redirectFromRefusal struct {
	host   string
	reason string
}

// redirectFromHostPattern is the shape a redirectFrom host must have: at
// least two lowercase DNS labels. A malformed host would be rejected by the
// API server when it appears in an Ingress rule, taking the canonical host's
// Ingress update down with it, so it is refused here instead.
var redirectFromHostPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// resolveRedirectFromHosts validates and claims a route's redirectFrom hosts
// for the app in namespace. Accepted hosts come back canonicalised, in spec
// order, with duplicates and the canonical route host itself dropped. A host
// the route cannot serve — malformed, under kipper.run (the gateway only
// routes registered subdomains), platform-reserved, owned by another live
// project, or already used by a sibling workload in this project — comes
// back as a refusal so the caller degrades the route instead of failing it.
// Alias claims are as sticky as the primary host's: removing an alias tears
// down its Ingress rule and middleware while the claim stays with the
// project.
func resolveRedirectFromHosts(ctx context.Context, reader client.Reader, writer client.Client, namespace, appName, clusterDomain, canonicalHost string, redirectFrom []string) (accepted []string, refused []redirectFromRefusal, err error) {
	if len(redirectFrom) == 0 {
		return nil, nil, nil
	}
	served, err := namespaceServedHosts(ctx, reader, namespace, appName, clusterDomain)
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{canonicalHost: true}
	for _, raw := range redirectFrom {
		h := canonicalRouteHost(raw)
		if seen[h] {
			continue
		}
		seen[h] = true
		// The API server caps an Ingress rule host at 253 characters, so a
		// longer name would be refused there and take the canonical host's
		// Ingress update down with it.
		if len(h) > 253 || !redirectFromHostPattern.MatchString(h) {
			refused = append(refused, redirectFromRefusal{host: raw, reason: "not a valid hostname"})
			continue
		}
		if h == "kipper.run" || strings.HasSuffix(h, ".kipper.run") {
			refused = append(refused, redirectFromRefusal{host: h, reason: "under kipper.run, which cannot serve redirects"})
			continue
		}
		if served[h] {
			refused = append(refused, redirectFromRefusal{host: h, reason: "already used by another app in this project"})
			continue
		}
		reserved, err := hostReservedForPlatform(ctx, reader, h, clusterDomain)
		if err != nil {
			return nil, nil, err
		}
		if reserved {
			refused = append(refused, redirectFromRefusal{host: h, reason: "reserved for a platform service"})
			continue
		}
		owned, err := reserveHost(ctx, reader, writer, namespace, h)
		if err != nil {
			return nil, nil, err
		}
		if !owned {
			refused = append(refused, redirectFromRefusal{host: h, reason: "already claimed by another project"})
			continue
		}
		accepted = append(accepted, h)
	}
	return accepted, refused, nil
}

// namespaceServedHosts collects every canonical hostname a workload in
// namespace other than appName uses: sibling Apps' route hosts and redirect
// aliases, plus the rule hosts of any Ingress the app does not own. Host
// claims are namespace-level so the claim system deliberately lets workloads
// in one project share a host for path-based routing, but a redirect answers
// every path and would divert a sibling's traffic, so an alias must not
// capture a host a sibling serves. Both sides of a mutual alias conflict
// refuse, which is stable across reconciles; the route condition says why.
func namespaceServedHosts(ctx context.Context, reader client.Reader, namespace, appName, clusterDomain string) (map[string]bool, error) {
	served := map[string]bool{}
	env, err := resolveNamespaceEnvironment(ctx, reader, namespace)
	if err != nil {
		return nil, err
	}
	var apps kipperv1.AppList
	if err := reader.List(ctx, &apps, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing apps in %s: %w", namespace, err)
	}
	for i := range apps.Items {
		other := &apps.Items[i]
		if other.Name == appName || other.Spec.Route == nil {
			continue
		}
		// appRouteHost resolves the explicit host or the derived
		// <app>[-env].<clusterDomain>, so a sibling serving its derived
		// hostname vetoes it even before its Ingress exists.
		if h := appRouteHost(other, clusterDomain, env); h != "" {
			served[canonicalRouteHost(h)] = true
		}
		for _, alias := range other.Spec.Route.RedirectFrom {
			served[canonicalRouteHost(alias)] = true
		}
	}
	var ingresses networkingv1.IngressList
	if err := reader.List(ctx, &ingresses, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing ingresses in %s: %w", namespace, err)
	}
	for i := range ingresses.Items {
		// The app's own Ingress carries its name, so skipping by name keeps
		// the app's current rules from vetoing its own next reconcile.
		if ingresses.Items[i].Name == appName {
			continue
		}
		for _, rule := range ingresses.Items[i].Spec.Rules {
			if rule.Host != "" {
				served[canonicalRouteHost(rule.Host)] = true
			}
		}
	}
	return served, nil
}

// aliasTLSSecretName derives the TLS secret name for a redirectFrom host.
// The hash keeps the name stable under list reordering and DNS-safe
// regardless of the host.
func aliasTLSSecretName(appName, host string) string {
	sum := sha256.Sum256([]byte(canonicalRouteHost(host)))
	return appName + "-redirect-" + hex.EncodeToString(sum[:])[:8] + "-tls"
}

// appRouteHost resolves the hostname an app's route serves: the explicit host,
// or the derived per-environment subdomain. Returns "" when the app has no
// route or no host can be derived.
func appRouteHost(app *kipperv1.App, clusterDomain, env string) string {
	if app.Spec.Route == nil {
		return ""
	}
	if app.Spec.Route.Host != "" {
		return app.Spec.Route.Host
	}
	if clusterDomain == "" {
		return ""
	}
	prefix := AppHostPrefix(app.Name, env)
	return domain.SubdomainFor(prefix, clusterDomain)
}

// AppHostPrefix is the subdomain an app serves on within an environment.
//
// A default environment adds nothing, the same way it adds nothing to the
// namespace name: project "shop" runs in namespace "shop" and its app "web"
// serves at "web", while an environment called "prod" gives "shop-prod" and
// "web-prod". Both ask isDefaultEnvironment, so a host and a namespace can
// never describe different environments.
//
// Note that this is not DefaultEnvironmentName, which is the environment a
// project gets when it declares none. That one is called "test" and is suffixed
// like any other.
func AppHostPrefix(app, env string) string {
	if isDefaultEnvironment(env) {
		return app
	}
	return app + "-" + env
}
