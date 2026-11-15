//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
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

// openConsole opens the console output buffer read-write: GetConsoleMode needs
// GENERIC_READ on the handle. A process without a console
// (a CI runner service) gets a hidden one from AllocConsole first.
func openConsole(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		// AllocConsole fails harmlessly if a console is already attached.
		_, _, _ = windows.NewLazySystemDLL("kernel32.dll").NewProc("AllocConsole").Call()
		f, err = os.OpenFile("CONOUT$", os.O_RDWR, 0)
	}
	if err != nil {
		t.Skipf("no console available: %v", err)
	}
	return f
}

func TestIsTerminalConsoleIsTerminal(t *testing.T) {
	f := openConsole(t)
	defer f.Close()
	if !isTerminal(f) {
		t.Fatal("a console output handle must be a terminal")
	}
}

// The renderer emits ESC[K, so isTerminal must leave virtual-terminal
// processing enabled on the console it accepts.
func TestIsTerminalConsoleEnablesVirtualTerminal(t *testing.T) {
	f := openConsole(t)
	defer f.Close()
	if !isTerminal(f) {
		t.Fatal("a console output handle must be a terminal")
	}
	var mode uint32
	if err := windows.GetConsoleMode(windows.Handle(f.Fd()), &mode); err != nil {
		t.Fatal(err)
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING == 0 {
		t.Fatalf("virtual-terminal processing not enabled: mode=%#x", mode)
	}
}
