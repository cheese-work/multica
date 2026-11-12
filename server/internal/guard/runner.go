package guard

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
)

// Result is a finished tool call whose output was captured.
type Result struct {
	Stdout, Stderr string
	Exit           int
}

// Runner runs the real tool. Tests replace it with a fake gh and git.
type Runner interface {
	// Run runs tool with args and captures its output. err is set only when the
	// tool could not start; a nonzero exit is a Result.
	Run(ctx context.Context, tool string, args ...string) (Result, error)
	// Stream runs tool with the caller's stdin and with stdout and stderr
	// inherited, and returns its exit code.
	Stream(ctx context.Context, tool string, args ...string) (int, error)
}

// ExecRunner runs real binaries. Real maps a tool name to the path of the real
// binary; it must not point at a shim on PATH, or the shim would call the guard
// again.
type ExecRunner struct {
	Real           func(tool string) string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

func (r ExecRunner) path(tool string) string {
	if r.Real != nil {
		if p := r.Real(tool); p != "" {
			return p
		}
	}
	return tool
}

func exitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, err
}

func (r ExecRunner) Run(ctx context.Context, tool string, args ...string) (Result, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, r.path(tool), args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r.Stdin, &out, &errb
	if cmd.Stdin == nil {
		cmd.Stdin = os.Stdin
	}
	code, err := exitCode(cmd.Run())
	return Result{Stdout: out.String(), Stderr: errb.String(), Exit: code}, err
}

func (r ExecRunner) Stream(ctx context.Context, tool string, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, r.path(tool), args...)
	cmd.Stdin = r.Stdin
	cmd.Stdout, cmd.Stderr = r.Stdout, r.Stderr
	if cmd.Stdin == nil {
		cmd.Stdin = os.Stdin
	}
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	return exitCode(cmd.Run())
}
