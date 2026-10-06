package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// Separate desired and processed generations to test stale status.
func appAt(host string, gen, processed int64, adopted string, routeReady map[string]interface{}) *unstructured.Unstructured {
	spec := map[string]interface{}{}
	if host != "" {
		spec["route"] = map[string]interface{}{"host": host, "path": "/api"}
	}
	conds := []interface{}{map[string]interface{}{"type": "ChildrenAdopted", "status": adopted, "message": "deployment update failed", "observedGeneration": processed}}
	if routeReady != nil {
		conds = append(conds, routeReady)
	}
	u := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec, "status": map[string]interface{}{"conditions": conds}}}
	u.SetGeneration(gen)
	return u
}

func refusal(reason, message string) map[string]interface{} {
	return map[string]interface{}{"type": "RouteReady", "status": "False", "reason": reason, "message": message}
}

func view(app *unstructured.Unstructured, hosts ...string) routeView {
	return routeView{app: app, ingressHosts: hosts}
}

func TestRouteOutcome(t *testing.T) {
	const host = "web.example.com"
	cases := map[string]struct {
		view routeView
		want string
		done bool
	}{
		"published with no route condition, the normal case": {view(appAt(host, 2, 2, "True", nil), host), "  ✔  Public URL: https://web.example.com/api", true},
		"published with a refused alias": {view(appAt(host, 2, 2, "True", refusal("RedirectHostUnavailable", "redirect host www.example.com is taken")), host),
			"  ✔  Public URL: https://web.example.com/api\n  !   redirect host www.example.com is taken", true},
		"refused name": {view(appAt(host, 2, 2, "True", refusal("RouteNameTaken", "This name conflicts with another app or service."))),
			"  ✗  Public URL unavailable: This name conflicts with another app or service.", true},
		"reserved host": {view(appAt("api.example.com", 2, 2, "True", refusal("HostUnavailable", `route host "api.example.com" is reserved for a platform service`))),
			`  ✗  Public URL unavailable: route host "api.example.com" is reserved for a platform service`, true},
		"waiting for a name check": {view(appAt(host, 2, 2, "True", refusal("RouteNamePending", "The route is waiting for a name check."))),
			"  !   Public URL pending: https://web.example.com/api. Try this URL again shortly.", true},
		"a step before the route failed": {view(appAt(host, 3, 3, "False", refusal("HostUnavailable", `route host "old.example.com" is taken`))),
			"  ✗  Deploy incomplete: deployment update failed", true},
		"a refusal of the previous spec":    {view(appAt(host, 3, 2, "True", refusal("HostUnavailable", `route host "old.example.com" is taken`))), "", false},
		"published before the new spec ran": {view(appAt(host, 3, 2, "True", nil), host), "", false},
		"an Ingress for the previous host":  {view(appAt("new.example.com", 3, 3, "True", nil), "old.example.com"), "", false},
		"an unreadable Ingress with a warning": {routeView{app: appAt(host, 2, 2, "True", refusal("RedirectHostUnavailable", "redirect host www.example.com is taken"))},
			"", false},
		"no route":                   {view(appAt("", 1, 1, "True", nil)), "  ✔  Deployed without a public URL", true},
		"no route and a failed step": {view(appAt("", 2, 2, "False", nil)), "  ✗  Deploy incomplete: deployment update failed", true},
		"no route, not reconciled":   {view(appAt("", 2, 1, "True", nil)), "", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, done := routeOutcome(tc.view)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.done, done)
		})
	}
}

func TestOwnedIngressHosts(t *testing.T) {
	app := &metav1.ObjectMeta{Name: "web", UID: types.UID("uid-web")}
	ing := func(owner types.UID) *networkingv1.Ingress {
		yes := true
		i := &networkingv1.Ingress{Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "web.example.com"}}}}
		if owner != "" {
			i.OwnerReferences = []metav1.OwnerReference{{UID: owner, Controller: &yes, Name: "web", Kind: "App"}}
		}
		return i
	}
	assert.Equal(t, []string{"web.example.com"}, ownedIngressHosts(ing("uid-web"), app))
	assert.Empty(t, ownedIngressHosts(ing(""), app), "an Ingress someone else created")
	assert.Empty(t, ownedIngressHosts(ing("uid-earlier-web"), app), "an Ingress of an earlier App of the same name")
}

func TestWaitForRouteOutcome(t *testing.T) {
	const host = "web.example.com"
	t.Run("keeps reading until the new spec's warnings are known", func(t *testing.T) {
		calls := 0
		get := func(context.Context) (routeView, error) {
			calls++
			if calls < 3 {
				return view(appAt(host, 2, 1, "True", nil), host), nil
			}
			return view(appAt(host, 2, 2, "True", refusal("RedirectHostUnavailable", "redirect host www.example.com is taken")), host), nil
		}
		got, _ := waitForRouteOutcome(context.Background(), get, time.Second, time.Millisecond)
		assert.Equal(t, "  ✔  Public URL: https://web.example.com/api\n  !   redirect host www.example.com is taken", got)
	})
	t.Run("a route-less app is unconfirmed when the wait ends", func(t *testing.T) {
		get := func(context.Context) (routeView, error) { return view(appAt("", 2, 1, "True", nil)), nil }
		got, _ := waitForRouteOutcome(context.Background(), get, 10*time.Millisecond, time.Millisecond)
		assert.Equal(t, "  !   Deployment still unconfirmed. Run 'kip app list' to check again.", got)
	})
	t.Run("an unseen route is unconfirmed when the wait ends", func(t *testing.T) {
		get := func(context.Context) (routeView, error) { return view(appAt(host, 2, 1, "True", nil), host), nil }
		got, last := waitForRouteOutcome(context.Background(), get, 10*time.Millisecond, time.Millisecond)
		assert.Equal(t, "  !   Public URL not confirmed: https://web.example.com/api. Try this URL again shortly.", got)
		assert.NotNil(t, last)
	})
	t.Run("a read that hangs cannot outlast the wait", func(t *testing.T) {
		get := func(ctx context.Context) (routeView, error) {
			<-ctx.Done()
			return routeView{}, ctx.Err()
		}
		start := time.Now()
		got, _ := waitForRouteOutcome(context.Background(), get, 20*time.Millisecond, time.Millisecond)
		assert.Less(t, time.Since(start), time.Second)
		assert.Equal(t, "  !   Deployment status unavailable. Run 'kip app list' to check again.", got)
	})
}
