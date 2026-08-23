package memberbinding

import (
	"strings"
	"testing"
)

func TestANameIsSeventyTwoCharactersWhateverTheInputs(t *testing.T) {
	long := strings.Repeat("a", 253)
	for _, tc := range []struct{ project, role string }{
		{"a", "b"},
		{"shop", "owner"},
		{long, long},
		{"", ""},
	} {
		got := Name(tc.project, tc.role)
		if len(got) != 72 {
			t.Errorf("Name(%.10q, %.10q) is %d characters; the length has to be fixed or the prefix stops being a prefix", tc.project, tc.role, len(got))
		}
		if !strings.HasPrefix(got, "kipper-") {
			t.Errorf("Name(%q, %q) = %q, which does not carry the shape the listing enumerates by", tc.project, tc.role, got)
		}
	}
}

// Fixed-length halves are what make the name usable as a prefix. With a
// variable-length project part, one project's prefix could be another's, and
// the cluster-wide listing would return somebody else's bindings.
func TestOneProjectsPrefixIsNeverAnothersPrefix(t *testing.T) {
	for _, pair := range [][2]string{
		{"acme", "acme-support"},
		{"a", "ab"},
		{"shop", "shop-prod"},
	} {
		first, second := Prefix(pair[0]), Prefix(pair[1])
		if strings.HasPrefix(first, second) || strings.HasPrefix(second, first) {
			t.Errorf("prefix(%q)=%q and prefix(%q)=%q: one contains the other, so a listing would cross projects",
				pair[0], first, pair[1], second)
		}
	}
}

func TestANameStartsWithItsProjectsPrefix(t *testing.T) {
	for _, role := range []string{"owner", "deployer", "viewer", "acme.support"} {
		name := Name("acme", role)
		if !strings.HasPrefix(name, Prefix("acme")) {
			t.Errorf("Name(acme, %q) = %q does not start with %q, so the prefix listing would miss it", role, name, Prefix("acme"))
		}
	}
}

// The property grant capping and the listing both rest on: one binding per
// (project, role) pair, and no two pairs sharing one.
func TestDistinctPairsGetDistinctNames(t *testing.T) {
	seen := map[string][2]string{}
	for _, project := range []string{"acme", "acme-support", "shop", "shop-prod", "a"} {
		for _, role := range []string{"owner", "deployer", "viewer", "acme.support", "support"} {
			name := Name(project, role)
			if prev, clash := seen[name]; clash {
				t.Errorf("(%q,%q) and (%q,%q) both generate %q", prev[0], prev[1], project, role, name)
			}
			seen[name] = [2]string{project, role}
		}
	}
}

// Generating the same name twice has to give the same answer, because the
// reconciler compares what it renders against what is on the cluster.
func TestNamesAreStable(t *testing.T) {
	first := Name("acme", "owner")
	for i := 0; i < 100; i++ {
		if got := Name("acme", "owner"); got != first {
			t.Fatalf("Name is not stable: %q then %q", first, got)
		}
	}
}

// The name is a valid Kubernetes object name. A role name reaches it, and a
// digest is what keeps a role called "Support Team!" from producing something
// nothing can address.
func TestANameIsAValidObjectName(t *testing.T) {
	for _, role := range []string{"owner", "Support Team!", "a/b", strings.Repeat("x", 300), "ünïcode"} {
		name := Name("acme", role)
		for _, c := range name {
			isLower := c >= 'a' && c <= 'z'
			isDigit := c >= '0' && c <= '9'
			if !isLower && !isDigit && c != '-' {
				t.Errorf("Name(acme, %.20q) = %q carries %q, which an object name may not", role, name, c)
				break
			}
		}
	}
}

func TestTheLegacyNamesAreTheThreeThatShipToday(t *testing.T) {
	got := LegacyNames()
	want := map[string]bool{
		"kipper-project-owner":    true,
		"kipper-project-deployer": true,
		"kipper-project-viewer":   true,
	}
	if len(got) != len(want) {
		t.Fatalf("LegacyNames returned %v", got)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("LegacyNames returned %q, which is not a name any released build wrote", n)
		}
	}
}

// The listing enumerates by name shape and applies no label selector, because a
// label is exactly what drifts. So the shape test has to be exact: too loose
// and it sweeps in objects that are not ours, too tight and drift hides one.
func TestIsManagedRecognisesBothGenerationsAndNothingElse(t *testing.T) {
	for _, name := range append(LegacyNames(), Name("acme", "owner"), Name("shop", "viewer")) {
		if !IsManaged(name) {
			t.Errorf("IsManaged(%q) = false, so the listing would not see a binding this build wrote", name)
		}
	}
	for _, name := range []string{
		"",
		"kipper-project-admin",
		"kipper-console-role-writer",
		"kipper-" + strings.Repeat("a", 32) + "-" + strings.Repeat("a", 31), // one short
		"kipper-" + strings.Repeat("a", 32) + "-" + strings.Repeat("z", 32), // z is not hex
		"kipper-" + strings.Repeat("a", 32),
		"acme-" + strings.Repeat("a", 32) + "-" + strings.Repeat("a", 32),
	} {
		if IsManaged(name) {
			t.Errorf("IsManaged(%q) = true, so the listing would treat somebody else's object as ours", name)
		}
	}
}

// ProjectPrefixOf is what lets a binding be attributed to its project when the
// label has gone. It must answer for generated names and refuse everything
// else, because the caller uses the answer to decide which project may edit the
// binding.
func TestProjectPrefixOfAnswersOnlyForGeneratedNames(t *testing.T) {
	got, ok := ProjectPrefixOf(Name("shop", "owner"))
	if !ok || got != Prefix("shop") {
		t.Errorf("ProjectPrefixOf(generated) = (%q, %v), want (%q, true)", got, ok, Prefix("shop"))
	}

	// Every role of one project shares the prefix, and that is the point:
	// listing by it finds a role the project no longer lists.
	for _, role := range []string{"owner", "deployer", "viewer", "some-custom-role"} {
		if p, ok := ProjectPrefixOf(Name("shop", role)); !ok || p != Prefix("shop") {
			t.Errorf("role %q indexes under %q, so a binding for it is invisible to the project's own listing", role, p)
		}
	}

	// A legacy name carries no digest. Returning any prefix for one would
	// attribute somebody's binding to a project by guesswork.
	for _, legacy := range LegacyNames() {
		if p, ok := ProjectPrefixOf(legacy); ok {
			t.Errorf("the fixed name %q was attributed to prefix %q, which is a guess", legacy, p)
		}
	}

	for _, name := range []string{"", "kipper-", "kipper-nothex-nothex", "somebody-elses-binding", Prefix("shop")} {
		if p, ok := ProjectPrefixOf(name); ok {
			t.Errorf("ProjectPrefixOf(%q) claimed prefix %q for a name this package never generated", name, p)
		}
	}

	// Two projects never share a prefix.
	if Prefix("shop") == Prefix("shop-support") {
		t.Error("two projects share a prefix, so one project's revoke pass edits the other's bindings")
	}
}
