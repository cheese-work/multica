package featureflags

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Names only CI watcher code would use: its tables and its generated query.
var ciWatcherStorageNames = []string{
	"ci_watch_config", "ci_failure_event", "ci_watch_action", "ci_signature_owner",
	"gh_installation_budget", "gh_budget_reservation",
	"DeleteExpiredCIFailureEvents",
}

// Allowed to name the tables in S1: their own query file, the workspace
// teardown (it must clear a deleted workspace's rows), migrations and the
// migrate runner's index registry, generated code and tests.
func ciWatcherStorageAllowed(rel string) bool {
	switch {
	case strings.HasSuffix(rel, "_test.go"),
		strings.HasPrefix(rel, "migrations/"),
		strings.HasPrefix(rel, "pkg/db/generated/"),
		rel == "cmd/migrate/main.go", // index registry: names, not access
		rel == "pkg/db/queries/ci_watch.sql",
		rel == "pkg/db/queries/workspace_delete.sql",
		// The installation budget ledger (S3); it is only reached through the
		// watcher, whose entry point checks CIWatcherEnabled.
		rel == "internal/ghbudget/ghbudget.go":
		return true
	}
	return false
}

// ciWatcherStorageUsers lists files under root, other than the allowed ones,
// that name the CI watcher storage.
func ciWatcherStorageUsers(t *testing.T, root string) []string {
	t.Helper()
	var users []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !(strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".sql")) {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if ciWatcherStorageAllowed(rel) {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, name := range ciWatcherStorageNames {
			if strings.Contains(string(body), name) {
				users = append(users, rel+" -> "+name)
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return users
}

// With the ci_watcher flag off no code path reads or writes the new tables.
// S1 adds storage only, so no non-test code outside the allowed files may name
// it; later slices must gate their access on CIWatcherEnabled and extend this
// allow-list deliberately.
func TestNoCodePathTouchesCIWatcherStorageInS1(t *testing.T) {
	if users := ciWatcherStorageUsers(t, "../.."); len(users) > 0 {
		t.Fatalf("code outside the allow-list names CI watcher storage:\n%s", strings.Join(users, "\n"))
	}
}

// Control: the scan does find a file that uses the storage.
func TestCIWatcherStorageScanFindsAUser(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "reader.go"), []byte("package x\nconst q = `SELECT 1 FROM ci_failure_event`\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reader_test.go"), []byte("ci_watch_action"), 0o600); err != nil {
		t.Fatal(err)
	}
	if users := ciWatcherStorageUsers(t, dir); len(users) != 1 || !strings.HasPrefix(users[0], "reader.go") {
		t.Fatalf("scan result = %v, want only reader.go", users)
	}
}
