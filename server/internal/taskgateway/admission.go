package taskgateway

import (
	"slices"

	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

func (operator *Provisioner) Admit(runtimeID, provider string, binding credentialexec.Binding, capabilities []string) (bool, error) {
	required := operator.Requires(runtimeID)
	if operator != nil {
		for _, policy := range operator.policies {
			required = required || policy.TaskID == binding.TaskID
		}
	}
	if !required {
		return false, nil
	}
	if !slices.Contains(capabilities, Capability) {
		return true, ErrUnavailable
	}
	_, err := operator.Authorize(runtimeID, provider, binding)
	return true, err
}
