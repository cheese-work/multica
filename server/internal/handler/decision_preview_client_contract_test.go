package handler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The server renders a preview for every action type in actionPreviewFields. The
// web client drops a preview whose type is not in PREVIEWED_ACTION_TYPES, so the
// card shows "Details unavailable" and Approve stays disabled (CHE-6242). The two
// lists must be equal.
func TestActionPreviewTypesMatchWebClientList(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	consts := map[string]string{}
	var cases []ast.Expr
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.ValueSpec:
					for i, name := range n.Names {
						if lit, ok := valueAt(n.Values, i).(*ast.BasicLit); ok && lit.Kind == token.STRING {
							consts[name.Name], _ = strconv.Unquote(lit.Value)
						}
					}
				case *ast.FuncDecl:
					if n.Name.Name == "actionPreviewFields" {
						for _, stmt := range n.Body.List {
							if sw, ok := stmt.(*ast.SwitchStmt); ok {
								for _, c := range sw.Body.List {
									cases = append(cases, c.(*ast.CaseClause).List...)
								}
							}
						}
					}
				}
				return true
			})
		}
	}
	server := map[string]bool{}
	for _, expr := range cases {
		switch e := expr.(type) {
		case *ast.BasicLit:
			s, _ := strconv.Unquote(e.Value)
			server[s] = true
		case *ast.Ident:
			if v, ok := consts[e.Name]; ok {
				server[v] = true
			} else {
				t.Fatalf("cannot resolve case %s to a string constant", e.Name)
			}
		}
	}
	if len(server) == 0 {
		t.Fatal("found no action types in actionPreviewFields")
	}

	src, err := os.ReadFile("../../../packages/core/issues/decision-action-preview.ts")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)PREVIEWED_ACTION_TYPES = \[(.*?)\] as const`).FindSubmatch(src)
	if block == nil {
		t.Fatal("PREVIEWED_ACTION_TYPES not found in decision-action-preview.ts")
	}
	client := map[string]bool{}
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllSubmatch(block[1], -1) {
		client[string(m[1])] = true
	}

	if missing, extra := diffKeys(server, client), diffKeys(client, server); len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("server preview types and web PREVIEWED_ACTION_TYPES differ: add to the web list %v; remove from the web list %v", missing, extra)
	}
}

func valueAt(values []ast.Expr, i int) ast.Expr {
	if i < len(values) {
		return values[i]
	}
	return nil
}

func diffKeys(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
