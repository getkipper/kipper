// Package capability shares project abilities across API authorization, RBAC
// rendering, and CLI descriptions. PlaneC controls console API access; PlaneK
// also produces Kubernetes rules. Installer tests check the catalog against
// console-api and project-owner permissions in the real manifests.
package capability

import (
	"slices"
	"sort"

	rbacv1 "k8s.io/api/rbac/v1"
)

// Name identifies a capability. It is the string stored in a ProjectRole and
// the string an operator types, so it is part of the released surface.
type Name string

// Plane is a bitmask: PlaneK grants Kubernetes RBAC rules; PlaneC gates
// console API routes.
type Plane uint8

const (
	PlaneK Plane = 1 << iota
	PlaneC
)

// Has reports whether p and q share at least one plane.
func (p Plane) Has(q Plane) bool { return p&q != 0 }

// Claim is one group of resources and the verbs a capability asks for on them.
// It becomes a PolicyRule unchanged, which is what lets a test compare a claim
// against the manifests without interpreting it.
type Claim struct {
	APIGroup  string
	Resources []string
	Verbs     []string
}

// Capability describes a grant and its operator-facing effects. Claims define
// Kubernetes rules; Touches and Action describe them. Effect describes grants
// without rules, AlsoReads adds console-only access, and Implications describes
// indirect grants. Only Grantable capabilities contribute to Rules.
type Capability struct {
	Name         Name
	Planes       Plane
	Claims       []Claim
	Touches      []string
	Action       string
	Effect       string
	Grantable    bool
	AlsoReads    string
	Implications []Implication
}

// Implication is a capability another one grants in practice, and the route by
// which it does.
type Implication struct {
	Grants Name
	Via    string
}

// Description is what the console and kip show. It is rendered from the
// declaration rather than written by hand, so prose cannot drift from rules.
func (c Capability) Description() string {
	if len(c.Touches) == 0 {
		out := c.Effect
		if c.AlsoReads != "" {
			out += ", and " + c.AlsoReads
		}
		return out + implicationSuffix(c.Implications)
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
	if c.AlsoReads != "" {
		out += ", and " + c.AlsoReads
	}
	return out + implicationSuffix(c.Implications)
}

// implicationSuffix puts the reach into the sentence a person reads. A grant
// decision made from a description that stops at the rules is made on less than
// the grant does.
func implicationSuffix(implications []Implication) string {
	if len(implications) == 0 {
		return ""
	}
	out := ""
	for i, imp := range implications {
		switch {
		case i == 0:
			out = ", which also grants " + string(imp.Grants)
		case i == len(implications)-1:
			out += " and " + string(imp.Grants)
		default:
			out += ", " + string(imp.Grants)
		}
		out += " " + imp.Via
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
		Name:      "files.read",
		Planes:    PlaneC,
		Grantable: true,
		Effect:    "read and download any file inside a running workload through the console",
		// Reading a container's filesystem reads whatever is mounted into it,
		// and a Secret mounted as a file is a file. Same reach as a shell,
		// arrived at through a different route, so it says the same thing.
		Implications: []Implication{{Grants: "secrets.read", Via: "by reading the files a Secret is mounted into"}},
	},
	{
		Name:      "files.write",
		Planes:    PlaneC,
		Grantable: true,
		Effect:    "change and upload files inside a running workload through the console",
		// Writing into a running container changes what it runs, so this is the
		// same reach as deploying to it: the code that ends up executing is the
		// caller's, in a namespace whose Secrets it can then read. The rule says
		// a file was written; this says what writing one is worth.
		Implications: []Implication{
			{Grants: "kipper.write", Via: "by changing what the running workload executes"},
			{Grants: "secrets.read", Via: "through the environment that code then runs with"},
		},
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
		Implications: []Implication{
			{Grants: "secrets.read", Via: "by declaring any Secret as an app's environment"},
			{Grants: "pods.logs.read", Via: "through the diagnosis routes, which read a workload's logs and send them to the AI provider"},
			{Grants: "workloads.read", Via: "through the optimization route, which reads deployments, pods and their metrics"},
		},
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
		Name:         "pods.exec",
		Planes:       PlaneK,
		Action:       "open a shell in",
		Grantable:    true,
		Touches:      []string{"pods/exec"},
		Implications: []Implication{{Grants: "secrets.read", Via: "by reading the environment of the container it opens"}},
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
		// Link consent is the widest of those settings and does not read like a
		// setting: it lets another project's apps reach this project's services
		// directly, past the ingress. Naming it is the difference between an
		// owner granting "settings" and an owner granting that. It belongs in
		// the effect and not in AlsoReads, which is for what a holder reads.
		Effect: "change the project's own settings, including which other projects may route into this one",
	},
	// secrets.read, secrets.write and pods.exec are the kubeconfig-level grants:
	// they render RBAC so a member can read and write Secrets and ConfigMaps
	// directly. No console route maps to them, because the console reaches the
	// same data through env.read, env.reveal and env.write, opens a shell
	// through terminal.open, and reads a container's files through files.read.
	// That is the plane split doing its job rather than an oversight: a deployer
	// changes an app's environment through the console today and holds no
	// Kubernetes access to the Secret underneath.
	//
	// The console doors are not narrower in what they can reach, only in what
	// they hand out. terminal.open and files.read both arrive at a container's
	// contents, so both declare that they grant secrets.read in practice; the
	// difference is that neither gives the member a kubeconfig.
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
		Name:         "secrets.write",
		Planes:       PlaneK,
		Action:       "create, change and delete",
		Grantable:    true,
		Touches:      []string{"secrets", "configmaps"},
		Implications: []Implication{{Grants: "secrets.read", Via: "because writing one requires reading what is there"}},
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
		// The same shell pods.exec reaches, arrived at through the console
		// rather than through Kubernetes, so it reads the same environment and
		// the same Secrets behind it. The plane split changes who holds
		// kubectl access; it does not change what a shell can read.
		Implications: []Implication{{Grants: "secrets.read", Via: "by reading the environment of the container it opens"}},
	},
	{
		Name:      "webhook.reveal",
		Planes:    PlaneC,
		Grantable: true,
		Effect:    "reveal the token an app's deploy webhook is called with",
		// The token is the deploy. Anyone holding it can post to the app's
		// webhook, and for an image app the image to run comes from the
		// request, so revealing the token is handing over the ability to run
		// chosen code in the namespace, with the app's environment and the
		// Secrets behind it. The rules say only that a string is read out of a
		// Secret, which is why this has to be declared: it is the widest reach
		// in the catalog that no rule expresses.
		Implications: []Implication{
			{Grants: "kipper.write", Via: "by posting an image of its choosing to the app's webhook"},
			{Grants: "secrets.read", Via: "through the environment the deployed image runs with"},
		},
	},
	{
		Name:      "workloads.read",
		Planes:    PlaneK | PlaneC,
		Action:    "read",
		Grantable: true,
		// The console routes under this read more than the rules grant: the
		// autoscale route returns the autoscaler's live status and current
		// utilization, and the usage route returns pod CPU and memory samples
		// from metrics.k8s.io. Neither API is in the claims, and neither can be,
		// because the staged ClusterRoles that back this capability do not carry
		// them and the bound test refuses a claim they do not.
		//
		// One more route under this capability is wider than any of that and is
		// not described here: the app build status reports whether the app's git
		// credential and the cluster's registry credentials authenticate, which
		// is a validity oracle for credentials the caller never sees. Narrowing
		// it would move who can reach it, so it is recorded against the route in
		// the console's authorization table rather than turned into a grant this
		// description could carry.
		AlsoReads: "the autoscaler status and resource metrics behind them",
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
		"files.read",
		"files.write",
		"kipper.read",
		"kipper.write",
		"members.read",
		"pods.logs.read",
		"project.read",
		"storage.read",
		"storage.write",
		"terminal.open",
		"webhook.reveal",
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
		"files.read",
		"files.write",
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
		"webhook.reveal",
		"workloads.read",
		"workloads.restart",
	},
}

// All returns deep copies of the catalog entries in name order. Callers may
// modify the returned entries without changing the authorization catalog.
func All() []Capability {
	out := make([]Capability, 0, len(catalogue))
	for _, c := range catalogue {
		out = append(out, c.clone())
	}
	return out
}

// Lookup returns a deep copy of the named capability, or a zero value and false
// if the name is unknown.
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
	out.Implications = slices.Clone(c.Implications)
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

// Unknown returns unrecognized names in input order, preserving duplicates.
// It returns nil when all names are known.
func Unknown(names []Name) []Name {
	var out []Name
	for _, n := range names {
		if _, ok := Lookup(n); !ok {
			out = append(out, n)
		}
	}
	return out
}

// Rules returns independent Kubernetes rules for known, grantable capabilities
// in capability-name order. Duplicate input names are collapsed; unknown and
// non-grantable names are ignored. It returns nil when no rules apply.
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

// Subset reports whether every capability in inner is also in outer.
// An empty inner set is always a subset.
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

// held is BuiltIn as a lookup, built once. Every gated request asks this
// question, and BuiltIn allocates and sorts a copy to answer it.
var held = func() map[Role]map[Name]bool {
	out := make(map[Role]map[Name]bool, len(builtIn))
	for role, names := range builtIn {
		set := make(map[Name]bool, len(names))
		for _, n := range names {
			set[n] = true
		}
		out[role] = set
	}
	return out
}()

// Holds reports whether a built-in project role carries a capability.
// Unknown roles and capabilities return false.
func Holds(role Role, name Name) bool {
	return held[role][name]
}

// KnownRole reports whether role names a built-in project role.
func KnownRole(role Role) bool {
	_, ok := held[role]
	return ok
}

// BuiltIn returns a sorted copy of a built-in role's capability names, or nil
// for an unknown role.
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
