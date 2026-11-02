package main

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MJ removal slice C2a (CHE-2760): the Jev/governance server code is gone. This
// guard fails if a route, or an import of the MJ packages from live code, comes
// back. The stacked package-deletion PRs extend goneDirs.
func TestNoMJServerCode(t *testing.T) {
	root := filepath.Join("..", "..")
	banned := []string{"/internal/governance", "/pkg/jev"}
	goneDirs := []string{"internal/governance/receipt", "internal/governance/credential", "cmd/govreplay", "internal/governance/evaluator.go"}

	for _, dir := range goneDirs {
		if _, err := os.Stat(filepath.Join(root, dir)); err == nil {
			t.Errorf("%s must not exist", dir)
		}
	}

	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "no_mj_server_code_test.go") {
			return err
		}
		// Packages that still exist may import each other; only live code is checked.
		slashed := filepath.ToSlash(path)
		for _, own := range []string{"/internal/governance/", "/pkg/jev/", "/cmd/govreplay/"} {
			if strings.Contains(slashed, own) {
				return nil
			}
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		for _, imp := range f.Imports {
			for _, b := range banned {
				if strings.Contains(imp.Path.Value, b) {
					t.Errorf("%s imports %s", path, imp.Path.Value)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	router, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/api/" + "governance", "/" + "governance/config", "/jev" + "/credential"} {
		if strings.Contains(string(router), route) {
			t.Errorf("router.go still registers %s", route)
		}
	}
}
