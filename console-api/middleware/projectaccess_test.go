package middleware

import (
	"context"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

// stubMembers is an in-memory ProjectMemberSource for tests.
type stubMembers map[string]map[string]string

func (s stubMembers) ProjectMembers(_ context.Context, project string) (map[string]string, bool, error) {
	m, ok := s[project]
	return m, ok, nil
}

func projectNamespace(name, project string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{kipperlabels.Project: project},
		},
	}
}

func TestProjectAccess_Allows(t *testing.T) {
	tests := []struct {
		role     string
		required string
		want     bool
	}{
		{"owner", "deployer", true},
		{"deployer", "deployer", true},
		{"viewer", "deployer", false},
		{"deployer", "viewer", true},
		{"owner", "owner", true},
		{"deployer", "owner", false},
		{"", "viewer", false},
		{"viewer", "", false},
	}
	for _, tt := range tests {
		a := ProjectAccess{Role: tt.role}
		if got := a.Allows(tt.required); got != tt.want {
			t.Errorf("Allows(role=%q, required=%q) = %v, want %v", tt.role, tt.required, got, tt.want)
		}
	}
}

func TestProjectAccessResolver_CacheIsBounded(t *testing.T) {
	client := fake.NewClientset(kipperNamespace())
	roles := NewRoleStore(fake.NewClientset(kipperNamespace(), roleConfigMap(`{}`)))
	r := NewProjectAccessResolver(client, roles, stubMembers{})
	ctx := context.Background()

	// Probe far more distinct (nonexistent) names than the cache holds.
	for i := 0; i < nsCacheMax*3; i++ {
		if _, err := r.projectForName(ctx, "ghost-"+strconv.Itoa(i)); err != nil {
			t.Fatalf("projectForName: %v", err)
		}
	}

	r.mu.RLock()
	size := len(r.nsProj)
	r.mu.RUnlock()
	if size > nsCacheMax {
		t.Fatalf("cache grew to %d entries, want <= %d", size, nsCacheMax)
	}
}

func TestProjectAccessResolver_Resolve(t *testing.T) {
	client := fake.NewClientset(
		kipperNamespace(),
		projectNamespace("blog", "blog"),
		projectNamespace("blog-test", "blog"),
		projectNamespace("shop", "shop"),
		// A multi-environment project: its namespaces are all suffixed, so no
		// namespace matches the bare project name "team".
		projectNamespace("team-prod", "team"),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
	)
	roles := NewRoleStore(fake.NewClientset(kipperNamespace(), roleConfigMap(
		`{"root@test.com":"admin","dev@test.com":"member","viewer@test.com":"member","outsider@test.com":"member","lead@test.com":"member"}`,
	)))
	members := stubMembers{
		"blog": {"dev@test.com": "deployer", "viewer@test.com": "viewer"},
		"shop": {"dev@test.com": "owner"},
		"team": {"lead@test.com": "owner"},
	}
	r := NewProjectAccessResolver(client, roles, members)
	ctx := context.Background()

	tests := []struct {
		name      string
		email     string
		namespace string
		wantOK    bool
		wantRole  string
		wantAdmin bool
	}{
		{"admin on any project", "root@test.com", "blog", true, "owner", true},
		{"admin on system namespace", "root@test.com", "kube-system", true, "owner", true},
		{"deployer in their project", "dev@test.com", "blog", true, "deployer", false},
		{"deployer in project env namespace", "dev@test.com", "blog-test", true, "deployer", false},
		{"owner in another project", "dev@test.com", "shop", true, "owner", false},
		{"multi-env project reached by project name", "lead@test.com", "team", true, "owner", false},
		{"multi-env project reached by env namespace", "lead@test.com", "team-prod", true, "owner", false},
		{"viewer role resolves to viewer", "viewer@test.com", "blog", true, "viewer", false},
		{"member of no projects denied", "outsider@test.com", "blog", false, "", false},
		{"non-member on foreign project denied", "viewer@test.com", "shop", false, "", false},
		{"non-admin on system namespace denied", "dev@test.com", "kube-system", false, "", false},
		{"unknown user denied", "ghost@test.com", "blog", false, "", false},
		{"nonexistent namespace denied for non-admin", "dev@test.com", "ghost-ns", false, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := r.Resolve(ctx, tt.email, tt.namespace)
			if ok != tt.wantOK {
				t.Fatalf("Resolve ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.Role != tt.wantRole {
				t.Errorf("role = %q, want %q", got.Role, tt.wantRole)
			}
			if got.IsAdmin != tt.wantAdmin {
				t.Errorf("isAdmin = %v, want %v", got.IsAdmin, tt.wantAdmin)
			}
		})
	}
}

// Namespace names are not unique across projects: project "shop" with an
// environment "prod" and project "shop-prod" with a default environment both
// resolve to the namespace "shop-prod". The reconciler refuses to adopt a
// namespace another project already owns, which contains that collision in the
// cluster — but the project-level /projects/{name} handlers act on the Project
// of that name, so resolving the segment through the namespace label would admit
// shop's owner to shop-prod's Project and lock shop-prod's own owner out of it.
// The namespace-level handlers on the same subtree need the opposite answer,
// which is why the two scopes exist.
func TestResolveProject_NamesTheProjectRatherThanWhoeverOwnsTheNamespace(t *testing.T) {
	client := fake.NewClientset(
		kipperNamespace(),
		// shop got there first, so the namespace carries its label.
		projectNamespace("shop-prod", "shop"),
	)
	roles := NewRoleStore(fake.NewClientset(kipperNamespace(), roleConfigMap(
		`{"shopowner@test.com":"deployer","sprowner@test.com":"deployer","root@test.com":"admin"}`)))
	r := NewProjectAccessResolver(client, roles, stubMembers{
		"shop":      {"shopowner@test.com": ProjectRoleOwner},
		"shop-prod": {"sprowner@test.com": ProjectRoleOwner},
	})
	ctx := context.Background()

	access, ok := r.ResolveProject(ctx, "sprowner@test.com", "shop-prod")
	if !ok {
		t.Fatal("shop-prod's own owner must reach shop-prod")
	}
	if access.Project != "shop-prod" || access.Role != ProjectRoleOwner {
		t.Errorf("resolved to %q as %q, want shop-prod as owner", access.Project, access.Role)
	}

	if _, ok := r.ResolveProject(ctx, "shopowner@test.com", "shop-prod"); ok {
		t.Error("owning the namespace shop-prod must not confer authority over the project shop-prod")
	}

	// The namespace path still answers with the project that owns it, which is
	// what routes naming a real namespace depend on.
	nsAccess, ok := r.Resolve(ctx, "shopowner@test.com", "shop-prod")
	if !ok || nsAccess.Project != "shop" {
		t.Errorf("Resolve on the namespace = %q (ok=%v), want shop", nsAccess.Project, ok)
	}
}

// A project whose environments are all suffixed has no namespace matching its
// bare name, and an admin is not a member of anything. Both reached the project
// through the old namespace lookup, so both need to keep working without it.
func TestResolveProject_MultiEnvironmentProjectsAndAdmins(t *testing.T) {
	client := fake.NewClientset(kipperNamespace(), projectNamespace("team-prod", "team"))
	roles := NewRoleStore(fake.NewClientset(kipperNamespace(), roleConfigMap(
		`{"lead@test.com":"deployer","root@test.com":"admin","ghost@test.com":""}`)))
	r := NewProjectAccessResolver(client, roles, stubMembers{
		"team": {"lead@test.com": ProjectRoleOwner},
	})
	ctx := context.Background()

	if access, ok := r.ResolveProject(ctx, "lead@test.com", "team"); !ok || access.Role != ProjectRoleOwner {
		t.Errorf("a member must reach a project with no namespace of its bare name: ok=%v role=%q", ok, access.Role)
	}
	access, ok := r.ResolveProject(ctx, "root@test.com", "team")
	if !ok || !access.IsAdmin || access.Role != ProjectRoleOwner {
		t.Errorf("an admin acts as owner on any project: ok=%v admin=%v role=%q", ok, access.IsAdmin, access.Role)
	}
	if _, ok := r.ResolveProject(ctx, "nobody@test.com", "team"); ok {
		t.Error("an unknown user must be denied")
	}
	if _, ok := r.ResolveProject(ctx, "lead@test.com", "ghost"); ok {
		t.Error("a project that does not exist must be denied")
	}
	if _, ok := r.ResolveProject(ctx, "lead@test.com", ""); ok {
		t.Error("an empty project name must be denied")
	}
}

// Resolve keeps answering exactly as it did before ResolveProject was split out
// of it, since every namespace-scoped route still depends on it. The admin cases
// are the ones a refactor can quietly change: an admin is a member of nothing,
// so they pass on the strength of the global role alone.
func TestResolve_AdminSemanticsAreUnchanged(t *testing.T) {
	client := fake.NewClientset(
		kipperNamespace(),
		projectNamespace("blog", "blog"),
		// A namespace no Kipper project owns, like a system one.
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "unowned"}},
	)
	roles := NewRoleStore(fake.NewClientset(kipperNamespace(), roleConfigMap(
		`{"root@test.com":"admin","dev@test.com":"deployer"}`)))
	r := NewProjectAccessResolver(client, roles, stubMembers{
		"blog":       {"dev@test.com": ProjectRoleDeployer},
		"memberless": {},
	})
	ctx := context.Background()

	access, ok := r.Resolve(ctx, "root@test.com", "unowned")
	if !ok || !access.IsAdmin || access.Project != "" {
		t.Errorf("admin on an unlabelled namespace = (ok=%v admin=%v project=%q), want allowed with no project",
			ok, access.IsAdmin, access.Project)
	}

	access, ok = r.Resolve(ctx, "root@test.com", "ghost-ns")
	if !ok || !access.IsAdmin || access.Project != "ghost-ns" {
		t.Errorf("admin on a namespace that does not exist = (ok=%v admin=%v project=%q), want allowed with the name itself",
			ok, access.IsAdmin, access.Project)
	}

	if _, ok := r.Resolve(ctx, "dev@test.com", "unowned"); ok {
		t.Error("a non-admin must not reach a namespace no project owns")
	}

	// A project that exists but lists nobody denies every non-admin, and still
	// admits an admin. This is the case that would break if ResolveProject
	// started proving the Project CR exists.
	if _, ok := r.ResolveProject(ctx, "dev@test.com", "memberless"); ok {
		t.Error("a project with no members must admit no non-admin")
	}
	if _, ok := r.ResolveProject(ctx, "root@test.com", "memberless"); !ok {
		t.Error("an admin reaches a project with no members")
	}
}

// The resolver is the gate every guarded project route runs through, so its
// answer for a role this build does not know is the one that matters most.
//
// It must be no. A member holding such a role holds nothing: the projection
// binds them nowhere, the project index hides the project from them, and this
// refuses them. All three have to agree, because a role arrives by kubectl, by
// a restore, or by a migration from a cluster that had it, and none of those
// asks this build's permission first.
func TestTheResolverRefusesARoleThisBuildDoesNotKnow(t *testing.T) {
	members := stubMembers{"blog": {
		"lead@test.com":     ProjectRoleOwner,
		"stranger@test.com": "acme.support",
	}}
	client := fake.NewClientset(kipperNamespace(), projectNamespace("blog", "blog"))
	roles := NewRoleStore(fake.NewClientset(kipperNamespace(), roleConfigMap(
		`{"lead@test.com":"member","stranger@test.com":"member"}`,
	)))
	resolver := NewProjectAccessResolver(client, roles, members)

	if _, ok := resolver.Resolve(context.Background(), "stranger@test.com", "blog"); ok {
		t.Error("the resolver admitted a member holding a role it does not know; nothing else grants them anything, so this would be their only access")
	}

	access, ok := resolver.Resolve(context.Background(), "lead@test.com", "blog")
	if !ok {
		t.Fatal("the resolver refused the owner, so it is refusing everybody rather than the unknown role")
	}
	if access.Role != ProjectRoleOwner {
		t.Errorf("the owner resolved as %q", access.Role)
	}
}
