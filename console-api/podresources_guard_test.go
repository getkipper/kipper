package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestAllContainersDeclareResources walks every non-test Go file in this
// module and fails when a corev1.Container is built without CPU and memory
// requests and limits. Namespace ResourceQuota relies on every pod Kipper
// creates declaring resources; a container that omits them falls back to the
// LimitRange defaults, which are sized for tiny helpers and will OOMKill
// anything real. New pod-creating code paths must declare resources
// explicitly.
//
// A container passes when its composite literal sets Resources inline, or
// when it is assigned to a variable that later receives a .Resources
// assignment in the same function (the build-then-assign pattern used by the
// app/service/function controllers). The exemption is tied to the specific
// variable, so a second container literal in the same function cannot ride on
// the first one's assignment. Inline ResourceRequirements literals are
// additionally checked for both Requests and Limits with both cpu and memory,
// where that is statically visible.
func TestAllContainersDeclareResources(t *testing.T) {
	fset := token.NewFileSet()
	var violations []string

	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".") && path != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}

		corev1Alias := importAlias(file, "k8s.io/api/core/v1")
		if corev1Alias == "" {
			return nil
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			violations = append(violations, checkFunction(fset, fn, corev1Alias)...)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(violations) > 0 {
		t.Errorf("containers without full CPU/memory requests and limits:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// importAlias returns the name the file uses for the given import path, or
// "" if the file does not import it.
func importAlias(file *ast.File, importPath string) string {
	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, `"`) != importPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "v1"
	}
	return ""
}

// checkFunction reports every corev1.Container literal in fn that neither
// sets Resources inline nor is assigned to a variable that later receives a
// .Resources assignment in the same function.
func checkFunction(fset *token.FileSet, fn *ast.FuncDecl, corev1Alias string) []string {
	// Variables whose .Resources field is assigned directly (container.Resources = ...).
	resourcesAssigned := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range assign.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Resources" {
				continue
			}
			if ident, ok := sel.X.(*ast.Ident); ok {
				resourcesAssigned[ident.Name] = true
			}
		}
		return true
	})

	// Container literals assigned to a simple variable (container := corev1.Container{...}).
	literalVar := map[*ast.CompositeLit]string{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != len(assign.Rhs) {
			return true
		}
		for i, rhs := range assign.Rhs {
			lit := containerLiteral(rhs, corev1Alias)
			if lit == nil {
				continue
			}
			if ident, ok := assign.Lhs[i].(*ast.Ident); ok {
				literalVar[lit] = ident.Name
			}
		}
		return true
	})

	var violations []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !isType(lit.Type, corev1Alias, "Container") {
			return true
		}
		// Empty literals are zero-value error returns, not real containers.
		if len(lit.Elts) == 0 {
			return true
		}
		covered := resourcesAssigned[literalVar[lit]]
		if problem := containerProblem(lit, corev1Alias, covered); problem != "" {
			pos := fset.Position(lit.Pos())
			violations = append(violations, fmt.Sprintf("%s:%d: %s", pos.Filename, pos.Line, problem))
		}
		return true
	})
	return violations
}

// containerLiteral unwraps expr to a corev1.Container composite literal,
// looking through a leading &, or returns nil.
func containerLiteral(expr ast.Expr, corev1Alias string) *ast.CompositeLit {
	if unary, ok := expr.(*ast.UnaryExpr); ok && unary.Op == token.AND {
		expr = unary.X
	}
	lit, ok := expr.(*ast.CompositeLit)
	if !ok || !isType(lit.Type, corev1Alias, "Container") {
		return nil
	}
	return lit
}

// containerProblem returns a description of what the container literal is
// missing, or "" if it is fine. coveredByAssignment means the literal's
// variable receives a .Resources assignment later in the same function.
func containerProblem(lit *ast.CompositeLit, corev1Alias string, coveredByAssignment bool) string {
	resources := fieldValue(lit, "Resources")
	if resources == nil {
		if coveredByAssignment {
			return ""
		}
		return "no Resources and no .Resources assignment on its variable in the enclosing function"
	}

	// Resources set from a helper call or variable: trust it.
	reqLit, ok := resources.(*ast.CompositeLit)
	if !ok {
		return ""
	}

	for _, section := range []string{"Requests", "Limits"} {
		value := fieldValue(reqLit, section)
		if value == nil {
			return "Resources literal missing " + section
		}
		listLit, ok := value.(*ast.CompositeLit)
		if !ok {
			continue
		}
		for _, res := range []string{"ResourceCPU", "ResourceMemory"} {
			if !hasResourceKey(listLit, corev1Alias, res) {
				return fmt.Sprintf("%s missing %s.%s", section, corev1Alias, res)
			}
		}
	}
	return ""
}

// fieldValue returns the value of the named key in a composite literal, or
// nil if the key is absent.
func fieldValue(lit *ast.CompositeLit, name string) ast.Expr {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if ident, ok := kv.Key.(*ast.Ident); ok && ident.Name == name {
			return kv.Value
		}
	}
	return nil
}

// hasResourceKey reports whether a ResourceList literal contains the given
// corev1 resource name (e.g. ResourceCPU) as a key.
func hasResourceKey(lit *ast.CompositeLit, corev1Alias, resourceName string) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if isType(kv.Key, corev1Alias, resourceName) {
			return true
		}
	}
	return false
}

// isType reports whether expr is the selector alias.name.
func isType(expr ast.Expr, alias, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == alias && sel.Sel.Name == name
}
