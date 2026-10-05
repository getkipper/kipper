package serving

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/getkipper/kipper/controller/pkg/routename"
)

// A platform backend missing from routename.PlatformRoutes could be taken by a
// tenant app whose flattened name collides with it.
func TestEveryServedBackendIsAListedPlatformRoute(t *testing.T) {
	grafana, _ := RenderGrafanaRoute(testGrafanaRoute())
	ingresses := append([]networkingv1.Ingress{}, grafana...)
	for _, phase := range allPhases {
		objs, err := Render(specFor(phase), Carry{})
		if err != nil {
			t.Fatalf("phase %q: %v", phase, err)
		}
		ingresses = append(ingresses, objs.Ingresses...)
	}
	if len(ingresses) == 0 {
		t.Fatal("rendered no Ingresses")
	}
	for _, ing := range ingresses {
		for _, b := range routename.Backends(ing) {
			if !routename.CollidesWithPlatform(b.Namespace, b.Service, b.Port) {
				t.Errorf("Ingress %s/%s routes to %s:%d, which routename.PlatformRoutes does not list", ing.Namespace, ing.Name, b.Key(), b.Port)
			}
		}
	}
}

var allPhases = []Phase{PhaseSteady, PhaseDualServe, PhaseAwaitingApproval, PhaseCuttingOver, PhaseVerifying, PhaseContracting}
