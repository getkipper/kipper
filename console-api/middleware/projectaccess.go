package middleware

import (
	"context"
	"fmt"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/console-api/internal/nsowner"
	"github.com/getkipper/kipper/controller/pkg/capability"
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
	// Role is the caller's project role: owner, deployer, or viewer. Admins
	// resolve to owner, which holds the whole catalogue. What the role may do
	// is Allows's question, and the catalogue's answer.
	Role string
	// IsAdmin is true when access comes from the cluster-wide admin role.
	IsAdmin bool
}

// The built-in project roles. They are the names stored on a Project and the
// names an operator types, so they are part of the released surface. What each
// one may do is the catalogue's to say, not this package's.
const (
	ProjectRoleViewer   = "viewer"
	ProjectRoleDeployer = "deployer"
	ProjectRoleOwner    = "owner"
)

// Allows reports whether the resolved access carries a capability.
//
// It asks the catalogue rather than comparing roles. The three built-ins nest,
// so while they are the only roles a Project can hold this answers what a
// viewer<deployer<owner comparison did; what it stops doing is deciding from
// the ordering, which is what made a fourth role unrepresentable. A role the
// catalogue does not know holds nothing and is refused everywhere, which is the
// same answer the reconciler gives it.
func (a ProjectAccess) Allows(required capability.Name) bool {
	return capability.Holds(capability.Role(a.Role), required)
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

	// owners answers which project holds a namespace, on every gated request.
	// It starts as a direct client and is swapped for the controller manager's
	// cached one when that has synced, which turns two live API calls per
	// request into two informer reads.
	//
	// The swap is guarded because it happens while requests are in flight: the
	// manager syncs in its own goroutine some time after serving starts.
	ownersMu sync.RWMutex
	owners   nsowner.Reader
	// ownersRetired records that a cache has been withdrawn, so a later offer
	// of one cannot put it back. Two goroutines offer readers and neither
	// orders against the other: the manager's cache arrives when its informers
	// sync, and the live client comes back when the manager stops. A mutex
	// makes each swap atomic and says nothing about which happens last, so
	// without this a cache offered a moment before the manager died could be
	// installed a moment after, and nothing would ever take it out again.
	ownersRetired bool
	// ownersGen counts swaps. A lookup takes the reader and the count together
	// and checks the count again afterwards: the lock makes taking the reader
	// atomic, and nothing else stops a request that took the manager's cache
	// from finishing its reads through it after that cache was withdrawn.
	ownersGen uint64
}

// UseOwners swaps in a reader for resolving namespace ownership.
//
// Nothing waits for this. Authorization is correct through either reader, so a
// cluster whose controller manager never starts — one whose CRDs have not been
// applied yet, which this image is documented to survive — keeps authorizing
// through the client it began with rather than refusing every request until an
// informer that will never sync does so.
func (r *ProjectAccessResolver) UseOwners(owners nsowner.Reader) {
	if owners == nil {
		return
	}
	r.ownersMu.Lock()
	defer r.ownersMu.Unlock()
	if r.ownersRetired {
		// The only cache this is ever offered belongs to the manager that has
		// since stopped. Installing it now would freeze ownership at whatever
		// its informers last saw, with no later event to undo it.
		return
	}
	r.owners = owners
	r.ownersGen++
}

// RetireOwners goes back to reading live and refuses any cache offered
// afterwards.
//
// The caller is the manager's own exit. A stopped manager's informers keep
// answering from the stores they last filled and answer without an error, so
// leaving one in place freezes ownership there while membership stays current:
// a namespace that changes hands afterwards keeps authorizing its old
// project's members until the process restarts.
func (r *ProjectAccessResolver) RetireOwners(live nsowner.Reader) {
	r.ownersMu.Lock()
	defer r.ownersMu.Unlock()
	r.ownersRetired = true
	if live != nil {
		r.owners = live
	}
	r.ownersGen++
}

// ownerReader is the reader to resolve with now, and the swap count it came
// from.
func (r *ProjectAccessResolver) ownerReader() (nsowner.Reader, uint64) {
	r.ownersMu.RLock()
	defer r.ownersMu.RUnlock()
	return r.owners, r.ownersGen
}

// ownersChangedSince reports whether the reader has been swapped since a lookup
// took it.
func (r *ProjectAccessResolver) ownersChangedSince(gen uint64) bool {
	r.ownersMu.RLock()
	defer r.ownersMu.RUnlock()
	return r.ownersGen != gen
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
	if !ok || !capability.KnownRole(capability.Role(role)) {
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
// This keeps no cache of its own. The TTL map that used to sit here held an
// answer for a minute, so a namespace whose ownership had been withdrawn kept
// authorising for the rest of it, and carrying the claim in the cached value
// would not have helped because nothing invalidated it.
//
// The reader underneath may be an informer, which is a cache with a watch on
// it: it goes stale only for as long as a watch takes to deliver, and it is
// swapped back to a live client if the manager driving it stops.
func (r *ProjectAccessResolver) projectForName(ctx context.Context, name string) (string, error) {
	// Read, then check the reader has not been swapped underneath. Taking it is
	// atomic and the lookup is not: a request that took the manager's cache and
	// was descheduled would otherwise finish both its reads through a cache
	// whose manager had since stopped, and answer from whatever it last saw.
	// It retries until the reader it read through is still the current one. A
	// process swaps at most twice, the cache arriving and the cache being
	// withdrawn, so this settles. The bound is a backstop, and running out of
	// attempts refuses rather than returning an answer already known to have
	// come from a reader that had been replaced.
	var project string
	var ok bool
	settled := false
	for attempt := 0; attempt < 4 && !settled; attempt++ {
		owners, gen := r.ownerReader()
		var err error
		project, ok, err = nsowner.Of(ctx, owners, name)
		if err != nil {
			return "", err
		}
		settled = !r.ownersChangedSince(gen)
	}
	if !settled {
		return "", fmt.Errorf("resolving the owner of namespace %s: the ownership reader changed under every attempt", name)
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
