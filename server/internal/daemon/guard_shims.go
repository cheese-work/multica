package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// guardShimNames are the tools the guard shim directory fronts (CHE-5194 T2).
var guardShimNames = []string{"git", "gh"}

// installGuardShims writes a `git` and a `gh` shim into <taskTempDir>/guard-bin
// and returns that directory. Each shim only execs the real tool found on the
// daemon's PATH, so a run behaves as before; the guard (CHE-5194 T1) hooks in
// here. It does not depend on credential-broker enforcement, which installs its
// own credential-aware shims when it is on. A tool missing from the host gets no
// shim. It returns "" when nothing was installed (Windows, no tools).
func installGuardShims(taskTempDir string) (string, error) {
	if runtime.GOOS == "windows" || strings.TrimSpace(taskTempDir) == "" {
		return "", nil
	}
	dir := filepath.Join(taskTempDir, "guard-bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("guard shims: %w", err)
	}
	installed := false
	for _, name := range guardShimNames {
		real, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if real, err = filepath.Abs(real); err != nil || strings.ContainsAny(real, "'\n") {
			continue
		}
		script := fmt.Sprintf("#!/bin/sh\nexec '%s' \"$@\"\n", real)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			return "", fmt.Errorf("guard shims: %w", err)
		}
		installed = true
	}
	if !installed {
		return "", nil
	}
	return dir, nil
}

// withGuardShims puts dir first on the agent's PATH. Claude and Codex both take
// their tool-call PATH from this map (Codex via its shell env allowlist).
func withGuardShims(agentEnv map[string]string, dir string) {
	if dir == "" {
		return
	}
	path, ok := agentEnv["PATH"]
	if !ok {
		path = os.Getenv("PATH")
	}
	agentEnv["PATH"] = dir + string(os.PathListSeparator) + path
}
