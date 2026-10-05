package ai

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/controller/pkg/routename"
)

// refuseOccupiedRouteName protects a bundle's normalized backend key across
// all ports while another namespace uses it.
func (i *Installer) refuseOccupiedRouteName(ctx context.Context, platform routename.PlatformRoute) error {
	list, err := i.Clientset.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing Ingresses to check route names: %w", err)
	}
	want := routename.Key(platform.Namespace, platform.Service)
	for _, ing := range list.Items {
		if ing.Namespace == platform.Namespace {
			continue
		}
		for _, b := range routename.Backends(ing) {
			if b.Key() == want {
				return fmt.Errorf("the route %s/%s already uses the traffic name this bundle needs; rename that app first", ing.Namespace, ing.Name)
			}
		}
	}
	return nil
}

func libreChatRoute() routename.PlatformRoute {
	r, _ := routename.Platform(routename.Key(Namespace, "librechat-librechat"))
	return r
}

func anythingLLMRoute() routename.PlatformRoute {
	r, _ := routename.Platform(routename.Key(Namespace, AnythingLLMServiceName))
	return r
}
