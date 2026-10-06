package cmd

import (
	"context"
	"fmt"
	"slices"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const routeOutcomeWait = 20 * time.Second

// These reasons block the main route; a refused redirect alias leaves it published.
var routeRefusals = []string{"RouteNameTaken", "RouteNameReservedForPlatform", "HostUnavailable", "RouteNamePending"}

type routeView struct {
	app *unstructured.Unstructured
	// Empty when the App's Ingress is absent or unreadable.
	ingressHosts []string
}

func (v routeView) url() string {
	host, _, _ := unstructured.NestedString(v.app.Object, "spec", "route", "host")
	path, _, _ := unstructured.NestedString(v.app.Object, "spec", "route", "path")
	return "https://" + host + path
}

func (v routeView) published() bool {
	host, _, _ := unstructured.NestedString(v.app.Object, "spec", "route", "host")
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
	if host, _, _ := unstructured.NestedString(v.app.Object, "spec", "route", "host"); host == "" {
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
	var last *unstructured.Unstructured
	for {
		if v, err := get(ctx); err == nil {
			last = v.app
			if line, done := routeOutcome(v); done {
				return line, last
			}
		}
		select {
		case <-ctx.Done():
			if last == nil {
				return "  !   Deployment status unavailable. Run 'kip app list' to check again.", nil
			}
			if host, _, _ := unstructured.NestedString(last.Object, "spec", "route", "host"); host != "" {
				return fmt.Sprintf("  !   Public URL not confirmed: %s. Try this URL again shortly.", routeView{app: last}.url()), last
			}
			return "  !   Deployment still unconfirmed. Run 'kip app list' to check again.", last
		case <-time.After(every):
		}
	}
}
