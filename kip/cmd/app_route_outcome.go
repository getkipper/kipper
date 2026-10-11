package cmd

import (
	"context"
	"fmt"
	"slices"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/kip/internal/deployer"
)

const routeOutcomeWait = 20 * time.Second

// These reasons block the main route; a refused redirect alias leaves it published.
var routeRefusals = []string{"RouteNameTaken", "RouteNameReservedForPlatform", "HostUnavailable", "RouteNamePending"}

type routeView struct {
	app          *unstructured.Unstructured
	ingressHosts []string
	derivedHost  string
}

func (v routeView) host() string {
	if host, _, _ := unstructured.NestedString(v.app.Object, "spec", "route", "host"); host != "" {
		return host
	}
	if _, hasRoute, _ := unstructured.NestedMap(v.app.Object, "spec", "route"); hasRoute {
		return v.derivedHost
	}
	return ""
}

func (v routeView) url() string {
	path, _, _ := unstructured.NestedString(v.app.Object, "spec", "route", "path")
	return "https://" + v.host() + path
}

func (v routeView) published() bool {
	host := v.host()
	return host != "" && slices.Contains(v.ingressHosts, host)
}

// ChildrenAdopted ties the outcome to the current spec: a failed pass reports
// its failure; a successful pass lets us interpret RouteReady, which has no
// observedGeneration. Publication also requires a matching owned Ingress.
func routeOutcome(v routeView) (line string, done bool) {
	adopted := condition(v.app, "ChildrenAdopted")
	if adopted == nil || !observedCurrent(v.app, adopted) {
		return "", false
	}
	if adopted["status"] != "True" {
		return "  ✗  Deploy incomplete: " + str(adopted["message"]), true
	}
	if v.host() == "" {
		return "  ✔  Deployed without a public URL", true
	}
	ready := condition(v.app, "RouteReady")
	if ready != nil && ready["status"] == "False" && slices.Contains(routeRefusals, str(ready["reason"])) {
		if ready["reason"] == "RouteNamePending" {
			return fmt.Sprintf("  !   Public URL pending: %s. Try this URL again shortly.", v.url()), true
		}
		return "  ✗  Public URL unavailable: " + str(ready["message"]), true
	}
	if !v.published() {
		return "", false
	}
	line = "  ✔  Public URL: " + v.url()
	if ready != nil && ready["status"] == "False" {
		line += "\n  !   " + str(ready["message"])
	}
	return line, true
}

// Only an Ingress controlled by this App confirms publication; names can be reused.
func ownedIngressHosts(ing *networkingv1.Ingress, app metav1.Object) []string {
	if !metav1.IsControlledBy(ing, app) {
		return nil
	}
	var hosts []string
	for _, rule := range ing.Spec.Rules {
		hosts = append(hosts, rule.Host)
	}
	return hosts
}

func observedCurrent(app *unstructured.Unstructured, c map[string]interface{}) bool {
	observed, ok := c["observedGeneration"].(int64)
	return ok && observed == app.GetGeneration()
}

func condition(app *unstructured.Unstructured, kind string) map[string]interface{} {
	conds, _, _ := unstructured.NestedSlice(app.Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]interface{}); ok && m["type"] == kind {
			return m
		}
	}
	return nil
}

func str(v interface{}) string {
	s, _ := v.(string)
	return s
}

// All reads share a deadline, which get must honor. Return the last App read
// so the caller can also report whether it is stopped.
func waitForRouteOutcome(ctx context.Context, get func(context.Context) (routeView, error), wait, every time.Duration) (string, *unstructured.Unstructured) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var last routeView
	for {
		if v, err := get(ctx); err == nil {
			last = v
			if line, done := routeOutcome(v); done {
				return line, last.app
			}
		}
		select {
		case <-ctx.Done():
			if last.app == nil {
				return "  !   Deployment status unavailable. Run 'kip app list' to check again.", nil
			}
			if last.host() != "" {
				return fmt.Sprintf("  !   Public URL not confirmed: %s. Try this URL again shortly.", last.url()), last.app
			}
			return "  !   Deployment still unconfirmed. Run 'kip app list' to check again.", last.app
		case <-time.After(every):
		}
	}
}

// Keep app status available when the Ingress read fails: it may explain a refusal.
// Publication still requires a matching Ingress owned by the app.
func routeReader(d *deployer.Deployer, namespace, name, derivedHost string) func(context.Context) (routeView, error) {
	return func(ctx context.Context) (routeView, error) {
		app, err := d.Dynamic.Resource(deployer.AppGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return routeView{}, err
		}
		v := routeView{app: app, derivedHost: derivedHost}
		if ing, ingErr := d.Client.NetworkingV1().Ingresses(namespace).Get(ctx, name, metav1.GetOptions{}); ingErr == nil {
			v.ingressHosts = ownedIngressHosts(ing, app)
		}
		return v, nil
	}
}
