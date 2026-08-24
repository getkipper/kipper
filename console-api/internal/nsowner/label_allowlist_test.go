package nsowner_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Reading `kipper.run/project` off a namespace and believing it is how nine
// separate places each ended up making an authorization decision from a value
// the caller could set. They now go through nsowner, which treats the label as
// a hint the named project has to back with a claim.
//
// Nothing stops the tenth. This is what stops the tenth: the label may be
// mentioned only where it is written, where it is displayed, or where it is
// used as a selector to gather candidates that something else then checks.
// Anywhere else, resolve through nsowner.
//
// The list is by file rather than by package, because a package that writes the
// label usually also reads it, and a package-wide exemption blinds this exactly
// where the misses happened.
var mayMentionTheLabel = map[string]string{
	// Writers: these stamp the label onto objects they create.
	"controllers/project_controller.go": "writes the label onto namespaces it creates",
	"controllers/project_quota.go":      "writes the label onto quota objects",
	"controllers/project_rbac.go":       "writes the label onto member bindings, and reads it as one of the revoke pass's anchors",
	"handlers/projects.go":              "writes the label onto namespaces it creates, and gathers candidates by it; every decision resolves through nsowner",

	// Selectors: these gather candidates by label, and whatever they hand the
	// candidates to decides.
	"controllers/app_links.go":           "lists a project's namespaces by label to map events; consent itself resolves through nsowner",
	"handlers/request_usage.go":          "lists a project's namespaces by label to gather candidates; which of them are the project's resolves through nsowner",
	"handlers/migration/capacity.go":     "lists namespaces by label to measure capacity",
	"handlers/routes.go":                 "puts the label in a response for display",
	"handlers/migration/orchestrator.go": "lists a project's namespaces by label to enumerate what to move",
	"handlers/migration/handler.go":      "mentions the label in a comment explaining why scope resolves through nsowner",
	"builder/builder.go":                 "declares the label constant used by the objects it builds",
	"internal/nsowner/nsowner.go":        "is the resolver",
}

// mentionsTheLabel matches the literal and the constant alike.
//
// Matching only the literal is how an earlier version of this rule would have
// waved through the two most dangerous readers on the cluster, both of which
// reach the label through labels.Project rather than by spelling it out.
func mentionsTheLabel(body string) bool {
	return strings.Contains(body, "kipper.run/project") ||
		strings.Contains(body, "labels.Project")
}

func TestOnlyTheResolverDecidesOwnershipFromTheLabel(t *testing.T) {
	const label = "kipper.run/project"

	root := "../.."
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// The root is "../..", whose Name() is "..", so a bare
			// hidden-directory test skips the whole walk and this passes
			// having read nothing. It did exactly that until a probe file
			// failed to trip it.
			if path == root {
				return nil
			}
			if info.Name() == "vendor" || info.Name() == "node_modules" || strings.HasPrefix(info.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, readErr := os.ReadFile(path) //nolint:gosec // walking this repository's own source at test time; the path comes from the walk, not from input
		if readErr != nil {
			return readErr
		}
		if !mentionsTheLabel(string(body)) {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if _, allowed := mayMentionTheLabel[filepath.ToSlash(rel)]; !allowed {
			offenders = append(offenders, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the source: %v", err)
	}

	for _, f := range offenders {
		t.Errorf("%s mentions %s. If it is deciding who owns a namespace, resolve through nsowner instead: the label is writable by anyone who can write a namespace. If it writes the label, displays it, or selects by it, add it to mayMentionTheLabel with the reason.", f, label)
	}

	// And the list may not rot. An entry naming a file that no longer mentions
	// the label is an exemption nobody needs, and the next person reads it as
	// permission.
	for f := range mayMentionTheLabel {
		body, readErr := os.ReadFile(filepath.Join(root, f))
		if readErr != nil {
			t.Errorf("mayMentionTheLabel names %s, which does not exist", f)
			continue
		}
		if !mentionsTheLabel(string(body)) {
			t.Errorf("mayMentionTheLabel exempts %s, which no longer mentions %s; take it off the list", f, label)
		}
	}
}
