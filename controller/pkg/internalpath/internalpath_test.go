package internalpath

import (
	"strings"
	"testing"
)

// Pin defaults so policy changes require an explicit test update.
func TestDefaultList(t *testing.T) {
	want := []string{"/actuator", "/debug/pprof", "/internal", "/.git", "/.env"}
	got := Default()
	if len(got) != len(want) {
		t.Fatalf("Default() = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Default() = %q, want %q", got, want)
		}
	}
}

func TestDefaultIsAbsoluteAndClean(t *testing.T) {
	for _, p := range Default() {
		if !strings.HasPrefix(p, "/") {
			t.Errorf("default entry %q is not absolute", p)
		}
		if p != strings.TrimSuffix(p, "/") {
			t.Errorf("default entry %q carries a trailing slash", p)
		}
	}
	if Validate(Default()) != nil {
		t.Errorf("the default list does not pass its own validation: %v", Validate(Default()))
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		ok    bool
	}{
		{"absolute path", []string{"/admin"}, true},
		{"nested path", []string{"/api/internal/v2"}, true},
		{"dotfile", []string{"/.env"}, true},
		{"trailing slash", []string{"/admin/"}, true},
		{"leading dots before a name", []string{"/...hidden"}, true},
		{"all dots", []string{"/..."}, true},
		{"empty list", nil, true},
		{"relative", []string{"admin"}, false},
		{"empty entry", []string{""}, false},
		{"the whole route", []string{"/"}, false},
		// Backquotes delimit Traefik router expressions.
		{"backquote", []string{"/ad`min"}, false},
		{"space", []string{"/ad min"}, false},
		{"parent traversal", []string{"/admin/../"}, false},
		{"current directory segment", []string{"/admin/./x"}, false},
		{"trailing dot segment", []string{"/admin/."}, false},
		{"consecutive slashes", []string{"/admin//x"}, false},
		{"encoded slash", []string{"/admin%2fx"}, false},
		{"percent encoding", []string{"/%61dmin"}, false},
		{"query string", []string{"/admin?x=1"}, false},
		{"too many", make([]string, MaxPaths+1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.paths)
			if tc.ok && err != nil {
				t.Fatalf("Validate(%q) = %v, want nil", tc.paths, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("Validate(%q) = nil, want an error", tc.paths)
			}
		})
	}
}

func TestCleanNormalisesAndDropsWhatCannotBeExpressed(t *testing.T) {
	got := Clean([]string{"/actuator/", "/actuator", "", "/", "/bad`path", "/Internal"})
	want := []string{"/actuator", "/Internal"}
	if len(got) != len(want) {
		t.Fatalf("Clean = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Clean = %q, want %q", got, want)
		}
	}
}

func TestCleanReturnsPathsAnIngressCanCarry(t *testing.T) {
	entries := append(Default(), "/admin", "/api/internal/v2", "/a.b~c-d_e")
	for _, p := range Clean(entries) {
		for _, form := range []string{p, p + "/", p + ";"} {
			if !IngressPathAllowed(form) {
				t.Errorf("Clean returned %q, and %q cannot be an Ingress path", p, form)
			}
		}
	}
}

func TestIngressPathAllowed(t *testing.T) {
	cases := map[string]bool{
		"/domains-api/actuator":   true,
		"/domains-api/actuator/":  true,
		"/domains-api/actuator;":  true,
		"/domains-api/.env":       true,
		"/domains-api//actuator":  false,
		"/domains-api/./actuator": false,
		"/domains-api/../x":       false,
		"/domains-api/actuator/.": false,
		"/x%2Factuator":           false,
		"/x%2factuator":           false,
		"relative/actuator":       false,
	}
	for p, want := range cases {
		if got := IngressPathAllowed(p); got != want {
			t.Errorf("IngressPathAllowed(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestCovers(t *testing.T) {
	cases := []struct {
		prefix, path string
		want         bool
	}{
		{"/actuator", "/actuator", true},
		{"/actuator", "/actuator/prometheus", true},
		{"/actuator", "/actuator/", true},
		{"/actuator", "/actuators", false},
		{"/actuator", "/act", false},
		{"/.env", "/.env", true},
		{"/actuator/health", "/actuator", false},
	}
	for _, tc := range cases {
		if got := Covers(tc.prefix, tc.path); got != tc.want {
			t.Errorf("Covers(%q, %q) = %v, want %v", tc.prefix, tc.path, got, tc.want)
		}
	}
}
