package handler

import (
	"encoding/json"
	"fmt"

	"github.com/multica-ai/multica/server/internal/guard"
)

// validateGuardPolicySetting refuses a stored tool-guard policy (settings key
// guard_policy) the daemon's guard would refuse to load: wrong version, unknown
// field, bad plugin or bad scope layer. Settings without the key pass. A null
// removes the key and passes.
func validateGuardPolicySetting(raw any) error {
	settings, _ := raw.(map[string]any)
	value, ok := settings["guard_policy"]
	if !ok || value == nil {
		return nil
	}
	body, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("guard_policy: %w", err)
	}
	_, err = guard.ParsePolicy(body)
	return err
}
