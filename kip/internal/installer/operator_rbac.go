package installer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/getkipper/kipper/controller/pkg/authncfg"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// OIDC identity prefixes, sourced from the shared authncfg package so the
// RBAC subjects kip binds carry exactly the prefixes the API server's
// claimMappings prepend. Only prefixed subjects can match an OIDC identity,
// which is what stops a compromised issuer or claim mapping from asserting a
// built-in name like system:masters.
const (
	oidcUsernamePrefix = authncfg.UsernamePrefix
	oidcGroupsPrefix   = authncfg.GroupsPrefix
)

// The operator-facing RBAC is staged before the API server trusts any OIDC
// identity: staging is additive and inert until the authenticator exists, and
// having authorization in place first means enabling authentication can never
// produce logins that all fail authorization.
//
// It is split in two because upgrade may re-apply only one half. The roles are
// desired state; the binding's subjects are live state.

// OperatorClusterRolesManifest defines the three project ClusterRoles the
// membership reconciler binds per namespace. It carries no placeholders and no
// subjects, which is what makes it safe to re-apply on every upgrade: a
// permission added for a new feature has to reach existing clusters, not only
// fresh installs, exactly as ConsoleRBACManifest does.
const OperatorClusterRolesManifest = `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kipper:project-viewer
  labels:
    app.kubernetes.io/managed-by: kipper
rules:
  - apiGroups: [""]
    resources: ["pods", "pods/log", "services", "configmaps", "endpoints", "events", "persistentvolumeclaims"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["apps"]
    resources: ["deployments", "statefulsets", "replicasets"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["batch"]
    resources: ["jobs", "cronjobs"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["networking.k8s.io"]
    resources: ["ingresses"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["kipper.run"]
    resources: ["apps", "services", "functions", "jobs", "volumes", "workloadnames"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kipper:project-deployer
  labels:
    app.kubernetes.io/managed-by: kipper
rules:
  - apiGroups: [""]
    resources: ["pods", "pods/log", "services", "configmaps", "endpoints", "events", "persistentvolumeclaims"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["apps"]
    resources: ["deployments", "statefulsets", "replicasets"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["batch"]
    resources: ["jobs", "cronjobs"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["networking.k8s.io"]
    resources: ["ingresses"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["kipper.run"]
    resources: ["apps", "services", "functions", "jobs", "volumes", "workloadnames"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["apps"]
    resources: ["deployments/scale", "statefulsets/scale"]
    verbs: ["update", "patch"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kipper:project-owner
  labels:
    app.kubernetes.io/managed-by: kipper
rules:
  - apiGroups: [""]
    resources: ["pods", "pods/log", "services", "configmaps", "endpoints", "events", "persistentvolumeclaims"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["apps"]
    resources: ["deployments", "statefulsets", "replicasets"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["batch"]
    resources: ["jobs", "cronjobs"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["networking.k8s.io"]
    resources: ["ingresses"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["kipper.run"]
    resources: ["apps", "services", "functions", "jobs", "volumes", "workloadnames"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  # resourceadjustments is deliberately absent although owners manage them
  # through the console: the CRD is cluster-scoped, and a namespaced
  # RoleBinding cannot authorize cluster-scoped resources, so the grant
  # would be dead weight that misstates the role's reach.
  - apiGroups: ["kipper.run"]
    resources: ["apikeys", "datatransfers"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["apps"]
    resources: ["deployments/scale", "statefulsets/scale"]
    verbs: ["update", "patch"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["delete"]
  - apiGroups: [""]
    resources: ["pods/exec", "pods/portforward"]
    verbs: ["create"]
  - apiGroups: [""]
    resources: ["secrets", "configmaps"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
`

// initialAdminBindingTemplate is the bootstrap cluster-admin grant for the
// identity the install creates in Dex.
//
// It is deliberately NOT re-applied on upgrade. Its subject list is live state:
// renderAdminSubjectPatch rewrites it as admins are added and removed, so
// re-applying the install-time manifest would silently reset the cluster to a
// single admin and revoke everyone added since. The placeholders are the
// username prefix and the cluster domain.
const initialAdminBindingTemplate = `---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  # Deliberately named: this is the bootstrap grant for the install's admin
  # identity, kept under this name so the final end-to-end verification step
  # can find, re-evaluate, and — if a tighter admin role replaces
  # cluster-admin — replace it in one place.
  name: kipper-initial-admin
  labels:
    app.kubernetes.io/managed-by: kipper
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  # Only the install's own admin identity. A group subject (for connectors
  # that supply groups) is deliberately absent: a standing grant to a group
  # name would hand cluster-admin to any future connector able to emit that
  # name, so the group grant arrives together with the connector design
  # that constrains it.
  - apiGroup: rbac.authorization.k8s.io
    kind: User
    name: %sadmin@%s
`

// InstallOperatorRBAC stages the operator RBAC model: the three project
// ClusterRoles the membership reconciler binds per namespace, and the
// initial admin grant for the identity the install itself creates in Dex.
// Everything here is inert until the API server authenticates OIDC
// identities; applying it first is what makes enabling authentication safe.
func InstallOperatorRBAC(client *ssh.Client, domain string) error {
	applyCmd := fmt.Sprintf("cat << 'KIPEOF' | kubectl apply -f -\n%sKIPEOF", operatorRBACManifest(domain))
	if _, err := client.Run(applyCmd); err != nil {
		return fmt.Errorf("applying operator rbac: %w", err)
	}
	return nil
}

// operatorRBACManifest composes what a fresh install applies: the three project
// ClusterRoles followed by the initial admin binding. Upgrade applies only the
// roles, through the Kubernetes API rather than over SSH.
func operatorRBACManifest(domain string) string {
	return OperatorClusterRolesManifest + fmt.Sprintf(initialAdminBindingTemplate, oidcUsernamePrefix, domain)
}

// renderAdminSubjectPatch builds the JSON that sets the kipper-initial-admin
// ClusterRoleBinding's subjects to exactly the oidc:-prefixed forms of the
// given emails, sorted and deduped. Pure so it is tested without SSH; callers
// must have validated the emails.
func renderAdminSubjectPatch(emails []string) string {
	seen := map[string]bool{}
	names := make([]string, 0, len(emails))
	for _, e := range emails {
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		names = append(names, oidcUsernamePrefix+e)
	}
	sort.Strings(names)
	var subjects []string
	for _, n := range names {
		subjects = append(subjects, fmt.Sprintf(`{"apiGroup":"rbac.authorization.k8s.io","kind":"User","name":%q}`, n))
	}
	return fmt.Sprintf(`{"subjects":[%s]}`, strings.Join(subjects, ","))
}

// EnsureAdminBindingSubjects sets the kipper-initial-admin ClusterRoleBinding
// to grant cluster-admin to exactly the given admin identities. A domain
// cutover stages it with (oldEmail, newEmail) so the new admin is authorized
// the instant the issuer flips, then contracts to (newEmail). The subjects
// carry the oidc: prefix, so nothing unprefixed is ever bindable to
// cluster-admin. Written over the server-side admin kubeconfig, keeping the
// console-api ServiceAccount descoped from cluster-admin-grade RBAC writes.
func EnsureAdminBindingSubjects(client *ssh.Client, emails ...string) error {
	for _, e := range emails {
		if err := authncfg.ValidateAdminEmail(e); err != nil {
			return err
		}
	}
	patch := renderAdminSubjectPatch(emails)
	// A merge patch on subjects replaces the array wholesale, so a removed
	// admin's subject is dropped, not merged.
	cmd := fmt.Sprintf("kubectl patch clusterrolebinding kipper-initial-admin --type merge -p '%s'", patch)
	if _, err := client.Run(cmd); err != nil {
		return fmt.Errorf("updating admin binding subjects: %w", err)
	}
	return nil
}
