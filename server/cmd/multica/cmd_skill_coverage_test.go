package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// leafPaths returns "issue comment resolve"-style paths for every runnable
// command that has no subcommands.
func leafPaths(c *cobra.Command, prefix []string, out *[]string) {
	for _, sub := range c.Commands() {
		if sub.Hidden {
			continue
		}
		p := append(append([]string{}, prefix...), sub.Name())
		if len(sub.Commands()) == 0 {
			*out = append(*out, strings.Join(p, " "))
			continue
		}
		leafPaths(sub, p, out)
	}
}

// TestEveryCommandIsDocumentedInTheBuiltinSkill fails when a registered
// command has no `multica <path>` mention in the shipped platform skill, so a
// new verb cannot land without agents being able to learn it.
func TestEveryCommandIsDocumentedInTheBuiltinSkill(t *testing.T) {
	var corpus strings.Builder
	root := filepath.Join("..", "..", "internal", "service", "builtin_skills")
	// The runtime brief is the other place an agent learns a verb.
	briefRoot := filepath.Join("..", "..", "internal", "daemon", "execenv")
	for _, dir := range []string{root, briefRoot} {
		err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(p, "_test.go") {
				return err
			}
			b, err := os.ReadFile(p)
			corpus.Write(b)
			return err
		})
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
	}
	docs := corpus.String()

	var leaves []string
	leafPaths(rootCmd, nil, &leaves)
	var missing []string
	for _, path := range leaves {
		if !strings.Contains(docs, "multica "+path) {
			missing = append(missing, path)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d command(s) have no `multica <command>` mention under internal/service/builtin_skills or the brief; document them in a reference:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}
