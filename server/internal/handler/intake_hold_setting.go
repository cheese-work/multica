package handler

import (
	"encoding/json"
	"fmt"

	"github.com/multica-ai/multica/server/internal/intakehold"
)

// validateIntakeHoldSetting checks the intake_hold setting on the ordinary
// settings save with the validator the settings patch uses. A missing or null
// key passes: null removes it.
func validateIntakeHoldSetting(raw any) error {
	settings, _ := raw.(map[string]any)
	value, ok := settings[intakehold.SettingKey]
	if !ok || value == nil {
		return nil
	}
	body, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%s: %w", intakehold.SettingKey, err)
	}
	return intakehold.Validate(body)
}
