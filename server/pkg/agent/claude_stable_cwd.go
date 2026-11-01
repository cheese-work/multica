package agent

import (
	"fmt"
	"os/exec"
)

// wrapStableCwd reruns cmd under bubblewrap with realCwd bind-mounted at
// stableCwd (an existing empty dir) and used as cwd. The mount namespace is
// private, so concurrent tasks can share one stableCwd. A symlink does not
// work: Claude Code resolves it and puts the real path in the prompt.
func wrapStableCwd(cmd *exec.Cmd, realCwd, stableCwd string) error {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return fmt.Errorf("stable cwd needs bwrap: %w", err)
	}
	cmd.Args = append([]string{"bwrap", "--dev-bind", "/", "/", "--bind", realCwd, stableCwd, "--chdir", stableCwd, "--", cmd.Path}, cmd.Args[1:]...)
	cmd.Path = bwrap
	return nil
}
