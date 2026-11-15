//go:build !linux

package runbinding

import (
	"context"
	"errors"
)

type Publisher struct{ ID string }

func New(context.Context, Spec) (*Publisher, error) {
	return nil, errors.New("managed host bindings require Linux")
}
func (*Publisher) Publish(int) error { return errors.New("managed host bindings require Linux") }
func (*Publisher) Close()            {}
