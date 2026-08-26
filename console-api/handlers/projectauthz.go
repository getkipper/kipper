package handlers

import (
	"net/http"

	corev1 "k8s.io/api/core/v1"

	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/controller/pkg/capability"
)

// projectResolver enforces project membership on routes that carry a namespace
// outside the /projects/{name} subtree (services, jobs, storage, bind, link).
// It is set once at startup by SetProjectResolver. When nil (unit tests that
// exercise handler logic without an auth context) enforcement is skipped;
// production always wires it in main.
var projectResolver *middleware.ProjectAccessResolver

// SetProjectResolver wires the shared project-access resolver. Call once at
// startup, before serving requests.
func SetProjectResolver(r *middleware.ProjectAccessResolver) { projectResolver = r }

// ProjectResolverWired reports whether the shared resolver has been set. Serving
// requests without it would skip project enforcement, so startup checks this
// and refuses to run rather than fail open.
func ProjectResolverWired() bool { return projectResolver != nil }

// ProjectResolver returns the shared resolver, or nil before startup has wired
// it. It is here so the controller manager can hand the resolver its cached
// client once the informers have synced, which is the only thing outside this
// package that needs the value rather than the answer.
func ProjectResolver() *middleware.ProjectAccessResolver { return projectResolver }

// enforceCapability reports whether the caller holds the capability on the
// project that owns the namespace. It writes a 401/403 and returns false when
// access is denied. Cluster admins pass for any namespace.
func enforceCapability(w http.ResponseWriter, r *http.Request, namespace string, required capability.Name) bool {
	if projectResolver == nil {
		return true
	}
	claims := middleware.UserFromContext(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	access, ok := projectResolver.Resolve(r.Context(), claims.Email, namespace)
	if !ok || !access.Allows(required) {
		respondError(w, http.StatusForbidden, "you do not have access to this project")
		return false
	}
	return true
}

// canAccessNamespace reports whether the caller may read the namespace, used to
// filter cluster-wide list responses. Admins and members return true.
func canAccessNamespace(r *http.Request, namespace string) bool {
	if projectResolver == nil {
		return true
	}
	claims := middleware.UserFromContext(r.Context())
	if claims == nil {
		return false
	}
	_, ok := projectResolver.Resolve(r.Context(), claims.Email, namespace)
	return ok
}

// filterPodsByAccess keeps only the pods in namespaces the caller can read,
// used to scope cluster-wide usage queries.
func filterPodsByAccess(r *http.Request, pods []corev1.Pod) []corev1.Pod {
	if projectResolver == nil {
		return pods
	}
	kept := make([]corev1.Pod, 0, len(pods))
	allowed := make(map[string]bool)
	for i := range pods {
		ns := pods[i].Namespace
		access, seen := allowed[ns]
		if !seen {
			access = canAccessNamespace(r, ns)
			allowed[ns] = access
		}
		if access {
			kept = append(kept, pods[i])
		}
	}
	return kept
}
