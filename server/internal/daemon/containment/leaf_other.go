//go:build !linux

package containment

import "os/exec"

// Options is the input to Create.
type Options struct {
	Root, StateDir, BootID, DaemonInstance, TaskID, ClaimGeneration, LaunchProfileID string
	Charge                                                                           int64
}

// Leaf does not exist off Linux.
type Leaf struct{}

func Create(Options) (*Leaf, error)    { return nil, ErrUnsupported }
func (*Leaf) Record() Record           { return Record{} }
func (*Leaf) Attach(*exec.Cmd) error   { return ErrUnsupported }
func (*Leaf) Wrap(*exec.Cmd) error     { return ErrUnsupported }
func (*Leaf) RecordClaim(string) error { return ErrUnsupported }
func (*Leaf) End() error               { return nil }
func (*Leaf) EventsFile() string       { return "" }
func DefaultRoot() (string, error)     { return "", ErrUnsupported }
func ReadBootID() (string, error)      { return "", ErrUnsupported }
