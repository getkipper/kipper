package cmd

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/routename"
)

// routeNameFindings records shared and reserved backend keys.
type routeNameFindings struct {
	// PlatformCollisions lists tenant Ingresses as namespace/name whose backends
	// match an installed platform backend's key and port.
	PlatformCollisions []string
	// Occupied lists distinct tenant Services as namespace/name using a platform
	// key while the platform backend is absent or the tenant port differs.
	Occupied []string
	// Shared groups distinct Services as namespace/name by shared non-platform
	// key, regardless of port.
	Shared [][]string
}

func (f routeNameFindings) decisions() int {
	return len(f.PlatformCollisions)
}

func (f routeNameFindings) empty() bool {
	return len(f.PlatformCollisions) == 0 && len(f.Occupied) == 0 && len(f.Shared) == 0
}

type routeProducer struct {
	backend routename.Backend
	route   string
}

// workload returns the Service identity used to deduplicate reports across Ingresses.
func (p routeProducer) workload() string {
	return p.backend.Namespace + "/" + p.backend.Service
}

// assessRouteNames checks normalized backend keys across all ports for tenant
// sharing, and requires matching ports for installed platform collisions.
func assessRouteNames(ingresses []networkingv1.Ingress) routeNameFindings {
	byKey := map[string][]routeProducer{}
	for _, ing := range ingresses {
		for _, b := range routename.Backends(ing) {
			byKey[b.Key()] = append(byKey[b.Key()], routeProducer{backend: b, route: ing.Namespace + "/" + ing.Name})
		}
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var f routeNameFindings
	for _, key := range keys {
		producers := byKey[key]
		if platform, ok := routename.Platform(key); ok {
			live := false
			for _, p := range producers {
				if p.backend.Namespace == platform.Namespace && p.backend.Port == platform.Port {
					live = true
				}
			}
			for _, p := range producers {
				if p.backend.Namespace == platform.Namespace {
					continue
				}
				if live && p.backend.Port == platform.Port {
					f.PlatformCollisions = append(f.PlatformCollisions, p.route)
				} else if w := p.workload(); !slices.Contains(f.Occupied, w) {
					f.Occupied = append(f.Occupied, w)
				}
			}
			continue
		}
		// Normalization can merge names within one namespace, such as
		// "prod-web" and "prod--web".
		var workloads []string
		for _, p := range producers {
			if w := p.workload(); !slices.Contains(workloads, w) {
				workloads = append(workloads, w)
			}
		}
		if len(workloads) > 1 {
			sort.Strings(workloads)
			f.Shared = append(f.Shared, workloads)
		}
	}
	return f
}

func assessClusterRouteNames(ctx context.Context, clientset kubernetes.Interface) (routeNameFindings, error) {
	list, err := clientset.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return routeNameFindings{}, fmt.Errorf("listing Ingresses to check route names: %w", err)
	}
	return assessRouteNames(list.Items), nil
}

func checkRouteNames(ctx context.Context, clientset kubernetes.Interface, _ dynamic.Interface, out io.Writer, _ string) (int, error) {
	f, err := assessClusterRouteNames(ctx, clientset)
	if err != nil {
		return 0, err
	}
	printRouteNameFindings(out, f)
	return f.decisions(), nil
}

func printRouteNameFindings(out io.Writer, f routeNameFindings) {
	if f.empty() {
		_, _ = fmt.Fprintf(out, "  ✔  Route names: no collisions.\n")
		return
	}
	if len(f.PlatformCollisions) > 0 {
		_, _ = fmt.Fprintf(out, "  ✗   These routes conflict with an installed Kipper component on the same port.\n"+
			"      Requests can reach the wrong app or service. The upgrade removes conflicting\n"+
			"      app routes managed by Kipper. Use different app names to keep them reachable.\n"+
			"      Remove conflicting routes that Kipper does not manage yourself:\n")
		for _, r := range f.PlatformCollisions {
			_, _ = fmt.Fprintf(out, "      - %s\n", r)
		}
	}
	if len(f.Occupied) > 0 {
		_, _ = fmt.Fprintf(out, "  !   These apps or services use names reserved for Kipper components that are absent\n"+
			"      or use different ports. The upgrade keeps their routes. Request figures are unavailable.\n"+
			"      Rename these apps or services before installing those components:\n")
		for _, r := range f.Occupied {
			_, _ = fmt.Fprintf(out, "      - %s\n", r)
		}
	}
	if len(f.Shared) > 0 {
		_, _ = fmt.Fprintf(out, "  !   These apps or services have conflicting route names. The upgrade keeps their routes.\n"+
			"      If ports match, requests can reach the wrong app or service. Give each a distinct\n"+
			"      name, then run 'kip upgrade' to recheck names and allow new request figures.\n"+
			"      In the same namespace, a removed route cannot be recreated while the conflicting\n"+
			"      app or service remains:\n")
		for _, group := range f.Shared {
			_, _ = fmt.Fprintf(out, "      - %s\n", joinRoutes(group))
		}
	}
}

func joinRoutes(routes []string) string {
	s := ""
	for i, r := range routes {
		if i > 0 {
			s += " and "
		}
		s += r
	}
	return s
}

// routeNameConsent requires confirmation at a terminal or via --yes for platform
// collisions, since the upgraded console-api removes conflicting app routes.
// It runs before any upgrade changes.
func routeNameConsent(ctx context.Context, clientset kubernetes.Interface, out io.Writer, isTTY, assumeYes bool, confirm func() (bool, error)) error {
	f, err := assessClusterRouteNames(ctx, clientset)
	if err != nil {
		return err
	}
	if f.empty() {
		return nil
	}
	printRouteNameFindings(out, f)
	if f.decisions() == 0 || assumeYes {
		return nil
	}
	if !isTTY {
		return fmt.Errorf("%d route(s) collide with Kipper's own routes; rename those apps, or confirm their removal at a terminal or with --yes", f.decisions())
	}
	ok, err := confirm()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("upgrade stopped; nothing was changed")
	}
	return nil
}
