package taskgateway

import (
	"context"

	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

func (operator *Provisioner) Prepare(ctx context.Context, runtimeID string, spec credentialexec.Spec) (*credentialexec.Boundary, error) {
	if _, err := operator.Authorize(runtimeID, spec.Provider, spec.Binding); err != nil {
		return nil, ErrUnavailable
	}
	boundary, err := credentialexec.Prepare(ctx, spec)
	if err != nil {
		return nil, ErrUnavailable
	}
	grant, err := operator.Provision(ctx, runtimeID, spec.Provider, spec.Binding)
	if err == nil {
		err = boundary.BindGateway(ctx, credentialexec.GatewayCredential{Binding: grant.Binding, BaseURL: grant.BaseURL, Key: grant.Key})
	}
	if err != nil {
		_ = boundary.Close()
		return nil, ErrUnavailable
	}
	return boundary, nil
}
