package auth

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every server file that verifies a session JWT must also check its
// generation, or a token revoked by passkey recovery keeps working there. A new
// verifier fails this test until it calls CheckSessionGeneration.
func TestEveryJWTVerifierChecksTheSessionGeneration(t *testing.T) {
	root := filepath.Join("..", "..")
	// session.go holds the shared parser; its callers are the files below.
	exempt := map[string]bool{filepath.Join("internal", "auth", "session.go"): true}
	verifiers := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(src)
		if !strings.Contains(text, "jwt.Parse(") && !strings.Contains(text, "jwt.ParseWithClaims(") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if exempt[rel] {
			return nil
		}
		verifiers++
		if !strings.Contains(strings.ToLower(text), "checksessiongeneration") {
			t.Errorf("%s verifies a JWT without CheckSessionGeneration", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if verifiers < 3 {
		t.Fatalf("found %d verifier files, want the Auth, DaemonAuth and realtime ones: the scan is broken", verifiers)
	}
}
