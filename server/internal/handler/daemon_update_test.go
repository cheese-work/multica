package handler

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDaemonUpdateSourceRejectsTamperedBinary(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	dir := t.TempDir()
	// sha256 of "ok"; the file on disk holds different bytes.
	manifest := `{"commit":"` + commit + `","version":"1","binaries":{"linux-amd64":{"sha256":"2689367b205c16ce32ed4200942b8b8b1e262dfc70d9bc9fbc77c49699a4f1df","size":2}}}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "multica-linux-amd64"), []byte("no"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDaemonUpdateSource(dir, commit); err == nil {
		t.Fatal("tampered binary was accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "multica-linux-amd64"), []byte("ok"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDaemonUpdateSource(dir, commit); err != nil {
		t.Fatalf("matching binary rejected: %v", err)
	}
}
