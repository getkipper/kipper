// Package routename names the backends Traefik routes to. Traefik's Ingress
// provider files every backend under "<namespace>-<service>-<port>" in one
// map, so two backends that flatten to the same name overwrite each other and
// both hosts route to one of them.
package routename

import (
	"strings"
	"unicode"

	networkingv1 "k8s.io/api/networking/v1"
)

// Key returns the port-independent part of the name Traefik files a backend
// under. Two backends with the same key can only be told apart by their port.
// Like Traefik's provider.Normalize, it splits on every character that is not
// a letter or digit and joins the parts with single dashes, so "prod--web"
// and "prod-web" share a key.
func Key(namespace, service string) string {
	parts := strings.FieldsFunc(namespace+"-"+service, func(c rune) bool {
		return !unicode.IsLetter(c) && !unicode.IsNumber(c)
	})
	return strings.Join(parts, "-")
}

// PlatformRoute is a backend one of Kipper's own routes points at.
type PlatformRoute struct {
	Namespace string
	Service   string
	Port      int32
}

// KEDAInterceptor is the backend every function route points at.
var KEDAInterceptor = PlatformRoute{Namespace: "keda", Service: "keda-add-ons-http-interceptor-proxy", Port: 8080}

// PlatformRoutes reserves Kipper's numeric-port backends even when their
// optional components are not installed.
var PlatformRoutes = []PlatformRoute{
	{Namespace: "kipper-system", Service: "console", Port: 80},
	{Namespace: "kipper-system", Service: "console-api", Port: 8080},
	{Namespace: "dex", Service: "dex", Port: 5556},
	{Namespace: "monitoring", Service: "kube-prometheus-stack-grafana", Port: 80},
	KEDAInterceptor,
	{Namespace: "kipper-ai", Service: "librechat-librechat", Port: 3080},
	{Namespace: "kipper-ai", Service: "anythingllm", Port: 3001},
}

// Platform returns the platform route whose key is key, and false for a
// tenant key.
func Platform(key string) (PlatformRoute, bool) {
	for _, r := range PlatformRoutes {
		if Key(r.Namespace, r.Service) == key {
			return r, true
		}
	}
	return PlatformRoute{}, false
}

// CollidesWithPlatform reports whether a backend produces exactly the same
// Traefik name as a platform route, port included, so that the two cannot
// both be routed.
func CollidesWithPlatform(namespace, service string, port int32) bool {
	r, ok := Platform(Key(namespace, service))
	return ok && r.Port == port
}

// Backend is one Service an Ingress routes to, on a numeric Port or a named
// PortName. Traefik files it under its key and that port.
type Backend struct {
	Namespace string
	Service   string
	Port      int32
	PortName  string
}

func (b Backend) Key() string {
	return Key(b.Namespace, b.Service)
}

// Backends returns the distinct Service backends of an Ingress, in the order
// they first appear. Backends on named ports collide with each other just as
// numeric ones do, so both are listed.
func Backends(ing networkingv1.Ingress) []Backend {
	var out []Backend
	seen := map[Backend]bool{}
	add := func(b *networkingv1.IngressBackend) {
		if b == nil || b.Service == nil || (b.Service.Port.Number == 0 && b.Service.Port.Name == "") {
			return
		}
		be := Backend{Namespace: ing.Namespace, Service: b.Service.Name, Port: b.Service.Port.Number}
		if be.Port == 0 {
			be.PortName = b.Service.Port.Name
		}
		if !seen[be] {
			seen[be] = true
			out = append(out, be)
		}
	}
	add(ing.Spec.DefaultBackend)
	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for i := range rule.HTTP.Paths {
			add(&rule.HTTP.Paths[i].Backend)
		}
	}
	return out
}
