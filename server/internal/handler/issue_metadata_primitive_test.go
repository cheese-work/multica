package handler

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func metadataSnapshot(t *testing.T, issueID string) (string, int64) {
	t.Helper()
	var md string
	var rev int64
	if err := testPool.QueryRow(context.Background(), `SELECT metadata::text, revision FROM issue WHERE id = $1`, issueID).Scan(&md, &rev); err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	return md, rev
}

// An internal write of a non-primitive value fails and leaves the row alone.
func TestSetIssueMetadataValueRejectsNonPrimitive(t *testing.T) {
	issueID := createMetadataTestIssue(t, "writer rejects non-primitive")
	wantMD, wantRev := metadataSnapshot(t, issueID)
	for name, v := range map[string]any{
		"object": map[string]any{"a": 1},
		"array":  []string{"x"},
		"null":   nil,
	} {
		if _, err := setIssueMetadataValue(context.Background(), testHandler.Queries, parseUUID(testWorkspaceID), parseUUID(issueID), "bad_"+name, v); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
	if _, err := setIssueMetadataValue(context.Background(), testHandler.Queries, parseUUID(testWorkspaceID), parseUUID(issueID), "bad key!", "x"); err == nil {
		t.Error("bad key: expected error, got nil")
	}
	if md, rev := metadataSnapshot(t, issueID); md != wantMD || rev != wantRev {
		t.Fatalf("row changed: metadata %s -> %s, revision %d -> %d", wantMD, md, wantRev, rev)
	}
	if _, err := setIssueMetadataValue(context.Background(), testHandler.Queries, parseUUID(testWorkspaceID), parseUUID(issueID), "ok", "fine"); err != nil {
		t.Fatalf("primitive write: %v", err)
	}
}

// The DB rejects a direct SQL write of a non-primitive value.
func TestIssueMetadataCheckRejectsNonPrimitive(t *testing.T) {
	issueID := createMetadataTestIssue(t, "db check non-primitive")
	for name, md := range map[string]string{
		"object": `{"a":{"b":1}}`,
		"array":  `{"a":[1]}`,
		"null":   `{"a":null}`,
	} {
		_, err := testPool.Exec(context.Background(), `UPDATE issue SET metadata = $2::jsonb WHERE id = $1`, issueID, md)
		if !isCheckViolation(err) {
			t.Errorf("%s: expected check violation, got %v", name, err)
		}
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET metadata = '{"a":"s","b":1,"c":true}'::jsonb WHERE id = $1`, issueID); err != nil {
		t.Fatalf("primitive update: %v", err)
	}
}

// Only setIssueMetadataValue may reference the SetIssueMetadataKey query
// (call or method value), whatever the receiver or context spelling.
func TestIssueMetadataSingleWriter(t *testing.T) {
	var refs []string
	err := filepath.WalkDir("../..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "generated" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			owner := "<package level>"
			if fn != nil {
				owner = fn.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "SetIssueMetadataKey" {
					return true
				}
				// router.go binds the HTTP handler method of the same name, not the query.
				if x, isIdent := sel.X.(*ast.Ident); isIdent && x.Name == "h" && filepath.Base(path) == "router.go" {
					return true
				}
				refs = append(refs, fmt.Sprintf("%s:%d in %s", filepath.ToSlash(path), fset.Position(sel.Pos()).Line, owner))
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || !strings.HasPrefix(refs[0], "../../internal/handler/issue_metadata.go:") || !strings.HasSuffix(refs[0], " in setIssueMetadataValue") {
		t.Fatalf("want exactly one SetIssueMetadataKey reference, inside setIssueMetadataValue; got: %v", refs)
	}
}
