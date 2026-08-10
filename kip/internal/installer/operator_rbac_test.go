package installer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// parseOperatorRBAC renders the manifest for a test domain and decodes every
// document into its typed object, so the assertions run against what the
// API server would actually store rather than against strings.
func parseOperatorRBAC(t *testing.T) (map[string]*rbacv1.ClusterRole, []*rbacv1.ClusterRoleBinding) {
	t.Helper()
	rendered := operatorRBACManifest("cluster.example.com")

	roles := map[string]*rbacv1.ClusterRole{}
	var bindings []*rbacv1.ClusterRoleBinding
	for _, doc := range strings.Split(rendered, "\n---\n") {
		var kind struct {
			Kind string `json:"kind"`
		}
		require.NoError(t, yaml.Unmarshal([]byte(doc), &kind))
		switch kind.Kind {
		case "ClusterRole":
			var role rbacv1.ClusterRole
			require.NoError(t, yaml.Unmarshal([]byte(doc), &role))
			roles[role.Name] = &role
		case "ClusterRoleBinding":
			var binding rbacv1.ClusterRoleBinding
			require.NoError(t, yaml.Unmarshal([]byte(doc), &binding))
			bindings = append(bindings, &binding)
		default:
			t.Fatalf("unexpected kind %q in operator RBAC manifest", kind.Kind)
		}
	}
	return roles, bindings
}

// grants reports whether a role allows a verb on a resource in an apiGroup.
func grants(role *rbacv1.ClusterRole, apiGroup, resource, verb string) bool {
	for _, rule := range role.Rules {
		groupMatch, resourceMatch, verbMatch := false, false, false
		for _, g := range rule.APIGroups {
			if g == apiGroup || g == "*" {
				groupMatch = true
			}
		}
		for _, r := range rule.Resources {
			if r == resource || r == "*" {
				resourceMatch = true
			}
		}
		for _, v := range rule.Verbs {
			if v == verb || v == "*" {
				verbMatch = true
			}
		}
		if groupMatch && resourceMatch && verbMatch {
			return true
		}
	}
	return false
}

func TestOperatorRBACRolesExistAndAreOrdered(t *testing.T) {
	roles, _ := parseOperatorRBAC(t)
	require.Len(t, roles, 3)

	viewer := roles["kipper:project-viewer"]
	deployer := roles["kipper:project-deployer"]
	owner := roles["kipper:project-owner"]
	require.NotNil(t, viewer)
	require.NotNil(t, deployer)
	require.NotNil(t, owner)

	// The role order viewer < deployer < owner must hold in RBAC terms:
	// everything a lesser role grants, the greater role grants too.
	for _, pair := range []struct {
		lesser, greater *rbacv1.ClusterRole
		name            string
	}{
		{viewer, deployer, "viewer ⊆ deployer"},
		{deployer, owner, "deployer ⊆ owner"},
	} {
		for _, rule := range pair.lesser.Rules {
			for _, g := range rule.APIGroups {
				for _, r := range rule.Resources {
					for _, v := range rule.Verbs {
						assert.True(t, grants(pair.greater, g, r, v),
							"%s: missing %s %s.%s", pair.name, v, g, r)
					}
				}
			}
		}
	}
}

func TestOperatorRBACEscalationBoundaries(t *testing.T) {
	roles, _ := parseOperatorRBAC(t)

	for name, role := range roles {
		// A subject who can create or bind roles can grant themselves
		// anything bindable; access management stays with the platform.
		for _, rule := range role.Rules {
			for _, g := range rule.APIGroups {
				assert.NotEqual(t, "rbac.authorization.k8s.io", g,
					"%s must carry no RBAC-resource rules", name)
				assert.NotEqual(t, "*", g, "%s must not wildcard apiGroups", name)
			}
			for _, r := range rule.Resources {
				assert.NotEqual(t, "*", r, "%s must not wildcard resources", name)
			}
			for _, v := range rule.Verbs {
				assert.NotEqual(t, "*", v, "%s must not wildcard verbs", name)
			}
		}

		// Workload identities are lateral movement, and the platform's
		// guardrail objects must not be editable by the roles they bound.
		for _, resource := range []string{"serviceaccounts", "serviceaccounts/token", "resourcequotas", "limitranges", "networkpolicies", "namespaces", "roles", "rolebindings"} {
			for _, verb := range []string{"get", "create", "update", "patch", "delete", "bind", "escalate", "impersonate"} {
				for _, g := range []string{"", "networking.k8s.io", "rbac.authorization.k8s.io"} {
					assert.False(t, grants(role, g, resource, verb),
						"%s must not grant %s on %s", name, verb, resource)
				}
			}
		}
	}

	// Secrets and exec arrive only with owner: exec reads every secret the
	// workload holds, so granting one without the other is an illusion.
	viewer := roles["kipper:project-viewer"]
	deployer := roles["kipper:project-deployer"]
	owner := roles["kipper:project-owner"]
	for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete"} {
		assert.False(t, grants(viewer, "", "secrets", verb), "viewer must never touch secrets (%s)", verb)
		assert.False(t, grants(deployer, "", "secrets", verb), "deployer must never touch secrets (%s)", verb)
	}
	assert.False(t, grants(viewer, "", "pods/exec", "create"), "viewer must not exec")
	assert.False(t, grants(deployer, "", "pods/exec", "create"), "deployer must not exec")
	assert.True(t, grants(owner, "", "pods/exec", "create"))
	assert.True(t, grants(owner, "", "secrets", "get"))
}

func TestOperatorRBACRoleCapabilities(t *testing.T) {
	roles, _ := parseOperatorRBAC(t)
	viewer := roles["kipper:project-viewer"]
	deployer := roles["kipper:project-deployer"]

	// Viewer reads the workload surface and changes nothing.
	assert.True(t, grants(viewer, "kipper.run", "apps", "get"))
	assert.True(t, grants(viewer, "", "pods/log", "get"))
	for _, verb := range []string{"create", "update", "patch", "delete"} {
		for _, res := range []string{"apps", "services", "functions", "jobs", "volumes"} {
			assert.False(t, grants(viewer, "kipper.run", res, verb), "viewer must not %s %s", verb, res)
		}
	}

	// Deployer changes workloads: Kipper CRs, scale, pod deletes for
	// restarts.
	assert.True(t, grants(deployer, "kipper.run", "apps", "update"))
	assert.True(t, grants(deployer, "apps", "deployments/scale", "patch"))
	assert.True(t, grants(deployer, "", "pods", "delete"))
	assert.False(t, grants(deployer, "kipper.run", "apikeys", "get"),
		"API keys are owner-level: they mint request credentials")

	// No role may create native batch Jobs or bare pods: a pod template
	// mounts any secret and selects any ServiceAccount, so it would smuggle
	// exactly what the enumerated grants withhold. Job runs go through the
	// kipper.run Job CR.
	owner := roles["kipper:project-owner"]
	for _, role := range []*rbacv1.ClusterRole{viewer, deployer, owner} {
		assert.False(t, grants(role, "batch", "jobs", "create"))
		assert.False(t, grants(role, "batch", "jobs", "delete"))
		assert.False(t, grants(role, "", "pods", "create"))
	}
}

func TestOperatorRBACInitialAdminBinding(t *testing.T) {
	_, bindings := parseOperatorRBAC(t)
	require.Len(t, bindings, 1)
	binding := bindings[0]

	// The bootstrap grant keeps its deliberate name so the final
	// verification step can find and re-evaluate it.
	assert.Equal(t, "kipper-initial-admin", binding.Name)
	assert.Equal(t, "cluster-admin", binding.RoleRef.Name)

	// Exactly one subject: the install's own admin identity. A group grant
	// here would hand cluster-admin to any future connector able to emit
	// the group's name; it arrives with the connector design instead.
	require.Len(t, binding.Subjects, 1)
	assert.Equal(t, "User", binding.Subjects[0].Kind)
	assert.Equal(t, "oidc:admin@cluster.example.com", binding.Subjects[0].Name)
}

func TestOperatorRBACEverySubjectIsPrefixed(t *testing.T) {
	rendered := operatorRBACManifest("cluster.example.com")

	// No subject line anywhere in the manifest may carry an unprefixed
	// name: an unprefixed subject could collide with built-in identities
	// (system:masters and friends), which is the exact spoofing surface the
	// prefixes exist to close.
	for _, line := range strings.Split(rendered, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "name:") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(trimmed, "name:"))
		if strings.HasPrefix(name, "kipper:project-") || name == "kipper-initial-admin" || name == "cluster-admin" {
			continue
		}
		assert.True(t, strings.HasPrefix(name, "oidc:"),
			"identity name %q in the manifest is unprefixed", name)
	}
}
