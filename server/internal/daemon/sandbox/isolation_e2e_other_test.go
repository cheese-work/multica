//go:build sandbox_e2e && !linux

package sandbox

import "testing"

func TestSandboxHidesOtherRunEnviron(t *testing.T) {
	t.Skip("sandbox_e2e: bubblewrap isolation runs on Linux only (X99)")
}
