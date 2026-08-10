package secretname

import "testing"

// An App, a Function and a Job may all be called "api" in one namespace. Before
// the kind was part of these names all three resolved to the same Secret, so
// two controllers authored it in turn and the third read whichever write landed
// last. These Secrets carry resolved service credentials, so that is one
// workload reading another's database password.
func TestDerivedNamesAreDisjointAcrossKinds(t *testing.T) {
	kinds := []Kind{KindApp, KindFunction, KindJob}

	for _, build := range []struct {
		what string
		name func(Kind) string
	}{
		{"env", func(k Kind) string { return Env(k, "api") }},
		{"secrets", func(k Kind) string { return Secrets(k, "api") }},
		{"binding", func(k Kind) string { return Binding("db", k, "api") }},
	} {
		seen := make(map[string]Kind, len(kinds))
		for _, k := range kinds {
			got := build.name(k)
			if other, clash := seen[got]; clash {
				t.Errorf("%s: %s and %s both derive %q", build.what, other, k, got)
			}
			seen[got] = k
		}
	}
}

func TestNames(t *testing.T) {
	tests := []struct {
		got  string
		want string
	}{
		{Env(KindApp, "api"), "app-api-env"},
		{Env(KindFunction, "api"), "function-api-env"},
		{Env(KindJob, "migrate"), "job-migrate-env"},
		{Secrets(KindApp, "api"), "app-api-secrets"},
		{Secrets(KindFunction, "api"), "function-api-secrets"},
		{Binding("db", KindApp, "api"), "db-app-api-credentials"},
		{Binding("db", KindFunction, "api"), "db-function-api-credentials"},
		{ServiceCredentials("db"), "db-credentials"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// A workload name that happens to start with another kind's prefix must not
// collide with that kind's Secret.
func TestKindPrefixIsNotAmbiguous(t *testing.T) {
	if a, b := Env(KindApp, "function-api"), Env(KindFunction, "api"); a == b {
		t.Errorf("App %q collides with Function %q", a, b)
	}
}
