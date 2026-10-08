//go:build !linux

package credentialexec

import (
	"context"
	"fmt"
	"os/exec"
)

func Prepare(context.Context, Spec) (*Boundary, error) {
	return nil, fmt.Errorf("%w: only Linux is supported", ErrUnavailable)
}
func (*Boundary) Probe(context.Context) error {
	return fmt.Errorf("%w: only Linux is supported", ErrUnavailable)
}
func (*Boundary) VerifyInputs(context.Context) error {
	return fmt.Errorf("%w: only Linux is supported", ErrUnavailable)
}
func (*Boundary) Wrap(*exec.Cmd) (func(), error) {
	return nil, fmt.Errorf("%w: only Linux is supported", ErrUnavailable)
}
