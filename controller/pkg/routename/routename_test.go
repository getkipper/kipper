package routename

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestKeyJoinsNamespaceAndService(t *testing.T) {
	if got := Key("team", "prod-web"); got != "team-prod-web" {
		t.Fatalf("Key = %q, want team-prod-web", got)
	}
	if Key("team", "prod-web") != Key("team-prod", "web") {
		t.Fatal("two backends Traefik cannot tell apart must share a key")
	}
}

func TestPlatformLooksUpEveryPlatformRouteByKey(t *testing.T) {
	for _, r := range PlatformRoutes {
		got, ok := Platform(Key(r.Namespace, r.Service))
		if !ok || got != r {
			t.Errorf("Platform(%q) = %+v, %v; want %+v", Key(r.Namespace, r.Service), got, ok, r)
		}
	}
	if _, ok := Platform("shop-web"); ok {
		t.Error("a tenant key is not a platform route")
	}
}

func TestPlatformRoutesCoverKipperOwnRoutes(t *testing.T) {
	want := map[string]int32{
		"kipper-system-console":                    80,
		"kipper-system-console-api":                8080,
		"dex-dex":                                  5556,
		"monitoring-kube-prometheus-stack-grafana": 80,
		"keda-keda-add-ons-http-interceptor-proxy": 8080,
		"kipper-ai-librechat-librechat":            3080,
		"kipper-ai-anythingllm":                    3001,
	}
	if len(PlatformRoutes) != len(want) {
		t.Fatalf("got %d platform routes, want %d", len(PlatformRoutes), len(want))
	}
	for key, port := range want {
		r, ok := Platform(key)
		if !ok || r.Port != port {
			t.Errorf("Platform(%q) = %+v, %v; want port %d", key, r, ok, port)
		}
	}
}

func TestCollidesWithPlatformComparesTheFullTraefikName(t *testing.T) {
	cases := []struct {
		name      string
		namespace string
		service   string
		port      int32
		want      bool
	}{
		{"same key and port as the console API", "kipper", "system-console-api", 8080, true},
		{"same key, other port", "kipper", "system-console-api", 9090, false},
		{"tenant key", "shop", "web", 8080, false},
	}
	for _, tc := range cases {
		if got := CollidesWithPlatform(tc.namespace, tc.service, tc.port); got != tc.want {
			t.Errorf("%s: CollidesWithPlatform = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBackendsListsEveryServiceBackendOnce(t *testing.T) {
	ing := networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop"},
		Spec: networkingv1.IngressSpec{
			DefaultBackend: serviceBackend("web", 8080, ""),
			Rules: []networkingv1.IngressRule{{IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{
					{Path: "/", Backend: *serviceBackend("web", 8080, "")},
					{Path: "/ui", Backend: *serviceBackend("db", 0, "ui")},
					{Path: "/api", Backend: *serviceBackend("api", 9000, "")},
				},
			}}}},
		},
	}
	got := Backends(ing)
	want := []Backend{
		{Namespace: "shop", Service: "web", Port: 8080},
		{Namespace: "shop", Service: "db", PortName: "ui"},
		{Namespace: "shop", Service: "api", Port: 9000},
	}
	if len(got) != len(want) {
		t.Fatalf("Backends = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Backends = %+v, want %+v", got, want)
		}
	}
}

func serviceBackend(name string, number int32, portName string) *networkingv1.IngressBackend {
	return &networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
		Name: name,
		Port: networkingv1.ServiceBackendPort{Number: number, Name: portName},
	}}
}

// Traefik's provider.Normalize splits a name on every character that is not a
// letter or digit and joins the parts with single dashes.
func TestKeyNormalizesLikeTraefik(t *testing.T) {
	if Key("team", "prod--web") != Key("team-prod", "web") {
		t.Fatal("a doubled dash must not make a colliding name look distinct")
	}
	if got := Key("team", "prod--web"); got != "team-prod-web" {
		t.Fatalf("Key = %q, want team-prod-web", got)
	}
}
