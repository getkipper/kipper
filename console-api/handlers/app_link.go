package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/go-chi/chi/v5"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
	"github.com/getkipper/kipper/console-api/middleware"
)

type linkRequest struct {
	Target    string `json:"target"`
	App       string `json:"app"`
	Namespace string `json:"namespace"`
	Public    bool   `json:"public,omitempty"`
}

type linkResponse struct {
	Target string `json:"target"`
	App    string `json:"app"`
	EnvVar string `json:"envVar"`
	URL    string `json:"url"`
	Public bool   `json:"public"`
}

// setAppLink records the target on the calling app, replacing any earlier link
// to the same app. The list is what this app depends on, and the reconciler
// turns the entries naming another namespace into egress.
//
// This mirrors the same-named helper in kip, which writes the App through the
// dynamic client from a module that cannot import this one. A link is two
// things written together — the variable the caller reads the target's address
// from, and the entry the reconciler turns into egress — and they have to move
// as a pair. An entry without a variable leaves an allowance the console's
// env-derived list cannot show or withdraw; a variable without an entry gives
// the caller an address nothing has opened a path to.
func setAppLink(app *kipperv1.App, target, targetNS string) {
	kept := make([]kipperv1.AppLink, 0, len(app.Spec.Links)+1)
	for _, link := range app.Spec.Links {
		if link.App == target {
			continue
		}
		kept = append(kept, link)
	}
	kept = append(kept, kipperv1.AppLink{App: target, Namespace: targetNS})
	app.Spec.Links = kept
}

// removeAppLink drops every link naming the target. Links are keyed by app name
// alone, as the env var is, so a target name used in two namespaces goes in one
// call — the same ambiguity the variable already has.
func removeAppLink(app *kipperv1.App, target string) {
	if len(app.Spec.Links) == 0 {
		return
	}
	kept := make([]kipperv1.AppLink, 0, len(app.Spec.Links))
	for _, link := range app.Spec.Links {
		if link.App == target {
			continue
		}
		kept = append(kept, link)
	}
	if len(kept) == 0 {
		app.Spec.Links = nil
		return
	}
	app.Spec.Links = kept
}

// Link injects a target app's URL into another app's environment.
// When public is true, uses the target's public route URL (for frontend apps).
// When public is false (default), uses the internal Kubernetes DNS URL.
// POST /api/v1/link
func (a *Apps) Link(w http.ResponseWriter, r *http.Request) {
	var req linkRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Target == "" || req.App == "" {
		respondError(w, http.StatusBadRequest, "target and app are required")
		return
	}

	if req.Target == req.App {
		respondError(w, http.StatusBadRequest, "an app cannot link to itself")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	ns := req.Namespace
	if ns == "" {
		ns = "default"
	}
	if !enforceProjectRole(w, r, ns, middleware.ProjectRoleDeployer) {
		return
	}

	// Look up the target app to get its port and route
	var targetCR kipperv1.App
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: ns, Name: req.Target}, &targetCR); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("target app %q not found in %s", req.Target, ns))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get target app")
		return
	}

	// Look up the calling app
	var appCR kipperv1.App
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: ns, Name: req.App}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found in %s", req.App, ns))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	envKey := controllers.AppEnvKey(req.Target)
	var url string

	if req.Public {
		// Use the target's public route URL. Resolve the host the same way the
		// route handler does, so a route whose host is derived from CLUSTER_DOMAIN
		// (no explicit custom domain — e.g. a coexist-migrated app, whose host is
		// left for the reconciler to derive) still yields a usable public URL
		// instead of reading as "no route".
		if targetCR.Spec.Route == nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("target app %q has no public route. Create one first", req.Target))
			return
		}
		host := a.resolveRouteHost(ctx, ns, req.Target, targetCR.Spec.Route.Host)
		if host == "" {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("target app %q has no public route. Create one first", req.Target))
			return
		}
		path := targetCR.Spec.Route.Path
		if path == "/" {
			path = ""
		}
		url = "https://" + host + path
	} else {
		// Use the internal Kubernetes DNS URL
		url = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", req.Target, ns, targetCR.Spec.Port)
	}

	// A public link is a plain environment variable and nothing more. The URL it
	// sets is for a browser, which no egress policy applies to, and there is no
	// declaration to derive it from — so this one is stored, and it withdraws
	// any internal link the app had to the same target.
	//
	// An internal link stores nothing. spec.links is the declaration and the
	// reconciler derives the address from it on every pass, so a target that
	// moves takes its callers with it instead of leaving them on an address that
	// was true once.
	//
	// If the operator already set that variable themselves, the link is refused
	// rather than taking the name. Deleting it destroys a value somebody chose —
	// a proxy, a path, an https endpoint — and leaving it is worse, because the
	// derived one is an explicit container env entry and would silently win over
	// the Secret while the editor went on showing theirs.
	if req.Public {
		if appCR.Spec.Env == nil {
			appCR.Spec.Env = make(map[string]string)
		}
		appCR.Spec.Env[envKey] = url
		removeAppLink(&appCR, req.Target)
	} else {
		if len(appCR.Spec.Links) >= controllers.MaxLinks {
			if !linkAlreadyDeclared(&appCR, req.Target) {
				respondError(w, http.StatusConflict, fmt.Sprintf(
					"%s already declares the most links an app may have (%d); unlink one first",
					req.App, controllers.MaxLinks))
				return
			}
		}
		if _, taken := appCR.Spec.Env[envKey]; taken {
			respondError(w, http.StatusConflict, fmt.Sprintf(
				"%s is already set on %s, and a link to %q would set it too; remove it first if the link should own it",
				envKey, req.App, req.Target))
			return
		}
		setAppLink(&appCR, req.Target, ns)
	}

	if err := a.CRClient.Update(ctx, &appCR); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to update app environment: %v", err))
		return
	}

	respondJSON(w, http.StatusOK, linkResponse{
		Target: req.Target,
		App:    req.App,
		EnvVar: envKey,
		URL:    url,
		Public: req.Public,
	})
}

// Unlink removes a linked app's URL from another app's environment.
// POST /api/v1/unlink
func (a *Apps) Unlink(w http.ResponseWriter, r *http.Request) {
	var req linkRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Target == "" || req.App == "" {
		respondError(w, http.StatusBadRequest, "target and app are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	ns := req.Namespace
	if ns == "" {
		ns = "default"
	}
	if !enforceProjectRole(w, r, ns, middleware.ProjectRoleDeployer) {
		return
	}

	var appCR kipperv1.App
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: ns, Name: req.App}, &appCR); err != nil {
		respondError(w, http.StatusOK, "no links found")
		return
	}

	envKey := controllers.AppEnvKey(req.Target)
	if appCR.Spec.Env != nil {
		delete(appCR.Spec.Env, envKey)
	}
	// The declared dependency goes with the variable. Dropping only the variable
	// leaves the reconciler rebuilding the egress it opened on every pass, and
	// the console derives what it shows from the variables — so the allowance
	// would stand with nothing on either surface admitting it was there.
	removeAppLink(&appCR, req.Target)

	if err := a.CRClient.Update(ctx, &appCR); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update app environment")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "unlinked"})
}

// appLink is one declared dependency, whether it carries traffic, and the
// address it resolves to.
type appLink struct {
	App       string `json:"app"`
	Namespace string `json:"namespace"`
	EnvVar    string `json:"envVar"`
	URL       string `json:"url,omitempty"`
	// Open is whether this link resolves: the target project consents, the
	// target exists and serves a port, and no other link claims its variable.
	Open bool `json:"open"`
	// Reason says why a link that does not resolve does not, empty when it does.
	Reason string `json:"reason,omitempty"`
	// Injected is whether the address has reached the app's Deployment yet. That
	// is the pod template rather than the running pods: a link can resolve
	// before the workload has been written, and a rollout that is stuck or slow
	// leaves pods on the previous template while this reads true. It separates
	// "resolved but not yet written" from "closed", which is the distinction
	// worth having; it does not prove any pod is running with the address.
	Injected bool `json:"injected"`
}

// Links lists what this app declares it reaches, whether each carries traffic,
// and the address it resolves to.
//
// Whether a link is open comes from the same resolver the reconciler builds the
// egress policy and the pod's environment from, so the console cannot report a
// link as carrying traffic that the cluster refuses, or the reverse. The
// Deployment is read only to say whether the address has reached the workload
// yet, which is its own fact and is reported as its own field.
//
// GET /api/v1/projects/{name}/apps/{app}/links
func (a *Apps) Links(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "name")
	name := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: name}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, []appLink{})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to read the app")
		return
	}

	live, blocked, err := controllers.ResolveLinks(ctx, a.CRClient, &appCR)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to resolve this app's links")
		return
	}
	resolved := make(map[string]controllers.ResolvedLink, len(live))
	for _, l := range live {
		resolved[l.Link.Namespace+"/"+l.Link.App] = l
	}
	reasons := make(map[string]string, len(blocked))
	for _, b := range blocked {
		// Each reads "<namespace>/<app> (why)".
		if open := strings.Index(b, " ("); open > 0 {
			reasons[b[:open]] = strings.TrimSuffix(b[open+2:], ")")
		}
	}

	// A Deployment that is not there yet is a real answer: nothing has been
	// injected. Any other read failure is not an answer at all.
	injected := map[string]string{}
	deploy, derr := a.Client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(derr):
	case derr != nil:
		respondError(w, http.StatusInternalServerError, "failed to read the app's workload")
		return
	default:
		if len(deploy.Spec.Template.Spec.Containers) > 0 {
			for _, ev := range deploy.Spec.Template.Spec.Containers[0].Env {
				injected[ev.Name] = ev.Value
			}
		}
	}

	links := make([]appLink, 0, len(appCR.Spec.Links))
	for _, link := range appCR.Spec.Links {
		key := controllers.AppEnvKey(link.App)
		id := link.Namespace + "/" + link.App
		entry := appLink{App: link.App, Namespace: link.Namespace, EnvVar: key}
		if l, ok := resolved[id]; ok {
			entry.Open = true
			entry.URL = l.URL()
		} else {
			entry.Reason = reasons[id]
		}
		_, entry.Injected = injected[key]
		links = append(links, entry)
	}
	respondJSON(w, http.StatusOK, links)
}

// linkAlreadyDeclared reports whether this app already links to the target, in
// which case setting it replaces an entry rather than adding one and the limit
// is not reached by doing so.
func linkAlreadyDeclared(app *kipperv1.App, target string) bool {
	for _, link := range app.Spec.Links {
		if link.App == target {
			return true
		}
	}
	return false
}
