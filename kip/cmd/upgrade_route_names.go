package cmd

import (
	"context"
	"fmt"
	"io"
	"sort"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/routename"
)

// routeNameFindings groups routes that share normalized backend keys.
type routeNameFindings struct {
	// PlatformCollisions match an installed platform backend's key and port.
	// The App reconciler removes matching routes it owns.
	PlatformCollisions []string
	// Occupied are tenant routes on the name of an optional Kipper route that
	// is not installed, or installed on another port. They keep working.
	Occupied []string
	// Shared lists, per colliding name, the tenant routes that share it. They
	// keep their routes and are reported.
	Shared [][]string
}

func (f routeNameFindings) decisions() int {
	return len(f.PlatformCollisions)
}

func (f routeNameFindings) empty() bool {
	return len(f.PlatformCollisions) == 0 && len(f.Occupied) == 0 && len(f.Shared) == 0
}

// assessRouteNames groups Ingress backends by normalized key, ignoring port
// for tenant sharing and comparing it for installed platform collisions.
func assessRouteNames(ingresses []networkingv1.Ingress) routeNameFindings {
	type producer struct {
		backend routename.Backend
		route   string
	}
	byKey := map[string][]producer{}
	for _, ing := range ingresses {
		for _, b := range routename.Backends(ing) {
			byKey[b.Key()] = append(byKey[b.Key()], producer{backend: b, route: ing.Namespace + "/" + ing.Name})
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
				} else {
					f.Occupied = append(f.Occupied, p.route)
				}
			}
			continue
		}
		// Reserve the key across ports, including differently spelled names
		// in one namespace such as "prod-web" and "prod--web".
		services := map[string]bool{}
		var routes []string
		for _, p := range producers {
			services[p.backend.Namespace+"/"+p.backend.Service] = true
			routes = append(routes, p.route)
		}
		if len(services) > 1 {
			sort.Strings(routes)
			f.Shared = append(f.Shared, routes)
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
		_, _ = fmt.Fprintf(out, "  ✗   These routes have the same traffic name as one of Kipper's own routes, so Traefik\n"+
			"      sends one host's requests to the other. The new console-api removes the routes of Kipper apps\n"+
			"      among them so Kipper's route works; rename each app to keep it reachable. An Ingress that\n"+
			"      Kipper does not manage is not removed and needs deleting by whoever created it:\n")
		for _, r := range f.PlatformCollisions {
			_, _ = fmt.Fprintf(out, "      - %s\n", r)
		}
	}
	if len(f.Occupied) > 0 {
		_, _ = fmt.Fprintf(out, "  !   These routes use the traffic name of an optional Kipper route that is not installed,\n"+
			"      or installed on another port. They keep working, show no traffic, and that Kipper route\n"+
			"      cannot be installed until they are renamed:\n")
		for _, r := range f.Occupied {
			_, _ = fmt.Fprintf(out, "      - %s\n", r)
		}
	}
	if len(f.Shared) > 0 {
		_, _ = fmt.Fprintf(out, "  !   These routes share a traffic name, so Traefik can send their requests to one of them.\n"+
			"      The upgrade keeps them as they are. Rename one app of each pair; traffic shows again after the\n"+
			"      next console-api start, which every 'kip upgrade' causes. Within one namespace, a route\n"+
			"      removed from one of the pair cannot come back under that name:\n")
		for _, routes := range f.Shared {
			_, _ = fmt.Fprintf(out, "      - %s\n", joinRoutes(routes))
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

// routeNameConsent runs before the upgrade changes anything. A Kipper app
// route that collides with a live Kipper route is removed by the new
// console-api, so the operator confirms that at a terminal or with --yes;
// otherwise the upgrade stops.
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
