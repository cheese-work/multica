//go:build !linux && !darwin

package cli

import "os"

// isTerminal: the fork update path runs on linux and darwin only; elsewhere stay silent.
func isTerminal(*os.File) bool { return false }
