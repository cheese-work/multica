//go:build !linux

package sandbox

import (
	"errors"
	"os/exec"
)

type scope struct{}

func newScope(string) (*scope, error) {
	return nil, errors.New("run cgroups need Linux")
}

func (*scope) attach(*exec.Cmd) {}
func (*scope) releaseFD()       {}
func (*scope) kill() error      { return nil }
func (*scope) close() error     { return nil }
