package middleware

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/console-api/internal/nsowner"
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
	owners  nsowner.Reader
}

// NewProjectAccessResolver builds a resolver over the given clients.
func NewProjectAccessResolver(client kubernetes.Interface, roles *RoleStore, members ProjectMemberSource, owners nsowner.Reader) *ProjectAccessResolver {
	return &ProjectAccessResolver{
		client:  client,
		roles:   roles,
		members: members,
		owners:  owners,
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

// projectForName maps a namespace to the project that owns it.
//
// An existing namespace is resolved through the shared owner lookup, because
// the label that used to answer this is writable by anyone who can write a
// namespace, which made every gated route's authority rest on a value the
// caller might have set. What that lookup requires, and the release it starts
// requiring the claim, is stated once in nsowner.Of.
//
// When no namespace by that name exists the name is returned unchanged, which
// is what a caller naming a project by its own name before its namespace exists
// relies on. Nothing is being trusted there: no metadata was read, and the
// caller named the project outright.
//
// There is no cache. The TTL map that used to sit here held an answer for a
// minute, so a namespace whose ownership had been withdrawn kept authorising
// for the rest of it, and carrying the claim in the cached value would not have
// helped because nothing invalidated it.
func (r *ProjectAccessResolver) projectForName(ctx context.Context, name string) (string, error) {
	project, ok, err := nsowner.Of(ctx, r.owners, name)
	if err != nil {
		return "", err
	}
	if ok {
		return project, nil
	}

	// Not owned. Either the namespace is nobody's, or there is no namespace by
	// that name and the caller is naming a project.
	if _, err := r.client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return name, nil
		}
		return "", err
	}
	return "", nil
}
