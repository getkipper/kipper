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

// memberBindingProjects routes managed binding events using the project
// label, a generated-name prefix matched against Projects, or the namespace
// label. The fallback lookups recover ownership when binding labels drift.
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

	// The immutable name embeds the project digest, so it takes precedence over
	// an edited label when deciding which project must reconcile the binding.
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
				// Preserve the IdP's casing for exact Kubernetes username matching;
				// trim accidental whitespace from membership writes.
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
				// Read then delete the object that was read, the same as every
				// other binding delete in this file. These names are fixed
				// across namespaces, so a delete by name alone can remove a
				// binding another project wrote after this pass looked.
				var stale rbacv1.RoleBinding
				switch err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &stale); {
				case errors.IsNotFound(err):
				case err != nil:
					return fmt.Errorf("reading %s member binding in %s: %w", role, ns, err)
				default:
					if err := r.deleteObserved(ctx, &stale); err != nil {
						return fmt.Errorf("removing %s member binding in %s: %w", role, ns, err)
					}
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
				// Bound to the object that was read, like every other delete in
				// this file. A namespace that changes hands means another
				// project legitimately writes the binding under this name, and
				// an unconditioned delete issued from a stale cache removes
				// theirs and then recreates this project's members over it.
				if err := r.deleteObserved(ctx, &existing); err != nil {
					return fmt.Errorf("removing drifted %s member binding in %s: %w", role, ns, err)
				}
				// A refused delete leaves the object standing, so the create
				// finds it there. That is the drift still unrepaired rather
				// than an error: the next pass reads it afresh and decides
				// again, which is what the refusal asked for.
				if err := r.Create(ctx, desired); err != nil && !errors.IsAlreadyExists(err) {
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

// memberBindingProjectIndex indexes a member RoleBinding under the prefix of
// the project that generated its name.
//
// The prefix is a digest of the project name and sits in the object's own name,
// which Kubernetes will not let anyone edit in place. That is what the index is
// for: every other trail from a binding back to its project is mutable. The
// label can be stripped, the namespace's label can be stripped, the environment
// can leave the spec, and status can forget the namespace. A binding that has
// lost all of those is still indexed here, and the revoke pass is the only
// thing that will ever visit it again.
const memberBindingProjectIndex = "kipper.run/member-binding-project"

// MemberBindingProjectKeys returns the index keys for a RoleBinding.
//
// Only generated names index, and each under exactly one prefix. A fixed legacy
// name carries no digest, so placing it under any project's prefix would be a
// guess; those are reached through the label and the namespace instead.
func MemberBindingProjectKeys(obj client.Object) []string {
	binding, ok := obj.(*rbacv1.RoleBinding)
	if !ok {
		return nil
	}
	projectPrefix, ok := memberbinding.ProjectPrefixOf(binding.Name)
	if !ok {
		return nil
	}
	return []string{projectPrefix}
}

// IndexMemberBindings registers the index the revoke pass lists by.
func IndexMemberBindings(ctx context.Context, indexer client.FieldIndexer) error {
	return indexer.IndexField(ctx, &rbacv1.RoleBinding{}, memberBindingProjectIndex, MemberBindingProjectKeys)
}

// revokeStaleMemberBindings removes obsolete subjects from this project's
// managed bindings, including namespaces whose ownership has drifted. It finds
// bindings by project label and generated-name index.
//
// Keep this separate from grant reconciliation: intersect existing subjects with
// current grants, delete empty or wrong-role bindings, and leave namespace
// ownership records untouched. Revocation must never establish new access.
func (r *ProjectReconciler) revokeStaleMemberBindings(ctx context.Context, project *kipperv1.Project) error {
	// Two ways in, because either can be the one that survives.
	//
	// The label is what console-api writes on every binding it creates, and is
	// the ordinary way. The generated name carries a project digest nothing can
	// edit in place, and is what remains when the label has drifted, which is
	// precisely the state this pass exists for. Selecting on the label alone
	// left the generated binding invisible to the only pass that reaches it.
	//
	// Both halves are list selectors, so neither walks the cluster. The label
	// half selects on the label. The name half selects on an index keyed by the
	// prefix, which is why the index exists: a prefix is not a selector on its
	// own, and deriving the namespaces instead from the spec and from status
	// could not reach a binding whose environment had left the spec and whose
	// namespace status had forgotten, which is the case the generated name was
	// introduced to survive.
	var bindings rbacv1.RoleBindingList
	if err := r.List(ctx, &bindings, client.MatchingLabels{
		kipperlabels.Project: project.Name,
	}); err != nil {
		return fmt.Errorf("listing this project's labeled member bindings: %w", err)
	}
	seen := map[string]bool{}
	for i := range bindings.Items {
		seen[bindings.Items[i].Namespace+"/"+bindings.Items[i].Name] = true
	}

	var generated rbacv1.RoleBindingList
	if err := r.List(ctx, &generated, client.MatchingFields{
		memberBindingProjectIndex: memberbinding.Prefix(project.Name),
	}); err != nil {
		return fmt.Errorf("listing this project's generated member bindings: %w", err)
	}
	for i := range generated.Items {
		b := &generated.Items[i]
		if seen[b.Namespace+"/"+b.Name] {
			continue
		}
		seen[b.Namespace+"/"+b.Name] = true
		bindings.Items = append(bindings.Items, *b)
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

		// A subject kept on a binding whose RoleRef drifted is not the access
		// the project granted: the role is inferred from the object's name, so
		// a binding called kipper-project-owner pointing at cluster-admin looks
		// like an owner binding and its subjects look granted. RoleRef is
		// immutable, so deleting is the only revoke-only answer, and in a
		// namespace whose ownership cannot be proved nothing else will come and
		// fix it.
		if binding.RoleRef.Name != memberClusterRoles[role] ||
			binding.RoleRef.Kind != "ClusterRole" ||
			binding.RoleRef.APIGroup != rbacv1.GroupName {
			if err := r.deleteObserved(ctx, binding); err != nil {
				return fmt.Errorf("removing the drifted %s binding in %s: %w", binding.Name, binding.Namespace, err)
			}
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
			if err := r.deleteObserved(ctx, binding); err != nil {
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

// deleteObserved removes the exact object this pass read.
//
// A fixed name is the same in every namespace, so a namespace changing hands
// means another project legitimately rewrites the binding under that name; an
// unconditioned delete issued from a stale cache would then remove the new
// owner's binding rather than the one that was examined. An update gets this
// from resourceVersion conflict detection for free and a delete does not.
//
// A conflict is not an error: somebody else changed the object, so this pass no
// longer knows whether it should go, and the next reconcile reads it afresh.
func (r *ProjectReconciler) deleteObserved(ctx context.Context, binding *rbacv1.RoleBinding) error {
	err := r.Delete(ctx, binding, client.Preconditions{
		UID:             &binding.UID,
		ResourceVersion: &binding.ResourceVersion,
	})
	if errors.IsNotFound(err) || errors.IsConflict(err) {
		return nil
	}
	return err
}

// grantedSubjects returns complete RBAC subjects for each known role. Comparing
// the kind and API group as well as the name prevents a Group from retaining
// access intended for a User. Unknown roles receive no subjects.
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

// managedMemberBindingRole matches legacy and generated binding names to a role.
// Both forms must be recognized so revocation removes either generation.
// Generated names contain digests, so candidates are compared rather than parsed.
func managedMemberBindingRole(project, name string) (kipperv1.ProjectMemberRole, bool) {
	for role := range memberClusterRoles {
		if name == "kipper-project-"+string(role) || name == memberbinding.Name(project, string(role)) {
			return role, true
		}
	}
	return "", false
}
