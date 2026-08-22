package capability

import (
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

func TestAllNamesAreUniqueAndSorted(t *testing.T) {
	seen := make(map[Name]bool, len(All()))
	previous := Name("")
	for _, c := range All() {
		if seen[c.Name] {
			t.Errorf("capability %q appears twice", c.Name)
		}
		seen[c.Name] = true
		if c.Name <= previous {
			t.Errorf("catalogue is out of order: %q follows %q", c.Name, previous)
		}
		previous = c.Name
	}
}

func TestLookupReportsWhetherTheNameExists(t *testing.T) {
	if _, ok := Lookup("kipper.read"); !ok {
		t.Fatal("kipper.read is in the catalogue and was not found")
	}
	if _, ok := Lookup("kipper.readd"); ok {
		t.Fatal("a name that is not in the catalogue was found")
	}
}

// Invariant 4, truthfulness: the resources a capability declares it touches are
// exactly the ones its rules name. A description generated from the declaration
// cannot then drift from what the capability actually grants.
func TestDeclaredResourcesMatchTheRules(t *testing.T) {
	for _, c := range All() {
		declared := make(map[string]bool, len(c.Touches))
		for _, r := range c.Touches {
			declared[r] = true
		}
		fromRules := make(map[string]bool)
		for _, rule := range c.Claims {
			for _, r := range rule.Resources {
				fromRules[r] = true
			}
		}
		for r := range fromRules {
			if !declared[r] {
				t.Errorf("%s: rules touch %q, which it does not declare", c.Name, r)
			}
		}
		for r := range declared {
			if !fromRules[r] {
				t.Errorf("%s: declares %q, which no rule touches", c.Name, r)
			}
		}
	}
}

func TestPlaneKCapabilitiesClaimRulesAndPlaneCOnesDoNot(t *testing.T) {
	for _, c := range All() {
		switch {
		case c.Planes.Has(PlaneK) && len(c.Claims) == 0:
			t.Errorf("%s is a plane K capability with no rules to render", c.Name)
		case !c.Planes.Has(PlaneK) && len(c.Claims) > 0:
			t.Errorf("%s renders rules but does not claim plane K", c.Name)
		}
		if !c.Planes.Has(PlaneC) && !c.Planes.Has(PlaneK) {
			t.Errorf("%s is in no plane", c.Name)
		}
	}
}

// A capability renders its own claim and nothing else. The bound the invariants
// check is a limit on what may be claimed, never the set that ships.
func TestRulesRenderOnlyWhatIsClaimed(t *testing.T) {
	rules := Rules([]Name{"pods.exec"})
	if len(rules) != 1 {
		t.Fatalf("pods.exec claims one rule, rendered %d", len(rules))
	}
	if got := rules[0].Resources; len(got) != 1 || got[0] != "pods/exec" {
		t.Errorf("pods.exec rendered %v", got)
	}
	if got := rules[0].Verbs; len(got) != 1 || got[0] != "create" {
		t.Errorf("pods.exec rendered verbs %v", got)
	}
}

func TestRulesSkipsPlaneCOnlyAndUnknownNames(t *testing.T) {
	if rules := Rules([]Name{"members.manage"}); len(rules) != 0 {
		t.Errorf("members.manage grants nothing in plane K, rendered %v", rules)
	}
	if rules := Rules([]Name{"not.a.capability"}); len(rules) != 0 {
		t.Errorf("an unknown name rendered %v rather than nothing", rules)
	}
}

// An unknown name renders nothing rather than everything, which is the failure
// mode that matters: a role naming a capability this build does not know grants
// less than it says instead of more.
func TestRulesRendersTheKnownPartOfAMixedSet(t *testing.T) {
	rules := Rules([]Name{"not.a.capability", "pods.logs.read"})
	if len(rules) != 1 {
		t.Fatalf("expected the one known capability to render, got %d rules", len(rules))
	}
	if got := rules[0].Resources[0]; got != "pods/log" {
		t.Errorf("rendered %q", got)
	}
}

func TestUnknownReportsNamesNotInTheCatalogue(t *testing.T) {
	got := Unknown([]Name{"kipper.read", "not.a.capability", "members.manage"})
	if len(got) != 1 || got[0] != "not.a.capability" {
		t.Errorf("Unknown returned %v", got)
	}
}

func TestDescriptionIsGeneratedFromTheClaim(t *testing.T) {
	c, ok := Lookup("secrets.read")
	if !ok {
		t.Fatal("secrets.read is missing")
	}
	got := c.Description()
	for _, want := range []string{"read", "secrets", "configmaps"} {
		if !strings.Contains(got, want) {
			t.Errorf("description %q does not mention %q", got, want)
		}
	}
}

func TestPlaneCOnlyCapabilitiesDescribeTheirEffect(t *testing.T) {
	for _, c := range All() {
		if c.Planes.Has(PlaneK) {
			continue
		}
		if c.Effect == "" {
			t.Errorf("%s grants nothing in plane K and states no effect, so nothing can describe it", c.Name)
		}
		if c.Description() == "" {
			t.Errorf("%s has an empty description", c.Name)
		}
	}
}

// Grant capping compares capability sets, so the subset test is load-bearing
// rather than a convenience.
func TestSubsetComparesSets(t *testing.T) {
	cases := []struct {
		name  string
		inner []Name
		outer []Name
		want  bool
	}{
		{"empty is a subset of anything", nil, []Name{"kipper.read"}, true},
		{"equal sets", []Name{"kipper.read"}, []Name{"kipper.read"}, true},
		{"proper subset", []Name{"kipper.read"}, []Name{"kipper.read", "pods.exec"}, true},
		{"not a subset", []Name{"pods.exec"}, []Name{"kipper.read"}, false},
		{"nothing is a subset of empty", []Name{"kipper.read"}, nil, false},
		{"order does not matter", []Name{"pods.exec", "kipper.read"}, []Name{"kipper.read", "pods.exec"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Subset(tc.inner, tc.outer); got != tc.want {
				t.Errorf("Subset(%v, %v) = %v", tc.inner, tc.outer, got)
			}
		})
	}
}

// Every capability in the first catalogue is grantable. The flag exists because
// the projection refuses to render one that is not, and a catalogue with no
// ungrantable entry would leave that branch untested.
func TestGrantableIsDeclaredForEveryCapability(t *testing.T) {
	for _, c := range All() {
		if !c.Grantable {
			continue
		}
		if len(c.Touches) == 0 && c.Effect == "" {
			t.Errorf("%s is grantable but says nothing about what it grants", c.Name)
		}
	}
}

// Guards the projection's own refusal: a name in the catalogue but not marked
// grantable renders nothing, so a hand-written or restored object carrying it
// projects as no access rather than as access nobody authorised.
//
// Every capability in the first catalogue is grantable, so this adds one for
// the length of the test. Iterating the real catalogue would pass without
// executing the branch.
func TestRulesRefusesAnUngrantableCapability(t *testing.T) {
	restore := catalogue
	t.Cleanup(func() { catalogue = restore })
	catalogue = append(append([]Capability{}, restore...), Capability{
		Name:      "zzz.withheld",
		Planes:    PlaneK,
		Action:    "read",
		Grantable: false,
		Touches:   []string{"nodes"},
		Claims: []Claim{{
			APIGroup:  groupCore,
			Resources: []string{"nodes"},
			Verbs:     []string{"get"},
		}},
	})

	if rules := Rules([]Name{"zzz.withheld"}); len(rules) > 0 {
		t.Errorf("an ungrantable capability rendered %v", rules)
	}
	if rules := Rules([]Name{"zzz.withheld", "pods.exec"}); len(rules) != 1 {
		t.Errorf("the grantable half of the set should still render, got %v", rules)
	}
}

func TestBuiltInSetsNest(t *testing.T) {
	viewer, deployer, owner := BuiltIn(RoleViewer), BuiltIn(RoleDeployer), BuiltIn(RoleOwner)
	if !Subset(viewer, deployer) {
		t.Error("viewer's capabilities are not a subset of deployer's")
	}
	if !Subset(deployer, owner) {
		t.Error("deployer's capabilities are not a subset of owner's")
	}
	if Subset(owner, viewer) {
		t.Error("owner's capabilities are a subset of viewer's, so the sets do not nest")
	}
}

func TestBuiltInNamesAreAllInTheCatalogue(t *testing.T) {
	for _, role := range []Role{RoleViewer, RoleDeployer, RoleOwner} {
		if got := Unknown(BuiltIn(role)); len(got) > 0 {
			t.Errorf("built-in %s names capabilities that do not exist: %v", role, got)
		}
	}
}

func TestBuiltInUnknownRoleGrantsNothing(t *testing.T) {
	if got := BuiltIn("supervisor"); len(got) > 0 {
		t.Errorf("an unknown built-in role granted %v", got)
	}
}

// Rendering a set must not depend on the order the names arrive in, because the
// reconciler compares rendered rules against what is already on the cluster and
// an unstable order would rewrite the Role on every pass.
func TestRulesIsStableAcrossInputOrder(t *testing.T) {
	forward := Rules([]Name{"kipper.read", "pods.exec", "secrets.read"})
	backward := Rules([]Name{"secrets.read", "pods.exec", "kipper.read"})
	if !equalRules(forward, backward) {
		t.Errorf("rendering depends on input order:\n%v\n%v", forward, backward)
	}
}

func equalRules(a, b []rbacv1.PolicyRule) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.Join(a[i].APIGroups, ",") != strings.Join(b[i].APIGroups, ",") {
			return false
		}
		if strings.Join(a[i].Resources, ",") != strings.Join(b[i].Resources, ",") {
			return false
		}
		if strings.Join(a[i].Verbs, ",") != strings.Join(b[i].Verbs, ",") {
			return false
		}
	}
	return true
}
