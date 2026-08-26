package middleware

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/getkipper/kipper/controller/pkg/capability"
)

const projectAccessKey = contextKey("project-access")

// ProjectAccessFromContext returns the caller's resolved project access.
func ProjectAccessFromContext(ctx context.Context) (ProjectAccess, bool) {
	access, ok := ctx.Value(projectAccessKey).(ProjectAccess)
	return access, ok
}

// WithProjectAccess returns ctx carrying access, as the project-scope
// middleware stores it. The key is unexported so that only this package decides
// the shape; a handler test driving a handler directly still has to put a
// caller's authority in front of it.
func WithProjectAccess(ctx context.Context, access ProjectAccess) context.Context {
	return context.WithValue(ctx, projectAccessKey, access)
}

// ProjectScope resolves the caller's access to the {name} project and stores it
// in the request context, rejecting callers who are not members of it.
//
// The /projects/{name} subtree is mixed: the segment is a project name on the
// routes that act on the Project itself (members, quota, environments, link
// consent, promote, delete) and an environment namespace on the routes that act
// on workloads inside one (apps, functions, volumes, api-keys, route-groups).
// Mount this on the first kind and NamespaceScope on the second. The two are not
// interchangeable, because a namespace and a project can carry the same name
// while belonging to different projects.
func ProjectScope(resolver *ProjectAccessResolver) func(http.Handler) http.Handler {
	return scopeBy(resolver.ResolveProject)
}

// NamespaceScope resolves the caller's access to the environment namespace named
// by {name}, through the project that owns it, and stores it in the request
// context. Mount it on the /projects/{name} routes whose handlers act inside a
// namespace rather than on the Project — see ProjectScope for the split.
func NamespaceScope(resolver *ProjectAccessResolver) func(http.Handler) http.Handler {
	return scopeBy(resolver.Resolve)
}

// scopeBy is the shared body of the two {name} scopes. They differ only in what
// the segment means, and keeping one implementation means a change to the gate
// cannot land on one kind of route and miss the other.
func scopeBy(resolve func(context.Context, string, string) (ProjectAccess, bool)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := UserFromContext(r.Context())
			if claims == nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			access, ok := resolve(r.Context(), claims.Email, chi.URLParam(r, "name"))
			if !ok {
				http.Error(w, "forbidden: you do not have access to this project", http.StatusForbidden)
				return
			}

			ctx := context.WithValue(r.Context(), projectAccessKey, access)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ProjectScopeQuery is ProjectScope for routes that identify their namespace
// through the ?namespace= query parameter instead of the {name} path segment
// (services, jobs). It rejects non-members and callers who omit the namespace,
// then stores the resolved access for RequireCapability.
func ProjectScopeQuery(resolver *ProjectAccessResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := UserFromContext(r.Context())
			if claims == nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			namespace := r.URL.Query().Get("namespace")
			if namespace == "" {
				http.Error(w, "namespace query parameter is required", http.StatusBadRequest)
				return
			}
			access, ok := resolver.Resolve(r.Context(), claims.Email, namespace)
			if !ok {
				http.Error(w, "forbidden: you do not have access to this project", http.StatusForbidden)
				return
			}

			ctx := context.WithValue(r.Context(), projectAccessKey, access)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireCapability gates a route on one capability. It must run after
// ProjectScope, NamespaceScope or ProjectScopeQuery, which is what resolved the
// caller's standing in the project.
//
// Which capability a route takes is also declared in routeAuthz. Nothing checks
// the router against that declaration — the wrappers are closures and chi.Walk
// cannot see inside them — so the two are kept in step by review. The route
// matrix catches a route wired to a capability of a different level; one wired
// to another capability of the same level nothing catches, until a member can
// hold exactly one.
func RequireCapability(required capability.Name) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			access, ok := ProjectAccessFromContext(r.Context())
			if !ok || !access.Allows(required) {
				http.Error(w, "forbidden: insufficient project permissions", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
