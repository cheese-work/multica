package handler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Every writer of a workflow's run rules takes the status catalog lock before
// it touches the workflow row, the order an issue's workflow move relies on
// (CHE-5505): a writer that took the row first could deadlock against it.
// A writer is a call to CreateIssueWorkflow, or to UpdateIssueWorkflow with
// MaxRunsPerIssue in its params or built by budgetUpdateParams: UpdateIssueWorkflow
// keeps a rule left out of the write, so a steps-only write does not change the
// cap. The budget start (run_budget_since) is set only by these rows. A writer
// passes when its function locks the catalog before the call, or when it is a
// config-change executor that declares CatalogLock, so lockApproveRows takes
// the catalog first. A delegate holds a writer for its caller, so its calls are
// audited in place of the writer inside it.
func TestWorkflowRunRuleWritersLockTheCatalogFirst(t *testing.T) {
	writers := map[string]bool{"CreateIssueWorkflow": true, "UpdateIssueWorkflow": true}
	catalogLocks := map[string]bool{"LockIssueStatusCatalog": true, "LockIssueStatusCatalogShared": true}
	delegates := map[string]bool{"updateBoundBudget": true, "saveHandoffOnProjectCopy": true}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || delegates[fn.Name.Name] {
				continue
			}
			var catalogAt []token.Pos
			var writes []*ast.CallExpr
			var stack []ast.Node
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch {
				case catalogLocks[sel.Sel.Name]:
					catalogAt = append(catalogAt, call.Pos())
				case delegates[sel.Sel.Name] || (writers[sel.Sel.Name] && writesBudget(call)):
					if declaresCatalogLock(stack) {
						seen++
						return true
					}
					writes = append(writes, call)
				}
				return true
			})
			for _, call := range writes {
				seen++
				locked := false
				for _, at := range catalogAt {
					locked = locked || at < call.Pos()
				}
				if !locked {
					t.Errorf("%s: %s writes a workflow's run rules in %s without the status catalog lock taken first", fset.Position(call.Pos()), call.Fun.(*ast.SelectorExpr).Sel.Name, fn.Name.Name)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("found no workflow writer: the audit looks at the wrong calls")
	}
}

// declaresCatalogLock reports whether the call sits in a registerConfigChange
// call that sets CatalogLock: true.
func declaresCatalogLock(stack []ast.Node) bool {
	for _, node := range stack {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			continue
		}
		if ident, ok := call.Fun.(*ast.Ident); !ok || ident.Name != "registerConfigChange" {
			continue
		}
		found := false
		ast.Inspect(call, func(n ast.Node) bool {
			if kv, ok := n.(*ast.KeyValueExpr); ok {
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "CatalogLock" {
					if val, ok := kv.Value.(*ast.Ident); ok && val.Name == "true" {
						found = true
					}
				}
			}
			return !found
		})
		return found
	}
	return false
}

// writesBudget reports whether the writer call can change the cap: a create
// always sets it, an update only when its params name MaxRunsPerIssue or come
// from budgetUpdateParams.
func writesBudget(call *ast.CallExpr) bool {
	if call.Fun.(*ast.SelectorExpr).Sel.Name == "CreateIssueWorkflow" {
		return true
	}
	found := false
	for _, arg := range call.Args {
		ast.Inspect(arg, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.KeyValueExpr:
				if key, ok := n.Key.(*ast.Ident); ok && key.Name == "MaxRunsPerIssue" {
					found = true
				}
			case *ast.CallExpr:
				if ident, ok := n.Fun.(*ast.Ident); ok && ident.Name == "budgetUpdateParams" {
					found = true
				}
			}
			return !found
		})
	}
	return found
}
