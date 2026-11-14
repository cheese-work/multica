//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsTerminalFileIsNotTerminal(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "err.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Fatal("a regular file must not be a terminal")
	}
}

func TestIsTerminalPipeIsNotTerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminal(w) || isTerminal(r) {
		t.Fatal("a pipe must not be a terminal")
	}
}

func TestIsTerminalNULIsNotTerminal(t *testing.T) {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Fatal("NUL must not be a terminal")
	}
}

// CONOUT$ opens only when the test process has a console; skip otherwise.
func TestIsTerminalConsoleIsTerminal(t *testing.T) {
	f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no console attached: %v", err)
	}
	defer f.Close()
	if !isTerminal(f) {
		t.Fatal("a console output handle must be a terminal")
	}
}
