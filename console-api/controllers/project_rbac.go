package controllers

import (
	"context"
	"fmt"
	"sort"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
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

// managedMemberBindingNames is the set of RoleBinding names the membership
// reconciler owns in project namespaces.
var managedMemberBindingNames = map[string]bool{
	"kipper-project-viewer":   true,
	"kipper-project-deployer": true,
	"kipper-project-owner":    true,
}

// mapMemberBindingToProject routes events on managed member RoleBindings
// back to their owning Project, so out-of-band deletion or edits of an
// authorization object are repaired on the next reconcile instead of
// lingering until an unrelated Project event. The delete event carries the
// object's last state, so the project label is present there too; a managed
// name stripped of its labels is unmappable and left to the periodic
// resync.
func mapMemberBindingToProject(_ context.Context, obj client.Object) []reconcile.Request {
	if !managedMemberBindingNames[obj.GetName()] {
		return nil
	}
	project := obj.GetLabels()["kipper.run/project"]
	if project == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: project}}}
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
		name := "kipper-project-" + string(role)

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
	return nil
}
