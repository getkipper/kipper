package handlers

import (
	"context"
	"net/http"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
	"github.com/getkipper/kipper/console-api/domain"
)

// Routes serves the cluster-wide list of configured public routes.
//
// The list is built from App.Spec.Route across all Kipper-managed
// namespaces — the same field the Settings tab on an app reads — so the
// two views can never disagree about which routes are configured.
// Whether a route is actually serving traffic (Ingress reconciled, TLS
// certificate issued) is reported separately via the shared routeHealth
// helper.
type Routes struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
	// Domain is the cluster's CLUSTER_DOMAIN. It is used to derive the
	// host for App routes that leave Spec.Route.Host empty — matching
	// what the App reconciler does so those routes still appear in the
	// listing.
	Domain string
}

type routeEntry struct {
	Path    string      `json:"path"`
	Service string      `json:"service"`
	Port    int32       `json:"port"`
	App     string      `json:"app"`
	Health  RouteHealth `json:"health"`
}

type routeGroupResponse struct {
	Name        string       `json:"name"`
	Namespace   string       `json:"namespace"`
	Host        string       `json:"host"`
	TLS         bool         `json:"tls"`
	Project     string       `json:"project"`
	Environment string       `json:"environment"`
	Routes      []routeEntry `json:"routes"`
	// Health is the aggregate state for the host. Group is ready only
	// when every member app's Ingress is ready; group TLS is ready only
	// when every member's certificate is issued. This way the host-level
	// badge in the UI reflects shared-host groups honestly.
	Health RouteHealth `json:"health"`
}

// List returns every configured public route, grouped by host. Each group
// contains one entry per app that maps a path on that host. Configuration
// comes from App.Spec.Route; live cluster state (Ingress + TLS) comes from
// the routeHealth helper.
//
// GET /api/v1/routes
func (rt *Routes) List(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appList kipperv1.AppList
	if err := rt.CRClient.List(ctx, &appList); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list apps")
		return
	}

	// Namespace labels are used only to decorate the response with
	// project/environment fields. We don't gate inclusion on the namespace
	// being kipper-labeled because the default namespace (and any user
	// namespace that pre-existed Kipper) won't carry the label even
	// though its App CRs and Ingresses do. The App CR is the source of
	// truth for which routes belong to the platform.
	namespaces, _ := rt.Client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	nsLabels := make(map[string]map[string]string)
	for _, ns := range namespaces.Items {
		nsLabels[ns.Name] = ns.Labels
	}

	type groupKey struct{ namespace, host string }
	grouped := make(map[groupKey]*routeGroupResponse)
	var order []groupKey

	for i := range appList.Items {
		app := &appList.Items[i]
		route := app.Spec.Route
		if route == nil {
			continue
		}
		// Only show routes in projects the caller belongs to.
		if !canAccessNamespace(r, app.Namespace) {
			continue
		}
		labels := nsLabels[app.Namespace]

		host := rt.resolveRouteHost(app, route, labels)
		if host == "" {
			// Spec.Route is set but no host can be derived. The
			// reconciler won't create an Ingress either, so there
			// is nothing to display.
			continue
		}

		key := groupKey{app.Namespace, host}
		path := route.Path
		if path == "" {
			path = "/"
		}

		group, exists := grouped[key]
		if !exists {
			group = &routeGroupResponse{
				Name:        app.Name,
				Namespace:   app.Namespace,
				Host:        host,
				TLS:         true,
				Project:     labels["kipper.run/project"],
				Environment: labels["kipper.run/environment"],
			}
			grouped[key] = group
			order = append(order, key)
		}

		group.Routes = append(group.Routes, routeEntry{
			Path:    path,
			Service: app.Name,
			Port:    app.Spec.Port,
			App:     app.Name,
		})
	}

	// Sort path entries within each group for stable output; root path
	// first, then alphabetical, so the primary app stays on top.
	for _, g := range grouped {
		sort.SliceStable(g.Routes, func(i, j int) bool {
			if g.Routes[i].Path == "/" {
				return true
			}
			if g.Routes[j].Path == "/" {
				return false
			}
			return g.Routes[i].Path < g.Routes[j].Path
		})
	}

	groups := make([]routeGroupResponse, 0, len(order))
	for _, key := range order {
		g := grouped[key]
		for i := range g.Routes {
			g.Routes[i].Health = routeHealth(ctx, rt.Client, key.namespace, g.Routes[i].App, key.host)
		}
		g.Health = aggregateRouteHealth(g.Routes)
		groups = append(groups, *g)
	}

	respondJSON(w, http.StatusOK, groups)
}

// resolveRouteHost returns the effective host for an app's route. When
// Spec.Route.Host is set explicitly that wins; otherwise the host is
// derived from CLUSTER_DOMAIN and the namespace's environment label, the
// same way the App reconciler does it — so routes with implicit hosts
// still show up in the listing.
func (rt *Routes) resolveRouteHost(app *kipperv1.App, route *kipperv1.AppRoute, nsLabels map[string]string) string {
	if route.Host != "" {
		return route.Host
	}
	if rt.Domain == "" {
		return ""
	}
	prefix := controllers.AppHostPrefix(app.Name, nsLabels["kipper.run/environment"])
	return domain.SubdomainFor(prefix, rt.Domain)
}

// aggregateRouteHealth folds the per-app health entries in a route group
// into a single host-level summary. Ingress and TLS are reported as ready
// only when every member is ready; the message surfaces the first member
// that isn't, so users can see what's holding the group back.
func aggregateRouteHealth(entries []routeEntry) RouteHealth {
	if len(entries) == 0 {
		return RouteHealth{}
	}
	out := RouteHealth{IngressReady: true, TLSReady: true}
	for _, e := range entries {
		if !e.Health.IngressReady {
			out.IngressReady = false
			if out.Message == "" {
				out.Message = e.Health.Message
			}
		}
		if !e.Health.TLSReady {
			out.TLSReady = false
			if out.Message == "" {
				out.Message = e.Health.Message
			}
		}
	}
	if out.IngressReady && out.TLSReady {
		out.Message = entries[0].Health.Message
	}
	return out
}
