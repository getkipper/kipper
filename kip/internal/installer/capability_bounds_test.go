package installer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/getkipper/kipper/controller/pkg/capability"
)

// parseConsoleClusterRole decodes the console-api ClusterRole out of the
// manifest the installer actually applies, so the bound below is checked
// against what the cluster gets rather than against a copy.
func parseConsoleClusterRole(t *testing.T) *rbacv1.ClusterRole {
	t.Helper()
	for _, doc := range strings.Split(ConsoleRBACManifest, "\n---\n") {
		var kind struct {
			Kind string `json:"kind"`
		}
		require.NoError(t, yaml.Unmarshal([]byte(doc), &kind))
		if kind.Kind != "ClusterRole" {
			continue
		}
		var role rbacv1.ClusterRole
		require.NoError(t, yaml.Unmarshal([]byte(doc), &role))
		if role.Name == "console-api" {
			return &role
		}
	}
	t.Fatal("no console-api ClusterRole in ConsoleRBACManifest")
	return nil
}

// Invariant 1, coverage. Console-api renders a capability's rules into a
// project namespace, so it cannot grant what it does not itself hold: RBAC's
// escalation prevention rejects the write, and the failure arrives as a broken
// projection rather than as a review comment.
//
// A capability partially covered fails here rather than shipping its covered
// half, because one that quietly grants less than its name says is as wrong as
// one that grants more.
func TestEveryCapabilityIsWithinTheConsoleClusterRole(t *testing.T) {
	console := parseConsoleClusterRole(t)

	for _, c := range capability.All() {
		for _, claim := range c.Claims {
			for _, resource := range claim.Resources {
				for _, verb := range claim.Verbs {
					if !grants(console, claim.APIGroup, resource, verb) {
						t.Errorf("capability %s claims %s on %s/%s, which console-api does not hold",
							c.Name, verb, claim.APIGroup, resource)
					}
				}
			}
		}
	}
}

// Invariant 2, owner containment. A project owner may already grant the owner
// role, so a capability inside the owner ClusterRole grants nothing new. One
// outside it would let an owner hand out reach they do not have themselves.
//
// Only grantable capabilities are bound: an ungrantable one renders nothing, so
// it cannot widen anybody.
func TestEveryGrantableCapabilityIsWithinTheOwnerClusterRole(t *testing.T) {
	roles, _ := parseOperatorRBAC(t)
	owner, ok := roles["kipper:project-owner"]
	require.True(t, ok, "no kipper:project-owner in the operator RBAC manifest")

	for _, c := range capability.All() {
		if !c.Grantable {
			continue
		}
		for _, claim := range c.Claims {
			for _, resource := range claim.Resources {
				for _, verb := range claim.Verbs {
					if !grants(owner, claim.APIGroup, resource, verb) {
						t.Errorf("grantable capability %s claims %s on %s/%s, which the owner role does not hold",
							c.Name, verb, claim.APIGroup, resource)
					}
				}
			}
		}
	}
}

// Invariant 5's second half and invariant 6's first: nothing a tenant can hold
// reaches the Project CR. Console-api's own ClusterRole names projects, because
// managing them is its job, so the assertion is over the catalogue and the
// member roles rather than over every ClusterRole.
func TestNoCapabilityAndNoMemberRoleReachesProjects(t *testing.T) {
	for _, c := range capability.All() {
		for _, claim := range c.Claims {
			for _, resource := range claim.Resources {
				switch resource {
				case "projects", "projectroles":
					t.Errorf("capability %s claims %s, which no tenant may reach", c.Name, resource)
				}
			}
		}
	}

	roles, _ := parseOperatorRBAC(t)
	for _, name := range []string{"kipper:project-viewer", "kipper:project-deployer", "kipper:project-owner"} {
		role, ok := roles[name]
		require.True(t, ok, "no %s in the operator RBAC manifest", name)
		for _, resource := range []string{"projects", "projectroles"} {
			for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete"} {
				if grants(role, "kipper.run", resource, verb) {
					t.Errorf("%s grants %s on %s", name, verb, resource)
				}
			}
		}
	}
}

// The built-in capability sets stand for the built-in roles wherever grant
// capping and the console compare a built-in against a custom role, so a name
// in one of them that the staged ClusterRole does not back would overstate what
// a built-in holder can do.
//
// The reverse does not hold and must not be asserted: the ClusterRoles are
// wider than the sets standing for them, which is exactly why assigning a
// built-in takes more than a capability comparison.
func TestBuiltInCapabilitySetsAreBackedByTheirClusterRoles(t *testing.T) {
	roles, _ := parseOperatorRBAC(t)
	for _, tc := range []struct {
		role        capability.Role
		clusterRole string
	}{
		{capability.RoleViewer, "kipper:project-viewer"},
		{capability.RoleDeployer, "kipper:project-deployer"},
		{capability.RoleOwner, "kipper:project-owner"},
	} {
		staged, ok := roles[tc.clusterRole]
		require.True(t, ok, "no %s in the operator RBAC manifest", tc.clusterRole)

		for _, name := range capability.BuiltIn(tc.role) {
			c, found := capability.Lookup(name)
			require.True(t, found, "built-in %s names %s, which is not in the catalogue", tc.role, name)
			for _, claim := range c.Claims {
				for _, resource := range claim.Resources {
					for _, verb := range claim.Verbs {
						if !grants(staged, claim.APIGroup, resource, verb) {
							t.Errorf("built-in %s carries %s, which claims %s on %s/%s, but %s does not grant it",
								tc.role, name, verb, claim.APIGroup, resource, tc.clusterRole)
						}
					}
				}
			}
		}
	}
}

// A rule restricted by resourceNames must not count towards the bound.
//
// The bound asks whether the operator (or the owner role) already grants what a
// capability claims. A rule that reaches one named object does not grant the
// resource, and counting it would let a widening pass the build: a capability
// claiming "bind clusterroles" would look covered by the operator's permission
// to bind one specific ClusterRole.
func TestARuleRestrictedToNamedObjectsGrantsNothingToTheBound(t *testing.T) {
	role := &rbacv1.ClusterRole{Rules: []rbacv1.PolicyRule{{
		APIGroups:     []string{"rbac.authorization.k8s.io"},
		Resources:     []string{"clusterroles"},
		Verbs:         []string{"bind"},
		ResourceNames: []string{"kipper-project-owner"},
	}}}

	if grants(role, "rbac.authorization.k8s.io", "clusterroles", "bind") {
		t.Error("a rule that may bind one named ClusterRole was read as permission to bind clusterroles, so a capability claiming it would pass the bound on coverage nobody granted")
	}

	// And the same rule without the restriction does grant it, so the exclusion
	// is the resourceNames and not the shape of the rule.
	role.Rules[0].ResourceNames = nil
	if !grants(role, "rbac.authorization.k8s.io", "clusterroles", "bind") {
		t.Error("an unrestricted rule stopped granting, so the bound now under-reports what the operator holds")
	}
}
