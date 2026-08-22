// Package capability holds the catalogue of things a project role may grant,
// because three components have to agree on it: console-api authorizes its
// routes from it, the reconciler renders Kubernetes rules from it, and kip
// prints it.
//
// A capability names an ability rather than a permission. Some of them exist
// only inside console-api, where the actor is known, and grant nothing a
// kubeconfig can use; the rest also render RBAC rules into a project's
// namespaces. The two are separate planes because a member holding a
// console-only capability must not be able to reach the same thing with
// kubectl, and a member holding a Kubernetes one keeps it whether or not the
// console is running.
//
// What a capability may claim is bounded by two manifests: console-api cannot
// grant what it does not itself hold, and a project owner cannot grant more
// than the owner ClusterRole. Both bounds are checked against the real
// manifests by tests in kip/internal/installer, which is the only package that
// can see them and this catalogue at once. Hand-copying a verb from a manifest
// into this file is the mistake those tests exist to catch.
package capability

import (
	"slices"
	"sort"

	rbacv1 "k8s.io/api/rbac/v1"
)

// Name identifies a capability. It is the string stored in a ProjectRole and
// the string an operator types, so it is part of the released surface.
type Name string

// Plane is where a capability takes effect.
type Plane uint8

const (
	// PlaneK is Kubernetes: the capability renders RBAC rules into the
	// project's namespaces and a member holds it through their kubeconfig.
	PlaneK Plane = 1 << iota
	// PlaneC is the console: console-api enforces the capability on its own
	// routes, where the actor behind a request is known.
	PlaneC
)

// Has reports whether p includes plane q.
func (p Plane) Has(q Plane) bool { return p&q != 0 }

// Claim is one group of resources and the verbs a capability asks for on them.
// It becomes a PolicyRule unchanged, which is what lets a test compare a claim
// against the manifests without interpreting it.
type Claim struct {
	APIGroup  string
	Resources []string
	Verbs     []string
}

// Capability is one entry in the catalogue.
type Capability struct {
	Name   Name
	Planes Plane

	// Claims are the Kubernetes rules this capability renders, and nothing
	// else. The bound the invariant tests check is a limit on what may be
	// claimed, never the set that ships.
	Claims []Claim

	// Touches names the resources the claims cover. It is declared rather
	// than derived so that a test can assert the two agree: a description
	// generated from the declaration cannot drift from what is granted.
	Touches []string

	// Action is the verb shown to an operator, for capabilities that render
	// rules.
	Action string

	// Effect describes a capability that renders no rules, where there is
	// nothing to generate a description from.
	Effect string

	// Grantable marks a capability a role may carry. The projection refuses
	// to render one that is not, which is what stops a hand-written or
	// restored object from granting something nobody authorised.
	Grantable bool
}

// Description is what the console and kip show. It is rendered from the
// declaration rather than written by hand, so prose cannot drift from rules.
func (c Capability) Description() string {
	if len(c.Touches) == 0 {
		return c.Effect
	}
	out := c.Action + " " + c.Touches[0]
	for i := 1; i < len(c.Touches); i++ {
		switch {
		case i == len(c.Touches)-1:
			out += " and " + c.Touches[i]
		default:
			out += ", " + c.Touches[i]
		}
	}
	return out
}

// Role is a built-in project role.
type Role string

const (
	RoleViewer   Role = "viewer"
	RoleDeployer Role = "deployer"
	RoleOwner    Role = "owner"
)

const (
	groupCore       = ""
	groupApps       = "apps"
	groupBatch      = "batch"
	groupNetworking = "networking.k8s.io"
	groupKipper     = "kipper.run"
)

var readVerbs = []string{"get", "list", "watch"}
var writeVerbs = []string{"create", "update", "patch", "delete"}

// catalogue is ordered by name so that All is stable and the console lists
// capabilities the same way twice.
var catalogue = []Capability{
	{
		Name:      "apikeys.manage",
		Planes:    PlaneC,
		Grantable: true,
		Effect:    "create, change and delete the project's API keys and their usage plans",
	},
	{
		Name:      "database.read",
		Planes:    PlaneC,
		Grantable: true,
		Effect:    "browse a service's databases, tables, rows and saved queries",
	},
	{
		Name:      "database.write",
		Planes:    PlaneC,
		Grantable: true,
		Effect:    "change rows, tables, indexes and saved queries in a service's database",
	},
	{
		Name:      "env.read",
		Planes:    PlaneC,
		Grantable: true,
		Effect:    "see an app's environment variables and which of them are secret, through the console",
	},
	{
		Name:      "env.reveal",
		Planes:    PlaneC,
		Grantable: true,
		Effect:    "reveal the stored value of a secret environment variable or git token through the console",
	},
	{
		Name:      "env.write",
		Planes:    PlaneC,
		Grantable: true,
		Effect:    "change an app's environment variables through the console",
	},
	{
		Name:      "kipper.read",
		Planes:    PlaneK | PlaneC,
		Action:    "read",
		Grantable: true,
		Touches:   []string{"apps", "services", "functions", "jobs", "volumes"},
		Claims: []Claim{{
			APIGroup:  groupKipper,
			Resources: []string{"apps", "services", "functions", "jobs", "volumes"},
			Verbs:     readVerbs,
		}},
	},
	{
		Name:      "kipper.write",
		Planes:    PlaneK | PlaneC,
		Action:    "create, change and delete",
		Grantable: true,
		Touches:   []string{"apps", "services", "functions", "jobs", "volumes"},
		Claims: []Claim{{
			APIGroup:  groupKipper,
			Resources: []string{"apps", "services", "functions", "jobs", "volumes"},
			Verbs:     writeVerbs,
		}},
	},
	{
		Name:      "members.manage",
		Planes:    PlaneC,
		Grantable: true,
		Effect:    "add and remove project members, and create, change and delete the project's own roles",
	},
	{
		Name:      "members.read",
		Planes:    PlaneC,
		Grantable: true,
		Effect:    "see who is in the project and what they hold",
	},
	{
		Name:      "pods.exec",
		Planes:    PlaneK,
		Action:    "open a shell in",
		Grantable: true,
		Touches:   []string{"pods/exec"},
		Claims: []Claim{{
			APIGroup:  groupCore,
			Resources: []string{"pods/exec"},
			Verbs:     []string{"create"},
		}},
	},
	{
		Name:      "pods.logs.read",
		Planes:    PlaneK | PlaneC,
		Action:    "read",
		Grantable: true,
		Touches:   []string{"pods/log"},
		Claims: []Claim{{
			APIGroup:  groupCore,
			Resources: []string{"pods/log"},
			Verbs:     []string{"get"},
		}},
	},
	{
		Name:      "project.delete",
		Planes:    PlaneC,
		Grantable: true,
		Effect:    "delete the project and everything in it",
	},
	{
		Name:      "project.read",
		Planes:    PlaneC,
		Grantable: true,
		Effect:    "see the project's settings, quota, resource requests, API keys and usage plans",
	},
	{
		Name:      "project.settings",
		Planes:    PlaneC,
		Grantable: true,
		Effect:    "change the project's own settings",
	},
	// secrets.read, secrets.write and pods.exec are the kubeconfig-level grants: they
	// render RBAC so a member can read and write Secrets and ConfigMaps
	// directly. No console route maps to them, because the console reaches the
	// same data through env.read, env.reveal and env.write, and opens a shell
	// through terminal.open, none of which grant anything outside it. That is the plane split doing its job rather than an
	// oversight: a deployer changes an app's environment through the console
	// today and holds no Kubernetes access to the Secret underneath.
	{
		Name:      "secrets.read",
		Planes:    PlaneK,
		Action:    "read",
		Grantable: true,
		Touches:   []string{"secrets", "configmaps"},
		Claims: []Claim{{
			APIGroup:  groupCore,
			Resources: []string{"secrets", "configmaps"},
			Verbs:     readVerbs,
		}},
	},
	{
		Name:      "secrets.write",
		Planes:    PlaneK,
		Action:    "create, change and delete",
		Grantable: true,
		Touches:   []string{"secrets", "configmaps"},
		Claims: []Claim{{
			APIGroup:  groupCore,
			Resources: []string{"secrets", "configmaps"},
			Verbs:     writeVerbs,
		}},
	},
	{
		Name:      "storage.read",
		Planes:    PlaneC,
		Grantable: true,
		Effect:    "list and download objects in a service's buckets",
	},
	{
		Name:      "storage.write",
		Planes:    PlaneC,
		Grantable: true,
		Effect:    "upload and delete objects, and change what a bucket exposes publicly",
	},
	{
		Name:      "terminal.open",
		Planes:    PlaneC,
		Grantable: true,
		Effect:    "open a shell into a running workload through the console",
	},
	{
		Name:      "workloads.read",
		Planes:    PlaneK | PlaneC,
		Action:    "read",
		Grantable: true,
		Touches: []string{
			"pods", "persistentvolumeclaims",
			"deployments", "statefulsets",
			"jobs", "cronjobs",
			"ingresses",
		},
		Claims: []Claim{
			{APIGroup: groupCore, Resources: []string{"pods", "persistentvolumeclaims"}, Verbs: readVerbs},
			{APIGroup: groupApps, Resources: []string{"deployments", "statefulsets"}, Verbs: readVerbs},
			{APIGroup: groupBatch, Resources: []string{"jobs", "cronjobs"}, Verbs: readVerbs},
			{APIGroup: groupNetworking, Resources: []string{"ingresses"}, Verbs: readVerbs},
		},
	},
	{
		Name:      "workloads.restart",
		Planes:    PlaneC,
		Grantable: true,
		Effect:    "restart a workload, which is an annotation the console writes on the App",
	},
}

// builtIn is what each built-in project role grants, for comparison against a
// custom role. The RBAC a built-in actually confers still comes from its staged
// ClusterRole; these sets exist so that grant capping and the console can talk
// about built-ins and custom roles in the same terms.
//
// The sets nest, and a test holds them to it. A built-in ClusterRole is wider
// in plane K than the set standing for it here, which is why assigning a
// built-in takes more than a capability comparison.
var builtIn = map[Role][]Name{
	RoleViewer: {
		"database.read",
		"env.read",
		"kipper.read",
		"members.read",
		"pods.logs.read",
		"project.read",
		"storage.read",
		"workloads.read",
	},
	RoleDeployer: {
		"apikeys.manage",
		"database.read",
		"database.write",
		"env.read",
		"env.reveal",
		"env.write",
		"kipper.read",
		"kipper.write",
		"members.read",
		"pods.logs.read",
		"project.read",
		"storage.read",
		"storage.write",
		"terminal.open",
		"workloads.read",
		"workloads.restart",
	},
	RoleOwner: {
		"apikeys.manage",
		"database.read",
		"database.write",
		"env.read",
		"env.reveal",
		"env.write",
		"kipper.read",
		"kipper.write",
		"members.manage",
		"members.read",
		"pods.exec",
		"pods.logs.read",
		"project.delete",
		"project.read",
		"project.settings",
		"secrets.read",
		"secrets.write",
		"storage.read",
		"storage.write",
		"terminal.open",
		"workloads.read",
		"workloads.restart",
	},
}

// All returns the catalogue in name order.
//
// It copies, as does Lookup, because this package is the authorization source
// of truth and a caller sorting or appending to what it hands back would be
// rewriting that: the verb slices are shared between capabilities, so one
// in-place sort would change what several of them render.
func All() []Capability {
	out := make([]Capability, 0, len(catalogue))
	for _, c := range catalogue {
		out = append(out, c.clone())
	}
	return out
}

// Lookup finds a capability by name.
func Lookup(name Name) (Capability, bool) {
	for _, c := range catalogue {
		if c.Name == name {
			return c.clone(), true
		}
	}
	return Capability{}, false
}

// clone deep-copies a capability, including the slices inside its claims.
func (c Capability) clone() Capability {
	out := c
	out.Touches = slices.Clone(c.Touches)
	out.Claims = make([]Claim, 0, len(c.Claims))
	for _, claim := range c.Claims {
		out.Claims = append(out.Claims, Claim{
			APIGroup:  claim.APIGroup,
			Resources: slices.Clone(claim.Resources),
			Verbs:     slices.Clone(claim.Verbs),
		})
	}
	return out
}

// Unknown returns the names that are not in the catalogue, so a caller can say
// which ones rather than only that something was wrong.
func Unknown(names []Name) []Name {
	var out []Name
	for _, n := range names {
		if _, ok := Lookup(n); !ok {
			out = append(out, n)
		}
	}
	return out
}

// Rules renders the Kubernetes rules for a set of capabilities.
//
// A name that is not in the catalogue, or one that is not grantable, renders
// nothing. That is the direction that fails safely: a role naming a capability
// this build does not know grants less than it says rather than more, which is
// what a rollback and a migration to an older target both need.
//
// The output is ordered by capability name and not by the order the names
// arrived in, so the reconciler can compare what it renders against what is on
// the cluster without rewriting the Role every pass.
func Rules(names []Name) []rbacv1.PolicyRule {
	wanted := make(map[Name]bool, len(names))
	for _, n := range names {
		wanted[n] = true
	}
	var out []rbacv1.PolicyRule
	for _, c := range catalogue {
		if !wanted[c.Name] || !c.Grantable {
			continue
		}
		for _, claim := range c.Claims {
			out = append(out, rbacv1.PolicyRule{
				APIGroups: []string{claim.APIGroup},
				Resources: slices.Clone(claim.Resources),
				Verbs:     slices.Clone(claim.Verbs),
			})
		}
	}
	return out
}

// Subset reports whether every capability in inner is also in outer. Grant
// capping is this comparison, so it is not a convenience.
func Subset(inner, outer []Name) bool {
	have := make(map[Name]bool, len(outer))
	for _, n := range outer {
		have[n] = true
	}
	for _, n := range inner {
		if !have[n] {
			return false
		}
	}
	return true
}

// BuiltIn returns the capability set standing for a built-in project role, or
// nothing for a name that is not one.
func BuiltIn(role Role) []Name {
	names, ok := builtIn[role]
	if !ok {
		return nil
	}
	out := make([]Name, len(names))
	copy(out, names)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
