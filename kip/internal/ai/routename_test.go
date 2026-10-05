package ai

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/getkipper/kipper/controller/pkg/routename"
)

// A platform backend missing from routename.PlatformRoutes could be taken by a
// tenant app whose flattened name collides with it.
func TestEveryAIBundleBackendIsAListedPlatformRoute(t *testing.T) {
	manifests := map[string]string{
		"LibreChat": LibreChatManifest(LibreChatConfig{
			Host: "chat.example.com", Model: "qwen2.5:3b-instruct-q4_K_M", Credentials: libreChatTestCreds(),
		}),
		"AnythingLLM": AnythingLLMManifest(anythingLLMTestConfig()),
	}
	for name, manifest := range manifests {
		found := 0
		for _, doc := range SplitYAMLDocuments(manifest) {
			if !strings.Contains(doc, "kind: Ingress") {
				continue
			}
			var ing networkingv1.Ingress
			if err := yaml.Unmarshal([]byte(doc), &ing); err != nil {
				t.Fatalf("%s: parsing Ingress: %v", name, err)
			}
			for _, b := range routename.Backends(ing) {
				found++
				if !routename.CollidesWithPlatform(b.Namespace, b.Service, b.Port) {
					t.Errorf("%s routes to %s:%d, which routename.PlatformRoutes does not list", name, b.Key(), b.Port)
				}
			}
		}
		if found == 0 {
			t.Errorf("%s: no Ingress backend found", name)
		}
	}
}

func TestOccupiedRouteNameRefusesTheBundle(t *testing.T) {
	tenant := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "librechat", Namespace: "kipper-ai-librechat"},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
				Path: "/", Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
					Name: "librechat", Port: networkingv1.ServiceBackendPort{Number: 8080},
				}},
			}}}},
		}}},
	}
	libreChat, _ := routename.Platform(routename.Key(Namespace, "librechat-librechat"))
	anythingLLM, _ := routename.Platform(routename.Key(Namespace, AnythingLLMServiceName))

	i := &Installer{Clientset: fake.NewClientset(tenant)}
	err := i.refuseOccupiedRouteName(context.Background(), libreChat)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kipper-ai-librechat/librechat")

	assert.NoError(t, i.refuseOccupiedRouteName(context.Background(), anythingLLM), "another name is free")
}

func TestInstallRefusesWhileItsRouteNameIsOccupied(t *testing.T) {
	tenant := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "librechat", Namespace: "kipper-ai-librechat"},
		Spec: networkingv1.IngressSpec{DefaultBackend: &networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
			Name: "librechat", Port: networkingv1.ServiceBackendPort{Number: 8080},
		}}},
	}
	i := &Installer{Clientset: fake.NewClientset(tenant)}

	err := i.Install(context.Background(), TierOne, Options{Host: "chat.example.com", NodeName: "node-1"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "traffic name")
}
