// Package internalpath validates and normalises route guard paths.
package internalpath

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxPaths caps route.internalPaths and route.publicPaths, matching the
// MaxItems markers on AppRoute. Keep the three in sync.
const MaxPaths = 20

// maxPathLength matches the MaxLength marker on the same fields.
const maxPathLength = 256

// Allow unreserved path characters for safe interpolation into Traefik rules.
var pathPattern = regexp.MustCompile(SchemaPattern)

// SchemaPattern validates canonical paths and matches the AppRoute kubebuilder
// markers; internal_paths_schema_test.go checks for drift. Writers use Clean
// to remove trailing slashes before storing paths.
//
// Segments allow unreserved characters and dot-prefixed names such as /.env,
// while excluding the traversal segments "." and "..".
const SchemaPattern = `^(/(\.{3,}|\.*[A-Za-z0-9_~-][A-Za-z0-9._~-]*))+$`

// Kubernetes rejects these sequences and suffixes in Exact/Prefix paths.
// Validate composed paths before writing an Ingress, which is accepted atomically.
var (
	invalidIngressSequences = []string{"//", "/./", "/../", "%2f", "%2F"}
	invalidIngressSuffixes  = []string{"/..", "/."}
)

// Default returns the prefixes blocked when the cluster route guard is enabled.
//
//   - /actuator: Spring Boot management endpoints.
//   - /debug/pprof: Go profiling endpoints exposing runtime data.
//   - /internal: conventionally private application APIs.
//   - /.git: repository contents and history.
//   - /.env: configuration that may contain credentials.
//
// Keep defaults narrow to limit disruption. Apps can add ambiguous paths such as
// /metrics and /debug/vars through route.internalPaths.
func Default() []string {
	return []string{"/actuator", "/debug/pprof", "/internal", "/.git", "/.env"}
}

// Validate checks path limits and syntax, identifying invalid entries in errors.
func Validate(paths []string) error {
	if len(paths) > MaxPaths {
		return fmt.Errorf("at most %d paths are supported, got %d", MaxPaths, len(paths))
	}
	for _, p := range paths {
		if err := validateOne(p); err != nil {
			return err
		}
	}
	return nil
}

func validateOne(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("an empty path refuses nothing")
	case len(p) > maxPathLength:
		return fmt.Errorf("path %q is longer than %d characters", p, maxPathLength)
	case !strings.HasPrefix(p, "/"):
		return fmt.Errorf("path %q must start with a slash", p)
	}
	// Accept one optional trailing slash.
	trimmed := strings.TrimSuffix(p, "/")
	if trimmed == "" {
		return fmt.Errorf(`path %q is the whole route, which would refuse every request`, p)
	}
	if !pathPattern.MatchString(trimmed) {
		for _, segment := range strings.Split(trimmed, "/") {
			if segment == "." || segment == ".." {
				return fmt.Errorf("path %q holds a %q segment, which names somewhere else", p, segment)
			}
		}
		return fmt.Errorf("path %q may only contain letters, digits and . _ ~ - between single slashes", p)
	}
	return nil
}

// IngressPathAllowed checks Kubernetes Exact/Prefix path restrictions.
// Apply it after composing a validated prefix with the route base path.
func IngressPathAllowed(p string) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}
	for _, seq := range invalidIngressSequences {
		if strings.Contains(p, seq) {
			return false
		}
	}
	for _, suffix := range invalidIngressSuffixes {
		if strings.HasSuffix(p, suffix) {
			return false
		}
	}
	return true
}

// Clean trims trailing slashes and deduplicates valid entries in input order.
// It drops invalid entries, including those accepted by older CRD schemas;
// callers accepting user input should Validate first.
func Clean(paths []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		if validateOne(p) != nil {
			continue
		}
		p = strings.TrimSuffix(p, "/")
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// Covers matches normalised paths at segment boundaries: /actuator covers
// /actuator/metrics while /actuators remains separate.
func Covers(prefix, path string) bool {
	path = strings.TrimSuffix(path, "/")
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}
