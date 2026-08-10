package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
	"github.com/getkipper/kipper/console-api/domain"
)

// RouteGroups provides handlers for managing route groups (shared-domain routing).
type RouteGroups struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
	Domain   string
}

type pathMapping struct {
	Path string `json:"path"`
	App  string `json:"app"`
}

type routeGroupRequest struct {
	Host     string        `json:"host,omitempty"` // empty = auto-generate
	Mappings []pathMapping `json:"mappings"`
}

type routeGroupResult struct {
	Host     string        `json:"host"`
	URL      string        `json:"url"`
	Mappings []pathMapping `json:"mappings"`
}

// Create sets up a route group — multiple apps sharing a single domain with path-based routing.
// POST /api/v1/projects/{name}/route-groups
func (rg *RouteGroups) Create(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")

	var req routeGroupRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if len(req.Mappings) == 0 {
		respondError(w, http.StatusBadRequest, "at least one path mapping is required")
		return
	}

	for i := range req.Mappings {
		req.Mappings[i].Path = normalizeRoutePath(req.Mappings[i].Path)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	host := req.Host
	if host == "" && rg.Domain != "" {
		// Use the first app's name as the route group name, include environment
		ns, nsErr := rg.Client.CoreV1().Namespaces().Get(ctx, project, metav1.GetOptions{})
		env := ""
		if nsErr == nil {
			env = ns.Labels["kipper.run/environment"]
		}
		groupName := req.Mappings[0].App
		host = domain.SubdomainFor(controllers.AppHostPrefix(groupName, env), rg.Domain)
	}

	if host == "" {
		respondError(w, http.StatusBadRequest, "could not determine hostname — provide one explicitly or configure CLUSTER_DOMAIN")
		return
	}

	// Set the route on each app
	for _, m := range req.Mappings {
		if err := rg.setAppRoute(ctx, project, m.App, host, m.Path); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to set route on %s: %v", m.App, err))
			return
		}
	}

	respondJSON(w, http.StatusOK, routeGroupResult{
		Host:     host,
		URL:      "https://" + host,
		Mappings: req.Mappings,
	})
}

// Update modifies a route group's path mappings.
// PUT /api/v1/projects/{name}/route-groups
func (rg *RouteGroups) Update(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")

	var req routeGroupRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Host == "" {
		respondError(w, http.StatusBadRequest, "host is required for updates")
		return
	}

	for i := range req.Mappings {
		req.Mappings[i].Path = normalizeRoutePath(req.Mappings[i].Path)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// Find all apps currently using this host and remove their routes
	var appList kipperv1.AppList
	if err := rg.CRClient.List(ctx, &appList, crclient.InNamespace(project)); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list apps")
		return
	}

	// Track which apps should keep their routes
	keepApps := make(map[string]bool)
	for _, m := range req.Mappings {
		keepApps[m.App] = true
	}

	// Remove route from apps no longer in the group
	for i := range appList.Items {
		app := &appList.Items[i]
		if app.Spec.Route != nil && app.Spec.Route.Host == req.Host {
			if !keepApps[app.Name] {
				app.Spec.Route = nil
				_ = rg.CRClient.Update(ctx, app)
			}
		}
	}

	// Set/update routes on current mappings
	for _, m := range req.Mappings {
		if err := rg.setAppRoute(ctx, project, m.App, req.Host, m.Path); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to set route on %s: %v", m.App, err))
			return
		}
	}

	respondJSON(w, http.StatusOK, routeGroupResult{
		Host:     req.Host,
		URL:      "https://" + req.Host,
		Mappings: req.Mappings,
	})
}

// Delete removes a route group — clears the route from all apps using this host.
// DELETE /api/v1/projects/{name}/route-groups/{host}
func (rg *RouteGroups) Delete(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	host := chi.URLParam(r, "host")

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	var appList kipperv1.AppList
	if err := rg.CRClient.List(ctx, &appList, crclient.InNamespace(project)); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list apps")
		return
	}

	for i := range appList.Items {
		app := &appList.Items[i]
		if app.Spec.Route != nil && app.Spec.Route.Host == host {
			app.Spec.Route = nil
			_ = rg.CRClient.Update(ctx, app)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func (rg *RouteGroups) setAppRoute(ctx context.Context, namespace, appName, host, path string) error {
	var app kipperv1.App
	if err := rg.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: appName}, &app); err != nil {
		return fmt.Errorf("app %q not found", appName)
	}

	path = normalizeRoutePath(path)

	if app.Spec.Route == nil {
		app.Spec.Route = &kipperv1.AppRoute{}
	}
	app.Spec.Route.Host = host
	app.Spec.Route.Path = path

	return rg.CRClient.Update(ctx, &app)
}

// normalizeRoutePath cleans a user-supplied route path so it can never
// produce a route Traefik won't match. A stray space or missing leading
// slash (easy to introduce by hand in the console) would otherwise be
// stored verbatim and silently break path routing.
func normalizeRoutePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}
