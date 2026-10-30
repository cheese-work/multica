//go:build linux

package sandbox

import (
	"os"
	"os/exec"
	"syscall"
)

// dieWithBridge kills the agent if the bridge dies: no network path, no run.
func dieWithBridge(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

func exitCode(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}
