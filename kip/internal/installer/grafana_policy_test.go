package installer

import (
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

func TestParseIngressPeer(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want platform.IngressPeer
	}{
		{"configured", "traefik|app|edge", platform.IngressPeer{Namespace: "traefik", LabelKey: "app", LabelValue: "edge"}},
		{"only a namespace", "ingress||", platform.IngressPeer{Namespace: "ingress"}},
		{"no ConfigMap", "", platform.IngressPeer{}},
		{"trailing newline", "traefik|app|edge\n", platform.IngressPeer{Namespace: "traefik", LabelKey: "app", LabelValue: "edge"}},
		{"a warning mixed into the output", "Warning: something\ntraefik|app|edge", platform.IngressPeer{}},
		{"an invalid namespace", "Bad_NS|app|edge", platform.IngressPeer{LabelKey: "app", LabelValue: "edge"}},
		{"an invalid label", "traefik|bad key|edge", platform.IngressPeer{Namespace: "traefik"}},
		{"too many fields", "a|b|c|d", platform.IngressPeer{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseIngressPeer(tt.out); got != tt.want {
				t.Errorf("parseIngressPeer(%q) = %+v, want %+v", tt.out, got, tt.want)
			}
		})
	}
}

func TestGrafanaPolicyDefaultsToTheInstalledTraefik(t *testing.T) {
	// Traefik's HelmChart object sits in kube-system, but its pods run in the
	// chart's targetNamespace. The policy must admit the pods.
	if !strings.Contains(traefikManifestTemplate, "targetNamespace: "+platform.TraefikNamespace+"\n") {
		t.Fatalf("the Traefik chart is not installed into %q", platform.TraefikNamespace)
	}
	var np networkingv1.NetworkPolicy
	if err := yaml.Unmarshal([]byte(platform.GrafanaNetworkPolicy(platform.IngressPeer{})), &np); err != nil {
		t.Fatal(err)
	}
	got := np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]
	if got != platform.TraefikNamespace {
		t.Errorf("default ingress namespace = %q, want %q where Traefik runs", got, platform.TraefikNamespace)
	}
}
