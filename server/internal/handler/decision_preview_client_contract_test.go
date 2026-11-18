package handler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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
	server, client := serverPreviewTypes(t), clientTypeList(t, "PREVIEWED_ACTION_TYPES")
	if missing, extra := diffKeys(server, client), diffKeys(client, server); len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("server preview types and web PREVIEWED_ACTION_TYPES differ: add to the web list %v; remove from the web list %v", missing, extra)
	}
}

// serverPreviewTypes is the set of case labels in actionPreviewFields.
func serverPreviewTypes(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	consts := map[string]string{}
	var cases []ast.Expr
	for _, file := range files {
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

	return server
}

// clientTypeList reads the string array `name` from the web client's preview module.
func clientTypeList(t *testing.T, name string) map[string]bool {
	t.Helper()
	src, err := os.ReadFile("../../../packages/core/issues/decision-action-preview.ts")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)` + name + ` = \[(.*?)\] as const`).FindSubmatch(src)
	if block == nil {
		t.Fatalf("%s not found in decision-action-preview.ts", name)
	}
	list := map[string]bool{}
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllSubmatch(block[1], -1) {
		list[string(m[1])] = true
	}
	return list
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
