package taskgateway

import (
	"context"

	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

func (operator *Provisioner) Prepare(ctx context.Context, runtimeID string, spec credentialexec.Spec) (*credentialexec.Boundary, error) {
	if _, err := operator.Authorize(runtimeID, spec.Provider, spec.Binding); err != nil {
		return nil, ErrUnavailable
	}
	return PrepareHandoff(ctx, spec, func(ctx context.Context) (Grant, error) {
		return operator.Provision(ctx, runtimeID, spec.Provider, spec.Binding)
	})
}

func PrepareHandoff(ctx context.Context, spec credentialexec.Spec, fetch func(context.Context) (Grant, error)) (*credentialexec.Boundary, error) {
	if fetch == nil {
		return nil, ErrUnavailable
	}
	boundary, err := credentialexec.Prepare(ctx, spec)
	if err != nil {
		return nil, ErrUnavailable
	}
	grant, err := fetch(ctx)
	if err == nil {
		err = boundary.BindGateway(ctx, credentialexec.GatewayCredential{Binding: grant.Binding, BaseURL: grant.BaseURL, Key: grant.Key})
	}
	if err != nil {
		_ = boundary.Close()
		return nil, ErrUnavailable
	}
	return boundary, nil
}
