//go:build !linux && !darwin && !windows

package cli

import "os"

// isTerminal: no terminal detection on other platforms; stay silent.
func isTerminal(*os.File) bool { return false }
