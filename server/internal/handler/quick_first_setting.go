package handler

import (
	"encoding/json"
	"fmt"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// validateQuickFirstSetting checks the quick_first setting on the ordinary
// settings save with the validator the settings patch uses. A missing or null
// key passes: null removes it.
func validateQuickFirstSetting(raw any) error {
	settings, _ := raw.(map[string]any)
	value, ok := settings[issueworkflow.SettingsKeyQuickFirst]
	if !ok || value == nil {
		return nil
	}
	body, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%s: %w", issueworkflow.SettingsKeyQuickFirst, err)
	}
	return issueworkflow.ValidateQuickFirst(body)
}
