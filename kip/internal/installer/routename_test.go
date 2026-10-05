package installer

import (
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"

	"github.com/getkipper/kipper/controller/pkg/routename"
)

// A platform backend missing from routename.PlatformRoutes could be taken by a
// tenant app whose flattened name collides with it.
func TestEveryInstalledPlatformBackendIsAListedRoute(t *testing.T) {
	manifests := map[string]string{
		"console": renderConsoleManifest("dex.example.com", "console.example.com", "api.example.com", "example.com", "example.kipper.run", "203.0.113.7"),
		"dex":     renderDexManifest("dex.example.com", "console.example.com", "example.com", "$2a$10$abcdefghijklmnopqrstuv"),
	}
	for name, manifest := range manifests {
		found := 0
		for _, doc := range strings.Split(manifest, "\n---") {
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
