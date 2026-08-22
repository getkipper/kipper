package controllers

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/memberbinding"
)

// oidcUsernamePrefix must match the API server's claimMappings username
// prefix and the subjects the installer stages
// (kip/internal/installer/operator_rbac.go).
const oidcUsernamePrefix = "oidc:"

// memberClusterRoles maps a project membership role to the ClusterRole the
// installer stages for it.
var memberClusterRoles = map[kipperv1.ProjectMemberRole]string{
	kipperv1.ProjectRoleViewer:   "kipper:project-viewer",
	kipperv1.ProjectRoleDeployer: "kipper:project-deployer",
	kipperv1.ProjectRoleOwner:    "kipper:project-owner",
}

// mapMemberBindingToProject routes events on managed member RoleBindings
// back to their owning Project, so out-of-band deletion or edits of an
// authorization object are repaired on the next reconcile instead of
// lingering until an unrelated Project event. The delete event carries the
// object's last state, so the project label is present there too; a managed
// name stripped of its labels is unmappable and left to the periodic
// resync.
func mapMemberBindingToProject(ctx context.Context, obj client.Object) []reconcile.Request {
	return memberBindingProjects(ctx, nil, obj)
}

// memberBindingProjects routes a member binding to the Project that owns it.
//
// Three ways, in order of how much they can be trusted, because a map function
// runs backwards from an object and the object may have lost the thing that
// says whose it is.
//
// The label, when it is there. Console-api writes it on every binding it
// creates, so this is the ordinary case and the other two are drift.
//
// Failing that, generation. A generated name carries a project digest, and a
// digest does not run backwards: nothing recovers a project name from a
// SHA-256. So it walks the Projects it can see and generates each one's prefix,
// which is forward and needs no parsing. The walk is bounded by the number of
// projects and only runs for a binding whose label has gone.
//
// Failing that, the namespace. A fixed name carries no digest at all, so a
// stripped label leaves nothing on the object; the containing namespace's own
// label is the last thing that says whose it is.
//
// A reader is only needed for the last two. The watch passes none, so an event
// on a labelled binding costs nothing extra.
func memberBindingProjects(ctx context.Context, reader client.Reader, obj client.Object) []reconcile.Request {
	if !memberbinding.IsManaged(obj.GetName()) {
		return nil
	}
	enqueue := func(project string) []reconcile.Request {
		if project == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: project}}}
	}

	// A generated name carries a project digest nothing can edit in place, so
	// where the two disagree the name wins. Taking the label's word for it sent
	// a binding to a project that could not recognise it while the project that
	// could was never woken, which is drift routed into silence.
	labelled := obj.GetLabels()[kipperlabels.Project]
	if labelled != "" && strings.HasPrefix(obj.GetName(), memberbinding.Prefix(labelled)) {
		return enqueue(labelled)
	}
	if labelled != "" && !strings.HasPrefix(obj.GetName(), "kipper-project-") {
		// A generated name whose label names some other project. Fall through
		// to the walk, which reads the name rather than the label.
		if reader == nil {
			return nil
		}
	} else if labelled != "" {
		// A fixed name carries no digest to check the label against, so the
		// label is all there is.
		return enqueue(labelled)
	}
	if reader == nil {
		return nil
	}

	var projects kipperv1.ProjectList
	if err := reader.List(ctx, &projects); err == nil {
		for i := range projects.Items {
			name := projects.Items[i].Name
			if strings.HasPrefix(obj.GetName(), memberbinding.Prefix(name)) {
				return enqueue(name)
			}
		}
	}

	var ns corev1.Namespace
	if err := reader.Get(ctx, types.NamespacedName{Name: obj.GetNamespace()}, &ns); err != nil {
		return nil
	}
	return enqueue(ns.Labels[kipperlabels.Project])
}

// reconcileMemberBindings projects the Project's membership onto Kubernetes
// RBAC: one RoleBinding per role in each environment namespace, its subjects
// being the members' OIDC identities. The bindings are inert until the API
// server authenticates OIDC users; keeping them reconciled now means
// enabling authentication never produces members who can log in but hold no
// authorization. Subjects are rewritten whole each reconcile and a role with
// no members loses its binding, so removing a member removes their access on
// the next reconcile.
func (r *ProjectReconciler) reconcileMemberBindings(ctx context.Context, project *kipperv1.Project, ns string) error {
	for role, clusterRole := range memberClusterRoles {
		// Both generations, alongside each other. The generated name carries a
		// project digest, which is what lets a cluster-wide listing find this
		// project's bindings without parsing anything; the fixed name is what
		// every released build writes, and a controller from one of those in a
		// rolling window would recreate it the moment this one deleted it. The
		// fixed generation is retired in a later release, once nothing writes
		// it.
		for _, name := range []string{
			"kipper-project-" + string(role),
			memberbinding.Name(project.Name, string(role)),
		} {

			var subjects []rbacv1.Subject
			for _, m := range project.Spec.Members {
				if m.Role != role {
					continue
				}
				// Kubernetes username matching is exact. Membership emails come
				// from the account store, which holds them as the IdP emits them;
				// trimming guards against whitespace slipping in through any
				// write path. Full canonicalisation (casing across connectors)
				// belongs to the connector design, together with the account
				// store that must apply the same rule.
				email := strings.TrimSpace(m.Email)
				if email == "" {
					continue
				}
				subjects = append(subjects, rbacv1.Subject{
					APIGroup: rbacv1.GroupName,
					Kind:     rbacv1.UserKind,
					Name:     oidcUsernamePrefix + email,
				})
			}
			sort.Slice(subjects, func(i, j int) bool { return subjects[i].Name < subjects[j].Name })

			if len(subjects) == 0 {
				stale := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
				if err := r.Delete(ctx, stale); err != nil && !errors.IsNotFound(err) {
					return fmt.Errorf("removing %s member binding in %s: %w", role, ns, err)
				}
				continue
			}

			desired := &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: ns,
					Labels: map[string]string{
						kipperLabel:          kipperValue,
						"kipper.run/project": project.Name,
					},
				},
				RoleRef: rbacv1.RoleRef{
					APIGroup: rbacv1.GroupName,
					Kind:     "ClusterRole",
					Name:     clusterRole,
				},
				Subjects: subjects,
			}

			var existing rbacv1.RoleBinding
			err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &existing)
			if errors.IsNotFound(err) {
				if err := r.Create(ctx, desired); err != nil {
					return fmt.Errorf("creating %s member binding in %s: %w", role, ns, err)
				}
				continue
			}
			if err != nil {
				return fmt.Errorf("reading %s member binding in %s: %w", role, ns, err)
			}
			// RoleRef is immutable, so a binding under a managed name that
			// references anything else (a restore from backup, a manual object)
			// can never be updated into the declared state — and rewriting its
			// subjects would grant members whatever it happens to reference.
			// Delete and recreate instead.
			if existing.RoleRef != desired.RoleRef {
				if err := r.Delete(ctx, &existing); err != nil && !errors.IsNotFound(err) {
					return fmt.Errorf("removing drifted %s member binding in %s: %w", role, ns, err)
				}
				if err := r.Create(ctx, desired); err != nil {
					return fmt.Errorf("recreating %s member binding in %s: %w", role, ns, err)
				}
				continue
			}
			existing.Labels = desired.Labels
			existing.Subjects = desired.Subjects
			if err := r.Update(ctx, &existing); err != nil {
				return fmt.Errorf("updating %s member binding in %s: %w", role, ns, err)
			}
		}
	}
	return nil
}

// revokeStaleMemberBindings takes access away and can do nothing else.
//
// The ordinary reconcile refuses a namespace whose ownership it cannot prove
// and skips it, which is correct for granting and leaves revocation unreachable
// exactly where it matters: a namespace whose project label has drifted still
// holds the bindings this project wrote, and a member removed from the project
// keeps them. Nothing else visits that namespace, because nothing else is
// allowed to.
//
// So this pass exists, and it is a separate one rather than a reordering.
// reconcileMemberBindings writes desired bindings, so running it before
// ownership is proven would let a project whose environment resolves to a
// namespace it does not own put its own members into that namespace's bindings,
// and the later refusal would leave the grant standing. Two projects can
// resolve to one namespace name, so that is not hypothetical.
//
// The rule is that the desired subject set here is the existing set minus
// whoever the project no longer grants. Never a union, never an addition. A
// binding it empties is deleted, and a RoleRef is never written: it is
// immutable in Kubernetes, so a binding pointing at the wrong role is deleted
// rather than corrected.
//
// It is scoped to bindings carrying this project's own label, so it cannot
// reach another tenant's, and it records nothing: writing the namespace into
// status would widen the Project finalizer's deletion backstop onto a namespace
// whose ownership was never proven.
func (r *ProjectReconciler) revokeStaleMemberBindings(ctx context.Context, project *kipperv1.Project) error {
	// Two ways in, because either can be the one that survives.
	//
	// The label is what console-api writes on every binding it creates, and is
	// the ordinary way. The generated name carries a project digest nothing can
	// edit in place, and is what remains when the label has drifted, which is
	// precisely the state this pass exists for. Selecting on the label alone
	// left the generated binding invisible to the only pass that reaches it.
	//
	// A prefix is not a list selector, so the name half needs an unfiltered
	// list and an in-memory match. That costs a pass over the cluster's
	// RoleBindings per project reconcile; the cache field index that makes it
	// an equality lookup lands with the manager wiring, and until then this is
	// correct and slow rather than fast and blind.
	var bindings rbacv1.RoleBindingList
	if err := r.List(ctx, &bindings); err != nil {
		return fmt.Errorf("listing member bindings: %w", err)
	}
	prefix := memberbinding.Prefix(project.Name)

	granted := grantedSubjects(project)

	for i := range bindings.Items {
		binding := &bindings.Items[i]
		role, managed := managedMemberBindingRole(project.Name, binding.Name)
		if !managed {
			continue
		}
		// Ours by either anchor. A fixed name is identical in every namespace,
		// so only its label says whose it is; a generated name says so itself.
		byLabel := binding.Labels[kipperlabels.Project] == project.Name
		byName := strings.HasPrefix(binding.Name, prefix)
		if !byLabel && !byName {
			continue
		}

		keep := make([]rbacv1.Subject, 0, len(binding.Subjects))
		for _, s := range binding.Subjects {
			if granted[role][s] {
				keep = append(keep, s)
			}
		}
		if len(keep) == len(binding.Subjects) {
			continue
		}

		if len(keep) == 0 {
			// Pinned to the object this pass read. A fixed name is the same in
			// every namespace, so a namespace changing hands means another
			// project legitimately rewrites the binding under that name; an
			// unconditioned delete issued from a stale cache would then remove
			// the new owner's binding rather than the one that was examined.
			// An update gets this from resourceVersion conflict detection for
			// free, and a delete does not.
			precondition := client.Preconditions{
				UID:             &binding.UID,
				ResourceVersion: &binding.ResourceVersion,
			}
			if err := r.Delete(ctx, binding, precondition); err != nil &&
				!errors.IsNotFound(err) && !errors.IsConflict(err) {
				return fmt.Errorf("removing the emptied %s binding in %s: %w", binding.Name, binding.Namespace, err)
			}
			continue
		}

		binding.Subjects = keep
		if err := r.Update(ctx, binding); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("revoking from the %s binding in %s: %w", binding.Name, binding.Namespace, err)
		}
	}
	return nil
}

// grantedSubjects is who the project still grants each built-in role, as whole
// RBAC subjects rather than names.
//
// A subject is a tuple of API group, kind and name, and comparing names alone
// kept a Group subject that happened to share a member's name. Both usernames
// and group claims carry the same oidc: prefix on this cluster, so that is one
// drifted field away from granting everyone in a group what the project granted
// one person. Removing such a subject adds no access, so this stays revoke-only.
//
// A member holding a role this build does not know appears under none of them,
// so the revoke pass takes their access away wherever it finds it. That is the
// same direction every other path takes for an unrecognised role.
func grantedSubjects(project *kipperv1.Project) map[kipperv1.ProjectMemberRole]map[rbacv1.Subject]bool {
	granted := make(map[kipperv1.ProjectMemberRole]map[rbacv1.Subject]bool, len(memberClusterRoles))
	for role := range memberClusterRoles {
		granted[role] = map[rbacv1.Subject]bool{}
	}
	for _, m := range project.Spec.Members {
		email := strings.TrimSpace(m.Email)
		if email == "" {
			continue
		}
		if subjects, ok := granted[m.Role]; ok {
			subjects[rbacv1.Subject{
				APIGroup: rbacv1.GroupName,
				Kind:     rbacv1.UserKind,
				Name:     oidcUsernamePrefix + email,
			}] = true
		}
	}
	return granted
}

// managedMemberBindingRole maps a managed binding's name back to its role, in
// either generation.
//
// It generates rather than parses, for the reason the whole naming scheme does:
// a generated name is two digests and neither runs backwards. Both candidate
// names are produced from the project and role and compared, which is three
// comparisons per binding and needs no separator that survives every name.
//
// Covering both generations matters here more than anywhere: the revoke pass is
// the security requirement, and one that recognised only the fixed names would
// leave a departed member bound under the generated one.
func managedMemberBindingRole(project, name string) (kipperv1.ProjectMemberRole, bool) {
	for role := range memberClusterRoles {
		if name == "kipper-project-"+string(role) || name == memberbinding.Name(project, string(role)) {
			return role, true
		}
	}
	return "", false
}
