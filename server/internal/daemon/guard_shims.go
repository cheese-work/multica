package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// guardShimNames are the tools the guard shim directory fronts (CHE-5194 T2).
var guardShimNames = []string{"git", "gh"}

// frontGuardShims puts a guard shim directory first on agentEnv["PATH"] for
// every run (CHE-5194 T2), whatever the runtime and whether or not credential
// broker enforcement is on. It returns the directory, or "" when nothing was
// installed (Windows, no tools).
//
// Each shim execs the first `git`/`gh` found on the PATH it is placed in front
// of. Call it after any credential-aware shims (broker.InstallShims, CHE-3969)
// are on agentEnv["PATH"]: the guard then runs first and hands over to those
// shims, so the broker still receives every push and gh call. With no broker
// shims the target is the host tool and the run behaves as before. The guard
// itself (CHE-5194 T1) hooks into the shim scripts.
func frontGuardShims(agentEnv map[string]string, taskTempDir string) (string, error) {
	if runtime.GOOS == "windows" || strings.TrimSpace(taskTempDir) == "" {
		return "", nil
	}
	path, ok := agentEnv["PATH"]
	if !ok {
		path = os.Getenv("PATH")
	}
	dir := filepath.Join(taskTempDir, "guard-bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("guard shims: %w", err)
	}
	installed := false
	for _, name := range guardShimNames {
		target := lookPathIn(path, name)
		if target == "" || strings.ContainsAny(target, "'\n") {
			continue
		}
		script := fmt.Sprintf("#!/bin/sh\nexec '%s' \"$@\"\n", target)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			return "", fmt.Errorf("guard shims: %w", err)
		}
		installed = true
	}
	if !installed {
		return "", nil
	}
	agentEnv["PATH"] = dir + string(os.PathListSeparator) + path
	return dir, nil
}

// lookPathIn returns the absolute path of the first executable file called name
// in the absolute directories of path, or "".
func lookPathIn(path, name string) string {
	for _, d := range filepath.SplitList(path) {
		if !filepath.IsAbs(d) {
			continue
		}
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return ""
}
