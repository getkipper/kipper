package middleware

import (
	"context"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

// ProjectMemberSource returns a project's members as an email->role map.
// found is false when the project does not exist.
type ProjectMemberSource interface {
	ProjectMembers(ctx context.Context, project string) (members map[string]string, found bool, err error)
}

// ProjectAccess is the outcome of resolving a user's access to a namespace.
type ProjectAccess struct {
	// Project is the Kipper project that owns the namespace. It is empty for
	// an admin acting on a namespace that no project owns.
	Project string
	// Role is the caller's capability: owner, deployer, or viewer. Admins
	// resolve to owner.
	Role string
	// IsAdmin is true when access comes from the cluster-wide admin role.
	IsAdmin bool
}

// Project role names, ordered viewer < deployer < owner.
const (
	ProjectRoleViewer   = "viewer"
	ProjectRoleDeployer = "deployer"
	ProjectRoleOwner    = "owner"
)

var projectRoleRank = map[string]int{
	ProjectRoleViewer:   1,
	ProjectRoleDeployer: 2,
	ProjectRoleOwner:    3,
}

// Allows reports whether the resolved access meets the required project role.
func (a ProjectAccess) Allows(required string) bool {
	return projectRoleRank[a.Role] >= projectRoleRank[required] && projectRoleRank[required] > 0
}

// ProjectAccessResolver decides whether a user may act on a namespace and at
// what project role. Cluster admins are allowed on any namespace; everyone
// else must be a member of the Kipper project that owns it. A namespace with
// no Kipper project label is unreachable to non-admins, which keeps system
// namespaces out of reach.
type ProjectAccessResolver struct {
	client  kubernetes.Interface
	roles   *RoleStore
	members ProjectMemberSource

	mu     sync.RWMutex
	nsProj map[string]nsCacheEntry
	ttl    time.Duration
}

// nsCacheMax bounds the namespace->project cache so an authenticated caller
// cannot grow it without limit by probing random names.
const nsCacheMax = 4096

type nsCacheEntry struct {
	project string
	at      time.Time
}

// NewProjectAccessResolver builds a resolver over the given clients.
func NewProjectAccessResolver(client kubernetes.Interface, roles *RoleStore, members ProjectMemberSource) *ProjectAccessResolver {
	return &ProjectAccessResolver{
		client:  client,
		roles:   roles,
		members: members,
		nsProj:  make(map[string]nsCacheEntry),
		ttl:     60 * time.Second,
	}
}

// Resolve returns the user's access to a namespace, traced back to the project
// that owns it. ok is false when access is denied: unknown user, a namespace no
// Kipper project owns (for non-admins), or a user who is not a member of the
// owning project.
//
// Use this only where the caller genuinely names a namespace. A route that
// names a project wants ResolveProject, because a namespace and a project can
// carry the same name while belonging to different projects.
func (r *ProjectAccessResolver) Resolve(ctx context.Context, email, namespace string) (ProjectAccess, bool) {
	globalRole := r.roles.GetRole(email)
	if globalRole == "" {
		return ProjectAccess{}, false
	}

	project, err := r.projectForName(ctx, namespace)
	if err != nil {
		return ProjectAccess{}, false
	}
	return r.resolveProject(ctx, email, project, globalRole)
}

// ResolveProject returns the user's access to a project named directly, as the
// project-level /projects/{name} routes name it. ok is false for an unknown
// user, or for a user who is not a member of that project — including when the
// project does not exist, which is indistinguishable from it by design, so a
// probe cannot tell one from the other. A cluster admin passes for any name,
// existing or not, exactly as they do on any namespace.
//
// The name is taken as the project's own and never traced through a namespace
// label. Namespace names are not unique across projects — project "shop" with
// an environment "prod" and project "shop-prod" with a default environment both
// resolve to the namespace "shop-prod" — so reading the label would answer with
// whichever project owns the namespace of that name while the handler behind
// the route goes on to act on the Project of that name. Those are different
// resources, and the gap between them is one project's owner holding authority
// over another's. Routes whose {name} is a namespace want Resolve.
func (r *ProjectAccessResolver) ResolveProject(ctx context.Context, email, project string) (ProjectAccess, bool) {
	return r.resolveProject(ctx, email, project, r.roles.GetRole(email))
}

// resolveProject is the shared tail of both entry points, taking the caller's
// global role as one snapshot so a role changing mid-request cannot be read
// twice with different answers.
func (r *ProjectAccessResolver) resolveProject(ctx context.Context, email, project, globalRole string) (ProjectAccess, bool) {
	if globalRole == "" {
		return ProjectAccess{}, false
	}

	if globalRole == RoleAdmin {
		return ProjectAccess{Project: project, Role: ProjectRoleOwner, IsAdmin: true}, true
	}

	if project == "" {
		return ProjectAccess{}, false
	}

	members, found, err := r.members.ProjectMembers(ctx, project)
	if err != nil || !found {
		return ProjectAccess{}, false
	}
	role, ok := members[email]
	if !ok || projectRoleRank[role] == 0 {
		return ProjectAccess{}, false
	}
	return ProjectAccess{Project: project, Role: role}, true
}

// projectForName maps a namespace to the project that owns it. When a namespace
// by that name exists, the project comes from its Kipper project label (empty
// for a namespace no project owns, e.g. a system namespace). When no such
// namespace exists the name is returned unchanged, which is what a caller
// naming a project's own namespace before it is created relies on. Results are
// cached for a short TTL.
func (r *ProjectAccessResolver) projectForName(ctx context.Context, name string) (string, error) {
	r.mu.RLock()
	entry, ok := r.nsProj[name]
	r.mu.RUnlock()
	if ok && time.Since(entry.at) < r.ttl {
		return entry.project, nil
	}

	var project string
	ns, err := r.client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		project = ns.Labels[kipperlabels.Project]
	case apierrors.IsNotFound(err):
		project = name
	default:
		return "", err
	}

	r.mu.Lock()
	if len(r.nsProj) >= nsCacheMax {
		// Evict one entry to keep the cache bounded. Map iteration order is
		// random, so this is a cheap random eviction; the TTL ages the rest.
		for k := range r.nsProj {
			delete(r.nsProj, k)
			break
		}
	}
	r.nsProj[name] = nsCacheEntry{project: project, at: time.Now()}
	r.mu.Unlock()
	return project, nil
}
