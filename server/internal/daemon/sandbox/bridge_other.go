//go:build !linux

package sandbox

import (
	"os"
	"os/exec"
)

func dieWithBridge(*exec.Cmd) {}

func exitCode(ps *os.ProcessState) int { return ps.ExitCode() }
