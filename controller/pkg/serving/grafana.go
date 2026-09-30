package serving

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/getkipper/kipper/controller/pkg/hostnames"
)

const (
	grafanaPrefix  = "grafana"
	monitoringNS   = "monitoring"
	grafanaService = "kube-prometheus-stack-grafana"
	grafanaPort    = 80

	// GrafanaRouteLabel marks every object of the Grafana route.
	GrafanaRouteLabel = "kipper.run/grafana-route"
	// GrafanaActiveAnnotation marks the Ingress of the host links point at.
	GrafanaActiveAnnotation = "kipper.run/grafana-active"

	grafanaHeaderStrip = "grafana-header-strip"
	grafanaCookieStrip = "grafana-cookie-strip"
	grafanaLoginDeny   = "grafana-login-deny"
	rateLimitRef       = "traefik-rate-limit@kubernetescrd"
	loginDenyPriority  = "100000"

	routerMiddlewaresAnnotation = "traefik.ingress.kubernetes.io/router.middlewares"
	routerPriorityAnnotation    = "traefik.ingress.kubernetes.io/router.priority"
)

// GrafanaHosts returns the hosts Grafana is served on for the Spec and the one
// that is active. It follows the console's phase table: both identities serve
// during a transition, the active one flips at cutover, and contraction drops
// the old host.
func GrafanaHosts(s Spec) (served []string, active string) {
	current := hostnames.SubdomainFor(grafanaPrefix, s.Domain)
	t := s.Transition
	if t == nil || t.FromDomain == "" || t.ToDomain == "" {
		return []string{current}, current
	}
	from := hostnames.SubdomainFor(grafanaPrefix, t.FromDomain)
	to := hostnames.SubdomainFor(grafanaPrefix, t.ToDomain)
	switch t.Phase {
	case PhaseCuttingOver, PhaseVerifying:
		return union(from, to), to
	case PhaseContracting:
		return []string{to}, to
	default:
		return union(from, to), from
	}
}

// GrafanaRoute is what the public Grafana route is rendered from.
type GrafanaRoute struct {
	Hosts       []string
	Active      string
	AuthAddress string                   // console-api's Grafana forwardAuth endpoint
	DenyAddress string                   // kipper-authz's deny endpoint
	CookieName  func(host string) string // the Kipper UI-session cookie name for a host
}

// GrafanaAuthMiddlewareName is the per-host forwardAuth Middleware; the cookie
// name differs per host, so each host needs its own.
func GrafanaAuthMiddlewareName(host string) string {
	return "grafana-auth-" + hostSuffix(host)
}

func grafanaIngressName(host string) string      { return "grafana-" + hostSuffix(host) }
func grafanaLoginIngressName(host string) string { return "grafana-login-" + hostSuffix(host) }

func hostSuffix(host string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(host)))
	return hex.EncodeToString(sum[:])[:10]
}

// RenderGrafanaRoute authenticates through Kipper before forwarding to Grafana.
// Strip client identity headers before authentication and cookies afterwards
// so Grafana uses only the verified identity. A separate route blocks /login.
func RenderGrafanaRoute(g GrafanaRoute) ([]networkingv1.Ingress, []*unstructured.Unstructured) {
	headers := SecurityHeadersMiddleware(monitoringNS)
	headers.SetLabels(map[string]string{GrafanaRouteLabel: "true"})
	mws := []*unstructured.Unstructured{
		headers,
		grafanaMiddleware(grafanaHeaderStrip, map[string]interface{}{"headers": map[string]interface{}{
			"customRequestHeaders": map[string]interface{}{
				"X-WEBAUTH-USER": "",
				"X-WEBAUTH-ROLE": "",
				"Authorization":  "",
			},
		}}),
		grafanaMiddleware(grafanaCookieStrip, map[string]interface{}{"headers": map[string]interface{}{
			"customRequestHeaders": map[string]interface{}{"Cookie": ""},
		}}),
		grafanaMiddleware(grafanaLoginDeny, map[string]interface{}{"forwardAuth": map[string]interface{}{
			"address": g.DenyAddress,
			// Forward no credential to kipper-authz.
			"authRequestHeaders": []interface{}{"X-Forwarded-Uri"},
		}}),
	}
	var ings []networkingv1.Ingress
	for _, host := range g.Hosts {
		auth := GrafanaAuthMiddlewareName(host)
		mws = append(mws, grafanaMiddleware(auth, map[string]interface{}{"forwardAuth": map[string]interface{}{
			"address":                  g.AuthAddress,
			"trustForwardHeader":       true,
			"authResponseHeaders":      []interface{}{"X-WEBAUTH-USER", "X-WEBAUTH-ROLE"},
			"addAuthCookiesToResponse": []interface{}{g.CookieName(host)},
		}}))

		main := grafanaIngress(grafanaIngressName(host), host, "/",
			mwRef(securityHeadersMiddleware), rateLimitRef, mwRef(grafanaHeaderStrip), mwRef(auth), mwRef(grafanaCookieStrip))
		if host == g.Active {
			main.Annotations[GrafanaActiveAnnotation] = "true"
		}
		login := grafanaIngress(grafanaLoginIngressName(host), host, "/login",
			mwRef(securityHeadersMiddleware), rateLimitRef, mwRef(grafanaLoginDeny))
		login.Annotations[routerPriorityAnnotation] = loginDenyPriority
		// The main Ingress requests the certificate; this one only serves it.
		delete(login.Annotations, clusterIssuerAnnotation)
		ings = append(ings, main, login)
	}
	return ings, mws
}

func mwRef(name string) string {
	return fmt.Sprintf("%s-%s@kubernetescrd", monitoringNS, name)
}

func grafanaIngress(name, host, path string, middlewares ...string) networkingv1.Ingress {
	ing := ingress(name, monitoringNS, grafanaIngressName(host)+"-tls", []string{host}, []networkingv1.HTTPIngressPath{
		{Path: path, PathType: pathTypePrefix(), Backend: backend(grafanaService, grafanaPort)},
	})
	ing.Labels = map[string]string{GrafanaRouteLabel: "true"}
	ing.Annotations[routerMiddlewaresAnnotation] = strings.Join(middlewares, ",")
	return ing
}

func grafanaMiddleware(name string, spec map[string]interface{}) *unstructured.Unstructured {
	mw := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": monitoringNS,
			"labels":    map[string]interface{}{GrafanaRouteLabel: "true"},
		},
		"spec": spec,
	}}
	mw.SetGroupVersionKind(schema.GroupVersionKind{Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware"})
	return mw
}
