package handler

import (
	"fmt"

	"github.com/multica-ai/multica/server/internal/service"
)

// validateHarnessProfileConfig checks the harness_profile key of a workspace
// settings or agent runtime_config object. An absent key or null (inherit) is
// valid.
func validateHarnessProfileConfig(value any) error {
	config, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	v, present := config["harness_profile"]
	if !present || v == nil {
		return nil
	}
	if s, isString := v.(string); !isString || !service.ValidHarnessProfile(s) {
		return fmt.Errorf("harness_profile must be %q or %q, or null to inherit", service.HarnessProfilePersonal, service.HarnessProfileManaged)
	}
	return nil
}
