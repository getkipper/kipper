package serving

import (
	"reflect"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGrafanaHosts(t *testing.T) {
	tests := []struct {
		name       string
		spec       Spec
		wantServed []string
		wantActive string
	}{
		{
			name:       "steady custom domain",
			spec:       Spec{Domain: "example.com"},
			wantServed: []string{"grafana.example.com"},
			wantActive: "grafana.example.com",
		},
		{
			name:       "steady kipper.run",
			spec:       Spec{Domain: "acme.kipper.run"},
			wantServed: []string{"grafana--acme.kipper.run"},
			wantActive: "grafana--acme.kipper.run",
		},
		{
			name:       "dual serve keeps the old identity active",
			spec:       Spec{Domain: "acme.kipper.run", Transition: &Transition{Phase: PhaseDualServe, FromDomain: "acme.kipper.run", ToDomain: "example.com"}},
			wantServed: []string{"grafana--acme.kipper.run", "grafana.example.com"},
			wantActive: "grafana--acme.kipper.run",
		},
		{
			name:       "awaiting approval keeps the old identity active",
			spec:       Spec{Domain: "acme.kipper.run", Transition: &Transition{Phase: PhaseAwaitingApproval, FromDomain: "acme.kipper.run", ToDomain: "example.com"}},
			wantServed: []string{"grafana--acme.kipper.run", "grafana.example.com"},
			wantActive: "grafana--acme.kipper.run",
		},
		{
			name:       "cutover serves both, new identity active",
			spec:       Spec{Domain: "example.com", Transition: &Transition{Phase: PhaseCuttingOver, FromDomain: "acme.kipper.run", ToDomain: "example.com"}},
			wantServed: []string{"grafana--acme.kipper.run", "grafana.example.com"},
			wantActive: "grafana.example.com",
		},
		{
			name:       "contraction drops the old host",
			spec:       Spec{Domain: "example.com", Transition: &Transition{Phase: PhaseContracting, FromDomain: "acme.kipper.run", ToDomain: "example.com"}},
			wantServed: []string{"grafana.example.com"},
			wantActive: "grafana.example.com",
		},
		{
			name:       "a transition without recorded domains serves only the spec domain",
			spec:       Spec{Domain: "example.com", Transition: &Transition{Phase: PhaseDualServe}},
			wantServed: []string{"grafana.example.com"},
			wantActive: "grafana.example.com",
		},
		{
			name:       "a transition that keeps the base domain serves one host",
			spec:       Spec{Domain: "example.com", Transition: &Transition{Phase: PhaseDualServe, FromDomain: "example.com", ToDomain: "example.com"}},
			wantServed: []string{"grafana.example.com"},
			wantActive: "grafana.example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			served, active := GrafanaHosts(tt.spec)
			if !reflect.DeepEqual(served, tt.wantServed) || active != tt.wantActive {
				t.Errorf("GrafanaHosts = (%v, %q), want (%v, %q)", served, active, tt.wantServed, tt.wantActive)
			}
		})
	}
}

func testGrafanaRoute() GrafanaRoute {
	return GrafanaRoute{
		Hosts:       []string{"grafana--acme.kipper.run", "grafana.example.com"},
		Active:      "grafana.example.com",
		AuthAddress: "http://console-api.kipper-system.svc.cluster.local:8080/auth/check/grafana",
		DenyAddress: "http://kipper-authz.kipper-system.svc.cluster.local:8080/deny",
		CookieName:  func(host string) string { return "cookie-for-" + host },
	}
}

func grafanaIngressFor(ings []networkingv1.Ingress, host string, login bool) *networkingv1.Ingress {
	for i := range ings {
		ing := &ings[i]
		if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != host {
			continue
		}
		isLogin := ing.Spec.Rules[0].HTTP.Paths[0].Path == "/login"
		if isLogin == login {
			return ing
		}
	}
	return nil
}

func middlewareNamed(mws []*unstructured.Unstructured, name string) *unstructured.Unstructured {
	for _, mw := range mws {
		if mw.GetName() == name {
			return mw
		}
	}
	return nil
}

// chainNames maps a router.middlewares annotation to the middleware names in order.
func chainNames(t *testing.T, ing *networkingv1.Ingress) []string {
	t.Helper()
	var names []string
	for _, ref := range strings.Split(ing.Annotations[routerMiddlewaresAnnotation], ",") {
		names = append(names, strings.TrimSuffix(ref, "@kubernetescrd"))
	}
	return names
}

func TestRenderGrafanaRoute_MainIngressChain(t *testing.T) {
	g := testGrafanaRoute()
	ings, mws := RenderGrafanaRoute(g)

	for _, host := range g.Hosts {
		ing := grafanaIngressFor(ings, host, false)
		if ing == nil {
			t.Fatalf("no main Ingress for %s", host)
		}
		if ing.Namespace != "monitoring" || ing.Labels[GrafanaRouteLabel] != "true" {
			t.Errorf("%s: namespace %q labels %v", host, ing.Namespace, ing.Labels)
		}
		b := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service
		if b.Name != "kube-prometheus-stack-grafana" || b.Port.Number != 80 {
			t.Errorf("%s: backend %s:%d", host, b.Name, b.Port.Number)
		}

		chain := chainNames(t, ing)
		authName := "monitoring-" + GrafanaAuthMiddlewareName(host)
		want := []string{"monitoring-security-headers", "traefik-rate-limit", "monitoring-grafana-header-strip", authName, "monitoring-grafana-cookie-strip"}
		if !reflect.DeepEqual(chain, want) {
			t.Errorf("%s: middleware chain = %v, want %v", host, chain, want)
		}

		auth := middlewareNamed(mws, GrafanaAuthMiddlewareName(host))
		if auth == nil {
			t.Fatalf("%s: no forwardAuth middleware", host)
		}
		fa, _, _ := unstructured.NestedMap(auth.Object, "spec", "forwardAuth")
		if fa["address"] != g.AuthAddress {
			t.Errorf("%s: forwardAuth address %v", host, fa["address"])
		}
		if !reflect.DeepEqual(fa["authResponseHeaders"], []interface{}{"X-WEBAUTH-USER", "X-WEBAUTH-ROLE"}) {
			t.Errorf("%s: authResponseHeaders %v", host, fa["authResponseHeaders"])
		}
		if !reflect.DeepEqual(fa["addAuthCookiesToResponse"], []interface{}{"cookie-for-" + host}) {
			t.Errorf("%s: addAuthCookiesToResponse %v", host, fa["addAuthCookiesToResponse"])
		}
	}

	if got := grafanaIngressFor(ings, "grafana.example.com", false).Annotations[GrafanaActiveAnnotation]; got != "true" {
		t.Errorf("active host not marked: %q", got)
	}
	if _, ok := grafanaIngressFor(ings, "grafana--acme.kipper.run", false).Annotations[GrafanaActiveAnnotation]; ok {
		t.Error("inactive host marked active")
	}
}

func TestRenderGrafanaRoute_StripsIdentityCredentialsAndCookies(t *testing.T) {
	_, mws := RenderGrafanaRoute(testGrafanaRoute())

	strip := middlewareNamed(mws, "grafana-header-strip")
	if strip == nil {
		t.Fatal("no header-strip middleware")
	}
	headers, _, _ := unstructured.NestedMap(strip.Object, "spec", "headers", "customRequestHeaders")
	for _, h := range []string{"X-WEBAUTH-USER", "X-WEBAUTH-ROLE", "Authorization"} {
		if v, ok := headers[h]; !ok || v != "" {
			t.Errorf("header strip does not clear %s: %v", h, headers)
		}
	}

	cookies := middlewareNamed(mws, "grafana-cookie-strip")
	if cookies == nil {
		t.Fatal("no cookie-strip middleware")
	}
	ch, _, _ := unstructured.NestedMap(cookies.Object, "spec", "headers", "customRequestHeaders")
	if v, ok := ch["Cookie"]; !ok || v != "" {
		t.Errorf("cookie strip does not blank Cookie: %v", ch)
	}
}

func TestRenderGrafanaRoute_DeniesLogin(t *testing.T) {
	g := testGrafanaRoute()
	ings, mws := RenderGrafanaRoute(g)

	for _, host := range g.Hosts {
		ing := grafanaIngressFor(ings, host, true)
		if ing == nil {
			t.Fatalf("no /login Ingress for %s", host)
		}
		if ing.Annotations[routerPriorityAnnotation] != "100000" {
			t.Errorf("%s: login router priority %q", host, ing.Annotations[routerPriorityAnnotation])
		}
		chain := chainNames(t, ing)
		if chain[len(chain)-1] != "monitoring-grafana-login-deny" {
			t.Errorf("%s: login chain %v does not end in the deny middleware", host, chain)
		}
	}
	deny := middlewareNamed(mws, "grafana-login-deny")
	addr, _, _ := unstructured.NestedString(deny.Object, "spec", "forwardAuth", "address")
	if addr != g.DenyAddress {
		t.Errorf("deny address %q", addr)
	}
	forwarded, _, _ := unstructured.NestedSlice(deny.Object, "spec", "forwardAuth", "authRequestHeaders")
	if !reflect.DeepEqual(forwarded, []interface{}{"X-Forwarded-Uri"}) {
		t.Errorf("deny forwards %v; only X-Forwarded-Uri, so no credential reaches kipper-authz", forwarded)
	}
	for _, host := range g.Hosts {
		chain := chainNames(t, grafanaIngressFor(ings, host, true))
		if len(chain) < 2 || chain[1] != "traefik-rate-limit" {
			t.Errorf("%s: login chain %v is not rate limited", host, chain)
		}
	}
}

func TestRenderGrafanaRoute_TLSFollowsTheHost(t *testing.T) {
	ings, _ := RenderGrafanaRoute(testGrafanaRoute())

	custom := grafanaIngressFor(ings, "grafana.example.com", false)
	if custom.Annotations[clusterIssuerAnnotation] != clusterIssuer || custom.Spec.TLS[0].SecretName == "" {
		t.Errorf("custom-domain host should get a cert-manager certificate: %v %v", custom.Annotations, custom.Spec.TLS)
	}
	login := grafanaIngressFor(ings, "grafana.example.com", true)
	if _, ok := login.Annotations[clusterIssuerAnnotation]; ok {
		t.Error("the /login Ingress must not request a second certificate for the same Secret")
	}
	if login.Spec.TLS[0].SecretName != custom.Spec.TLS[0].SecretName {
		t.Errorf("the /login Ingress should serve the main Ingress's certificate: %q vs %q", login.Spec.TLS[0].SecretName, custom.Spec.TLS[0].SecretName)
	}
	free := grafanaIngressFor(ings, "grafana--acme.kipper.run", false)
	if _, ok := free.Annotations[clusterIssuerAnnotation]; ok || free.Spec.TLS[0].SecretName != "" {
		t.Errorf("kipper.run host must use the gateway hop certificate: %v %v", free.Annotations, free.Spec.TLS)
	}
}

func TestRenderGrafanaRoute_NamesAreStablePerHost(t *testing.T) {
	a, b := GrafanaAuthMiddlewareName("grafana.example.com"), GrafanaAuthMiddlewareName("grafana.example.com")
	if a != b || a == GrafanaAuthMiddlewareName("grafana.example.org") {
		t.Errorf("auth middleware names not stable and distinct: %s %s", a, b)
	}
}

func TestRenderGrafanaRoute_EveryMiddlewareIsLabelled(t *testing.T) {
	// Withdrawal deletes by label; an unlabelled Middleware would outlive the route.
	_, mws := RenderGrafanaRoute(testGrafanaRoute())
	for _, mw := range mws {
		if mw.GetLabels()[GrafanaRouteLabel] != "true" {
			t.Errorf("middleware %s is not labelled", mw.GetName())
		}
	}
}
