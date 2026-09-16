package v1alpha1

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/getkipper/kipper/controller/pkg/internalpath"
)

// Keep the generated CRD pattern in sync with CLI and API validation.
func TestInternalPathSchemaMatchesTheValidator(t *testing.T) {
	crd, err := os.ReadFile("../../../deploy/crds/kipper.run_apps.yaml")
	if err != nil {
		t.Fatalf("reading the generated CRD: %v", err)
	}

	found := 0
	for _, line := range strings.Split(string(crd), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "pattern: ^(/") {
			continue
		}
		found++
		if got := strings.TrimPrefix(line, "pattern: "); got != internalpath.SchemaPattern {
			t.Errorf("the CRD carries %q, the validator uses %q", got, internalpath.SchemaPattern)
		}
	}
	if found != 2 {
		t.Fatalf("found %d path patterns in the CRD, want 2 (internalPaths and publicPaths)", found)
	}
}

// Validate accepts a trailing slash; Clean removes it before CRD validation.
func TestInternalPathSchemaAndValidatorAgree(t *testing.T) {
	schema := regexp.MustCompile(internalpath.SchemaPattern)
	for _, p := range []string{
		"/actuator", "/internal", "/.git", "/.env", "/admin", "/api/internal/v2",
		"/a.b~c-d_e", "/...", "/..foo", "/.foo", "/...hidden",
		"/", "//", "/.", "/..", "/a/./b", "/a/../b", "/a//b", "/a%2fb", "/a b",
		"/ad`min", "/a?b", "/a;b",
	} {
		bySchema := schema.MatchString(p)
		byValidator := internalpath.Validate([]string{p}) == nil
		if bySchema != byValidator {
			t.Errorf("%q: schema accepts %v, validator accepts %v", p, bySchema, byValidator)
		}
	}

	if internalpath.Validate([]string{"/actuator/"}) != nil {
		t.Error("a trailing slash is how people write a prefix, and Validate takes it")
	}
	if schema.MatchString("/actuator/") {
		t.Error("the schema expects the canonical form Clean produces")
	}
	if got := internalpath.Clean([]string{"/actuator/"}); len(got) != 1 || !schema.MatchString(got[0]) {
		t.Errorf("Clean(%q) = %q, which the schema must accept", "/actuator/", got)
	}
}
