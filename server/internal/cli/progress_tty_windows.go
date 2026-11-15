//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// isTerminal reports whether f is a Windows console. The renderer emits
// ESC[K, so the console must also accept virtual-terminal sequences; enable
// that mode, and report false (stay silent) if it cannot be enabled.
// Files, pipes and NUL fail GetConsoleMode and are not terminals.
func isTerminal(f *os.File) bool {
	h := windows.Handle(f.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return false
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
